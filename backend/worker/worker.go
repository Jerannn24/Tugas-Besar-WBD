package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Jerannn24/Tugas-Besar-WBD.git/executioncontract"
	"github.com/Jerannn24/Tugas-Besar-WBD.git/jobs"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// Config configures runtime behavior for a Worker instance.
type Config struct {
	WorkerID         string
	LeaseDuration    time.Duration
	RenewInterval    time.Duration
	PollInterval     time.Duration
	RecoveryInterval time.Duration
	DrainTimeout     time.Duration
	MaxAttempts      int
	RetryDelay       time.Duration
	RecoveryLimit    int
	Logger           func(event string, fields map[string]any)
}

type activeWork struct {
	kind   string
	cancel context.CancelFunc
	done   chan struct{}
}

// Worker executes persistent jobs and performs receipt-only recovery in a sequential loop.
type Worker struct {
	jobs             *jobs.Service
	deps             jobs.ExecutionDeps
	cfg              Config
	stopCh           chan struct{}
	stopOnce         sync.Once
	stopping         atomic.Bool
	shutdownDone     chan struct{}
	loopDone         chan struct{}
	activeMu         sync.Mutex
	activeWork       *activeWork
	lastRecoveryTime time.Time
	stdCursor        string
	batchCursor      string
}

// New creates a runnable Worker with required production dependencies.
func New(jobsSvc *jobs.Service, deps jobs.ExecutionDeps, cfg Config) (*Worker, error) {
	if jobsSvc == nil {
		return nil, errors.New("jobs service is required")
	}
	if deps.Gate == nil {
		return nil, errors.New("workflow gate is required")
	}
	if deps.Acquire == nil {
		return nil, errors.New("target acquire function is required")
	}
	if cfg.WorkerID == "" {
		cfg.WorkerID = "worker-" + uuid.NewString()
	}
	if cfg.LeaseDuration <= 0 {
		cfg.LeaseDuration = 30 * time.Second
	}
	if cfg.RenewInterval <= 0 {
		cfg.RenewInterval = cfg.LeaseDuration / 3
		if cfg.RenewInterval < time.Millisecond {
			cfg.RenewInterval = time.Millisecond
		}
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 200 * time.Millisecond
	}
	if cfg.RecoveryInterval <= 0 {
		cfg.RecoveryInterval = 5 * time.Second
	}
	if cfg.DrainTimeout <= 0 {
		cfg.DrainTimeout = 10 * time.Second
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = jobs.DefaultMaxAttempts
	}
	if cfg.RetryDelay <= 0 {
		cfg.RetryDelay = jobs.DefaultRetryDelay
	}
	if cfg.RecoveryLimit <= 0 {
		cfg.RecoveryLimit = 10
	}
	if deps.Timeout <= 0 {
		deps.Timeout = 30 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = defaultLogger
	}

	return &Worker{
		jobs:         jobsSvc,
		deps:         deps,
		cfg:          cfg,
		stopCh:       make(chan struct{}),
		shutdownDone: make(chan struct{}),
		loopDone:     make(chan struct{}),
	}, nil
}

func (w *Worker) registerActive(kind string, cancel context.CancelFunc) (*activeWork, bool) {
	w.activeMu.Lock()
	defer w.activeMu.Unlock()
	if w.stopping.Load() || w.activeWork != nil {
		return nil, false
	}
	aw := &activeWork{
		kind:   kind,
		cancel: cancel,
		done:   make(chan struct{}),
	}
	w.activeWork = aw
	return aw, true
}

func (w *Worker) unregisterActive(aw *activeWork) {
	close(aw.done)
	w.activeMu.Lock()
	if w.activeWork == aw {
		w.activeWork = nil
	}
	w.activeMu.Unlock()
}

