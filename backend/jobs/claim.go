package jobs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Jerannn24/Tugas-Besar-WBD.git/executioncontract"
	"github.com/google/uuid"
)

const (
	// DefaultMaxAttempts is the maximum number of attempts allowed for pre-dispatch automatic retries.
	// When an expired attempt reaches this bound, the job transitions to 'failed' rather than 'retry_wait'.
	DefaultMaxAttempts = 3

	// DefaultRetryDelay is the waiting duration before an expired job in 'retry_wait' becomes eligible for reclaim.
	DefaultRetryDelay = 10 * time.Second
)

type ClaimRequest struct {
	WorkerID      string
	LeaseDuration time.Duration
	MaxAttempts   int
	RetryDelay    time.Duration
	afterLock     func(ctx context.Context) error
}

type Claim struct {
	AttemptID           string                      `json:"attempt_id"`
	JobID               string                      `json:"job_id"`
	ExecutionID         string                      `json:"execution_id"`
	DatabaseID          string                      `json:"database_id"`
	TargetID            string                      `json:"target_id"`
	ProposalID          string                      `json:"proposal_id"`
	MigrationID         string                      `json:"migration_id"`
	Revision            int                         `json:"revision"`
	ActorID             string                      `json:"actor_id"`
	Action              executioncontract.Action    `json:"action"`
	Direction           executioncontract.Direction `json:"direction"`
	Mode                executioncontract.Mode      `json:"mode"`
	SourceExecutionID   *executioncontract.ID       `json:"source_execution_id"`
	SnapshotHash        string                      `json:"snapshot_hash"`
	ApprovalFingerprint string                      `json:"approval_fingerprint"`
	AttemptNo           int                         `json:"attempt_no"`
	ClaimedBy           string                      `json:"claimed_by"`
	LeaseToken          string                      `json:"lease_token"`
	LeaseExpiresAt      time.Time                   `json:"lease_expires_at"`
}

func (c Claim) RevalidationRequest() executioncontract.RevalidationRequest {
	return executioncontract.RevalidationRequest{
		JobID: c.JobID, ExecutionID: c.ExecutionID, ActorID: c.ActorID,
		DatabaseID: c.DatabaseID, TargetID: c.TargetID, MigrationID: c.MigrationID,
		ProposalID: c.ProposalID, Revision: c.Revision, Action: c.Action,
		Direction: c.Direction, SourceExecutionID: c.SourceExecutionID,
		SnapshotHash: c.SnapshotHash, ApprovalFingerprint: c.ApprovalFingerprint,
	}
}

