package jobs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
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

// src and j are the candidate's approved source and candidate respectively.
const validDownResumeSource = `(j.direction <> 'down' OR (
	src.action = 'compensate' AND src.direction = 'down' AND src.status = 'partially_reverted'
	AND EXISTS (
		SELECT 1 FROM app.jobs up
		WHERE up.execution_id = src.source_execution_id AND up.id <> src.id AND up.id <> j.id
		  AND up.target_id = src.target_id AND up.database_id = src.database_id
		  AND up.migration_id = src.migration_id AND up.revision = src.revision
		  AND up.snapshot_hash = src.snapshot_hash AND up.mode = 'batch'
		  AND up.direction = 'up' AND up.status IN ('succeeded', 'partial', 'partially_reverted')
		  AND EXISTS (
			SELECT 1 FROM app.job_batch_step_authorizations bsa
			JOIN app.job_batch_commit_proofs p ON p.step_auth_id = bsa.id
			  AND p.target_xid = bsa.target_xid AND p.physical_target_id = bsa.physical_target_id
			  AND p.direction = bsa.direction AND p.scope_execution_id = bsa.scope_execution_id
			WHERE bsa.scope_execution_id = up.execution_id
			  AND bsa.physical_target_id = src.target_id AND bsa.direction = 'up'
		  )
	)
))`

// downResumeLineage recognizes only the proved origin's up job and down siblings.
// The caller must separately preserve the live-lease guard for these jobs.
func downResumeLineage(candidateID, blocked string) string {
	return fmt.Sprintf(`EXISTS (
		SELECT 1 FROM app.jobs candidate
		JOIN app.jobs src ON src.execution_id = candidate.source_execution_id AND src.id <> candidate.id
		WHERE candidate.id = %s AND candidate.mode = 'batch' AND candidate.action = 'resume' AND candidate.direction = 'down'
		  AND src.target_id = candidate.target_id AND src.database_id = candidate.database_id
		  AND src.migration_id = candidate.migration_id AND src.revision = candidate.revision
		  AND src.snapshot_hash = candidate.snapshot_hash AND src.mode = 'batch'
		  AND %s
		  AND %[3]s.mode = 'batch' AND %[3]s.target_id = candidate.target_id
		  AND %[3]s.database_id = candidate.database_id AND %[3]s.migration_id = candidate.migration_id
		  AND %[3]s.revision = candidate.revision AND %[3]s.snapshot_hash = candidate.snapshot_hash
		  AND ((%[3]s.execution_id = src.source_execution_id AND %[3]s.direction = 'up')
		       OR (%[3]s.direction = 'down' AND (
		           %[3]s.execution_id = src.execution_id
		           OR (%[3]s.action = 'compensate' AND %[3]s.source_execution_id = src.source_execution_id)
		           OR (%[3]s.action = 'resume' AND %[3]s.source_execution_id = src.execution_id))))
	)`, candidateID, strings.ReplaceAll(validDownResumeSource, "j.", "candidate."), blocked)
}

func batchEvidenceScope(job string) string {
	return fmt.Sprintf(`bsa.scope_execution_id = CASE
		WHEN %[1]s.direction = 'up' THEN %[1]s.execution_id
		WHEN %[1]s.action = 'compensate' THEN %[1]s.source_execution_id
		WHEN %[1]s.action = 'resume' THEN (
			SELECT origin.source_execution_id FROM app.jobs origin
			WHERE origin.execution_id = %[1]s.source_execution_id AND origin.target_id = %[1]s.target_id
			  AND origin.database_id = %[1]s.database_id AND origin.migration_id = %[1]s.migration_id
			  AND origin.revision = %[1]s.revision AND origin.snapshot_hash = %[1]s.snapshot_hash
			  AND origin.mode = 'batch' AND origin.action = 'compensate' AND origin.direction = 'down'
		) END`, job)
}