// Run starts the worker loop until ctx is canceled or Stop is called. Call Run once.
func (w *Worker) Run(ctx context.Context) error {
	w.log("worker_started", map[string]any{
		"worker_id":         w.cfg.WorkerID,
		"lease_duration_ms": w.cfg.LeaseDuration.Milliseconds(),
		"renew_interval_ms": w.cfg.RenewInterval.Milliseconds(),
		"drain_timeout_ms":  w.cfg.DrainTimeout.Milliseconds(),
	})

	// The loop retains caller values but execution is canceled only after draining.
	go func() {
		defer close(w.loopDone)
		w.runLoop(context.WithoutCancel(ctx))
	}()
	select {
	case <-ctx.Done():
		w.Stop()
	case <-w.stopCh:
	}
	<-w.shutdownDone
	w.log("shutdown_complete", map[string]any{"worker_id": w.cfg.WorkerID})
	return nil
}

// Done closes once the loop, including canceled execution and lease renewal, has exited.
// Run can return first when a dependency does not respond to cancellation promptly.
func (w *Worker) Done() <-chan struct{} { return w.loopDone }

func (w *Worker) runLoop(ctx context.Context) {
	startupRecCtx, cancelStartupRec := context.WithCancel(ctx)
	if aw, ok := w.registerActive("recovery", cancelStartupRec); ok {
		if _, err := w.ReconcileUnresolved(startupRecCtx); err != nil && !errors.Is(err, context.Canceled) {
			w.log("startup_recovery_error", map[string]any{"error_code": classifyError(err)})
		}
		cancelStartupRec()
		w.unregisterActive(aw)
	} else {
		cancelStartupRec()
	}
	if w.stopping.Load() {
		return
	}
	w.lastRecoveryTime = time.Now()

	for !w.stopping.Load() {
		didWork, err := w.Step(ctx)
		if err != nil && !errors.Is(err, context.Canceled) {
			w.log("step_error", map[string]any{"error_code": classifyError(err)})
		}
		if w.stopping.Load() {
			return
		}
		if !didWork {
			select {
			case <-w.stopCh:
				return
			case <-time.After(w.cfg.PollInterval):
			}
		}
	}
}

// Stop initiates graceful shutdown by stopping new claims and draining active execution.
func (w *Worker) Stop() {
	w.stopOnce.Do(func() {
		w.activeMu.Lock()
		w.stopping.Store(true)
		aw := w.activeWork
		w.activeMu.Unlock()
		close(w.stopCh)
		w.log("shutdown_started", map[string]any{"worker_id": w.cfg.WorkerID})

		go func() {
			defer close(w.shutdownDone)
			if aw == nil {
				return
			}
			if aw.kind == "recovery" || aw.kind == "claim" {
				w.log("shutdown_cancel_active_"+aw.kind, map[string]any{"worker_id": w.cfg.WorkerID})
				aw.cancel()
			} else {
				w.log("shutdown_drain_active_job", map[string]any{
					"worker_id":        w.cfg.WorkerID,
					"drain_timeout_ms": w.cfg.DrainTimeout.Milliseconds(),
				})
			}
			timer := time.NewTimer(w.cfg.DrainTimeout)
			defer timer.Stop()
			select {
			case <-aw.done:
				if aw.kind == "exec" {
					w.log("shutdown_drained_cleanly", map[string]any{"worker_id": w.cfg.WorkerID})
				}
			case <-timer.C:
				if aw.kind == "exec" {
					w.log("shutdown_drain_timeout_canceling", map[string]any{"worker_id": w.cfg.WorkerID})
					aw.cancel()
					w.log("shutdown_drain_abandoned", map[string]any{"worker_id": w.cfg.WorkerID})
				}
			}
		}()
	})
}