func (s *Service) reclaimOneExpiredJob(ctx context.Context, maxAttempts int, retryDelay time.Duration) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin reclaim tx: %w", err)
	}
	defer tx.Rollback()

	var (
		jobID     string
		attemptNo int
	)

	err = tx.QueryRowContext(ctx, `
		SELECT j.id, j.attempt_no
		FROM app.jobs j
		WHERE j.status = 'running'
		  AND j.execution_status = 'running'
		  AND ((j.action = 'apply' AND j.source_execution_id IS NULL) OR
		       (j.action IN ('rollback', 'forward') AND j.source_execution_id IS NOT NULL
		        AND j.source_execution_id <> j.execution_id))
		  AND NOT j.blocks_database
		  AND j.lease_expires_at <= clock_timestamp()
		  AND NOT EXISTS (
		      SELECT 1 FROM app.job_attempts a
		      WHERE a.job_id = j.id
		        AND (
		            a.sql_entered = true
		            OR a.status IN ('succeeded', 'partial', 'partially_reverted', 'unknown')
		            OR EXISTS (
		                SELECT 1 FROM app.job_statement_evidence se
		                WHERE se.attempt_id = a.id
		            )
		        )
		  )
		  AND (SELECT count(*) FROM app.job_attempts a WHERE a.job_id = j.id) = j.attempt_no
		  AND (SELECT count(DISTINCT a.attempt_no) FROM app.job_attempts a WHERE a.job_id = j.id) = j.attempt_no
		  AND NOT EXISTS (
		      SELECT 1 FROM app.job_attempts a WHERE a.job_id = j.id AND (
		          a.attempt_no > j.attempt_no OR
		          (a.attempt_no = j.attempt_no AND (a.status <> 'running' OR a.finished_at IS NOT NULL)) OR
		          (a.attempt_no < j.attempt_no AND (a.status <> 'failed' OR a.finished_at IS NULL))
		      )
		  )
		ORDER BY j.created_at ASC
		LIMIT 1
		FOR UPDATE OF j SKIP LOCKED
	`).Scan(&jobID, &attemptNo)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("query expired job: %w", err)
	}

	rows, err := tx.QueryContext(ctx, `
		SELECT id::text, attempt_no, status, sql_entered, finished_at IS NOT NULL
		FROM app.job_attempts
		WHERE job_id = $1
		ORDER BY attempt_no ASC
		FOR UPDATE
	`, jobID)
	if err != nil {
		return false, fmt.Errorf("query attempts for reclaim: %w", err)
	}
	defer rows.Close()

	type attRecord struct {
		id          string
		attemptNo   int
		status      string
		sqlEntered  bool
		hasFinished bool
	}

	var attempts []attRecord
	for rows.Next() {
		var a attRecord
		if err := rows.Scan(&a.id, &a.attemptNo, &a.status, &a.sqlEntered, &a.hasFinished); err != nil {
			return false, fmt.Errorf("scan attempt for reclaim: %w", err)
		}
		attempts = append(attempts, a)
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("iterate attempts for reclaim: %w", err)
	}

	var runningAttempt *attRecord
	runningCount := 0
	ambiguous := false

	for i := range attempts {
		a := &attempts[i]
		if a.sqlEntered {
			ambiguous = true
			break
		}
		if a.status == "running" {
			runningCount++
			runningAttempt = a
		} else if a.attemptNo < attemptNo {
			if a.status != "failed" || !a.hasFinished {
				ambiguous = true
				break
			}
		} else {
			ambiguous = true
			break
		}
	}

	if ambiguous || runningCount != 1 || runningAttempt == nil || runningAttempt.attemptNo != attemptNo {
		return false, nil
	}

	retryable := attemptNo < maxAttempts

	res, err := tx.ExecContext(ctx, `
		UPDATE app.job_attempts
		SET status = 'failed',
		    finished_at = clock_timestamp(),
		    error_code = 'LEASE_EXPIRED',
		    error_message = 'lease expired before sql dispatch',
		    error_retryable = $1
		WHERE id = $2 AND status = 'running' AND sql_entered = false
	`, retryable, runningAttempt.id)
	if err != nil {
		return false, fmt.Errorf("interrupt expired attempt: %w", err)
	}
	rowsAffected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("check affected rows: %w", err)
	}
	if rowsAffected != 1 {
		return false, errors.New("exact cardinality mismatch when interrupting expired attempt")
	}

	if retryable {
		_, err = tx.ExecContext(ctx, `
			UPDATE app.jobs
			SET status = 'retry_wait',
			    execution_status = 'retry_wait',
			    claimed_by = NULL,
			    claimed_at = NULL,
			    lease_token = NULL,
			    lease_expires_at = NULL,
			    next_retry_at = clock_timestamp() + ($1 * interval '1 millisecond'),
			    error_code = 'LEASE_EXPIRED',
			    error_message = 'lease expired before sql dispatch',
			    error_retryable = true,
			    updated_at = clock_timestamp()
			WHERE id = $2
		`, retryDelay.Milliseconds(), jobID)
		if err != nil {
			return false, fmt.Errorf("transition job to retry_wait: %w", err)
		}
	} else {
		_, err = tx.ExecContext(ctx, `
			UPDATE app.jobs
			SET status = 'failed',
			    execution_status = 'failed',
			    claimed_by = NULL,
			    claimed_at = NULL,
			    lease_token = NULL,
			    lease_expires_at = NULL,
			    next_retry_at = NULL,
			    finished_at = clock_timestamp(),
			    error_code = 'LEASE_EXPIRED',
			    error_message = 'lease expired before sql dispatch',
			    error_retryable = false,
			    updated_at = clock_timestamp()
			WHERE id = $1
		`, jobID)
		if err != nil {
			return false, fmt.Errorf("transition job to failed at bound: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit reclaim tx: %w", err)
	}
	return true, nil
}

func (s *Service) reclaimExpiredJobs(ctx context.Context, maxAttempts int, retryDelay time.Duration) (int, error) {
	count := 0
	// shortcut: bounded batch limit of 20 to avoid monopolizing claim transaction, upgrade in worker loop
	for i := 0; i < 20; i++ {
		reclaimed, err := s.reclaimOneExpiredJob(ctx, maxAttempts, retryDelay)
		if err != nil {
			return count, err
		}
		if !reclaimed {
			break
		}
		count++
	}
	return count, nil
}