func resolvedBlocker(blocked string) string {
	return fmt.Sprintf(`(
		EXISTS (
			SELECT 1 FROM app.jobs res
			WHERE (res.execution_id = %[1]s.execution_id OR res.source_execution_id = %[1]s.execution_id)
			  AND res.target_id = %[1]s.target_id
			  AND res.database_id = %[1]s.database_id
			  AND res.migration_id = %[1]s.migration_id
			  AND res.revision = %[1]s.revision
			  AND res.snapshot_hash = %[1]s.snapshot_hash
			  AND res.mode = %[1]s.mode
			  AND res.direction = %[1]s.direction
			  AND %[1]s.direction = 'up'
			  AND res.status = 'succeeded'
			  AND res.execution_status = 'succeeded'
			  AND NOT res.blocks_database
		)
		OR (
			%[1]s.mode = 'batch'
			AND EXISTS (
				SELECT 1 FROM app.jobs src
				JOIN app.jobs comp ON comp.source_execution_id = src.execution_id AND comp.id <> src.id
				JOIN app.jobs res ON (
					res.id = comp.id
					OR (
						comp.status = 'partially_reverted'
						AND res.id <> comp.id
						AND res.mode = 'batch'
						AND res.action = 'resume'
						AND res.direction = 'down'
						AND res.source_execution_id = comp.execution_id
						AND res.execution_id = comp.execution_id
					)
				)
				WHERE src.mode = 'batch'
				  AND src.direction = 'up'
				  AND src.action <> 'resume'
				  AND src.status IN ('succeeded', 'partial', 'partially_reverted')
				  AND NOT src.blocks_database
				  AND src.target_id = %[1]s.target_id
				  AND src.database_id = %[1]s.database_id
				  AND src.migration_id = %[1]s.migration_id
				  AND src.revision = %[1]s.revision
				  AND src.snapshot_hash = %[1]s.snapshot_hash
				  AND jsonb_typeof(src.snapshot->'batch') = 'object'
				  AND src.snapshot->'batch' = %[1]s.snapshot->'batch'
				  AND comp.mode = 'batch'
				  AND comp.action = 'compensate'
				  AND comp.direction = 'down'
				  AND NOT comp.blocks_database
				  AND comp.target_id = src.target_id
				  AND comp.database_id = src.database_id
				  AND comp.migration_id = src.migration_id
				  AND comp.revision = src.revision
				  AND comp.snapshot_hash = src.snapshot_hash
				  AND comp.snapshot->'batch' = src.snapshot->'batch'
				  AND res.target_id = src.target_id
				  AND res.database_id = src.database_id
				  AND res.migration_id = src.migration_id
				  AND res.revision = src.revision
				  AND res.snapshot_hash = src.snapshot_hash
				  AND res.snapshot->'batch' = src.snapshot->'batch'
				  AND res.status = 'succeeded'
				  AND res.execution_status = 'succeeded'
				  AND NOT res.blocks_database
				  AND (
					  (%[1]s.direction = 'up' AND (
						  (%[1]s.action <> 'resume' AND %[1]s.execution_id = src.execution_id)
						  OR (%[1]s.action = 'resume' AND %[1]s.source_execution_id = src.execution_id)
					  ))
					  OR (%[1]s.direction = 'down' AND (
						  %[1]s.id = comp.id
						  OR (%[1]s.action = 'resume' AND %[1]s.source_execution_id = comp.execution_id AND %[1]s.execution_id = comp.execution_id)
					  ))
				  )
				  AND NOT EXISTS (
					  SELECT 1 FROM app.job_attempts sa
					  WHERE sa.job_id = src.id AND sa.status IN ('running', 'unknown')
				  )
				  AND NOT EXISTS (
					  SELECT 1 FROM app.job_attempts ca
					  WHERE ca.job_id = comp.id AND ca.status IN ('running', 'unknown')
				  )
				  AND NOT EXISTS (
					  SELECT 1 FROM app.job_attempts ra
					  WHERE ra.job_id = res.id AND ra.status IN ('running', 'unknown')
				  )
				  AND EXISTS (
					  SELECT 1 FROM app.job_attempts ra
					  WHERE ra.job_id = res.id AND ra.status = 'succeeded'
				  )
				  AND EXISTS (
					  SELECT 1 FROM app.job_batch_commit_proofs up_p
					  JOIN app.job_batch_step_authorizations up_bsa ON up_bsa.id = up_p.step_auth_id
					  JOIN app.jobs uj ON uj.id = up_p.job_id
					  JOIN app.job_attempts ua ON ua.id = up_p.attempt_id AND ua.job_id = uj.id
					  WHERE up_p.scope_execution_id = src.execution_id
					    AND up_p.physical_target_id = src.target_id
					    AND up_p.direction = 'up'
					    AND up_bsa.job_id = up_p.job_id
					    AND up_bsa.attempt_id = up_p.attempt_id
					    AND up_bsa.scope_execution_id = up_p.scope_execution_id
					    AND up_bsa.physical_target_id = up_p.physical_target_id
					    AND up_bsa.direction = up_p.direction
					    AND up_bsa.batch_index = up_p.batch_index
					    AND up_bsa.target_xid = up_p.target_xid
					    AND up_bsa.job_execution_id = src.execution_id
					    AND uj.execution_id = src.execution_id
					    AND uj.target_id = src.target_id
					    AND uj.database_id = src.database_id
					    AND uj.migration_id = src.migration_id
					    AND uj.revision = src.revision
					    AND uj.snapshot_hash = src.snapshot_hash
					    AND uj.mode = 'batch'
					    AND uj.direction = 'up'
					    AND EXISTS (
						    SELECT 1 FROM app.job_statement_evidence up_se
						    WHERE up_se.attempt_id = up_p.attempt_id
						      AND up_se.ordinal = up_bsa.ordinal
						      AND up_se.batch_index = up_p.batch_index
						      AND up_se.outcome = 'succeeded'
					    )
				  )
				  AND NOT EXISTS (
					  SELECT 1 FROM app.job_batch_commit_proofs up_p
					  WHERE up_p.scope_execution_id = src.execution_id
					    AND up_p.physical_target_id = src.target_id
					    AND up_p.direction = 'up'
					    AND NOT EXISTS (
						    SELECT 1 FROM app.job_batch_step_authorizations up_bsa
						    JOIN app.job_batch_commit_proofs down_p ON down_p.scope_execution_id = src.execution_id
						      AND down_p.physical_target_id = src.target_id
						      AND down_p.direction = 'down'
						      AND down_p.batch_index = up_p.batch_index
						    JOIN app.job_batch_step_authorizations down_bsa ON down_bsa.id = down_p.step_auth_id
						    JOIN app.jobs dj ON dj.id = down_p.job_id
						    JOIN app.job_attempts da ON da.id = down_p.attempt_id AND da.job_id = dj.id
						    WHERE up_bsa.id = up_p.step_auth_id
						      AND up_bsa.job_id = up_p.job_id
						      AND up_bsa.attempt_id = up_p.attempt_id
						      AND up_bsa.scope_execution_id = up_p.scope_execution_id
						      AND up_bsa.physical_target_id = up_p.physical_target_id
						      AND up_bsa.direction = up_p.direction
						      AND up_bsa.batch_index = up_p.batch_index
						      AND up_bsa.target_xid = up_p.target_xid
						      AND down_bsa.job_id = down_p.job_id
						      AND down_bsa.attempt_id = down_p.attempt_id
						      AND down_bsa.scope_execution_id = down_p.scope_execution_id
						      AND down_bsa.physical_target_id = down_p.physical_target_id
						      AND down_bsa.direction = down_p.direction
						      AND down_bsa.batch_index = down_p.batch_index
						      AND down_bsa.target_xid = down_p.target_xid
						      AND down_bsa.job_execution_id = comp.execution_id
						      AND dj.mode = 'batch'
						      AND dj.direction = 'down'
						      AND dj.execution_id = comp.execution_id
						      AND dj.target_id = src.target_id
						      AND dj.database_id = src.database_id
						      AND dj.migration_id = src.migration_id
						      AND dj.revision = src.revision
						      AND dj.snapshot_hash = src.snapshot_hash
						      AND dj.snapshot->'batch' = src.snapshot->'batch'
						      AND (
							      dj.id = comp.id
							      OR (dj.action = 'resume' AND dj.source_execution_id = comp.execution_id)
						      )
						      AND EXISTS (
							      SELECT 1 FROM app.job_statement_evidence down_se
							      WHERE down_se.attempt_id = down_p.attempt_id
							        AND down_se.ordinal = down_bsa.ordinal
							        AND down_se.batch_index = down_p.batch_index
							        AND down_se.outcome = 'succeeded'
						      )
					    )
				  )
			)
		)
	)`, blocked)
}

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
	Traceparent         string                      `json:"traceparent,omitempty"`
	Tracestate          string                      `json:"tracestate,omitempty"`
	QueueWaitSeconds    float64                     `json:"-"`
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
		jobID             string
		attemptNo         int
		action            string
		mode              string
		sourceExecutionID sql.NullString
	)

	err = tx.QueryRowContext(ctx, `
		SELECT j.id, j.attempt_no, j.action, j.mode, j.source_execution_id::text
		FROM app.jobs j
		WHERE j.status = 'running'
		  AND j.execution_status = 'running'
		  AND (
		      (j.mode = 'standard' AND NOT j.blocks_database
		       AND ((j.action = 'apply' AND j.source_execution_id IS NULL) OR
		            (j.action IN ('rollback', 'forward') AND (j.source_execution_id IS NULL OR j.source_execution_id <> j.execution_id))))
		      OR
		      (j.mode = 'batch' AND (
		          (j.action = 'apply' AND j.source_execution_id IS NULL AND NOT j.blocks_database) OR
		          (j.action = 'forward' AND j.direction = 'up' AND NOT j.blocks_database AND j.source_execution_id IS NOT NULL AND EXISTS (
		              SELECT 1 FROM app.jobs src
		              WHERE src.execution_id = j.source_execution_id AND src.id <> j.id
		                AND src.target_id = j.target_id AND src.database_id = j.database_id
		                AND src.migration_id = j.migration_id AND src.revision = j.revision
		                AND src.snapshot_hash = j.snapshot_hash AND src.mode = 'batch'
		                AND src.direction = 'up' AND src.status = 'succeeded'
		          )) OR
		          (j.action = 'resume' AND j.source_execution_id IS NOT NULL AND (NOT j.blocks_database OR j.blocks_database) AND EXISTS (
		              SELECT 1 FROM app.jobs src
		              WHERE src.execution_id = j.source_execution_id
		                AND src.id <> j.id
		                AND src.target_id = j.target_id
		                AND src.database_id = j.database_id
		                AND src.migration_id = j.migration_id
		                AND src.revision = j.revision
		                AND src.direction = j.direction
		                AND src.snapshot_hash = j.snapshot_hash
		                AND src.mode = 'batch'
		                AND src.status <> 'succeeded'
		                AND `+validDownResumeSource+`
		          )) OR
		          (j.action = 'compensate' AND j.direction = 'down' AND j.source_execution_id IS NOT NULL AND EXISTS (
		              SELECT 1 FROM app.jobs src
		              WHERE src.execution_id = j.source_execution_id
		                AND src.id <> j.id
		                AND src.target_id = j.target_id
		                AND src.database_id = j.database_id
		                AND src.migration_id = j.migration_id
		                AND src.revision = j.revision
		                AND src.direction = 'up'
		                AND src.snapshot_hash = j.snapshot_hash
		                AND src.mode = 'batch'
		                AND src.status IN ('succeeded', 'partial', 'partially_reverted')
		                AND EXISTS (
		                    SELECT 1 FROM app.job_batch_step_authorizations bsa
		                    JOIN app.job_batch_commit_proofs p ON p.step_auth_id = bsa.id
		                      AND p.target_xid = bsa.target_xid
		                      AND p.physical_target_id = bsa.physical_target_id
		                      AND p.direction = bsa.direction
		                      AND p.scope_execution_id = bsa.scope_execution_id
		                    WHERE bsa.scope_execution_id = src.execution_id
		                      AND bsa.physical_target_id = j.target_id
		                      AND bsa.direction = 'up'
		                )
		          ))
		      ))
		  )
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
		            OR EXISTS (
		                SELECT 1 FROM app.job_batch_step_authorizations bsa
		                WHERE bsa.attempt_id = a.id
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
	`).Scan(&jobID, &attemptNo, &action, &mode, &sourceExecutionID)
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

	var hasAuth bool
	err = tx.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM app.job_dispatch_authorizations WHERE attempt_id = $1)
		    OR EXISTS (SELECT 1 FROM app.job_batch_step_authorizations WHERE attempt_id = $1)
	`, runningAttempt.id).Scan(&hasAuth)
	if err != nil || hasAuth {
		return false, nil
	}

	if action == "resume" || action == "compensate" {
		var srcValid bool
		if action == "resume" {
			err = tx.QueryRowContext(ctx, `
				SELECT EXISTS (
					SELECT 1 FROM app.jobs src
					WHERE src.execution_id = j.source_execution_id
					  AND src.id <> j.id
					  AND src.target_id = j.target_id
					  AND src.database_id = j.database_id
					  AND src.migration_id = j.migration_id
					  AND src.revision = j.revision
					  AND src.snapshot_hash = j.snapshot_hash
					  AND src.mode = 'batch'
					  AND src.direction = j.direction
					  AND src.status <> 'succeeded'
					  AND `+validDownResumeSource+`
				)
				FROM app.jobs j WHERE j.id = $1
			`, jobID).Scan(&srcValid)
		} else {
			err = tx.QueryRowContext(ctx, `
				SELECT EXISTS (
					SELECT 1 FROM app.jobs src
					WHERE src.execution_id = j.source_execution_id
					  AND src.id <> j.id
					  AND src.target_id = j.target_id
					  AND src.database_id = j.database_id
					  AND src.migration_id = j.migration_id
					  AND src.revision = j.revision
					  AND src.direction = 'up'
					  AND src.snapshot_hash = j.snapshot_hash
					  AND src.mode = 'batch'
					  AND src.status IN ('succeeded', 'partial', 'partially_reverted')
					  AND EXISTS (
						  SELECT 1 FROM app.job_batch_step_authorizations bsa
						  JOIN app.job_batch_commit_proofs p ON p.step_auth_id = bsa.id
						    AND p.target_xid = bsa.target_xid
						    AND p.physical_target_id = bsa.physical_target_id
						    AND p.direction = bsa.direction
						    AND p.scope_execution_id = bsa.scope_execution_id
						  WHERE bsa.scope_execution_id = src.execution_id
						    AND bsa.physical_target_id = j.target_id
						    AND bsa.direction = 'up'
					  )
				)
				FROM app.jobs j WHERE j.id = $1
			`, jobID).Scan(&srcValid)
		}
		if err != nil || !srcValid {
			return false, nil
		}
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
			    blocks_database = false,
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
		queueWaitSeconds    float64
		traceparent         sql.NullString
		tracestate          sql.NullString
	)

	err = tx.QueryRowContext(ctx, `
		SELECT j.id, j.database_id, j.target_id, j.proposal_id, j.migration_id, j.revision,
		       j.execution_id, j.actor_id, j.action, j.direction, j.mode, j.attempt_no,
		       j.source_execution_id::text, j.snapshot_hash, j.approval_fingerprint,
		       j.status, j.traceparent, j.tracestate,
		       GREATEST(0, EXTRACT(EPOCH FROM (clock_timestamp() - j.created_at)))
		FROM app.jobs j
		JOIN app.migration_target_state target ON target.id = j.target_id
		WHERE ((j.status = 'queued' AND j.next_retry_at IS NULL
		        AND (
		            (j.mode = 'standard' AND NOT j.blocks_database)
		            OR
		            (j.mode = 'batch' AND (
		                (j.action = 'apply' AND j.source_execution_id IS NULL AND NOT j.blocks_database)
		                OR
		                (j.action = 'forward' AND j.direction = 'up' AND NOT j.blocks_database AND j.source_execution_id IS NOT NULL AND EXISTS (
		                    SELECT 1 FROM app.jobs src
		                    WHERE src.execution_id = j.source_execution_id AND src.id <> j.id
		                      AND src.target_id = j.target_id AND src.database_id = j.database_id
		                      AND src.migration_id = j.migration_id AND src.revision = j.revision
		                      AND src.snapshot_hash = j.snapshot_hash AND src.mode = 'batch'
		                      AND src.direction = 'up' AND src.status = 'succeeded'
		                ))
		                OR
		                (j.action = 'resume' AND j.source_execution_id IS NOT NULL AND EXISTS (
		                    SELECT 1 FROM app.jobs src
		                    WHERE src.execution_id = j.source_execution_id
		                      AND src.id <> j.id
		                      AND src.target_id = j.target_id
		                      AND src.database_id = j.database_id
		                      AND src.migration_id = j.migration_id
		                      AND src.revision = j.revision
		                      AND src.snapshot_hash = j.snapshot_hash
		                      AND src.mode = 'batch'
		                      AND src.direction = j.direction
		                      AND src.status <> 'succeeded'
		                      AND `+validDownResumeSource+`
		                ))
		                OR
		                (j.action = 'compensate' AND j.mode = 'batch' AND j.direction = 'down' AND j.source_execution_id IS NOT NULL AND EXISTS (
		                    SELECT 1 FROM app.jobs src
		                    WHERE src.execution_id = j.source_execution_id
		                      AND src.id <> j.id
		                      AND src.target_id = j.target_id
		                      AND src.database_id = j.database_id
		                      AND src.migration_id = j.migration_id
		                      AND src.revision = j.revision
		                      AND src.snapshot_hash = j.snapshot_hash
		                      AND src.mode = 'batch'
		                      AND src.direction = 'up'
		                      AND src.status IN ('succeeded', 'partial', 'partially_reverted')
		                      AND EXISTS (
		                          SELECT 1 FROM app.job_batch_step_authorizations bsa
		                          JOIN app.job_batch_commit_proofs p ON p.step_auth_id = bsa.id
		                            AND p.target_xid = bsa.target_xid
		                            AND p.physical_target_id = bsa.physical_target_id
		                            AND p.direction = bsa.direction
		                            AND p.scope_execution_id = bsa.scope_execution_id
		                          WHERE bsa.scope_execution_id = src.execution_id
		                            AND bsa.physical_target_id = j.target_id
		                            AND bsa.direction = 'up'
		                      )
		                ))
		            ))
		        )) OR
		       (j.status = 'retry_wait' AND j.next_retry_at IS NOT NULL
		        AND j.next_retry_at <= clock_timestamp() AND j.attempt_no < $1
		        AND (
		            (j.mode = 'standard' AND NOT j.blocks_database
		             AND ((j.action = 'apply' AND j.source_execution_id IS NULL) OR
		                  (j.action IN ('rollback', 'forward') AND (j.source_execution_id IS NULL OR j.source_execution_id <> j.execution_id))))
		            OR
		            (j.mode = 'batch' AND (
		                (j.action = 'apply' AND j.source_execution_id IS NULL AND NOT j.blocks_database)
		                OR
		                (j.action = 'forward' AND j.direction = 'up' AND NOT j.blocks_database AND j.source_execution_id IS NOT NULL AND EXISTS (
		                    SELECT 1 FROM app.jobs src
		                    WHERE src.execution_id = j.source_execution_id AND src.id <> j.id
		                      AND src.target_id = j.target_id AND src.database_id = j.database_id
		                      AND src.migration_id = j.migration_id AND src.revision = j.revision
		                      AND src.snapshot_hash = j.snapshot_hash AND src.mode = 'batch'
		                      AND src.direction = 'up' AND src.status = 'succeeded'
		                ))
		                OR
		                (j.action = 'resume' AND j.source_execution_id IS NOT NULL AND EXISTS (
		                    SELECT 1 FROM app.jobs src
		                    WHERE src.execution_id = j.source_execution_id
		                      AND src.id <> j.id
		                      AND src.target_id = j.target_id
		                      AND src.database_id = j.database_id
		                      AND src.migration_id = j.migration_id
		                      AND src.revision = j.revision
		                      AND src.snapshot_hash = j.snapshot_hash
		                      AND src.mode = 'batch'
		                      AND src.direction = j.direction
		                      AND src.status <> 'succeeded'
		                      AND `+validDownResumeSource+`
		                ))
		                OR
		                (j.action = 'compensate' AND j.mode = 'batch' AND j.direction = 'down' AND j.source_execution_id IS NOT NULL AND EXISTS (
		                    SELECT 1 FROM app.jobs src
		                    WHERE src.execution_id = j.source_execution_id
		                      AND src.id <> j.id
		                      AND src.target_id = j.target_id
		                      AND src.database_id = j.database_id
		                      AND src.migration_id = j.migration_id
		                      AND src.revision = j.revision
		                      AND src.snapshot_hash = j.snapshot_hash
		                      AND src.mode = 'batch'
		                      AND src.direction = 'up'
		                      AND src.status IN ('succeeded', 'partial', 'partially_reverted')
		                      AND EXISTS (
		                          SELECT 1 FROM app.job_batch_step_authorizations bsa
		                          JOIN app.job_batch_commit_proofs p ON p.step_auth_id = bsa.id
		                            AND p.target_xid = bsa.target_xid
		                            AND p.physical_target_id = bsa.physical_target_id
		                            AND p.direction = bsa.direction
		                            AND p.scope_execution_id = bsa.scope_execution_id
		                          WHERE bsa.scope_execution_id = src.execution_id
		                            AND bsa.physical_target_id = j.target_id
		                            AND bsa.direction = 'up'
		                      )
		                ))
		            ))
		        )
		        AND (SELECT count(*) FROM app.job_attempts a WHERE a.job_id = j.id) = j.attempt_no
		        AND (SELECT count(DISTINCT a.attempt_no) FROM app.job_attempts a WHERE a.job_id = j.id) = j.attempt_no
		        AND NOT EXISTS (
		            SELECT 1 FROM app.job_attempts a WHERE a.job_id = j.id AND
		                (a.attempt_no > j.attempt_no OR a.status <> 'failed' OR
		                 a.finished_at IS NULL OR a.sql_entered OR
		                 EXISTS (SELECT 1 FROM app.job_statement_evidence se WHERE se.attempt_id = a.id) OR
		                 EXISTS (SELECT 1 FROM app.job_batch_step_authorizations bsa WHERE bsa.attempt_id = a.id))
		        )))
		  AND NOT EXISTS (
		      SELECT 1 FROM app.migration_executions legacy
		      WHERE legacy.execution_target_id = j.target_id AND legacy.status IN ('running', 'unknown')
		  )
		  AND NOT EXISTS (
		      SELECT 1 FROM app.jobs active
		      WHERE active.target_id = j.target_id AND active.id <> j.id AND active.status = 'running'
		  )
		  AND NOT EXISTS (
		      SELECT 1 FROM app.jobs blocked
		      WHERE blocked.target_id = j.target_id AND blocked.id <> j.id
		        AND (
		            (
		             NOT (
		                 (
		                 j.mode = 'batch' AND blocked.mode = 'batch'
		                 AND blocked.target_id = j.target_id AND blocked.database_id = j.database_id
		                 AND blocked.migration_id = j.migration_id AND blocked.revision = j.revision
		                 AND blocked.snapshot_hash = j.snapshot_hash
		                 AND (
		                     (j.action = 'resume' AND j.direction = blocked.direction AND (
		                         blocked.execution_id IS NOT DISTINCT FROM j.source_execution_id
		                         OR (blocked.action = 'resume' AND blocked.source_execution_id IS NOT DISTINCT FROM j.source_execution_id)
		                     ))
		                     OR
		                     (j.action = 'compensate' AND j.direction = 'down' AND (
		                         blocked.execution_id IS NOT DISTINCT FROM j.source_execution_id
		                         OR (blocked.action IN ('resume', 'compensate') AND blocked.source_execution_id IS NOT DISTINCT FROM j.source_execution_id)
		                     ))
		                 )
		                 ) OR `+downResumeLineage("j.id", "blocked")+`
		             )
		             AND (
		                 blocked.blocks_database
		                 OR (
		                     (blocked.status IN ('unknown', 'partial', 'partially_reverted')
		                      OR blocked.execution_status IN ('unknown', 'partial', 'partially_reverted'))
		                     AND NOT `+resolvedBlocker("blocked")+`
		                 )
		                 OR EXISTS (SELECT 1 FROM app.job_attempts a
		                            WHERE a.job_id = blocked.id
		                       AND (a.sql_entered OR EXISTS (
		                           SELECT 1 FROM app.job_statement_evidence se WHERE se.attempt_id = a.id))
		                       AND (a.status = 'running' OR NOT (
		                           (blocked.mode = 'standard' AND EXISTS (SELECT 1 FROM app.job_dispatch_authorizations d WHERE d.attempt_id = a.id))
		                           OR (
		                               EXISTS (SELECT 1 FROM app.job_batch_step_authorizations bsa WHERE bsa.attempt_id = a.id)
		                               AND NOT EXISTS (
		                                   SELECT 1 FROM app.job_batch_step_authorizations unp
		                                   WHERE unp.attempt_id = a.id
		                                     AND NOT EXISTS (
		                                         SELECT 1 FROM app.job_batch_commit_proofs p
		                                         WHERE p.step_auth_id = unp.id
		                                           AND p.target_xid = unp.target_xid
		                                           AND p.physical_target_id = unp.physical_target_id
		                                           AND p.direction = unp.direction
		                                           AND p.scope_execution_id = unp.scope_execution_id
		                                     )
		                                     AND NOT EXISTS (
		                                         SELECT 1 FROM app.job_statement_evidence se
		                                         WHERE se.attempt_id = a.id AND se.ordinal = unp.ordinal AND se.batch_index = unp.batch_index AND se.outcome = 'failed'
		                                     )
		                               )
		                               AND NOT EXISTS (
		                                   SELECT 1 FROM app.job_statement_evidence unse
		                                   WHERE unse.attempt_id = a.id
		                                     AND (
		                                         unse.batch_index IS NULL
		                                         OR unse.outcome = 'unknown'
		                                         OR NOT (
		                                             (unse.outcome = 'failed' AND EXISTS (
		                                                 SELECT 1 FROM app.job_batch_step_authorizations bsa
		                                                 WHERE bsa.attempt_id = unse.attempt_id
		                                                   AND bsa.job_id = blocked.id
		                                                   AND bsa.ordinal = unse.ordinal
		                                                   AND bsa.batch_index = unse.batch_index
		                                                   AND bsa.physical_target_id = blocked.target_id
		                                                   AND bsa.direction = blocked.direction
		                                                   AND `+batchEvidenceScope("blocked")+`
		                                             ))
		                                             OR (unse.outcome = 'succeeded' AND EXISTS (
		                                                 SELECT 1 FROM app.job_batch_step_authorizations bsa
		                                                 JOIN app.job_batch_commit_proofs p ON p.step_auth_id = bsa.id
		                                                   AND p.target_xid = bsa.target_xid
		                                                   AND p.physical_target_id = bsa.physical_target_id
		                                                   AND p.direction = bsa.direction
		                                                   AND p.scope_execution_id = bsa.scope_execution_id
		                                                 WHERE bsa.attempt_id = unse.attempt_id
		                                                   AND bsa.job_id = blocked.id
		                                                   AND bsa.ordinal = unse.ordinal
		                                                   AND bsa.batch_index = unse.batch_index
		                                                   AND bsa.physical_target_id = blocked.target_id
		                                                   AND bsa.direction = blocked.direction
		                                                   AND `+batchEvidenceScope("blocked")+`
		                                             ))
		                                         )
		                                     )
		                               )
		                           ))))
		             )
		            )
		            OR
		             (
		                 (
		                 j.action = 'resume' AND j.mode = 'batch' AND blocked.mode = 'batch'
		                 AND blocked.target_id = j.target_id AND blocked.database_id = j.database_id
		                 AND blocked.migration_id = j.migration_id AND blocked.revision = j.revision
		                 AND blocked.direction = j.direction
		                 AND blocked.snapshot_hash = j.snapshot_hash
		                 AND (
		                     blocked.execution_id IS NOT DISTINCT FROM j.source_execution_id
		                     OR (blocked.action = 'resume' AND blocked.source_execution_id IS NOT DISTINCT FROM j.source_execution_id)
		                 )
		                 ) OR `+downResumeLineage("j.id", "blocked")+`
		             )
		             AND (
		                 (blocked.status = 'running' AND blocked.lease_expires_at > clock_timestamp())
		                 OR EXISTS (SELECT 1 FROM app.job_attempts a WHERE a.job_id = blocked.id AND a.status = 'running' AND blocked.lease_expires_at > clock_timestamp())
		             )
		            )
		        )
		ORDER BY j.created_at ASC
		LIMIT 1
		FOR UPDATE OF j, target SKIP LOCKED
	`, maxAttempts).Scan(
		&jobID, &databaseID, &targetID, &proposalID, &migrationID, &revision,
		&executionID, &actorID, &action, &direction, &mode, &attemptNo,
		&sourceExecutionID, &snapshotHash, &approvalFingerprint, &currentStatus,
		&traceparent, &tracestate, &queueWaitSeconds,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query claimable job: %w", err)
	}

	// The candidate snapshot may predate another claimant's commit.
	var busy bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM app.jobs WHERE target_id = $1 AND id <> $2 AND status = 'running'
	) OR EXISTS (
		SELECT 1 FROM app.migration_executions WHERE execution_target_id = $1 AND status IN ('running', 'unknown')
	)`, targetID, jobID).Scan(&busy); err != nil {
		return nil, fmt.Errorf("recheck claim target: %w", err)
	}
	if busy {
		return nil, nil
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
		Traceparent:         traceparent.String,
		Tracestate:          tracestate.String,
		QueueWaitSeconds:    queueWaitSeconds,
	}, nil
}