// Step performs one sequential worker cycle: runs recovery if due, then claims and executes at most one job.
func (w *Worker) Step(ctx context.Context) (bool, error) {
	if w.stopping.Load() || ctx.Err() != nil {
		return false, ctx.Err()
	}

	didWork := false

	if time.Since(w.lastRecoveryTime) >= w.cfg.RecoveryInterval {
		w.lastRecoveryTime = time.Now()
		recCtx, cancelRec := context.WithCancel(ctx)
		aw, ok := w.registerActive("recovery", cancelRec)
		if ok {
			n, err := w.ReconcileUnresolved(recCtx)
			cancelRec()
			w.unregisterActive(aw)
			if err != nil && !errors.Is(err, context.Canceled) {
				w.log("recovery_error", map[string]any{"error_code": classifyError(err)})
			}
			if n > 0 {
				didWork = true
			}
		} else {
			cancelRec()
		}
	}

	if w.stopping.Load() || ctx.Err() != nil {
		return didWork, ctx.Err()
	}

	claimCtx, cancelClaim := context.WithCancel(ctx)
	awClaim, ok := w.registerActive("claim", cancelClaim)
	if !ok {
		cancelClaim()
		return didWork, nil
	}

	claim, err := w.jobs.ClaimJob(claimCtx, jobs.ClaimRequest{
		WorkerID:      w.cfg.WorkerID,
		LeaseDuration: w.cfg.LeaseDuration,
		MaxAttempts:   w.cfg.MaxAttempts,
		RetryDelay:    w.cfg.RetryDelay,
	})
	cancelClaim()
	w.unregisterActive(awClaim)

	if err != nil {
		if errors.Is(err, context.Canceled) && w.stopping.Load() {
			return didWork, nil
		}
		return didWork, fmt.Errorf("claim job: %w", err)
	}
	if claim == nil {
		return didWork, nil
	}

	didWork = true
	fields := map[string]any{
		"worker_id":    w.cfg.WorkerID,
		"job_id":       claim.JobID,
		"execution_id": claim.ExecutionID,
		"attempt_id":   claim.AttemptID,
		"database_id":  claim.DatabaseID,
		"target_id":    claim.TargetID,
		"mode":         string(claim.Mode),
		"action":       string(claim.Action),
		"direction":    string(claim.Direction),
		"attempt_no":   claim.AttemptNo,
	}
	if claim.Traceparent != "" {
		fields["traceparent"] = claim.Traceparent
	}
	if claim.Tracestate != "" {
		fields["tracestate"] = claim.Tracestate
	}
	w.log("job_claimed", fields)

	outcome, execErr := w.executeWithRenewal(ctx, claim)
	if execErr != nil {
		failFields := map[string]any{
			"worker_id":  w.cfg.WorkerID,
			"job_id":     claim.JobID,
			"attempt_id": claim.AttemptID,
			"error_code": classifyError(execErr),
		}
		if claim.Traceparent != "" {
			failFields["traceparent"] = claim.Traceparent
		}
		if claim.Tracestate != "" {
			failFields["tracestate"] = claim.Tracestate
		}
		w.log("job_execution_failed", failFields)
	} else if outcome != nil {
		doneFields := map[string]any{
			"worker_id":  w.cfg.WorkerID,
			"job_id":     claim.JobID,
			"attempt_id": claim.AttemptID,
			"status":     string(outcome.Status),
			"receipt":    outcome.Receipt,
		}
		if claim.Traceparent != "" {
			doneFields["traceparent"] = claim.Traceparent
		}
		if claim.Tracestate != "" {
			doneFields["tracestate"] = claim.Tracestate
		}
		w.log("job_execution_completed", doneFields)
	}

	return didWork, execErr
}