// ReclaimExpiredJobs scans for and reclaims expired pre-dispatch jobs, transitioning them
// to retry_wait (or failed at bound) with delayed retry scheduling and cleared ownership.
func (s *Service) ReclaimExpiredJobs(ctx context.Context, maxAttempts int, retryDelay time.Duration) (int, error) {
	if maxAttempts <= 0 || maxAttempts > DefaultMaxAttempts {
		maxAttempts = DefaultMaxAttempts
	}
	if retryDelay < time.Millisecond {
		retryDelay = DefaultRetryDelay
	}
	return s.reclaimExpiredJobs(ctx, maxAttempts, retryDelay)
}

// ClaimJob claims the next available queued or due retry job using a short platform transaction
// with FOR UPDATE SKIP LOCKED.
// If a job is claimed, atomic running state is set and a running attempt is durably created.
// If no job is available, it returns (nil, nil) as a distinct no-work result.
func (s *Service) ClaimJob(ctx context.Context, in ClaimRequest) (*Claim, error) {
	if s.db == nil {
		return nil, errors.New("db is required")
	}
	if in.WorkerID == "" {
		return nil, executioncontract.ContractError{
			Code:    "INVALID_REQUEST",
			Message: "worker_id is required",
		}
	}
	if in.LeaseDuration == 0 {
		in.LeaseDuration = 30 * time.Second
	} else if in.LeaseDuration < time.Millisecond {
		return nil, executioncontract.ContractError{
			Code:    "INVALID_REQUEST",
			Message: "lease_duration must be at least 1ms",
		}
	}
	maxAttempts := in.MaxAttempts
	if maxAttempts <= 0 || maxAttempts > DefaultMaxAttempts {
		maxAttempts = DefaultMaxAttempts
	}
	retryDelay := in.RetryDelay
	if retryDelay < time.Millisecond {
		retryDelay = DefaultRetryDelay
	}

	if _, err := s.reclaimExpiredJobs(ctx, maxAttempts, retryDelay); err != nil {
		return nil, fmt.Errorf("reclaim expired jobs: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin claim tx: %w", err)
	}
	defer tx.Rollback()

	// Retry candidates must have a complete, unambiguous pre-dispatch attempt history.
	var (
		jobID               string
		databaseID          string
		targetID            string
		proposalID          string
		migrationID         string
		revision            int
		executionID         string
		actorID             string
		action              string
		direction           string
		mode                string
		attemptNo           int
		sourceExecutionID   *string
		snapshotHash        string
		approvalFingerprint sql.NullString
		currentStatus       string
	)

	err = tx.QueryRowContext(ctx, `
		SELECT id, database_id, target_id, proposal_id, migration_id, revision,
		       execution_id, actor_id, action, direction, mode, attempt_no,
		       source_execution_id::text, snapshot_hash, approval_fingerprint,
		       status
		FROM app.jobs j
		WHERE ((j.status = 'queued' AND j.next_retry_at IS NULL AND NOT j.blocks_database) OR
		       (j.status = 'retry_wait' AND j.next_retry_at IS NOT NULL
		        AND j.next_retry_at <= clock_timestamp() AND j.attempt_no < $1
		        AND NOT j.blocks_database
		        AND ((j.action = 'apply' AND j.source_execution_id IS NULL) OR
		             (j.action IN ('rollback', 'forward') AND j.source_execution_id IS NOT NULL
		              AND j.source_execution_id <> j.execution_id))
		        AND (SELECT count(*) FROM app.job_attempts a WHERE a.job_id = j.id) = j.attempt_no
		        AND (SELECT count(DISTINCT a.attempt_no) FROM app.job_attempts a WHERE a.job_id = j.id) = j.attempt_no
		        AND NOT EXISTS (
		            SELECT 1 FROM app.job_attempts a WHERE a.job_id = j.id AND
		                (a.attempt_no > j.attempt_no OR a.status <> 'failed' OR
		                 a.finished_at IS NULL OR a.sql_entered OR
		                 EXISTS (SELECT 1 FROM app.job_statement_evidence se WHERE se.attempt_id = a.id))
		        )))
		  AND NOT EXISTS (
		      SELECT 1 FROM app.jobs blocked
		      WHERE blocked.target_id = j.target_id AND blocked.id <> j.id
		        AND (blocked.blocks_database OR blocked.status IN ('unknown', 'partial', 'partially_reverted')
		             OR blocked.execution_status IN ('unknown', 'partial', 'partially_reverted')
		             OR EXISTS (SELECT 1 FROM app.job_attempts a
		                        WHERE a.job_id = blocked.id
		                          AND (a.sql_entered OR EXISTS (
		                              SELECT 1 FROM app.job_statement_evidence se WHERE se.attempt_id = a.id))
		                          AND (a.status = 'running' OR NOT EXISTS (
		                              SELECT 1 FROM app.job_dispatch_authorizations d WHERE d.attempt_id = a.id))))
		  )
		ORDER BY j.created_at ASC
		LIMIT 1
		FOR UPDATE OF j SKIP LOCKED
	`, maxAttempts).Scan(
		&jobID, &databaseID, &targetID, &proposalID, &migrationID, &revision,
		&executionID, &actorID, &action, &direction, &mode, &attemptNo,
		&sourceExecutionID, &snapshotHash, &approvalFingerprint, &currentStatus,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query claimable job: %w", err)
	}

	if in.afterLock != nil {
		if err := in.afterLock(ctx); err != nil {
			return nil, err
		}
	}

	leaseToken := uuid.NewString()
	var leaseExpiresAt time.Time

	if currentStatus == "retry_wait" {
		attemptNo++
		err = tx.QueryRowContext(ctx, `
			UPDATE app.jobs
			SET status = 'running',
			    execution_status = 'running',
			    claimed_by = $1,
			    claimed_at = clock_timestamp(),
			    lease_token = $2,
			    lease_expires_at = clock_timestamp() + ($3 * interval '1 millisecond'),
			    attempt_no = $4,
			    next_retry_at = NULL,
			    error_code = NULL,
			    error_message = NULL,
			    error_retryable = NULL,
			    updated_at = clock_timestamp()
			WHERE id = $5
			RETURNING lease_expires_at
		`, in.WorkerID, leaseToken, in.LeaseDuration.Milliseconds(), attemptNo, jobID).Scan(&leaseExpiresAt)
		if err != nil {
			return nil, fmt.Errorf("claim retry job: %w", err)
		}
	} else {
		err = tx.QueryRowContext(ctx, `
			UPDATE app.jobs
			SET status = 'running',
			    execution_status = 'running',
			    claimed_by = $1,
			    claimed_at = clock_timestamp(),
			    lease_token = $2,
			    lease_expires_at = clock_timestamp() + ($3 * interval '1 millisecond'),
			    started_at = COALESCE(started_at, clock_timestamp()),
			    updated_at = clock_timestamp()
			WHERE id = $4
			RETURNING lease_expires_at
		`, in.WorkerID, leaseToken, in.LeaseDuration.Milliseconds(), jobID).Scan(&leaseExpiresAt)
		if err != nil {
			return nil, fmt.Errorf("update claimed job: %w", err)
		}
	}

	var attemptID string
	err = tx.QueryRowContext(ctx, `
		INSERT INTO app.job_attempts (
			job_id, execution_id, database_id, migration_id, revision,
			actor_id, attempt_no, status, sql_entered, started_at, created_at
		) VALUES (
			$1, $2, $3, $4, $5,
			$6, $7, 'running', false, clock_timestamp(), clock_timestamp()
		)
		RETURNING id
	`, jobID, executionID, databaseID, migrationID, revision, actorID, attemptNo).Scan(&attemptID)
	if err != nil {
		return nil, fmt.Errorf("insert running attempt: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit claim tx: %w", err)
	}

	return &Claim{
		AttemptID:           attemptID,
		JobID:               jobID,
		ExecutionID:         executionID,
		DatabaseID:          databaseID,
		TargetID:            targetID,
		ProposalID:          proposalID,
		MigrationID:         migrationID,
		Revision:            revision,
		ActorID:             actorID,
		Action:              executioncontract.Action(action),
		Direction:           executioncontract.Direction(direction),
		Mode:                executioncontract.Mode(mode),
		SourceExecutionID:   sourceExecutionID,
		SnapshotHash:        snapshotHash,
		ApprovalFingerprint: approvalFingerprint.String,
		AttemptNo:           attemptNo,
		ClaimedBy:           in.WorkerID,
		LeaseToken:          leaseToken,
		LeaseExpiresAt:      leaseExpiresAt,
	}, nil
}
