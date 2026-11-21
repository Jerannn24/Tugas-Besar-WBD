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

type ClaimRequest struct {
	WorkerID      string
	LeaseDuration time.Duration
	// AfterLock is an optional test seam called while holding the row lock before commit.
	AfterLock func(ctx context.Context) error
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

// ClaimJob claims the next available queued job using a short platform transaction
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

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin claim tx: %w", err)
	}
	defer tx.Rollback()

	// shortcut: claims queued and pre-dispatch expired running jobs excluding resume/source-linked jobs until source-aware reconciliation, upgrade in reconciliation increment
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
		WHERE (
			(j.status = 'queued' AND (j.next_retry_at IS NULL OR j.next_retry_at <= clock_timestamp()))
			OR
			(
				j.status = 'running'
				AND j.execution_status = 'running'
				AND j.action <> 'resume'
				AND j.source_execution_id IS NULL
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
				AND EXISTS (
					SELECT 1 FROM app.job_attempts a
					WHERE a.job_id = j.id
					  AND a.attempt_no = j.attempt_no
					  AND a.status = 'running'
					  AND a.sql_entered = false
				)
			)
		)
		ORDER BY j.created_at ASC
		LIMIT 1
		FOR UPDATE OF j SKIP LOCKED
	`).Scan(
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

	if in.AfterLock != nil {
		if err := in.AfterLock(ctx); err != nil {
			return nil, err
		}
	}

	leaseToken := uuid.NewString()

	var leaseExpiresAt time.Time
	if currentStatus == "running" {
		res, err := tx.ExecContext(ctx, `
			UPDATE app.job_attempts
			SET status = 'failed',
			    finished_at = clock_timestamp(),
			    error_code = 'LEASE_EXPIRED',
			    error_message = 'lease expired before sql dispatch',
			    error_retryable = true
			WHERE job_id = $1 AND attempt_no = $2 AND status = 'running' AND sql_entered = false
		`, jobID, attemptNo)
		if err != nil {
			return nil, fmt.Errorf("interrupt expired attempt: %w", err)
		}
		rowsAffected, err := res.RowsAffected()
		if err != nil {
			return nil, fmt.Errorf("interrupt expired attempt rows: %w", err)
		}
		if rowsAffected == 0 {
			return nil, errors.New("failed to interrupt prior running attempt")
		}

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
			    error_code = NULL,
			    error_message = NULL,
			    error_retryable = NULL,
			    updated_at = clock_timestamp()
			WHERE id = $5
			RETURNING lease_expires_at
		`, in.WorkerID, leaseToken, in.LeaseDuration.Milliseconds(), attemptNo, jobID).Scan(&leaseExpiresAt)
		if err != nil {
			return nil, fmt.Errorf("takeover claimed job: %w", err)
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