// ReconcileUnresolved discovers eligible expired/unknown candidates without re-dispatch/stealing live jobs.
func (w *Worker) ReconcileUnresolved(ctx context.Context) (int, error) {
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}

	reconciledCount := 0

	stdIDs, err := w.jobs.ListUnresolvedStandardJobIDs(ctx, w.cfg.RecoveryLimit, w.stdCursor)
	if err != nil {
		return reconciledCount, fmt.Errorf("list unresolved standard jobs: %w", err)
	}
	if ctx.Err() != nil || w.stopping.Load() {
		return reconciledCount, ctx.Err()
	}
	if len(stdIDs) > 0 {
		w.stdCursor = stdIDs[len(stdIDs)-1]
	} else if w.stdCursor != "" {
		w.stdCursor = ""
	}

	for _, id := range stdIDs {
		if ctx.Err() != nil || w.stopping.Load() {
			return reconciledCount, ctx.Err()
		}
		outcome, rerr := w.jobs.ReconcileStandardJobByID(ctx, id, w.deps)
		if rerr != nil {
			w.log("reconcile_standard_job_failed", map[string]any{
				"worker_id":  w.cfg.WorkerID,
				"job_id":     id,
				"error_code": classifyError(rerr),
			})
			continue
		}
		if outcome != nil && outcome.Reconciled && outcome.Status != executioncontract.Unknown {
			reconciledCount++
			w.log("reconcile_standard_job_succeeded", map[string]any{
				"worker_id": w.cfg.WorkerID,
				"job_id":    id,
				"status":    string(outcome.Status),
			})
		} else if outcome != nil {
			w.log("reconcile_standard_job_unresolved", map[string]any{
				"worker_id": w.cfg.WorkerID,
				"job_id":    id,
				"status":    string(outcome.Status),
			})
		}
	}

	if ctx.Err() != nil || w.stopping.Load() {
		return reconciledCount, ctx.Err()
	}
	batchIDs, err := w.jobs.ListUnresolvedBatchJobIDs(ctx, w.cfg.RecoveryLimit, w.batchCursor)
	if err != nil {
		return reconciledCount, fmt.Errorf("list unresolved batch jobs: %w", err)
	}
	if ctx.Err() != nil || w.stopping.Load() {
		return reconciledCount, ctx.Err()
	}
	if len(batchIDs) > 0 {
		w.batchCursor = batchIDs[len(batchIDs)-1]
	} else if w.batchCursor != "" {
		w.batchCursor = ""
	}

	for _, id := range batchIDs {
		if ctx.Err() != nil || w.stopping.Load() {
			return reconciledCount, ctx.Err()
		}
		outcome, rerr := w.jobs.ReconcileBatchJobByID(ctx, id, w.deps)
		if rerr != nil {
			w.log("reconcile_batch_job_failed", map[string]any{
				"worker_id":  w.cfg.WorkerID,
				"job_id":     id,
				"error_code": classifyError(rerr),
			})
			continue
		}
		if outcome != nil && outcome.Reconciled && outcome.Status != executioncontract.Unknown {
			reconciledCount++
			w.log("reconcile_batch_job_succeeded", map[string]any{
				"worker_id": w.cfg.WorkerID,
				"job_id":    id,
				"status":    string(outcome.Status),
			})
		} else if outcome != nil {
			w.log("reconcile_batch_job_unresolved", map[string]any{
				"worker_id": w.cfg.WorkerID,
				"job_id":    id,
				"status":    string(outcome.Status),
			})
		}
	}

	return reconciledCount, nil
}

type traceContextKey struct{}

// TraceFromContext returns the W3C context stored with the job.
func TraceFromContext(ctx context.Context) (executioncontract.TraceContext, bool) {
	trace, ok := ctx.Value(traceContextKey{}).(executioncontract.TraceContext)
	return trace, ok
}

