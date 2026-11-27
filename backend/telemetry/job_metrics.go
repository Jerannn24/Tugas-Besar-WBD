package telemetry

import (
	"context"
	"database/sql"
	"log/slog"
	"time"

	"github.com/Jerannn24/Tugas-Besar-WBD.git/executioncontract"
)

func StartJobMetrics(ctx context.Context, db *sql.DB, registry *Registry, interval time.Duration) func() {
	if interval <= 0 {
		interval = 10 * time.Second
	}
	if registry == nil {
		registry = DefaultRegistry()
	}
	workerCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		state := jobMetricState{
			seenAttempts: make(map[string]struct{}),
			pending:      make(map[string]pendingAttempt),
			lastBatches:  map[string]int64{"up": 0, "down": 0},
			lastRows:     map[string]int64{"up": 0, "down": 0},
		}
		collectJobMetrics(workerCtx, db, registry, &state)
		for {
			select {
			case <-workerCtx.Done():
				return
			case <-ticker.C:
				collectJobMetrics(workerCtx, db, registry, &state)
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

type pendingAttempt struct {
	action string
	start  time.Time
}

type jobMetricState struct {
	seenAttempts map[string]struct{}
	pending      map[string]pendingAttempt
	lastRetries  int64
	lastBatches  map[string]int64
	lastRows     map[string]int64
}

func collectJobMetrics(ctx context.Context, db *sql.DB, registry *Registry, state *jobMetricState) {
	if db == nil || registry == nil {
		return
	}
	now := time.Now()
	counts, err := db.QueryContext(ctx, `SELECT status, count(*) FROM app.jobs GROUP BY status`)
	if err != nil {
		DefaultLogger().Warn(ctx, "observability.jobs_status_failed", slog.String("error", SafeErrorText(err)))
		return
	}
	statuses := map[string]float64{
		"queued": 0, "running": 0, "retry_wait": 0, "succeeded": 0,
		"failed": 0, "partial": 0, "partially_reverted": 0, "unknown": 0,
	}
	for counts.Next() {
		var status string
		var count float64
		if err := counts.Scan(&status, &count); err != nil {
			_ = counts.Close()
			return
		}
		if _, ok := statuses[status]; ok {
			statuses[status] = count
		}
	}
	_ = counts.Close()
	for status, count := range statuses {
		registry.Set(ctx, executioncontract.GaugeJobs, count,
			executioncontract.MetricLabels{executioncontract.LabelStatus: executioncontract.MetricValue(status)})
	}

	// Read recent attempts once. A small in-memory set prevents double-counting
	// when the same recent rows are visible on consecutive scrapes.
	rows, err := db.QueryContext(ctx, `
		SELECT a.id::text, a.started_at, a.finished_at,
		       GREATEST(0, EXTRACT(EPOCH FROM (a.started_at - COALESCE(previous.finished_at, j.created_at)))),
		       j.action, j.mode, a.status
		FROM app.job_attempts a
		JOIN app.jobs j ON j.id = a.job_id
		LEFT JOIN app.job_attempts previous
		  ON previous.job_id = a.job_id AND previous.attempt_no = a.attempt_no - 1
		WHERE a.created_at >= clock_timestamp() - interval '24 hours'
		ORDER BY a.created_at
	`)
	if err == nil {
		for rows.Next() {
			var id, action, mode, status string
			var started time.Time
			var finished sql.NullTime
			var queueWait float64
			if scanErr := rows.Scan(&id, &started, &finished, &queueWait, &action, &mode, &status); scanErr != nil {
				break
			}
			if _, exists := state.seenAttempts[id]; exists {
				continue
			}
			state.seenAttempts[id] = struct{}{}
			registry.Observe(ctx, executioncontract.HistogramQueueWait, queueWait,
				executioncontract.MetricLabels{executioncontract.LabelMode: executioncontract.MetricValue(mode)})
			if finished.Valid {
				observeMigrationDuration(ctx, registry, action, status, finished.Time.Sub(started))
			} else {
				state.pending[id] = pendingAttempt{action: action, start: started}
			}
		}
		_ = rows.Close()
	}

	for id, pending := range state.pending {
		var finished sql.NullTime
		var status string
		err := db.QueryRowContext(ctx, `SELECT finished_at, status FROM app.job_attempts WHERE id = $1::uuid`, id).Scan(&finished, &status)
		if err == nil && finished.Valid {
			observeMigrationDuration(ctx, registry, pending.action, status, finished.Time.Sub(pending.start))
			delete(state.pending, id)
		} else if now.Sub(pending.start) > 24*time.Hour {
			delete(state.pending, id)
		}
	}

	var retries int64
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM app.job_attempts WHERE attempt_no > 1`).Scan(&retries); err == nil {
		if delta := retries - state.lastRetries; delta > 0 {
			registry.Add(ctx, executioncontract.CounterRetries, float64(delta),
				executioncontract.MetricLabels{executioncontract.LabelReason: "unknown"})
		}
		state.lastRetries = retries
	}

	for _, direction := range []string{"up", "down"} {
		var batches, rowCount int64
		err := db.QueryRowContext(ctx, `
			SELECT count(*), COALESCE(sum(row_count), 0)
			FROM app.job_batch_commit_proofs WHERE direction = $1
		`, direction).Scan(&batches, &rowCount)
		if err != nil {
			continue
		}
		if delta := batches - state.lastBatches[direction]; delta > 0 {
			registry.Add(ctx, executioncontract.CounterBatches, float64(delta),
				executioncontract.MetricLabels{executioncontract.LabelDirection: executioncontract.MetricValue(direction)})
		}
		if delta := rowCount - state.lastRows[direction]; delta > 0 {
			registry.Add(ctx, executioncontract.CounterRows, float64(delta),
				executioncontract.MetricLabels{executioncontract.LabelDirection: executioncontract.MetricValue(direction)})
		}
		state.lastBatches[direction] = batches
		state.lastRows[direction] = rowCount
	}
}

func observeMigrationDuration(ctx context.Context, registry *Registry, action, status string, duration time.Duration) {
	if duration < 0 {
		duration = 0
	}
	result := executioncontract.ValueFailure
	if status == "succeeded" {
		result = executioncontract.ValueSuccess
	}
	allowed := map[string]bool{"apply": true, "rollback": true, "forward": true, "resume": true, "compensate": true}
	if !allowed[action] {
		action = "apply"
	}
	registry.Observe(ctx, executioncontract.HistogramDuration, duration.Seconds(),
		executioncontract.MetricLabels{
			executioncontract.LabelAction: executioncontract.MetricValue(action),
			executioncontract.LabelResult: result,
		})
}

func SafeErrorText(err error) string {
	message, _ := SafeError(err)
	return message
}