func (w *Worker) executeWithRenewal(ctx context.Context, claim *jobs.Claim) (*jobs.ExecutionOutcome, error) {
	execCtx, cancelExec := context.WithCancel(ctx)
	defer cancelExec()

	aw, ok := w.registerActive("exec", cancelExec)
	if !ok {
		return nil, errors.New("worker stopped before execution registration")
	}
	defer w.unregisterActive(aw)

	if claim.Traceparent != "" || claim.Tracestate != "" {
		execCtx = context.WithValue(execCtx, traceContextKey{}, executioncontract.TraceContext{
			Traceparent: claim.Traceparent,
			Tracestate:  claim.Tracestate,
		})
	}

	execFields := map[string]any{
		"worker_id":  w.cfg.WorkerID,
		"job_id":     claim.JobID,
		"attempt_id": claim.AttemptID,
	}
	if claim.Traceparent != "" {
		execFields["traceparent"] = claim.Traceparent
	}
	if claim.Tracestate != "" {
		execFields["tracestate"] = claim.Tracestate
	}
	w.log("job_executing", execFields)

	renewTicker := time.NewTicker(w.cfg.RenewInterval)
	defer renewTicker.Stop()

	renewDone := make(chan struct{})
	go func() {
		defer close(renewDone)
		for {
			select {
			case <-execCtx.Done():
				return
			case <-renewTicker.C:
				if execCtx.Err() != nil {
					return
				}
				renewCtx, renewCancel := context.WithTimeout(execCtx, 5*time.Second)
				res, err := w.jobs.RenewLease(renewCtx, jobs.RenewLeaseRequest{
					JobID:         claim.JobID,
					LeaseToken:    claim.LeaseToken,
					LeaseDuration: w.cfg.LeaseDuration,
				})
				renewCancel()

				if err != nil || res == nil || !res.Renewed {
					failFields := map[string]any{
						"worker_id":  w.cfg.WorkerID,
						"job_id":     claim.JobID,
						"attempt_id": claim.AttemptID,
						"error_code": classifyError(err),
					}
					if claim.Traceparent != "" {
						failFields["traceparent"] = claim.Traceparent
					}
					if claim.Tracestate != "" {
						failFields["tracestate"] = claim.Tracestate
					}
					w.log("lease_renewal_failed", failFields)
					cancelExec()
					return
				}

				renFields := map[string]any{
					"worker_id":  w.cfg.WorkerID,
					"job_id":     claim.JobID,
					"attempt_id": claim.AttemptID,
					"expires_at": res.LeaseExpiresAt.Format(time.RFC3339Nano),
				}
				if claim.Traceparent != "" {
					renFields["traceparent"] = claim.Traceparent
				}
				if claim.Tracestate != "" {
					renFields["tracestate"] = claim.Tracestate
				}
				w.log("lease_renewed", renFields)
			}
		}
	}()

	start := time.Now()
	var outcome *jobs.ExecutionOutcome
	var err error

	switch claim.Mode {
	case executioncontract.Batch:
		outcome, err = w.jobs.ExecuteBatchJob(execCtx, claim, w.deps)
	case executioncontract.Standard:
		outcome, err = w.jobs.ExecuteStandardJob(execCtx, claim, w.deps)
	default:
		outcome, err = w.jobs.ExecuteStandardJob(execCtx, claim, w.deps)
	}

	cancelExec()
	<-renewDone

	duration := time.Since(start)
	if err != nil {
		finFields := map[string]any{
			"worker_id":   w.cfg.WorkerID,
			"job_id":      claim.JobID,
			"duration_ms": duration.Milliseconds(),
			"error_code":  classifyError(err),
		}
		if claim.Traceparent != "" {
			finFields["traceparent"] = claim.Traceparent
		}
		if claim.Tracestate != "" {
			finFields["tracestate"] = claim.Tracestate
		}
		w.log("job_finished_error", finFields)
	} else if outcome != nil {
		finFields := map[string]any{
			"worker_id":   w.cfg.WorkerID,
			"job_id":      claim.JobID,
			"duration_ms": duration.Milliseconds(),
			"status":      string(outcome.Status),
		}
		if claim.Traceparent != "" {
			finFields["traceparent"] = claim.Traceparent
		}
		if claim.Tracestate != "" {
			finFields["tracestate"] = claim.Tracestate
		}
		w.log("job_finished_success", finFields)
	}

	return outcome, err
}

func (w *Worker) log(event string, fields map[string]any) {
	if w.cfg.Logger != nil {
		w.cfg.Logger(event, fields)
	}
}

func defaultLogger(event string, fields map[string]any) {
	b, err := json.Marshal(map[string]any{
		"time":   time.Now().UTC().Format(time.RFC3339Nano),
		"event":  event,
		"fields": fields,
	})
	if err == nil {
		_, _ = os.Stderr.Write(append(b, '\n'))
	}
}

func classifyError(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.Canceled) {
		return "CONTEXT_CANCELED"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "CONTEXT_DEADLINE_EXCEEDED"
	}
	var cErr executioncontract.ContractError
	if errors.As(err, &cErr) {
		if cErr.Code != "" {
			return cErr.Code
		}
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		if pgErr.Code != "" {
			return "SQLSTATE:" + pgErr.Code
		}
	}
	return "INTERNAL_ERROR"
}
