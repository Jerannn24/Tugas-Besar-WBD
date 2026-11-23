package jobs

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/Jerannn24/Tugas-Besar-WBD.git/executioncontract"
	"github.com/Jerannn24/Tugas-Besar-WBD.git/migration"
)
var (
	chkPageLimit      = 20
	recoveryPageLimit = 20
)
func validateAgreedBatchSQL(dir executioncontract.Direction, sqlText string) error {
	normalized := strings.ToUpper(strings.Join(strings.Fields(strings.TrimRight(strings.TrimSpace(sqlText), ";")), " "))
	if dir == executioncontract.Up {
		if normalized != "UPDATE MANAGED.BATCH_ITEMS SET PROCESSED = TRUE WHERE ID = ANY($1::BIGINT[])" {
			return executioncontract.ContractError{
				Code:    "SQL_UNSUPPORTED",
				Message: "batch SQL does not match agreed immutable UPDATE pattern",
			}
		}
		return nil
	}
	if dir == executioncontract.Down {
		if normalized != "UPDATE MANAGED.BATCH_ITEMS SET PROCESSED = FALSE WHERE ID = ANY($1::BIGINT[])" {
			return executioncontract.ContractError{
				Code:    "SQL_UNSUPPORTED",
				Message: "batch SQL does not match agreed immutable UPDATE pattern",
			}
		}
		return nil
	}
	return executioncontract.ContractError{
		Code:    "INVALID_DIRECTION",
		Message: "unsupported direction for batch execution",
	}
}

func validateAgreedBatchConfig(cfg *executioncontract.BatchConfig) error {
	if cfg == nil {
		return executioncontract.ContractError{
			Code:    "INVALID_BATCH_CONFIG",
			Message: "batch configuration is required",
		}
	}
	if cfg.Schema != "managed" || cfg.Table != "batch_items" || cfg.CursorColumn != "id" ||
		cfg.BatchSize < 1 || cfg.BatchSize > 10000 {
		return executioncontract.ContractError{
			Code:    "INVALID_BATCH_CONFIG",
			Message: "batch configuration must target managed.batch_items with cursor id and batch size 1..10000",
		}
	}
	return nil
}

func (s *Service) failPartialJob(ctx context.Context, claim *Claim, code, msg string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin partial failure update: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `
		UPDATE app.jobs
		SET status = 'partial',
		    execution_status = 'partial',
		    error_code = $1,
		    error_message = $2,
		    error_retryable = false,
		    blocks_database = true,
		    finished_at = clock_timestamp(),
		    updated_at = clock_timestamp()
		WHERE id = $3 AND attempt_no = $4
		  AND status = 'running' AND execution_status = 'running'
		  AND lease_token = $5::uuid
		  AND lease_expires_at > clock_timestamp()
		  AND execution_id = $6 AND database_id = $7 AND target_id = $8
		  AND proposal_id = $9 AND migration_id = $10 AND revision = $11 AND actor_id = $12
		  AND action = $13 AND direction = $14 AND mode = $15
		  AND source_execution_id IS NOT DISTINCT FROM $16::uuid
		  AND snapshot_hash = $17 AND COALESCE(approval_fingerprint, '') = $18
	`, code, msg, claim.JobID, claim.AttemptNo, claim.LeaseToken,
		claim.ExecutionID, claim.DatabaseID, claim.TargetID, claim.ProposalID, claim.MigrationID,
		claim.Revision, claim.ActorID, claim.Action, claim.Direction, claim.Mode,
		claim.SourceExecutionID, claim.SnapshotHash, claim.ApprovalFingerprint)
	if err := requireOne(res, err, "fail partial job"); err != nil {
		return err
	}

	res, err = tx.ExecContext(ctx, `
		UPDATE app.job_attempts
		SET status = 'partial',
		    error_code = $1,
		    error_message = $2,
		    error_retryable = false,
		    finished_at = clock_timestamp()
		WHERE id = $3 AND job_id = $4 AND attempt_no = $5 AND status = 'running'
		  AND EXISTS (SELECT 1 FROM app.jobs WHERE id = $4 AND status = 'partial'
		              AND lease_token = $6::uuid AND lease_expires_at > clock_timestamp() AND attempt_no = $5)
	`, code, msg, claim.AttemptID, claim.JobID, claim.AttemptNo, claim.LeaseToken)
	if err := requireOne(res, err, "fail partial attempt"); err != nil {
		return err
	}

	return tx.Commit()
}
func (s *Service) failPartiallyRevertedJob(ctx context.Context, claim *Claim, code, msg string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin partially reverted failure update: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `
		UPDATE app.jobs
		SET status = 'partially_reverted',
		    execution_status = 'partially_reverted',
		    error_code = $1,
		    error_message = $2,
		    error_retryable = false,
		    blocks_database = true,
		    finished_at = clock_timestamp(),
		    updated_at = clock_timestamp()
		WHERE id = $3 AND attempt_no = $4
		  AND status = 'running' AND execution_status = 'running'
		  AND lease_token = $5::uuid
		  AND lease_expires_at > clock_timestamp()
		  AND execution_id = $6 AND database_id = $7 AND target_id = $8
		  AND proposal_id = $9 AND migration_id = $10 AND revision = $11 AND actor_id = $12
		  AND action = $13 AND direction = $14 AND mode = $15
		  AND source_execution_id IS NOT DISTINCT FROM $16::uuid
		  AND snapshot_hash = $17 AND COALESCE(approval_fingerprint, '') = $18
	`, code, msg, claim.JobID, claim.AttemptNo, claim.LeaseToken,
		claim.ExecutionID, claim.DatabaseID, claim.TargetID, claim.ProposalID, claim.MigrationID,
		claim.Revision, claim.ActorID, claim.Action, claim.Direction, claim.Mode,
		claim.SourceExecutionID, claim.SnapshotHash, claim.ApprovalFingerprint)
	if err := requireOne(res, err, "fail partially reverted job"); err != nil {
		return err
	}

	res, err = tx.ExecContext(ctx, `
		UPDATE app.job_attempts
		SET status = 'partially_reverted',
		    error_code = $1,
		    error_message = $2,
		    error_retryable = false,
		    finished_at = clock_timestamp()
		WHERE id = $3 AND job_id = $4 AND attempt_no = $5 AND status = 'running'
		  AND EXISTS (SELECT 1 FROM app.jobs WHERE id = $4 AND status = 'partially_reverted'
		              AND lease_token = $6::uuid AND lease_expires_at > clock_timestamp() AND attempt_no = $5)
	`, code, msg, claim.AttemptID, claim.JobID, claim.AttemptNo, claim.LeaseToken)
	if err := requireOne(res, err, "fail partially reverted attempt"); err != nil {
		return err
	}

	return tx.Commit()
}
func (s *Service) failCleanJob(ctx context.Context, claim *Claim, code, msg string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin clean failure update: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `
		UPDATE app.jobs
		SET status = 'failed',
		    execution_status = 'failed',
		    error_code = $1,
		    error_message = $2,
		    error_retryable = false,
		    blocks_database = false,
		    finished_at = clock_timestamp(),
		    updated_at = clock_timestamp()
		WHERE id = $3 AND attempt_no = $4
		  AND status = 'running' AND execution_status = 'running'
		  AND lease_token = $5::uuid
		  AND lease_expires_at > clock_timestamp()
		  AND execution_id = $6 AND database_id = $7 AND target_id = $8
		  AND proposal_id = $9 AND migration_id = $10 AND revision = $11 AND actor_id = $12
		  AND action = $13 AND direction = $14 AND mode = $15
		  AND source_execution_id IS NOT DISTINCT FROM $16::uuid
		  AND snapshot_hash = $17 AND COALESCE(approval_fingerprint, '') = $18
	`, code, msg, claim.JobID, claim.AttemptNo, claim.LeaseToken,
		claim.ExecutionID, claim.DatabaseID, claim.TargetID, claim.ProposalID, claim.MigrationID,
		claim.Revision, claim.ActorID, claim.Action, claim.Direction, claim.Mode,
		claim.SourceExecutionID, claim.SnapshotHash, claim.ApprovalFingerprint)
	if err := requireOne(res, err, "fail clean job"); err != nil {
		return err
	}

	res, err = tx.ExecContext(ctx, `
		UPDATE app.job_attempts
		SET status = 'failed',
		    error_code = $1,
		    error_message = $2,
		    error_retryable = false,
		    finished_at = clock_timestamp()
		WHERE id = $3 AND job_id = $4 AND attempt_no = $5 AND status = 'running'
		  AND EXISTS (SELECT 1 FROM app.jobs WHERE id = $4 AND status = 'failed'
		              AND lease_token = $6::uuid AND lease_expires_at > clock_timestamp() AND attempt_no = $5)
	`, code, msg, claim.AttemptID, claim.JobID, claim.AttemptNo, claim.LeaseToken)
	if err := requireOne(res, err, "fail clean attempt"); err != nil {
		return err
	}

	return tx.Commit()
}

func (s *Service) failBlockedBatchJob(ctx context.Context, claim *Claim, code, msg string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin blocked failure update: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `
		UPDATE app.jobs
		SET status = 'failed',
		    execution_status = 'failed',
		    error_code = $1,
		    error_message = $2,
		    error_retryable = false,
		    blocks_database = true,
		    finished_at = clock_timestamp(),
		    updated_at = clock_timestamp()
		WHERE id = $3 AND attempt_no = $4
		  AND status = 'running' AND execution_status = 'running'
		  AND lease_token = $5::uuid
		  AND lease_expires_at > clock_timestamp()
	`, code, msg, claim.JobID, claim.AttemptNo, claim.LeaseToken)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("fail blocked job: expected 1 row affected, got %d", n)
	}

	res, err = tx.ExecContext(ctx, `
		UPDATE app.job_attempts
		SET status = 'failed',
		    error_code = $1,
		    error_message = $2,
		    error_retryable = false,
		    finished_at = clock_timestamp()
		WHERE id = $3 AND job_id = $4 AND attempt_no = $5 AND status = 'running'
	`, code, msg, claim.AttemptID, claim.JobID, claim.AttemptNo)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("fail blocked attempt: expected 1 row affected, got %d", n)
	}

	return tx.Commit()
}

func (s *Service) failedBlockedOutcome(ctx context.Context, claim *Claim, code string, cause error) (*ExecutionOutcome, error) {
	if claim != nil && claim.LeaseToken != "" {
		if err := s.failBlockedBatchJob(ctx, claim, code, cause.Error()); err != nil {
			return nil, errors.Join(cause, err)
		}
	}
	errOut := cause
	var cErr executioncontract.ContractError
	if !errors.As(cause, &cErr) {
		errOut = executioncontract.ContractError{Code: code, Message: cause.Error()}
	}
	return &ExecutionOutcome{Status: executioncontract.Failed, Error: errOut}, nil
}

func (s *Service) completeBatchSuccessTx(ctx context.Context, ptx *sql.Tx, claim *Claim, scopeExecID string, progJSON []byte) error {
	res, err := ptx.ExecContext(ctx, `
		UPDATE app.jobs
		SET status = 'succeeded',
		    execution_status = 'succeeded',
		    blocks_database = false,
		    progress = $1::jsonb,
		    error_code = NULL,
		    error_message = NULL,
		    error_retryable = NULL,
		    finished_at = clock_timestamp(),
		    updated_at = clock_timestamp()
		WHERE id = $2 AND attempt_no = $3
		  AND status = 'running' AND execution_status = 'running'
		  AND lease_token = $4::uuid
		  AND lease_expires_at > clock_timestamp()
	`, string(progJSON), claim.JobID, claim.AttemptNo, claim.LeaseToken)
	if err != nil {
		return fmt.Errorf("finalize batch job success: %w", err)
	}
	if err := requireOne(res, err, "finalize batch job success"); err != nil {
		return err
	}

	res, err = ptx.ExecContext(ctx, `
		UPDATE app.job_attempts
		SET status = 'succeeded',
		    error_code = NULL,
		    error_message = NULL,
		    error_retryable = NULL,
		    finished_at = clock_timestamp()
		WHERE id = $1 AND job_id = $2 AND attempt_no = $3 AND status = 'running'
	`, claim.AttemptID, claim.JobID, claim.AttemptNo)
	if err != nil {
		return fmt.Errorf("finalize batch attempt success: %w", err)
	}
	if err := requireOne(res, err, "finalize batch attempt success"); err != nil {
		return err
	}

	if claim.Action == executioncontract.Resume {
		return s.releaseVerifiedBatchLineageBlockersTx(ctx, ptx, scopeExecID, claim.TargetID, claim.DatabaseID, claim.MigrationID, claim.Revision, claim.Direction, claim.SnapshotHash, claim.JobID, progJSON)
	}

	return nil
}

func (s *Service) releaseVerifiedBatchLineageBlockersTx(
	ctx context.Context,
	ptx *sql.Tx,
	scopeExecID string,
	targetID string,
	databaseID string,
	migrationID string,
	revision int,
	direction executioncontract.Direction,
	snapshotHash string,
	excludeJobID string,
	progJSON []byte,
) error {
	_, err := ptx.ExecContext(ctx, `
		UPDATE app.jobs
		SET execution_status = 'succeeded',
		    blocks_database = false,
		    progress = $1::jsonb,
		    updated_at = clock_timestamp()
		WHERE ((execution_id = $2::uuid AND action <> 'resume')
		       OR (source_execution_id = $2::uuid AND action = 'resume'))
		  AND target_id = $3::uuid
		  AND database_id = $4::uuid
		  AND migration_id = $5::uuid
		  AND revision = $6
		  AND direction = $7
		  AND snapshot_hash = $8
		  AND mode = 'batch'
		  AND ($9 = '' OR id <> $9::uuid)
		  AND status IN ('partial', 'failed')
		  AND (execution_status IN ('partial', 'failed') OR progress IS DISTINCT FROM $1::jsonb)
		  AND (blocks_database = false OR EXISTS (
		      SELECT 1 FROM app.job_batch_step_authorizations bsa
		      WHERE bsa.job_id = app.jobs.id
		  ))
		  AND NOT EXISTS (
		      SELECT 1 FROM app.job_attempts ja
		      WHERE ja.job_id = app.jobs.id AND ja.status IN ('running', 'unknown')
		  )
		  AND NOT EXISTS (
		      SELECT 1 FROM app.job_attempts ja
		      WHERE ja.job_id = app.jobs.id
		        AND (ja.sql_entered OR EXISTS (
		            SELECT 1 FROM app.job_statement_evidence se
		            WHERE se.attempt_id = ja.id
		        ))
		        AND NOT EXISTS (
		            SELECT 1 FROM app.job_batch_step_authorizations bsa
		            WHERE bsa.attempt_id = ja.id
		              AND bsa.job_id = app.jobs.id
		              AND bsa.physical_target_id = $3::uuid
		              AND bsa.direction = $7
		              AND bsa.scope_execution_id = $2::uuid
		        )
		  )
		  AND NOT EXISTS (
		      SELECT 1 FROM app.job_batch_step_authorizations bsa
		      WHERE bsa.job_id = app.jobs.id
		        AND (bsa.physical_target_id <> $3::uuid OR bsa.direction <> $7 OR bsa.scope_execution_id <> $2::uuid)
		  )
		  AND NOT EXISTS (
		      SELECT 1 FROM app.job_batch_step_authorizations bsa
		      WHERE bsa.job_id = app.jobs.id
		        AND NOT (
		            EXISTS (
		                SELECT 1 FROM app.job_batch_commit_proofs p
		                WHERE p.step_auth_id = bsa.id
		                  AND p.job_id = bsa.job_id
		                  AND p.attempt_id = bsa.attempt_id
		                  AND p.scope_execution_id = bsa.scope_execution_id
		                  AND p.physical_target_id = bsa.physical_target_id
		                  AND p.direction = bsa.direction
		                  AND p.batch_index = bsa.batch_index
		                  AND p.target_xid = bsa.target_xid
		            )
		            OR EXISTS (
		                SELECT 1 FROM app.job_statement_evidence se
		                WHERE se.attempt_id = bsa.attempt_id
		                  AND se.ordinal = bsa.ordinal
		                  AND se.batch_index = bsa.batch_index
		                  AND se.outcome = 'failed'
		            )
		        )
		  )
		  AND NOT EXISTS (
		      SELECT 1 FROM app.job_attempts ja
		      JOIN app.job_statement_evidence se ON se.attempt_id = ja.id
		      WHERE ja.job_id = app.jobs.id
		        AND (
		            se.batch_index IS NULL
		            OR se.outcome = 'unknown'
		            OR NOT (
		                (se.outcome = 'failed' AND EXISTS (
		                    SELECT 1 FROM app.job_batch_step_authorizations bsa
		                    WHERE bsa.attempt_id = se.attempt_id
		                      AND bsa.job_id = app.jobs.id
		                      AND bsa.ordinal = se.ordinal
		                      AND bsa.batch_index = se.batch_index
		                      AND bsa.physical_target_id = $3::uuid
		                      AND bsa.direction = $7
		                      AND bsa.scope_execution_id = $2::uuid
		                ))
		                OR
		                (se.outcome = 'succeeded' AND EXISTS (
		                    SELECT 1 FROM app.job_batch_step_authorizations bsa
		                    JOIN app.job_batch_commit_proofs p ON p.step_auth_id = bsa.id
		                      AND p.job_id = bsa.job_id
		                      AND p.attempt_id = bsa.attempt_id
		                      AND p.scope_execution_id = bsa.scope_execution_id
		                      AND p.physical_target_id = bsa.physical_target_id
		                      AND p.direction = bsa.direction
		                      AND p.batch_index = bsa.batch_index
		                      AND p.target_xid = bsa.target_xid
		                    WHERE bsa.attempt_id = se.attempt_id
		                      AND bsa.job_id = app.jobs.id
		                      AND bsa.ordinal = se.ordinal
		                      AND bsa.batch_index = se.batch_index
		                      AND bsa.physical_target_id = $3::uuid
		                      AND bsa.direction = $7
		                      AND bsa.scope_execution_id = $2::uuid
		                ))
		            )
		        )
		  )
	`, string(progJSON), scopeExecID, targetID, databaseID, migrationID, revision, string(direction), snapshotHash, excludeJobID)
	if err != nil {
		return fmt.Errorf("release verified batch lineage blockers: %w", err)
	}
	return nil
}

func (s *Service) releaseVerifiedBatchDownLineageBlockersTx(
	ctx context.Context,
	ptx *sql.Tx,
	scopeExecID string,
	targetID string,
	databaseID string,
	migrationID string,
	revision int,
	snapshotHash string,
	excludeJobID string,
) error {
	_, err := ptx.ExecContext(ctx, `
		UPDATE app.jobs
		SET blocks_database = false,
		    updated_at = clock_timestamp()
		WHERE ((execution_id = $1::uuid AND action <> 'resume')
		       OR (source_execution_id = $1::uuid AND action IN ('resume', 'compensate'))
		       OR ` + downResumeLineage("$7::uuid", "app.jobs") + `)
		  AND target_id = $2::uuid
		  AND database_id = $3::uuid
		  AND migration_id = $4::uuid
		  AND revision = $5
		  AND snapshot_hash = $6
		  AND mode = 'batch'
		  AND id <> $7::uuid
		  AND blocks_database = true
		  AND NOT EXISTS (
		      SELECT 1 FROM app.job_attempts ja
		      WHERE ja.job_id = app.jobs.id AND ja.status IN ('running', 'unknown')
		  )
		  AND NOT EXISTS (
		      SELECT 1 FROM app.job_attempts ja
		      WHERE ja.job_id = app.jobs.id
		        AND (ja.sql_entered OR EXISTS (
		            SELECT 1 FROM app.job_statement_evidence se
		            WHERE se.attempt_id = ja.id
		        ))
		        AND NOT EXISTS (
		            SELECT 1 FROM app.job_batch_step_authorizations bsa
		            WHERE bsa.attempt_id = ja.id
		              AND bsa.job_id = app.jobs.id
		              AND bsa.physical_target_id = $2::uuid
		              AND bsa.scope_execution_id = $1::uuid
		        )
		  )
	`, scopeExecID, targetID, databaseID, migrationID, revision, snapshotHash, excludeJobID)
	if err != nil {
		return fmt.Errorf("release verified down lineage blockers: %w", err)
	}
	return nil
}
func (s *Service) checkBatchLeaseAndTargetBlock(ctx context.Context, claim *Claim) error {
	var (
		status     string
		execStatus string
		token      sql.NullString
		unexpired  sql.NullBool
		attemptNo  int
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT status, execution_status, lease_token::text, lease_expires_at > clock_timestamp(), attempt_no
		FROM app.jobs
		WHERE id = $1
	`, claim.JobID).Scan(&status, &execStatus, &token, &unexpired, &attemptNo)
	if err != nil {
		return fmt.Errorf("check batch lease: %w", err)
	}
	if status != "running" || execStatus != "running" ||
		!token.Valid || token.String != claim.LeaseToken ||
		!unexpired.Valid || !unexpired.Bool ||
		attemptNo != claim.AttemptNo {
		return executioncontract.ContractError{
			Code:    "LEASE_MISMATCH",
			Message: "Job lease expired or ownership changed",
		}
	}

	var blocked bool
	err = s.db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM app.jobs j
			WHERE j.target_id = $1 AND j.id <> $2
			  AND (
			      (
			       NOT (
			           (
			           ($4 = 'resume' OR $4 = 'compensate') AND j.mode = 'batch'
			           AND ($4 <> 'resume' OR EXISTS (SELECT 1 FROM app.jobs current_job WHERE current_job.id = $2 AND current_job.direction = 'up'))
			           AND j.target_id = $1 AND j.database_id = $5
			           AND j.migration_id = $6 AND j.revision = $7
			           AND j.snapshot_hash = $8
			           AND (
			               j.execution_id IS NOT DISTINCT FROM $3::uuid
			               OR (j.action IN ('resume', 'compensate') AND j.source_execution_id IS NOT DISTINCT FROM $3::uuid)
			           )
			           ) OR ` + downResumeLineage("$2::uuid", "j") + `
			       )
			       AND (
			           j.blocks_database
			           OR (
			               (j.status IN ('unknown', 'partial', 'partially_reverted')
			                OR j.execution_status IN ('unknown', 'partial', 'partially_reverted'))
			               AND NOT EXISTS (
			                   SELECT 1 FROM app.jobs res
			                   WHERE (res.execution_id = j.execution_id OR res.source_execution_id = j.execution_id)
			                     AND res.target_id = j.target_id
			                     AND res.database_id = j.database_id
			                     AND res.direction = j.direction
			                     AND res.status = 'succeeded'
			                     AND res.execution_status = 'succeeded'
			                     AND NOT res.blocks_database
			               )
			           )
			           OR EXISTS (SELECT 1 FROM app.job_attempts a WHERE a.job_id = j.id
			                  AND (a.sql_entered OR EXISTS (
			                      SELECT 1 FROM app.job_statement_evidence se WHERE se.attempt_id = a.id))
			                  AND (a.status = 'running' OR NOT (
			                      (j.mode = 'standard' AND EXISTS (SELECT 1 FROM app.job_dispatch_authorizations d WHERE d.attempt_id = a.id))
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
			                                              AND bsa.job_id = j.id
			                                              AND bsa.ordinal = unse.ordinal
			                                              AND bsa.batch_index = unse.batch_index
			                                              AND bsa.physical_target_id = j.target_id
			                                              AND bsa.direction = j.direction
			                                              AND (bsa.scope_execution_id = j.execution_id OR (j.action = 'resume' AND bsa.scope_execution_id = j.source_execution_id))
			                                        ))
			                                        OR (unse.outcome = 'succeeded' AND EXISTS (
			                                            SELECT 1 FROM app.job_batch_step_authorizations bsa
			                                            JOIN app.job_batch_commit_proofs p ON p.step_auth_id = bsa.id
			                                              AND p.target_xid = bsa.target_xid
			                                              AND p.physical_target_id = bsa.physical_target_id
			                                              AND p.direction = bsa.direction
			                                              AND p.scope_execution_id = bsa.scope_execution_id
			                                            WHERE bsa.attempt_id = unse.attempt_id
			                                              AND bsa.job_id = j.id
			                                              AND bsa.ordinal = unse.ordinal
			                                              AND bsa.batch_index = unse.batch_index
			                                              AND bsa.physical_target_id = j.target_id
			                                              AND bsa.direction = j.direction
			                                              AND (bsa.scope_execution_id = j.execution_id OR (j.action = 'resume' AND bsa.scope_execution_id = j.source_execution_id))
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
			           (
			           ($4 = 'resume' OR $4 = 'compensate') AND j.mode = 'batch'
			           AND ($4 <> 'resume' OR EXISTS (SELECT 1 FROM app.jobs current_job WHERE current_job.id = $2 AND current_job.direction = 'up'))
			           AND j.target_id = $1 AND j.database_id = $5
			           AND j.migration_id = $6 AND j.revision = $7
			           AND j.snapshot_hash = $8
			           AND (
			               j.execution_id IS NOT DISTINCT FROM $3::uuid
			               OR (j.action IN ('resume', 'compensate') AND j.source_execution_id IS NOT DISTINCT FROM $3::uuid)
			           )
			           ) OR ` + downResumeLineage("$2::uuid", "j") + `
			       )
			       AND (
			           (j.status = 'running' AND j.lease_expires_at > clock_timestamp())
			           OR EXISTS (SELECT 1 FROM app.job_attempts a WHERE a.job_id = j.id AND a.status = 'running' AND j.lease_expires_at > clock_timestamp())
			       )
			      )
			  )
		)`, claim.TargetID, claim.JobID, claim.SourceExecutionID, claim.Action,
		claim.DatabaseID, claim.MigrationID, claim.Revision, claim.SnapshotHash).Scan(&blocked)
	if err != nil {
		return fmt.Errorf("check target block for batch: %w", err)
	}
	if blocked {
		return executioncontract.ContractError{
			Code:    "TARGET_BLOCKED",
			Message: "target has an unresolved execution",
		}
	}
	return nil
}

func (s *Service) authorizeBatchDispatch(ctx context.Context, claim *Claim, scopeExecID string, plan executioncontract.ExecutionPlan, sqlText, xid string, version int64, ordinal, batchIndex int, chunkIDs []int64) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("batch dispatch authorization aborted: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin dispatch auth tx: %w", err)
	}
	defer tx.Rollback()

	snapshotJSON, err := json.Marshal(plan.Snapshot)
	if err != nil {
		return fmt.Errorf("encode plan snapshot: %w", err)
	}
	if plan.Snapshot.MigrationID != claim.MigrationID || plan.Snapshot.DatabaseID != claim.DatabaseID ||
		plan.Snapshot.Revision != claim.Revision || plan.Snapshot.Mode != claim.Mode {
		return errors.New("plan snapshot identity mismatch")
	}

	var (
		status     string
		execStatus string
		token      sql.NullString
		unexpired  sql.NullBool
		attemptNo  int
	)
	err = tx.QueryRowContext(ctx, `
		SELECT status, execution_status, lease_token::text, lease_expires_at > clock_timestamp(), attempt_no
		FROM app.jobs
		WHERE id = $1 AND execution_id = $2 AND database_id = $3 AND target_id = $4
		  AND proposal_id = $5 AND migration_id = $6 AND revision = $7 AND actor_id = $8
		  AND action = $9 AND direction = $10 AND mode = $11
		  AND source_execution_id IS NOT DISTINCT FROM $12::uuid
		  AND snapshot_hash = $13 AND COALESCE(approval_fingerprint, '') = $14
		  AND snapshot = $15::jsonb
		FOR UPDATE
	`, claim.JobID, claim.ExecutionID, claim.DatabaseID, claim.TargetID, claim.ProposalID,
		claim.MigrationID, claim.Revision, claim.ActorID, claim.Action, claim.Direction, claim.Mode,
		claim.SourceExecutionID, claim.SnapshotHash, claim.ApprovalFingerprint, string(snapshotJSON)).
		Scan(&status, &execStatus, &token, &unexpired, &attemptNo)
	if err != nil {
		return fmt.Errorf("lock job for dispatch: %w", err)
	}
	if status != "running" || execStatus != "running" ||
		!token.Valid || token.String != claim.LeaseToken ||
		!unexpired.Valid || !unexpired.Bool ||
		attemptNo != claim.AttemptNo {
		return executioncontract.ContractError{
			Code:    "LEASE_MISMATCH",
			Message: "Job lease expired or ownership changed",
		}
	}

	var blocked bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM app.jobs j
			WHERE j.target_id = $1 AND j.id <> $2
			  AND (
			      (
			       NOT (
			           (
			           ($4 = 'resume' OR $4 = 'compensate') AND j.mode = 'batch'
			           AND ($4 <> 'resume' OR EXISTS (SELECT 1 FROM app.jobs current_job WHERE current_job.id = $2 AND current_job.direction = 'up'))
			           AND j.target_id = $1 AND j.database_id = $5
			           AND j.migration_id = $6 AND j.revision = $7
			           AND j.snapshot_hash = $8
			           AND (
			               j.execution_id IS NOT DISTINCT FROM $3::uuid
			               OR (j.action IN ('resume', 'compensate') AND j.source_execution_id IS NOT DISTINCT FROM $3::uuid)
			           )
			           ) OR ` + downResumeLineage("$2::uuid", "j") + `
			       )
			       AND (
			           j.blocks_database
			           OR (
			               (j.status IN ('unknown', 'partial', 'partially_reverted')
			                OR j.execution_status IN ('unknown', 'partial', 'partially_reverted'))
			               AND NOT EXISTS (
			                   SELECT 1 FROM app.jobs res
			                   WHERE (res.execution_id = j.execution_id OR res.source_execution_id = j.execution_id)
			                     AND res.target_id = j.target_id
			                     AND res.database_id = j.database_id
			                     AND res.direction = j.direction
			                     AND res.status = 'succeeded'
			                     AND res.execution_status = 'succeeded'
			                     AND NOT res.blocks_database
			               )
			           )
			           OR EXISTS (SELECT 1 FROM app.job_attempts a WHERE a.job_id = j.id
			                  AND (a.sql_entered OR EXISTS (
			                      SELECT 1 FROM app.job_statement_evidence se WHERE se.attempt_id = a.id))
			                  AND (a.status = 'running' OR NOT (
			                      (j.mode = 'standard' AND EXISTS (SELECT 1 FROM app.job_dispatch_authorizations d WHERE d.attempt_id = a.id))
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
			                                              AND bsa.job_id = j.id
			                                              AND bsa.ordinal = unse.ordinal
			                                              AND bsa.batch_index = unse.batch_index
			                                              AND bsa.physical_target_id = j.target_id
			                                              AND bsa.direction = j.direction
			                                              AND (bsa.scope_execution_id = j.execution_id OR (j.action = 'resume' AND bsa.scope_execution_id = j.source_execution_id))
			                                        ))
			                                        OR (unse.outcome = 'succeeded' AND EXISTS (
			                                            SELECT 1 FROM app.job_batch_step_authorizations bsa
			                                            JOIN app.job_batch_commit_proofs p ON p.step_auth_id = bsa.id
			                                              AND p.target_xid = bsa.target_xid
			                                              AND p.physical_target_id = bsa.physical_target_id
			                                              AND p.direction = bsa.direction
			                                              AND p.scope_execution_id = bsa.scope_execution_id
			                                            WHERE bsa.attempt_id = unse.attempt_id
			                                              AND bsa.job_id = j.id
			                                              AND bsa.ordinal = unse.ordinal
			                                              AND bsa.batch_index = unse.batch_index
			                                              AND bsa.physical_target_id = j.target_id
			                                              AND bsa.direction = j.direction
			                                              AND (bsa.scope_execution_id = j.execution_id OR (j.action = 'resume' AND bsa.scope_execution_id = j.source_execution_id))
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
			           (
			           ($4 = 'resume' OR $4 = 'compensate') AND j.mode = 'batch'
			           AND ($4 <> 'resume' OR EXISTS (SELECT 1 FROM app.jobs current_job WHERE current_job.id = $2 AND current_job.direction = 'up'))
			           AND j.target_id = $1 AND j.database_id = $5
			           AND j.migration_id = $6 AND j.revision = $7
			           AND j.snapshot_hash = $8
			           AND (
			               j.execution_id IS NOT DISTINCT FROM $3::uuid
			               OR (j.action IN ('resume', 'compensate') AND j.source_execution_id IS NOT DISTINCT FROM $3::uuid)
			           )
			           ) OR ` + downResumeLineage("$2::uuid", "j") + `
			       )
			       AND (
			           (j.status = 'running' AND j.lease_expires_at > clock_timestamp())
			           OR EXISTS (SELECT 1 FROM app.job_attempts a WHERE a.job_id = j.id AND a.status = 'running' AND j.lease_expires_at > clock_timestamp())
			       )
			      )
			  )
		)`, claim.TargetID, claim.JobID, claim.SourceExecutionID, claim.Action,
		claim.DatabaseID, claim.MigrationID, claim.Revision, claim.SnapshotHash).Scan(&blocked); err != nil {
		return fmt.Errorf("check target block: %w", err)
	}
	if blocked {
		return executioncontract.ContractError{Code: "TARGET_BLOCKED", Message: "target has an unresolved execution"}
	}

	planJSON, err := json.Marshal(plan)
	if err != nil {
		return fmt.Errorf("encode authorized plan: %w", err)
	}
	hash := sha256.Sum256([]byte(sqlText))
	var authExists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM app.job_dispatch_authorizations WHERE job_id = $1)`, claim.JobID).Scan(&authExists); err != nil {
		return fmt.Errorf("check existing dispatch auth: %w", err)
	}
	if !authExists {
		_, err = tx.ExecContext(ctx, `
			INSERT INTO app.job_dispatch_authorizations
			    (job_id, attempt_id, plan, sql_text, sql_sha256, target_xid, target_version)
			VALUES ($1, $2, $3::jsonb, $4, $5, $6, $7)
		`, claim.JobID, claim.AttemptID, string(planJSON), sqlText, hash[:], xid, version)
		if err != nil {
			return fmt.Errorf("record dispatch authorization: %w", err)
		}
	}

	chunkIDsJSON, err := json.Marshal(chunkIDs)
	if err != nil {
		return fmt.Errorf("encode chunk ids: %w", err)
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO app.job_batch_step_authorizations
		    (job_id, attempt_id, actor_id, job_execution_id, scope_execution_id,
		     physical_target_id, direction, batch_index, ordinal, chunk_ids, plan, sql_text, sql_sha256,
		     target_xid, target_version)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::jsonb, $11::jsonb, $12, $13, $14, $15)
	`, claim.JobID, claim.AttemptID, claim.ActorID, claim.ExecutionID, scopeExecID,
		claim.TargetID, string(claim.Direction), batchIndex, ordinal, string(chunkIDsJSON), string(planJSON), sqlText, hash[:],
		xid, version)
	if err != nil {
		return fmt.Errorf("record batch step dispatch authorization: %w", err)
	}

	res, err := tx.ExecContext(ctx, `
		UPDATE app.job_attempts
		SET sql_entered = true
		WHERE id = $1 AND job_id = $2 AND attempt_no = $3 AND status = 'running'
	`, claim.AttemptID, claim.JobID, claim.AttemptNo)
	if err != nil {
		return fmt.Errorf("record dispatch auth in attempt: %w", err)
	}
	rows, err := res.RowsAffected()
	if err != nil || rows != 1 {
		return executioncontract.ContractError{
			Code:    "ATTEMPT_MISMATCH",
			Message: "Active attempt could not be authorized for dispatch",
		}
	}

	res, err = tx.ExecContext(ctx, `
		UPDATE app.jobs SET blocks_database = true, updated_at = clock_timestamp()
		WHERE id = $1 AND status = 'running' AND execution_status = 'running'
		  AND lease_token = $2::uuid AND lease_expires_at > clock_timestamp() AND attempt_no = $3
	`, claim.JobID, claim.LeaseToken, claim.AttemptNo)
	if err := requireOne(res, err, "block authorized job"); err != nil {
		return err
	}

	paramsJSON, err := json.Marshal(map[string]any{"ids": chunkIDs})
	if err != nil {
		return fmt.Errorf("encode chunk parameters: %w", err)
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO app.job_statement_evidence
		    (attempt_id, ordinal, batch_index, sql_text, safe_parameters, outcome, executed_at)
		VALUES ($1::uuid, $2, $3, $4, $5::jsonb, 'unknown', clock_timestamp())
		ON CONFLICT (attempt_id, ordinal) DO UPDATE
		SET outcome = 'unknown', batch_index = EXCLUDED.batch_index,
		    sql_text = EXCLUDED.sql_text, safe_parameters = EXCLUDED.safe_parameters,
		    executed_at = clock_timestamp()
	`, claim.AttemptID, ordinal, batchIndex, sqlText, string(paramsJSON))
	if err != nil {
		return fmt.Errorf("record batch statement evidence: %w", err)
	}

	return tx.Commit()
}

func (s *Service) reserveBatchTicket(ctx context.Context, session *migration.TargetSession, scopeExecID, targetID, xid string, batchIdx int, direction executioncontract.Direction, capability []byte) error {
	_, err := session.Preparer().ExecContext(ctx, `
		SELECT wbd_auth.reserve_batch_ticket($1::uuid, $2::uuid, $3::xid8, $4, $5, $6::bytea)
	`, scopeExecID, targetID, xid, batchIdx, string(direction), capability)
	if err != nil {
		return fmt.Errorf("reserve batch ticket: %w", err)
	}
	return nil
}

func (s *Service) rollbackAndFailBatch(ctx context.Context, claim *Claim, session *migration.TargetSession, xid string, batchIdx int, ordinal int, earlierCommitted bool, code string, cause error, deps ExecutionDeps) (*ExecutionOutcome, error) {
	rollbackErr := session.Rollback()
	if ctx.Err() != nil || rollbackErr != nil {
		return s.unknownOutcome(ctx, claim, fmt.Errorf("Target rollback could not be confirmed: %w; original cause: %w", errors.Join(rollbackErr, ctx.Err()), cause))
	}

	errMsg := ""
	if cause != nil {
		errMsg = cause.Error()
	}

	if ordinal == 0 {
		if claim.Direction == executioncontract.Down {
			if earlierCommitted {
				if err := s.failPartiallyRevertedJob(ctx, claim, code, errMsg); err != nil {
					return nil, errors.Join(cause, err)
				}
				return &ExecutionOutcome{Status: executioncontract.PartiallyReverted, Error: cause}, nil
			}
			if err := s.failBlockedBatchJob(ctx, claim, code, errMsg); err != nil {
				return nil, errors.Join(cause, err)
			}
			return &ExecutionOutcome{Status: executioncontract.Failed, Error: cause}, nil
		}
		if earlierCommitted {
			if err := s.failPartialJob(ctx, claim, code, errMsg); err != nil {
				return nil, errors.Join(cause, err)
			}
			return &ExecutionOutcome{Status: executioncontract.Partial, Error: cause}, nil
		}
		if err := s.failCleanJob(ctx, claim, code, errMsg); err != nil {
			return nil, errors.Join(cause, err)
		}
		return &ExecutionOutcome{Status: executioncontract.Failed, Error: cause}, nil
	}

	updateCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(updateCtx, nil)
	if err != nil {
		return nil, errors.Join(cause, fmt.Errorf("begin rolled-back batch failure update: %w", err))
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(updateCtx, `
		UPDATE app.job_statement_evidence
		SET outcome = 'failed'
		WHERE attempt_id = $1 AND ordinal = $2 AND batch_index = $3
		  AND EXISTS (
		      SELECT 1 FROM app.job_batch_step_authorizations bsa
		      WHERE bsa.job_id = $4 AND bsa.attempt_id = $1 AND bsa.ordinal = $2
		        AND bsa.batch_index = $3 AND bsa.target_xid = $5 AND bsa.physical_target_id = $6
		  )
	`, claim.AttemptID, ordinal, batchIdx, claim.JobID, xid, claim.TargetID)
	if err := requireOne(res, err, "record batch statement evidence failure"); err != nil {
		return nil, errors.Join(cause, err)
	}

	if deps.afterRollbackFirstWrite != nil {
		if hookErr := deps.afterRollbackFirstWrite(updateCtx); hookErr != nil {
			return nil, errors.Join(cause, hookErr)
		}
	}

	var targetStatus, targetExecStatus string
	var targetBlocks bool
	if claim.Direction == executioncontract.Down {
		if earlierCommitted {
			targetStatus = "partially_reverted"
			targetExecStatus = "partially_reverted"
			targetBlocks = true
		} else {
			targetStatus = "failed"
			targetExecStatus = "failed"
			targetBlocks = true
		}
	} else {
		if earlierCommitted {
			targetStatus = "partial"
			targetExecStatus = "partial"
			targetBlocks = true
		} else {
			targetStatus = "failed"
			targetExecStatus = "failed"
			targetBlocks = false
		}
	}

	res, err = tx.ExecContext(updateCtx, `
		UPDATE app.jobs
		SET status = $1, execution_status = $2, blocks_database = $3,
		    error_code = $4, error_message = $5, error_retryable = false,
		    finished_at = clock_timestamp(), updated_at = clock_timestamp()
		WHERE id = $6 AND status = 'running' AND execution_status = 'running'
		  AND blocks_database AND lease_token = $7::uuid
		  AND lease_expires_at > clock_timestamp() AND attempt_no = $8
		  AND execution_id = $9 AND database_id = $10 AND target_id = $11
		  AND proposal_id = $12 AND migration_id = $13 AND revision = $14 AND actor_id = $15
		  AND action = $16 AND direction = $17 AND mode = $18
		  AND source_execution_id IS NOT DISTINCT FROM $19::uuid
		  AND snapshot_hash = $20 AND COALESCE(approval_fingerprint, '') = $21
		  AND EXISTS (
		      SELECT 1 FROM app.job_batch_step_authorizations d
		      JOIN app.job_attempts a ON a.id = d.attempt_id AND a.job_id = d.job_id
		      WHERE d.job_id = $6 AND d.attempt_id = $22 AND d.physical_target_id = $11
		        AND d.target_xid = $23 AND d.batch_index = $24 AND d.ordinal = $25
		        AND d.direction = $17 AND d.job_execution_id = $9
		        AND a.attempt_no = $8 AND a.status = 'running' AND a.sql_entered
		  )
	`, targetStatus, targetExecStatus, targetBlocks,
		code, errMsg, claim.JobID, claim.LeaseToken, claim.AttemptNo,
		claim.ExecutionID, claim.DatabaseID, claim.TargetID,
		claim.ProposalID, claim.MigrationID, claim.Revision, claim.ActorID,
		claim.Action, claim.Direction, claim.Mode,
		claim.SourceExecutionID, claim.SnapshotHash, claim.ApprovalFingerprint,
		claim.AttemptID, xid, batchIdx, ordinal)
	if err = requireOne(res, err, "fail rolled-back batch job"); err != nil {
		return nil, errors.Join(cause, err)
	}

	res, err = tx.ExecContext(updateCtx, `
		UPDATE app.job_attempts
		SET status = $1, error_code = $2,
		    error_message = $3, error_retryable = false, finished_at = clock_timestamp()
		WHERE id = $4 AND job_id = $5 AND attempt_no = $6 AND status = 'running' AND sql_entered
		  AND EXISTS (
		      SELECT 1 FROM app.jobs WHERE id = $5 AND status = $1
		        AND lease_token = $7::uuid AND lease_expires_at > clock_timestamp() AND attempt_no = $6
		  )
	`, targetStatus, code, errMsg, claim.AttemptID, claim.JobID, claim.AttemptNo, claim.LeaseToken)
	if err = requireOne(res, err, "fail rolled-back batch attempt"); err != nil {
		return nil, errors.Join(cause, err)
	}

	if err := ctx.Err(); err != nil {
		if rollbackErr := tx.Rollback(); rollbackErr != nil {
			return nil, errors.Join(cause, err, fmt.Errorf("rollback failure update: %w", rollbackErr))
		}
		return s.unknownOutcome(ctx, claim, errors.Join(cause, err))
	}

	if err = tx.Commit(); err != nil {
		return nil, errors.Join(cause, fmt.Errorf("commit rolled-back batch failure update: %w", err))
	}

	if claim.Direction == executioncontract.Down {
		if earlierCommitted {
			return &ExecutionOutcome{Status: executioncontract.PartiallyReverted, Error: cause}, nil
		}
		return &ExecutionOutcome{Status: executioncontract.Failed, Error: cause}, nil
	}
	if earlierCommitted {
		return &ExecutionOutcome{Status: executioncontract.Partial, Error: cause}, nil
	}
	return &ExecutionOutcome{Status: executioncontract.Failed, Error: cause}, nil
}

func queryBatchCommitTimestamp(ctx context.Context, session *migration.TargetSession, scopeExecID, direction string, batchIdx int, xid string) (time.Time, error) {
	var timestamp sql.NullTime
	err := session.Preparer().QueryRowContext(ctx, `
		SELECT CASE WHEN current_setting('track_commit_timestamp') = 'on'
		  AND pg_catalog.pg_xact_status($1::text::xid8) = 'committed'
		  AND $1::text::numeric <= pg_catalog.pg_current_xact_id()::text::numeric
		  AND pg_catalog.pg_current_xact_id()::text::numeric - $1::text::numeric < 2147483648
		THEN pg_catalog.pg_xact_commit_timestamp((($1::text::numeric % 4294967296)::bigint::text)::xid)
		END
		FROM wbd_auth.batch_checkpoints
		WHERE execution_id = $2::uuid AND direction = $3 AND batch_index = $4 AND target_xid = $1::text::xid8
	`, xid, scopeExecID, direction, batchIdx).Scan(&timestamp)
	if err != nil || !timestamp.Valid {
		return time.Time{}, errors.Join(errors.New("target commit timestamp is unavailable"), err)
	}
	var valid bool
	err = session.Preparer().QueryRowContext(ctx, `
		SELECT $2::timestamptz >= recorded_at
		FROM wbd_auth.batch_checkpoints
		WHERE execution_id = $1::uuid AND direction = $3 AND batch_index = $4 AND target_xid = $5::xid8
	`, scopeExecID, timestamp.Time, direction, batchIdx, xid).Scan(&valid)
	if err != nil || !valid {
		return time.Time{}, errors.New("target commit timestamp precedes receipt/checkpoint")
	}
	return timestamp.Time, nil
}
func fetchScopeChunk(ctx context.Context, session *migration.TargetSession, scopeExecID string, afterID *int64, limit int64) ([]int64, error) {
	var rows *sql.Rows
	var err error
	if afterID == nil {
		rows, err = session.Preparer().QueryContext(ctx, `
			SELECT item_id
			FROM wbd_auth.batch_scope_items
			WHERE execution_id = $1::uuid
			ORDER BY item_id ASC
			LIMIT $2
		`, scopeExecID, limit)
	} else {
		rows, err = session.Preparer().QueryContext(ctx, `
			SELECT item_id
			FROM wbd_auth.batch_scope_items
			WHERE execution_id = $1::uuid AND item_id > $2
			ORDER BY item_id ASC
			LIMIT $3
		`, scopeExecID, *afterID, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("read chunk items from scope: %w", err)
	}
	defer rows.Close()

	var chunk []int64
	for rows.Next() {
		var idVal int64
		if err := rows.Scan(&idVal); err != nil {
			return nil, fmt.Errorf("scan chunk item: %w", err)
		}
		chunk = append(chunk, idVal)
	}
	return chunk, rows.Err()
}

type unresStep struct {
	id           string
	jobID        string
	attemptID    string
	batchIndex   int
	ordinal      int
	targetXID    string
	sqlText      string
	chunkIDsJSON string
}

type origRunning struct {
	jobID          string
	executionID    string
	attemptNo      int
	jobStatus      string
	execStatus     string
	attemptID      string
	attemptNoAtt   int
	attemptStatus  string
	leaseExpiresAt time.Time
	isExpired      bool
}

type chkInfo struct {
	index int
	rows  int64
	xid   string
	time  time.Time
}

type origAuthRow struct {
	bsaID            string
	bsaJobID         string
	bsaAttemptID     string
	bsaActorID       string
	bsaJobExecID     string
	bsaScopeExecID   string
	bsaTargetID      string
	bsaDirection     string
	bsaBatchIdx      int
	bsaOrdinal       int
	bsaChunkIDsJSON  string
	bsaPlanJSON      string
	bsaSQLText       string
	bsaSQLSha256     []byte
	bsaTargetXID     string
	bsaTargetVersion int64

	origJobID        string
	origExecID       string
	origProposalID   string
	origMigrationID  string
	origRevision     int
	origActorID      string
	origAction       string
	origDirection    string
	origMode         string
	origDatabaseID   string
	origTargetID     string
	origSourceExecID sql.NullString
	origSnapHash     string
	origSnapJSON     string

	origAttID         string
	origAttJobID      string
	origAttNo         int
	origAttStatus     string
	origAttSQLEntered bool
}

type pendingFinalStep struct {
	step        origAuthRow
	chk         chkInfo
	chunkIDs    []int64
	committedAt time.Time
	evidence    executioncontract.CommitEvidence
	progJSON    []byte
}

func (s *Service) handleClosureTxError(ctx context.Context, claim *Claim, err error) (*ExecutionOutcome, error) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return nil, err
	}
	var cErr executioncontract.ContractError
	if errors.As(err, &cErr) && cErr.Code == "TARGET_BLOCKED" {
		return s.failedBlockedOutcome(ctx, claim, "TARGET_BLOCKED", err)
	}
	return s.unknownOutcome(ctx, claim, err)
}

func (s *Service) applyOriginatingClosuresTx(
	ctx context.Context,
	ptx *sql.Tx,
	claim *Claim,
	session *migration.TargetSession,
	scopeExecID string,
	totalBatches int,
	pendingFinal *pendingFinalStep,
	finalizerJobID string,
	finalizerAttemptID string,
	overallSucceeded bool,
	deps ExecutionDeps,
) error {
	var (
		lastBatchIdx = -1
		lastOrdinal  = -1
		lastID       = ""
		authPageIdx  = 0
	)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		var page []unresStep
		err := func() error {
			var rows *sql.Rows
			var qErr error
			if lastID == "" {
				rows, qErr = ptx.QueryContext(ctx, `
					SELECT bsa.id::text, bsa.job_id::text, bsa.attempt_id::text,
					       bsa.batch_index, bsa.ordinal, bsa.target_xid,
					       bsa.sql_text, bsa.chunk_ids::text
					FROM app.job_batch_step_authorizations bsa
					WHERE bsa.scope_execution_id = $1::uuid
					  AND bsa.physical_target_id = $2::uuid
					  AND bsa.direction = $3
					ORDER BY bsa.batch_index ASC, bsa.ordinal ASC, bsa.id ASC
					LIMIT $4
				`, scopeExecID, claim.TargetID, string(claim.Direction), recoveryPageLimit)
			} else {
				rows, qErr = ptx.QueryContext(ctx, `
					SELECT bsa.id::text, bsa.job_id::text, bsa.attempt_id::text,
					       bsa.batch_index, bsa.ordinal, bsa.target_xid,
					       bsa.sql_text, bsa.chunk_ids::text
					FROM app.job_batch_step_authorizations bsa
					WHERE bsa.scope_execution_id = $1::uuid
					  AND bsa.physical_target_id = $2::uuid
					  AND bsa.direction = $3
					  AND (bsa.batch_index, bsa.ordinal, bsa.id) > ($4, $5, $6::uuid)
					ORDER BY bsa.batch_index ASC, bsa.ordinal ASC, bsa.id ASC
					LIMIT $7
				`, scopeExecID, claim.TargetID, string(claim.Direction), lastBatchIdx, lastOrdinal, lastID, recoveryPageLimit)
			}
			if qErr != nil {
				return fmt.Errorf("query authorized steps page: %w", qErr)
			}
			defer rows.Close()

			for rows.Next() {
				var st unresStep
				if err := rows.Scan(&st.id, &st.jobID, &st.attemptID, &st.batchIndex, &st.ordinal, &st.targetXID, &st.sqlText, &st.chunkIDsJSON); err != nil {
					return fmt.Errorf("scan authorized step: %w", err)
				}
				page = append(page, st)
			}
			return rows.Err()
		}()
		if err != nil {
			return err
		}
		if len(page) == 0 {
			break
		}

		if deps.beforeRecoveryClosurePage != nil {
			if err := deps.beforeRecoveryClosurePage(ctx, "authorizations", authPageIdx, len(page)); err != nil {
				return err
			}
		}
		authPageIdx++

		for _, st := range page {
			if err := ctx.Err(); err != nil {
				return err
			}
			lastBatchIdx = st.batchIndex
			lastOrdinal = st.ordinal
			lastID = st.id

			var proofExists bool
			err := ptx.QueryRowContext(ctx, `
				SELECT EXISTS (
					SELECT 1 FROM app.job_batch_commit_proofs
					WHERE step_auth_id = $1::uuid
					  AND target_xid = $2
					  AND physical_target_id = $3::uuid
					  AND direction = $4
					  AND scope_execution_id = $5::uuid
				)
			`, st.id, st.targetXID, claim.TargetID, string(claim.Direction), scopeExecID).Scan(&proofExists)
			if err != nil {
				return fmt.Errorf("check commit proof for step %s: %w", st.id, err)
			}
			if proofExists {
				continue
			}
			if pendingFinal != nil && pendingFinal.step.bsaID == st.id && pendingFinal.chk.index == st.batchIndex && pendingFinal.chk.xid == st.targetXID {
				continue
			}

			var hasTargetCheckpoint bool
			err = session.Preparer().QueryRowContext(ctx, `
				SELECT true
				FROM wbd_auth.batch_checkpoints
				WHERE execution_id = $1::uuid AND direction = $2 AND target_xid = $3::xid8
			`, scopeExecID, string(claim.Direction), st.targetXID).Scan(&hasTargetCheckpoint)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return executioncontract.ContractError{
					Code:    "TARGET_BLOCKED",
					Message: fmt.Sprintf("query target checkpoint for unproven step %s: %v", st.id, err),
				}
			}
			if hasTargetCheckpoint {
				return executioncontract.ContractError{
					Code: "TARGET_BLOCKED",
					Message: fmt.Sprintf("unproven authorized step %s batch %d target xid %s has unverified target checkpoint",
						st.id, st.batchIndex, st.targetXID),
				}
			}

			var xactStatus string
			err = session.Preparer().QueryRowContext(ctx, `
				SELECT pg_catalog.pg_xact_status($1::xid8)
			`, st.targetXID).Scan(&xactStatus)
			if err != nil {
				return executioncontract.ContractError{
					Code: "TARGET_BLOCKED",
					Message: fmt.Sprintf("unresolved authorized step %s batch %d target xid %s status unavailable: %v",
						st.id, st.batchIndex, st.targetXID, err),
				}
			}

			switch xactStatus {
			case "aborted":
				var bsaChunkIDs []int64
				if err := json.Unmarshal([]byte(st.chunkIDsJSON), &bsaChunkIDs); err != nil {
					return executioncontract.ContractError{
						Code: "TARGET_BLOCKED",
						Message: fmt.Sprintf("unresolved authorized step %s batch %d target xid %s chunk IDs malformed: %v",
							st.id, st.batchIndex, st.targetXID, err),
					}
				}

				type evRow struct {
					ordinal   int
					batchIdx  sql.NullInt64
					outcome   string
					sqlText   string
					paramsRaw sql.NullString
				}
				var evList []evRow
				qRows, err := ptx.QueryContext(ctx, `
					SELECT ordinal, batch_index, outcome, sql_text, safe_parameters::text
					FROM app.job_statement_evidence
					WHERE attempt_id = $1::uuid AND (batch_index = $2 OR ordinal = $3)
					LIMIT 2
				`, st.attemptID, st.batchIndex, st.ordinal)
				if err != nil {
					return fmt.Errorf("query statement evidence for aborted step %s: %w", st.id, err)
				}
				for qRows.Next() {
					var r evRow
					if err := qRows.Scan(&r.ordinal, &r.batchIdx, &r.outcome, &r.sqlText, &r.paramsRaw); err != nil {
						qRows.Close()
						return fmt.Errorf("scan statement evidence for aborted step %s: %w", st.id, err)
					}
					evList = append(evList, r)
				}
				qRows.Close()
				if err := qRows.Err(); err != nil {
					return fmt.Errorf("iterate statement evidence for aborted step %s: %w", st.id, err)
				}

				if len(evList) != 1 {
					return executioncontract.ContractError{
						Code: "TARGET_BLOCKED",
						Message: fmt.Sprintf("unresolved authorized step %s batch %d target xid %s expected exactly 1 statement evidence row, got %d",
							st.id, st.batchIndex, st.targetXID, len(evList)),
					}
				}

				ev := evList[0]
				if !ev.batchIdx.Valid || int(ev.batchIdx.Int64) != st.batchIndex || ev.ordinal != st.ordinal {
					return executioncontract.ContractError{
						Code: "TARGET_BLOCKED",
						Message: fmt.Sprintf("unresolved authorized step %s statement evidence key mismatch: batch_index=%v ordinal=%d, expected (%d, %d)",
							st.id, ev.batchIdx, ev.ordinal, st.batchIndex, st.ordinal),
					}
				}

				if ev.sqlText == "" || ev.sqlText != st.sqlText {
					return executioncontract.ContractError{
						Code: "TARGET_BLOCKED",
						Message: fmt.Sprintf("unresolved authorized step %s statement evidence SQL mismatch or empty", st.id),
					}
				}

				if !ev.paramsRaw.Valid || ev.paramsRaw.String == "" || ev.paramsRaw.String == "{}" {
					return executioncontract.ContractError{
						Code: "TARGET_BLOCKED",
						Message: fmt.Sprintf("unresolved authorized step %s statement evidence parameters missing or empty", st.id),
					}
				}
				var pMap map[string]json.RawMessage
				if err := json.Unmarshal([]byte(ev.paramsRaw.String), &pMap); err != nil {
					return executioncontract.ContractError{
						Code: "TARGET_BLOCKED",
						Message: fmt.Sprintf("unresolved authorized step %s statement evidence parameters not an object: %v", st.id, err),
					}
				}
				rawIDs, hasIDs := pMap["ids"]
				if !hasIDs || string(rawIDs) == "null" {
					return executioncontract.ContractError{
						Code: "TARGET_BLOCKED",
						Message: fmt.Sprintf("unresolved authorized step %s statement evidence parameters missing ids", st.id),
					}
				}
				var evIDs []int64
				if err := json.Unmarshal(rawIDs, &evIDs); err != nil || len(evIDs) == 0 {
					return executioncontract.ContractError{
						Code: "TARGET_BLOCKED",
						Message: fmt.Sprintf("unresolved authorized step %s statement evidence ids invalid type or empty", st.id),
					}
				}
				if len(evIDs) != len(bsaChunkIDs) {
					return executioncontract.ContractError{
						Code: "TARGET_BLOCKED",
						Message: fmt.Sprintf("unresolved authorized step %s statement evidence ids count mismatch: got %d, expected %d",
							st.id, len(evIDs), len(bsaChunkIDs)),
					}
				}
				for i := range bsaChunkIDs {
					if evIDs[i] != bsaChunkIDs[i] {
						return executioncontract.ContractError{
							Code: "TARGET_BLOCKED",
							Message: fmt.Sprintf("unresolved authorized step %s statement evidence id mismatch at index %d: got %d, expected %d",
								st.id, i, evIDs[i], bsaChunkIDs[i]),
						}
					}
				}

				if ev.outcome != "unknown" && ev.outcome != "running" && ev.outcome != "failed" {
					return executioncontract.ContractError{
						Code: "TARGET_BLOCKED",
						Message: fmt.Sprintf("unresolved authorized step %s statement evidence unsupported outcome %q for aborted target transaction",
							st.id, ev.outcome),
					}
				}

				res, err := ptx.ExecContext(ctx, `
					UPDATE app.job_statement_evidence
					SET outcome = 'failed'
					WHERE attempt_id = $1::uuid AND ordinal = $2 AND batch_index = $3
				`, st.attemptID, st.ordinal, st.batchIndex)
				if err != nil {
					return fmt.Errorf("update aborted statement evidence: %w", err)
				}
				if err := requireOne(res, err, "update aborted statement evidence"); err != nil {
					return err
				}
			case "committed":
				return executioncontract.ContractError{
					Code: "TARGET_BLOCKED",
					Message: fmt.Sprintf("unresolved authorized step %s batch %d target xid %s committed without checkpoint",
						st.id, st.batchIndex, st.targetXID),
				}
			case "in progress":
				return executioncontract.ContractError{
					Code: "TARGET_BLOCKED",
					Message: fmt.Sprintf("unresolved authorized step %s batch %d target xid %s is in progress",
						st.id, st.batchIndex, st.targetXID),
				}
			default:
				return executioncontract.ContractError{
					Code: "TARGET_BLOCKED",
					Message: fmt.Sprintf("unresolved authorized step %s batch %d target xid %s status is %s",
						st.id, st.batchIndex, st.targetXID, xactStatus),
				}
			}
		}

		if len(page) < recoveryPageLimit {
			break
		}
	}

	var (
		lastAttCreatedAt time.Time
		lastAttJobID     string
		lastAttemptID    string
		attPageIdx       = 0
	)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		var page []origRunning
		err := func() error {
			var rows *sql.Rows
			var qErr error
			if lastAttemptID == "" {
				rows, qErr = ptx.QueryContext(ctx, `
					SELECT j.id::text, j.execution_id::text, j.attempt_no, j.status, j.execution_status,
					       ja.id::text, ja.attempt_no, ja.status, j.lease_expires_at,
					       (j.lease_expires_at IS NULL OR j.lease_expires_at <= clock_timestamp()), j.created_at
					FROM app.jobs j
					JOIN app.job_attempts ja ON ja.job_id = j.id AND ja.attempt_no = j.attempt_no
					WHERE ((j.execution_id = $1::uuid AND j.action <> 'resume')
					       OR (j.source_execution_id = $1::uuid AND j.action = 'resume'))
					  AND j.target_id = $2::uuid
					  AND j.database_id = $3::uuid
					  AND j.migration_id = $4::uuid
					  AND j.revision = $5
					  AND j.direction = $6
					  AND j.snapshot_hash = $7
					  AND j.mode = 'batch'
					  AND (j.status IN ('running', 'unknown') OR ja.status IN ('running', 'unknown'))
					  AND ($8 = '' OR (j.id <> $9::uuid OR ja.attempt_no < $10))
					ORDER BY j.created_at ASC, j.id ASC, ja.id ASC
					LIMIT $11
				`, scopeExecID, claim.TargetID, claim.DatabaseID, claim.MigrationID, claim.Revision, string(claim.Direction), claim.SnapshotHash, claim.LeaseToken, claim.JobID, claim.AttemptNo, recoveryPageLimit)
			} else {
				rows, qErr = ptx.QueryContext(ctx, `
					SELECT j.id::text, j.execution_id::text, j.attempt_no, j.status, j.execution_status,
					       ja.id::text, ja.attempt_no, ja.status, j.lease_expires_at,
					       (j.lease_expires_at IS NULL OR j.lease_expires_at <= clock_timestamp()), j.created_at
					FROM app.jobs j
					JOIN app.job_attempts ja ON ja.job_id = j.id AND ja.attempt_no = j.attempt_no
					WHERE ((j.execution_id = $1::uuid AND j.action <> 'resume')
					       OR (j.source_execution_id = $1::uuid AND j.action = 'resume'))
					  AND j.target_id = $2::uuid
					  AND j.database_id = $3::uuid
					  AND j.migration_id = $4::uuid
					  AND j.revision = $5
					  AND j.direction = $6
					  AND j.snapshot_hash = $7
					  AND j.mode = 'batch'
					  AND (j.status IN ('running', 'unknown') OR ja.status IN ('running', 'unknown'))
					  AND ($8 = '' OR (j.id <> $9::uuid OR ja.attempt_no < $10))
					  AND (j.created_at, j.id, ja.id) > ($11, $12::uuid, $13::uuid)
					ORDER BY j.created_at ASC, j.id ASC, ja.id ASC
					LIMIT $14
				`, scopeExecID, claim.TargetID, claim.DatabaseID, claim.MigrationID, claim.Revision, string(claim.Direction), claim.SnapshotHash, claim.LeaseToken, claim.JobID, claim.AttemptNo, lastAttCreatedAt, lastAttJobID, lastAttemptID, recoveryPageLimit)
			}
			if qErr != nil {
				return fmt.Errorf("query originating running attempts page: %w", qErr)
			}
			defer rows.Close()

			for rows.Next() {
				var r origRunning
				var createdAt time.Time
				if err := rows.Scan(
					&r.jobID, &r.executionID, &r.attemptNo, &r.jobStatus, &r.execStatus,
					&r.attemptID, &r.attemptNoAtt, &r.attemptStatus, &r.leaseExpiresAt, &r.isExpired,
					&createdAt,
				); err != nil {
					return fmt.Errorf("scan originating running attempt: %w", err)
				}
				lastAttCreatedAt = createdAt
				lastAttJobID = r.jobID
				lastAttemptID = r.attemptID
				page = append(page, r)
			}
			return rows.Err()
		}()
		if err != nil {
			return err
		}
		if len(page) == 0 {
			break
		}

		if deps.beforeRecoveryClosurePage != nil {
			if err := deps.beforeRecoveryClosurePage(ctx, "attempts", attPageIdx, len(page)); err != nil {
				return err
			}
		}
		attPageIdx++

		for _, r := range page {
			if err := ctx.Err(); err != nil {
				return err
			}
			if (r.jobStatus == "running" || r.attemptStatus == "running") && !r.isExpired {
				return executioncontract.ContractError{
					Code: "TARGET_BLOCKED",
					Message: fmt.Sprintf("originating job %s lease has not expired (expires at %v)",
						r.jobID, r.leaseExpiresAt),
				}
			}

			var committedCount int
			err := ptx.QueryRowContext(ctx, `
				SELECT count(*)
				FROM app.job_batch_commit_proofs
				WHERE attempt_id = $1::uuid
			`, r.attemptID).Scan(&committedCount)
			if err != nil {
				return fmt.Errorf("count committed proofs for originating attempt %s: %w", r.attemptID, err)
			}
			if pendingFinal != nil && pendingFinal.step.bsaAttemptID == r.attemptID {
				committedCount++
			}

			var targetStatus string
			var errCode, errMsg *string
			codeVal := "LEASE_EXPIRED"
			msgVal := "lease expired and target transaction aborted"

			if overallSucceeded && finalizerAttemptID != "" && r.attemptID == finalizerAttemptID {
				targetStatus = "succeeded"
				errCode = nil
				errMsg = nil
			} else if committedCount >= totalBatches && totalBatches > 0 {
				targetStatus = "succeeded"
				errCode = nil
				errMsg = nil
			} else if committedCount > 0 {
				targetStatus = "partial"
				errCode = &codeVal
				msgVal = "lease expired after partial batch execution"
				errMsg = &msgVal
			} else {
				targetStatus = "failed"
				errCode = &codeVal
				errMsg = &msgVal
			}

			if r.attemptStatus == "running" || r.attemptStatus == "unknown" {
				res, err := ptx.ExecContext(ctx, `
					UPDATE app.job_attempts
					SET status = $1,
					    error_code = $2,
					    error_message = $3,
					    error_retryable = false,
					    finished_at = clock_timestamp()
					WHERE id = $4::uuid AND job_id = $5::uuid AND status IN ('running', 'unknown')
				`, targetStatus, errCode, errMsg, r.attemptID, r.jobID)
				if err := requireOne(res, err, "close originating attempt"); err != nil {
					return err
				}
			}

			if claim.LeaseToken == "" || r.jobID != claim.JobID {
				if r.jobStatus == "running" || r.jobStatus == "unknown" {
					var jobCommittedCount int
					err := ptx.QueryRowContext(ctx, `
						SELECT count(DISTINCT batch_index)
						FROM app.job_batch_commit_proofs
						WHERE job_id = $1::uuid
					`, r.jobID).Scan(&jobCommittedCount)
					if err != nil {
						return fmt.Errorf("count committed proofs for originating job %s: %w", r.jobID, err)
					}
					if pendingFinal != nil && pendingFinal.step.bsaJobID == r.jobID {
						jobCommittedCount++
					}

					var pendingBsaIDVal *string
					if pendingFinal != nil {
						pendingBsaIDVal = &pendingFinal.step.bsaID
					}
					var hasUnresolvedBlock bool
					err = ptx.QueryRowContext(ctx, `
						SELECT EXISTS (
							SELECT 1 FROM app.job_attempts ja
							WHERE ja.job_id = $1::uuid
							  AND (ja.status IN ('running', 'unknown')
							       OR (ja.sql_entered AND NOT EXISTS (
							           SELECT 1 FROM app.job_statement_evidence se WHERE se.attempt_id = ja.id
							       ))
							       OR (ja.sql_entered AND NOT EXISTS (
							           SELECT 1 FROM app.job_batch_step_authorizations bsa WHERE bsa.attempt_id = ja.id
							       )))
						) OR EXISTS (
							SELECT 1 FROM app.job_attempts ja
							JOIN app.job_statement_evidence se ON se.attempt_id = ja.id
							WHERE ja.job_id = $1::uuid
							  AND (
							      se.batch_index IS NULL
							      OR se.outcome = 'unknown'
							      OR NOT (
							          (se.outcome = 'failed' AND EXISTS (
							              SELECT 1 FROM app.job_batch_step_authorizations bsa
							              WHERE bsa.attempt_id = se.attempt_id
							                AND bsa.job_id = ja.job_id
							                AND bsa.ordinal = se.ordinal
							                AND bsa.batch_index = se.batch_index
							                AND bsa.physical_target_id = $2::uuid
							                AND bsa.direction = $3
							                AND bsa.scope_execution_id = $4::uuid
							          ))
							          OR
							          (se.outcome = 'succeeded' AND EXISTS (
							              SELECT 1 FROM app.job_batch_step_authorizations bsa
							              JOIN app.job_batch_commit_proofs p ON p.step_auth_id = bsa.id
							                AND p.job_id = bsa.job_id
							                AND p.attempt_id = bsa.attempt_id
							                AND p.scope_execution_id = bsa.scope_execution_id
							                AND p.physical_target_id = bsa.physical_target_id
							                AND p.direction = bsa.direction
							                AND p.batch_index = bsa.batch_index
							                AND p.target_xid = bsa.target_xid
							              WHERE bsa.attempt_id = se.attempt_id
							                AND bsa.job_id = ja.job_id
							                AND bsa.ordinal = se.ordinal
							                AND bsa.batch_index = se.batch_index
							                AND bsa.physical_target_id = $2::uuid
							                AND bsa.direction = $3
							                AND bsa.scope_execution_id = $4::uuid
							          ))
							          OR
							          (se.outcome = 'succeeded' AND $5::uuid IS NOT NULL AND EXISTS (
							              SELECT 1 FROM app.job_batch_step_authorizations bsa
							              WHERE bsa.id = $5::uuid
							                AND bsa.attempt_id = se.attempt_id
							                AND bsa.job_id = ja.job_id
							                AND bsa.ordinal = se.ordinal
							                AND bsa.batch_index = se.batch_index
							                AND bsa.physical_target_id = $2::uuid
							                AND bsa.direction = $3
							                AND bsa.scope_execution_id = $4::uuid
							          ))
							      )
							  )
						) OR EXISTS (
							SELECT 1 FROM app.job_batch_step_authorizations bsa
							WHERE bsa.job_id = $1::uuid
							  AND NOT (
							      ($5::uuid IS NOT NULL AND bsa.id = $5::uuid)
							      OR EXISTS (
							          SELECT 1 FROM app.job_batch_commit_proofs p
							          WHERE p.step_auth_id = bsa.id
							            AND p.job_id = bsa.job_id
							            AND p.attempt_id = bsa.attempt_id
							            AND p.scope_execution_id = bsa.scope_execution_id
							            AND p.physical_target_id = bsa.physical_target_id
							            AND p.direction = bsa.direction
							            AND p.batch_index = bsa.batch_index
							            AND p.target_xid = bsa.target_xid
							      )
							      OR EXISTS (
							          SELECT 1 FROM app.job_statement_evidence se
							          WHERE se.attempt_id = bsa.attempt_id
							            AND se.ordinal = bsa.ordinal
							            AND se.batch_index = bsa.batch_index
							            AND se.outcome = 'failed'
							      )
							  )
						)
					`, r.jobID, claim.TargetID, string(claim.Direction), scopeExecID, pendingBsaIDVal).Scan(&hasUnresolvedBlock)
					if err != nil {
						return fmt.Errorf("check unresolved block for originating job %s: %w", r.jobID, err)
					}

					var jobStatus string
					var jobBlocks bool
					var jobCode, jobMsg *string
					if hasUnresolvedBlock {
						jobBlocks = true
						if jobCommittedCount > 0 {
							jobStatus = "partial"
							jobCode = &codeVal
							jobMsg = &msgVal
						} else {
							jobStatus = "failed"
							jobCode = &codeVal
							jobMsg = &msgVal
						}
					} else if overallSucceeded && finalizerJobID != "" && r.jobID == finalizerJobID {
						jobStatus = "succeeded"
						jobBlocks = false
						jobCode = nil
						jobMsg = nil
					} else if jobCommittedCount >= totalBatches && totalBatches > 0 {
						jobStatus = "succeeded"
						jobBlocks = false
					} else if jobCommittedCount > 0 {
						jobStatus = "partial"
						jobBlocks = !overallSucceeded
						jobCode = &codeVal
						jobMsg = &msgVal
					} else {
						jobStatus = "failed"
						jobBlocks = false
						jobCode = &codeVal
						jobMsg = &msgVal
					}

					res, err := ptx.ExecContext(ctx, `
						UPDATE app.jobs
						SET status = $1,
						    execution_status = CASE WHEN ($7::boolean AND NOT $8::boolean) OR (execution_status = 'succeeded' AND NOT $8::boolean) THEN 'succeeded' ELSE $1 END,
						    blocks_database = CASE WHEN ($7::boolean AND NOT $8::boolean) OR (execution_status = 'succeeded' AND NOT $8::boolean) THEN false ELSE $2 END,
						    error_code = $3,
						    error_message = $4,
						    error_retryable = false,
						    finished_at = COALESCE(finished_at, clock_timestamp()),
						    updated_at = clock_timestamp()
						WHERE id = $5::uuid AND status IN ('running', 'unknown')
						  AND (lease_expires_at IS NULL OR lease_expires_at <= clock_timestamp() OR status = 'unknown') AND attempt_no = $6
					`, jobStatus, jobBlocks, jobCode, jobMsg, r.jobID, r.attemptNo, overallSucceeded, hasUnresolvedBlock)
					if err := requireOne(res, err, "close originating job"); err != nil {
						return err
					}
				}
			}
		}

		if len(page) < recoveryPageLimit {
			break
		}
	}

	return nil
}

type batchRecoveryScanResult struct {
	nextBatchIndex     int
	totalCommittedRows int64
	lastSeenItemID     *int64
	pendingFinal       *pendingFinalStep
	finalizerJobID     string
	finalizerAttemptID string
}

func (s *Service) scanAndReconcileBatchCheckpoints(
	ctx context.Context,
	claim *Claim,
	session *migration.TargetSession,
	scopeExecID string,
	totalBatches int,
	batchSize int,
	batchScope *BatchScope,
	approvedSQLText string,
	approvedBatchConfig executioncontract.BatchConfig,
	deps ExecutionDeps,
) (*batchRecoveryScanResult, *ExecutionOutcome, error) {
	var totalCommittedRows int64
	var nextBatchIndex int
	var lastSeenItemID *int64
	var pendingFinal *pendingFinalStep
	var finalizerJobID string
	var finalizerAttemptID string
	for {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}

		var page []chkInfo
		err := func() error {
			rows, qErr := session.Preparer().QueryContext(ctx, `
				SELECT batch_index, rows_affected, target_xid::text, recorded_at
				FROM wbd_auth.batch_checkpoints
				WHERE execution_id = $1::uuid AND direction = $2 AND batch_index >= $3
				ORDER BY batch_index ASC
				LIMIT $4
			`, scopeExecID, string(claim.Direction), nextBatchIndex, chkPageLimit)
			if qErr != nil {
				return fmt.Errorf("read target batch checkpoints page: %w", qErr)
			}
			defer rows.Close()
			for rows.Next() {
				var c chkInfo
				if err := rows.Scan(&c.index, &c.rows, &c.xid, &c.time); err != nil {
					return fmt.Errorf("scan target batch checkpoint: %w", err)
				}
				page = append(page, c)
			}
			return rows.Err()
		}()
		if err != nil {
			return nil, nil, err
		}
		if len(page) == 0 {
			break
		}

		for _, c := range page {
			if err := ctx.Err(); err != nil {
				return nil, nil, err
			}
			if deps.beforeRecoveryScanStep != nil {
				if err := deps.beforeRecoveryScanStep(ctx, c.index); err != nil {
					return nil, nil, err
				}
			}
			if err := ctx.Err(); err != nil {
				return nil, nil, err
			}
			if c.index < 0 || c.index >= totalBatches {
				outc, outcErr := s.failedBlockedOutcome(ctx, claim, "TARGET_BLOCKED",
					fmt.Errorf("checkpoint index %d out of bounds [0,%d)", c.index, totalBatches))
				return nil, outc, outcErr
			}
			if c.index != nextBatchIndex {
				outc, outcErr := s.failedBlockedOutcome(ctx, claim, "TARGET_BLOCKED",
					fmt.Errorf("corrupted checkpoints: expected index %d, got %d", nextBatchIndex, c.index))
				return nil, outc, outcErr
			}

			var xactStatus string
			err := session.Preparer().QueryRowContext(ctx, `SELECT pg_catalog.pg_xact_status($1::xid8)`, c.xid).Scan(&xactStatus)
			if err != nil || xactStatus != "committed" {
				outc, outcErr := s.unknownOutcome(ctx, claim, fmt.Errorf("checkpoint batch %d target xid %s status is %s: %w", c.index, c.xid, xactStatus, err))
				return nil, outc, outcErr
			}

			offset := int64(c.index) * int64(batchSize)
			expectedLimit := int64(batchSize)
			if rem := batchScope.TotalCount - offset; rem < expectedLimit {
				expectedLimit = rem
			}
			if expectedLimit <= 0 || c.rows <= 0 || c.rows != expectedLimit {
				outc, outcErr := s.failedBlockedOutcome(ctx, claim, "TARGET_BLOCKED",
					fmt.Errorf("checkpoint batch %d invalid row count or chunk size: rows=%d, expected=%d", c.index, c.rows, expectedLimit))
				return nil, outc, outcErr
			}

			chunkIDs, err := fetchScopeChunk(ctx, session, scopeExecID, lastSeenItemID, expectedLimit)
			if err != nil {
				return nil, nil, fmt.Errorf("read chunk items for checkpoint batch %d: %w", c.index, err)
			}
			if int64(len(chunkIDs)) != expectedLimit || len(chunkIDs) == 0 {
				outc, outcErr := s.failedBlockedOutcome(ctx, claim, "TARGET_BLOCKED",
					fmt.Errorf("checkpoint %d unexpected empty or mismatched chunk: expected %d, got %d", c.index, expectedLimit, len(chunkIDs)))
				return nil, outc, outcErr
			}
			lastSeenItemID = &chunkIDs[len(chunkIDs)-1]

			committedAt, err := queryBatchCommitTimestamp(ctx, session, scopeExecID, string(claim.Direction), c.index, c.xid)
			if err != nil {
				outc, outcErr := s.failedBlockedOutcome(ctx, claim, "TARGET_BLOCKED", fmt.Errorf("checkpoint %d commit timestamp: %w", c.index, err))
				return nil, outc, outcErr
			}
			rows, err := s.db.QueryContext(ctx, `
				SELECT bsa.id::text, bsa.job_id::text, bsa.attempt_id::text, bsa.actor_id::text,
				       bsa.job_execution_id::text, bsa.scope_execution_id::text, bsa.physical_target_id::text,
				       bsa.direction, bsa.batch_index, bsa.ordinal, bsa.chunk_ids::text, bsa.plan::text,
				       bsa.sql_text, bsa.sql_sha256, bsa.target_xid, bsa.target_version,
				       j.id::text, j.execution_id::text, j.proposal_id::text, j.migration_id::text,
				       j.revision, j.actor_id::text, j.action, j.direction, j.mode,
				       j.database_id::text, j.target_id::text, j.source_execution_id::text,
				       j.snapshot_hash, j.snapshot::text,
				       ja.id::text, ja.job_id::text, ja.attempt_no, ja.status, ja.sql_entered
				FROM app.job_batch_step_authorizations bsa
				JOIN app.jobs j ON j.id = bsa.job_id
				JOIN app.job_attempts ja ON ja.id = bsa.attempt_id AND ja.job_id = j.id
				WHERE bsa.scope_execution_id = $1::uuid
				  AND bsa.physical_target_id = $2::uuid
				  AND bsa.direction = $3
				  AND bsa.batch_index = $4
				  AND bsa.target_xid = $5
			`, scopeExecID, claim.TargetID, string(claim.Direction), c.index, c.xid)
			if err != nil {
				outc, outcErr := s.unknownOutcome(ctx, claim, fmt.Errorf("query authorized step: %w", err))
				return nil, outc, outcErr
			}

			var matchingSteps []origAuthRow
			for rows.Next() {
				var r origAuthRow
				if err := rows.Scan(
					&r.bsaID, &r.bsaJobID, &r.bsaAttemptID, &r.bsaActorID,
					&r.bsaJobExecID, &r.bsaScopeExecID, &r.bsaTargetID,
					&r.bsaDirection, &r.bsaBatchIdx, &r.bsaOrdinal, &r.bsaChunkIDsJSON, &r.bsaPlanJSON,
					&r.bsaSQLText, &r.bsaSQLSha256, &r.bsaTargetXID, &r.bsaTargetVersion,
					&r.origJobID, &r.origExecID, &r.origProposalID, &r.origMigrationID,
					&r.origRevision, &r.origActorID, &r.origAction, &r.origDirection, &r.origMode,
					&r.origDatabaseID, &r.origTargetID, &r.origSourceExecID,
					&r.origSnapHash, &r.origSnapJSON,
					&r.origAttID, &r.origAttJobID, &r.origAttNo, &r.origAttStatus, &r.origAttSQLEntered,
				); err != nil {
					rows.Close()
					outc, outcErr := s.unknownOutcome(ctx, claim, fmt.Errorf("scan authorized step: %w", err))
					return nil, outc, outcErr
				}
				matchingSteps = append(matchingSteps, r)
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				outc, outcErr := s.unknownOutcome(ctx, claim, fmt.Errorf("iterate authorized steps: %w", err))
				return nil, outc, outcErr
			}

			if len(matchingSteps) != 1 {
				outc, outcErr := s.failedBlockedOutcome(ctx, claim, "TARGET_BLOCKED",
					fmt.Errorf("checkpoint batch %d target xid %s matched %d authorized steps (expected 1)", c.index, c.xid, len(matchingSteps)))
				return nil, outc, outcErr
			}
			step := matchingSteps[0]

			if step.bsaJobID != step.origJobID || step.bsaAttemptID != step.origAttID ||
				step.bsaActorID != step.origActorID || step.bsaJobExecID != step.origExecID ||
				step.bsaTargetID != step.origTargetID || step.origTargetID != claim.TargetID ||
				step.bsaDirection != step.origDirection || step.origDirection != string(claim.Direction) ||
				step.origMode != "batch" || !step.origAttSQLEntered ||
				step.origDatabaseID != claim.DatabaseID {
				outc, outcErr := s.failedBlockedOutcome(ctx, claim, "TARGET_BLOCKED", errors.New("originating authorization and job/attempt identity mismatch"))
				return nil, outc, outcErr
			}

			var stepChunkIDs []int64
			if err := json.Unmarshal([]byte(step.bsaChunkIDsJSON), &stepChunkIDs); err != nil {
				outc, outcErr := s.failedBlockedOutcome(ctx, claim, "TARGET_BLOCKED", fmt.Errorf("decode step chunk IDs: %w", err))
				return nil, outc, outcErr
			}
			if len(stepChunkIDs) != len(chunkIDs) {
				outc, outcErr := s.failedBlockedOutcome(ctx, claim, "TARGET_BLOCKED",
					fmt.Errorf("step chunk count mismatch: expected %d, got %d", len(chunkIDs), len(stepChunkIDs)))
				return nil, outc, outcErr
			}
			for i := range chunkIDs {
				if stepChunkIDs[i] != chunkIDs[i] {
					outc, outcErr := s.failedBlockedOutcome(ctx, claim, "TARGET_BLOCKED",
						fmt.Errorf("step chunk ID mismatch at index %d: expected %d, got %d", i, chunkIDs[i], stepChunkIDs[i]))
					return nil, outc, outcErr
				}
			}

			computedHash := sha256.Sum256([]byte(step.bsaSQLText))
			if len(step.bsaSQLSha256) != 32 || computedHash != [32]byte(step.bsaSQLSha256) {
				outc, outcErr := s.failedBlockedOutcome(ctx, claim, "TARGET_BLOCKED", errors.New("authorized step SQL sha256 mismatch"))
				return nil, outc, outcErr
			}
			if err := validateAgreedBatchSQL(claim.Direction, step.bsaSQLText); err != nil {
				outc, outcErr := s.failedBlockedOutcome(ctx, claim, "TARGET_BLOCKED", fmt.Errorf("authorized step SQL invalid: %w", err))
				return nil, outc, outcErr
			}
			if step.bsaSQLText != approvedSQLText {
				outc, outcErr := s.failedBlockedOutcome(ctx, claim, "TARGET_BLOCKED", errors.New("authorized step SQL does not match approved SQL"))
				return nil, outc, outcErr
			}

			var stepPlan executioncontract.ExecutionPlan
			if err := json.Unmarshal([]byte(step.bsaPlanJSON), &stepPlan); err != nil {
				outc, outcErr := s.failedBlockedOutcome(ctx, claim, "TARGET_BLOCKED", fmt.Errorf("decode step plan: %w", err))
				return nil, outc, outcErr
			}
			if err := validateAgreedBatchConfig(stepPlan.Snapshot.Batch); err != nil {
				outc, outcErr := s.failedBlockedOutcome(ctx, claim, "TARGET_BLOCKED", fmt.Errorf("authorized step batch config invalid: %w", err))
				return nil, outc, outcErr
			}
			if !reflect.DeepEqual(*stepPlan.Snapshot.Batch, approvedBatchConfig) {
				outc, outcErr := s.failedBlockedOutcome(ctx, claim, "TARGET_BLOCKED", errors.New("authorized step batch config does not match approved plan"))
				return nil, outc, outcErr
			}

			var origSnap executioncontract.Draft
			if err := json.Unmarshal([]byte(step.origSnapJSON), &origSnap); err != nil {
				outc, outcErr := s.failedBlockedOutcome(ctx, claim, "TARGET_BLOCKED", fmt.Errorf("decode original job snapshot: %w", err))
				return nil, outc, outcErr
			}
			if !reflect.DeepEqual(stepPlan.Snapshot, origSnap) {
				outc, outcErr := s.failedBlockedOutcome(ctx, claim, "TARGET_BLOCKED", errors.New("authorized step plan snapshot does not match original job snapshot"))
				return nil, outc, outcErr
			}

			if stepPlan.Snapshot.MigrationID != step.origMigrationID ||
				stepPlan.Snapshot.DatabaseID != step.origDatabaseID ||
				stepPlan.Snapshot.Revision != step.origRevision ||
				string(stepPlan.Snapshot.Mode) != step.origMode ||
				stepPlan.Request.JobID != step.origJobID ||
				stepPlan.Request.ExecutionID != step.origExecID ||
				stepPlan.Request.ActorID != step.origActorID ||
				stepPlan.Request.ProposalID != step.origProposalID ||
				stepPlan.Request.MigrationID != step.origMigrationID ||
				stepPlan.Request.Revision != step.origRevision ||
				string(stepPlan.Request.Action) != step.origAction ||
				string(stepPlan.Request.Direction) != step.origDirection ||
				stepPlan.Request.DatabaseID != step.origDatabaseID ||
				stepPlan.Request.TargetID != step.origTargetID ||
				stepPlan.Request.SnapshotHash != step.origSnapHash ||
				stepPlan.Version != strconv.FormatInt(step.bsaTargetVersion, 10) {
				outc, outcErr := s.failedBlockedOutcome(ctx, claim, "TARGET_BLOCKED", errors.New("authorized step plan does not agree with originating job"))
				return nil, outc, outcErr
			}

			if claim.Action == executioncontract.Resume {
				if claim.SourceExecutionID == nil || *claim.SourceExecutionID != scopeExecID ||
					claim.DatabaseID != step.origDatabaseID || claim.TargetID != step.origTargetID ||
					claim.MigrationID != step.origMigrationID || claim.Revision != step.origRevision ||
					claim.SnapshotHash != step.origSnapHash {
					outc, outcErr := s.failedBlockedOutcome(ctx, claim, "TARGET_BLOCKED", errors.New("current resume claim scope mismatch with recovered checkpoint"))
					return nil, outc, outcErr
				}
				lineageOK := (step.origExecID == scopeExecID && step.origAction != string(executioncontract.Resume)) ||
					(step.origAction == string(executioncontract.Resume) && step.origSourceExecID.Valid && step.origSourceExecID.String == scopeExecID)
				if !lineageOK {
					outc, outcErr := s.failedBlockedOutcome(ctx, claim, "TARGET_BLOCKED", errors.New("checkpoint step does not belong to authorized source lineage"))
					return nil, outc, outcErr
				}
			} else {
				if claim.ExecutionID != scopeExecID || claim.JobID != step.origJobID {
					outc, outcErr := s.failedBlockedOutcome(ctx, claim, "TARGET_BLOCKED", errors.New("current claim execution mismatch with recovered checkpoint"))
					return nil, outc, outcErr
				}
			}

			type proofRow struct {
				id               string
				stepAuthID       string
				jobID            string
				attemptID        string
				scopeExecutionID string
				physicalTargetID string
				direction        string
				batchIndex       int
				targetXID        string
				rowCount         int64
				committedAt      time.Time
			}
			var existingProof proofRow
			proofErr := s.db.QueryRowContext(ctx, `
				SELECT id::text, step_auth_id::text, job_id::text, attempt_id::text,
				       scope_execution_id::text, physical_target_id::text, direction,
				       batch_index, target_xid, row_count, committed_at
				FROM app.job_batch_commit_proofs
				WHERE scope_execution_id = $1::uuid AND physical_target_id = $2::uuid AND direction = $3 AND batch_index = $4
			`, scopeExecID, claim.TargetID, string(claim.Direction), c.index).Scan(
				&existingProof.id, &existingProof.stepAuthID, &existingProof.jobID, &existingProof.attemptID,
				&existingProof.scopeExecutionID, &existingProof.physicalTargetID, &existingProof.direction,
				&existingProof.batchIndex, &existingProof.targetXID, &existingProof.rowCount, &existingProof.committedAt,
			)
			if proofErr != nil && !errors.Is(proofErr, sql.ErrNoRows) {
				outc, outcErr := s.unknownOutcome(ctx, claim, fmt.Errorf("check commit proof for batch %d: %w", c.index, proofErr))
				return nil, outc, outcErr
			}

			type evInfo struct {
				ordinal   int
				batchIdx  sql.NullInt64
				outcome   string
				rowCount  sql.NullInt64
				sqlText   string
				paramsRaw sql.NullString
			}
			evRowsQuery, qErr := s.db.QueryContext(ctx, `
				SELECT ordinal, batch_index, outcome, row_count, sql_text, safe_parameters::text
				FROM app.job_statement_evidence
				WHERE attempt_id = $1::uuid AND (batch_index = $2 OR ordinal = $3)
				LIMIT 2
			`, step.bsaAttemptID, c.index, step.bsaOrdinal)
			if qErr != nil {
				outc, outcErr := s.unknownOutcome(ctx, claim, fmt.Errorf("query statement evidence for batch %d: %w", c.index, qErr))
				return nil, outc, outcErr
			}
			var evList []evInfo
			for evRowsQuery.Next() {
				var info evInfo
				if err := evRowsQuery.Scan(&info.ordinal, &info.batchIdx, &info.outcome, &info.rowCount, &info.sqlText, &info.paramsRaw); err != nil {
					evRowsQuery.Close()
					outc, outcErr := s.unknownOutcome(ctx, claim, fmt.Errorf("scan statement evidence for batch %d: %w", c.index, err))
					return nil, outc, outcErr
				}
				evList = append(evList, info)
			}
			evRowsQuery.Close()
			if err := evRowsQuery.Err(); err != nil {
				outc, outcErr := s.unknownOutcome(ctx, claim, fmt.Errorf("iterate statement evidence for batch %d: %w", c.index, err))
				return nil, outc, outcErr
			}

			if len(evList) != 1 {
				outc, outcErr := s.failedBlockedOutcome(ctx, claim, "TARGET_BLOCKED",
					fmt.Errorf("originating statement evidence for batch %d expected exactly 1 row, got %d", c.index, len(evList)))
				return nil, outc, outcErr
			}
			ev := evList[0]
			if !ev.batchIdx.Valid || int(ev.batchIdx.Int64) != c.index || ev.ordinal != step.bsaOrdinal {
				outc, outcErr := s.failedBlockedOutcome(ctx, claim, "TARGET_BLOCKED",
					fmt.Errorf("originating statement evidence key mismatch: batch_index=%v ordinal=%d, expected (%d, %d)",
						ev.batchIdx, ev.ordinal, c.index, step.bsaOrdinal))
				return nil, outc, outcErr
			}

			if ev.sqlText == "" || ev.sqlText != approvedSQLText || ev.sqlText != step.bsaSQLText {
				outc, outcErr := s.failedBlockedOutcome(ctx, claim, "TARGET_BLOCKED",
					errors.New("originating statement evidence sql mismatch or empty"))
				return nil, outc, outcErr
			}

			if !ev.paramsRaw.Valid || ev.paramsRaw.String == "" || ev.paramsRaw.String == "{}" {
				outc, outcErr := s.failedBlockedOutcome(ctx, claim, "TARGET_BLOCKED",
					errors.New("originating statement evidence parameters missing or empty"))
				return nil, outc, outcErr
			}
			var pMap map[string]json.RawMessage
			if err := json.Unmarshal([]byte(ev.paramsRaw.String), &pMap); err != nil {
				outc, outcErr := s.failedBlockedOutcome(ctx, claim, "TARGET_BLOCKED",
					fmt.Errorf("originating statement evidence parameters not an object: %w", err))
				return nil, outc, outcErr
			}
			rawIDs, hasIDs := pMap["ids"]
			if !hasIDs || string(rawIDs) == "null" {
				outc, outcErr := s.failedBlockedOutcome(ctx, claim, "TARGET_BLOCKED",
					errors.New("originating statement evidence parameters missing ids"))
				return nil, outc, outcErr
			}
			var evIDs []int64
			if err := json.Unmarshal(rawIDs, &evIDs); err != nil || len(evIDs) == 0 {
				outc, outcErr := s.failedBlockedOutcome(ctx, claim, "TARGET_BLOCKED",
					errors.New("originating statement evidence ids invalid type or empty"))
				return nil, outc, outcErr
			}
			if len(evIDs) != len(chunkIDs) {
				outc, outcErr := s.failedBlockedOutcome(ctx, claim, "TARGET_BLOCKED",
					fmt.Errorf("originating statement evidence ids count mismatch: got %d, expected %d", len(evIDs), len(chunkIDs)))
				return nil, outc, outcErr
			}
			for i := range chunkIDs {
				if evIDs[i] != chunkIDs[i] {
					outc, outcErr := s.failedBlockedOutcome(ctx, claim, "TARGET_BLOCKED",
						fmt.Errorf("originating statement evidence id mismatch at index %d: got %d, expected %d", i, evIDs[i], chunkIDs[i]))
					return nil, outc, outcErr
				}
			}

			if proofErr == nil {
				if existingProof.stepAuthID != step.bsaID ||
					existingProof.jobID != step.bsaJobID ||
					existingProof.attemptID != step.bsaAttemptID ||
					existingProof.scopeExecutionID != scopeExecID ||
					existingProof.physicalTargetID != claim.TargetID ||
					existingProof.direction != string(claim.Direction) ||
					existingProof.batchIndex != c.index ||
					existingProof.targetXID != c.xid ||
					existingProof.rowCount != c.rows ||
					!existingProof.committedAt.Equal(committedAt) {
					outc, outcErr := s.failedBlockedOutcome(ctx, claim, "TARGET_BLOCKED",
						fmt.Errorf("checkpoint %d proof mismatch: target_xid=%s vs %s, rows=%d vs %d, committed_at=%v vs %v",
							c.index, existingProof.targetXID, c.xid, existingProof.rowCount, c.rows, existingProof.committedAt, committedAt))
					return nil, outc, outcErr
				}

				if ev.outcome != "succeeded" || !ev.rowCount.Valid || ev.rowCount.Int64 != c.rows {
					outc, outcErr := s.failedBlockedOutcome(ctx, claim, "TARGET_BLOCKED", errors.New("proof references inconsistent statement evidence outcome or row count"))
					return nil, outc, outcErr
				}
				if c.index == totalBatches-1 {
					finalizerJobID = step.bsaJobID
					finalizerAttemptID = step.bsaAttemptID
				}
				totalCommittedRows += c.rows
				nextBatchIndex++
				continue
			}

			if (ev.outcome != "succeeded" && ev.outcome != "running" && ev.outcome != "unknown") ||
				(ev.rowCount.Valid && ev.rowCount.Int64 != c.rows) {
				outc, outcErr := s.failedBlockedOutcome(ctx, claim, "TARGET_BLOCKED", errors.New("originating statement evidence inconsistent outcome or row count"))
				return nil, outc, outcErr
			}

			isFinalStep := (c.index == totalBatches-1)
			startIDStr := strconv.FormatInt(chunkIDs[0], 10)
			endIDStr := strconv.FormatInt(chunkIDs[len(chunkIDs)-1], 10)

			var origSourceExecID *string
			if step.origSourceExecID.Valid && step.origSourceExecID.String != "" {
				sVal := step.origSourceExecID.String
				origSourceExecID = &sVal
			}

			evidence := executioncontract.CommitEvidence{
				JobID:             step.origJobID,
				ExecutionID:       step.origExecID,
				ProposalID:        step.origProposalID,
				MigrationID:       step.origMigrationID,
				ActorID:           step.origActorID,
				DatabaseID:        step.origDatabaseID,
				TargetID:          step.origTargetID,
				Revision:          step.origRevision,
				Version:           strconv.FormatInt(step.bsaTargetVersion, 10),
				Action:            executioncontract.Action(step.origAction),
				Direction:         executioncontract.Direction(step.origDirection),
				SQLSnapshot:       step.bsaSQLText,
				SafeParameters:    map[string]any{"ids": chunkIDs},
				SnapshotHash:      step.origSnapHash,
				Receipt:           step.origExecID,
				CommittedAt:       committedAt,
				RowCount:          strconv.FormatInt(c.rows, 10),
				SourceExecutionID: origSourceExecID,
				Batch: &executioncontract.BatchCommit{
					JobID:          step.origJobID,
					ExecutedBy:     executioncontract.Actor{ID: step.origActorID},
					RangeStart:     &startIDStr,
					RangeEnd:       &endIDStr,
					SQLSnapshot:    step.bsaSQLText,
					SafeParameters: map[string]any{"ids": chunkIDs},
					BatchIndex:     strconv.Itoa(c.index),
					RowCount:       strconv.FormatInt(c.rows, 10),
					CommittedAt:    committedAt,
					Receipt:        step.origExecID,
				},
				FinalStep: isFinalStep,
			}

			totalRowsStr := strconv.FormatInt(batchScope.TotalCount, 10)
			stepProg := executioncontract.Progress{
				Direction:        claim.Direction,
				BatchesCommitted: strconv.Itoa(c.index + 1),
				RowsCommitted:    strconv.FormatInt(totalCommittedRows+c.rows, 10),
				BatchesReverted:  "0",
				RowsReverted:     "0",
				TotalRows:        &totalRowsStr,
			}
			stepProgJSON, _ := json.Marshal(stepProg)

			if isFinalStep {
				finalizerJobID = step.bsaJobID
				finalizerAttemptID = step.bsaAttemptID
				pendingFinal = &pendingFinalStep{
					step:        step,
					chk:         c,
					chunkIDs:    chunkIDs,
					committedAt: committedAt,
					evidence:    evidence,
					progJSON:    stepProgJSON,
				}
				totalCommittedRows += c.rows
				nextBatchIndex++
				continue
			}

			reconErr := func() error {
				ptx, err := s.db.BeginTx(ctx, nil)
				if err != nil {
					return fmt.Errorf("begin platform reconciliation tx: %w", err)
				}
				defer ptx.Rollback()

				res, err := ptx.ExecContext(ctx, `
					UPDATE app.job_statement_evidence
					SET outcome = 'succeeded',
					    row_count = $1,
					    executed_at = clock_timestamp()
					WHERE attempt_id = $2::uuid AND ordinal = $3 AND batch_index = $4
				`, c.rows, step.bsaAttemptID, step.bsaOrdinal, step.bsaBatchIdx)
				if err != nil {
					return fmt.Errorf("reconcile statement evidence: %w", err)
				}
				if err := requireOne(res, err, "reconcile statement evidence"); err != nil {
					return err
				}

				_, err = ptx.ExecContext(ctx, `
					INSERT INTO app.job_batch_commit_proofs
					    (step_auth_id, job_id, attempt_id, scope_execution_id, physical_target_id, direction, batch_index, target_xid, row_count, committed_at)
					VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
					ON CONFLICT (scope_execution_id, direction, batch_index) DO NOTHING
				`, step.bsaID, step.bsaJobID, step.bsaAttemptID, scopeExecID, claim.TargetID, string(claim.Direction), c.index, c.xid, c.rows, committedAt)
				if err != nil {
					return fmt.Errorf("reconcile batch commit proof: %w", err)
				}

				_, err = ptx.ExecContext(ctx, `
					INSERT INTO app.job_commit_proofs (job_id, attempt_id, target_xid, committed_at)
					VALUES ($1, $2, $3, $4)
					ON CONFLICT (job_id) DO NOTHING
				`, step.bsaJobID, step.bsaAttemptID, c.xid, committedAt)
				if err != nil {
					return fmt.Errorf("reconcile job commit proof: %w", err)
				}

				if err := deps.Gate.RecordCommitTx(ctx, ptx, evidence); err != nil {
					return fmt.Errorf("gate reconcile record commit for batch %d: %w", c.index, err)
				}

				if claim.LeaseToken != "" {
					res, err = ptx.ExecContext(ctx, `
						UPDATE app.jobs
						SET progress = $1::jsonb,
						    updated_at = clock_timestamp()
						WHERE id = $2 AND attempt_no = $3
						  AND status = 'running' AND execution_status = 'running'
						  AND lease_token = $4::uuid
						  AND lease_expires_at > clock_timestamp()
					`, string(stepProgJSON), claim.JobID, claim.AttemptNo, claim.LeaseToken)
					if err != nil || requireOne(res, err, "update progress for resume step") != nil {
						return fmt.Errorf("update progress for resume step: %w", err)
					}
				}

				return ptx.Commit()
			}()
			if reconErr != nil {
				outc, outcErr := s.unknownOutcome(ctx, claim, reconErr)
				return nil, outc, outcErr
			}

			totalCommittedRows += c.rows
			nextBatchIndex++
		}

		if len(page) < chkPageLimit {
			break
		}
	}

	var (
		chkCount int
		maxIdx   sql.NullInt64
	)
	err := session.Preparer().QueryRowContext(ctx, `
		SELECT count(*), max(batch_index)
		FROM wbd_auth.batch_checkpoints
		WHERE execution_id = $1::uuid AND direction = $2
	`, scopeExecID, string(claim.Direction)).Scan(&chkCount, &maxIdx)
	if err != nil {
		outc, outcErr := s.unknownOutcome(ctx, claim, fmt.Errorf("validate checkpoint tip: %w", err))
		return nil, outc, outcErr
	}
	if chkCount != nextBatchIndex || (maxIdx.Valid && int(maxIdx.Int64) != nextBatchIndex-1) {
		outc, outcErr := s.failedBlockedOutcome(ctx, claim, "TARGET_BLOCKED",
			fmt.Errorf("observed checkpoint tip mismatch: count=%d, max=%v, expected=%d", chkCount, maxIdx, nextBatchIndex))
		return nil, outc, outcErr
	}

	return &batchRecoveryScanResult{
		nextBatchIndex:     nextBatchIndex,
		totalCommittedRows: totalCommittedRows,
		lastSeenItemID:     lastSeenItemID,
		pendingFinal:       pendingFinal,
		finalizerJobID:     finalizerJobID,
		finalizerAttemptID: finalizerAttemptID,
	}, nil, nil
}
// ExecuteBatchJob executes an approved batch job over managed.batch_items/id in bounded chunks,
// verifying workflow authorization, target lock and preflight, recording durable checkpoints
// and statement evidence, and supporting crash recovery and safe resume without replay.
func (s *Service) ExecuteBatchJob(ctx context.Context, claim *Claim, deps ExecutionDeps) (*ExecutionOutcome, error) {
	if s.db == nil {
		return nil, errors.New("db is required")
	}
	if claim == nil {
		return nil, executioncontract.ContractError{Code: "INVALID_REQUEST", Message: "claim is required"}
	}
	if claim.Mode != executioncontract.Batch {
		return nil, executioncontract.ContractError{Code: "UNSUPPORTED_MODE", Message: "batch execution only"}
	}
	if claim.Action != executioncontract.Apply &&
		claim.Action != executioncontract.Forward &&
		claim.Action != executioncontract.Resume &&
		claim.Action != executioncontract.Compensate {
		return nil, executioncontract.ContractError{Code: "UNSUPPORTED_ACTION", Message: "unsupported action for batch"}
	}
	if claim.Direction != executioncontract.Up && claim.Direction != executioncontract.Down {
		return nil, executioncontract.ContractError{Code: "INVALID_DIRECTION", Message: "batch execution supports up or down direction"}
	}
	if claim.Action == executioncontract.Compensate && claim.Direction != executioncontract.Down {
		return nil, executioncontract.ContractError{Code: "INVALID_DIRECTION", Message: "compensate must have down direction"}
	}
	if (claim.Action == executioncontract.Resume || claim.Action == executioncontract.Compensate) && (claim.SourceExecutionID == nil || *claim.SourceExecutionID == "") {
		return nil, executioncontract.ContractError{Code: "INVALID_REQUEST", Message: fmt.Sprintf("%s requires source_execution_id", claim.Action)}
	}
	if deps.Gate == nil {
		return nil, executioncontract.ContractError{Code: "GATE_REQUIRED", Message: "workflow gate is required"}
	}
	if deps.Acquire == nil {
		return nil, executioncontract.ContractError{Code: "ACQUIRE_REQUIRED", Message: "target lease acquirer is required"}
	}
	if deps.Timeout <= 0 {
		deps.Timeout = 30 * time.Second
	}
	if claim.Direction == executioncontract.Down {
		return s.executeBatchJobDown(ctx, claim, deps)
	}

	lease, err := deps.Acquire(ctx, claim.DatabaseID)
	if err != nil {
		discardLease(lease)
		return nil, fmt.Errorf("acquire target lease: %w", err)
	}
	session, err := migration.OpenTargetSession(ctx, lease, claim.TargetID, deps.Timeout)
	if err != nil {
		return nil, fmt.Errorf("open target session: %w", err)
	}
	defer session.Close(deps.Timeout)
	if err := session.VerifyExecutorTarget(ctx, claim.TargetID); err != nil {
		return nil, fmt.Errorf("verify executor target: %w", err)
	}

	plan, err := deps.Gate.Revalidate(ctx, claim.RevalidationRequest())
	if err != nil {
		var cErr executioncontract.ContractError
		if errors.As(err, &cErr) && cErr.Code == "REVIEW_STALE" {
			return nil, fmt.Errorf("REVIEW_STALE requires WorkflowGate.ReturnToReviewTx and atomic job finalization: %w", err)
		}
		return nil, fmt.Errorf("workflow revalidation failed: %w", err)
	}
	if !reflect.DeepEqual(plan.Request, claim.RevalidationRequest()) {
		return nil, executioncontract.ContractError{Code: "PLAN_MISMATCH", Message: "workflow plan request mismatch"}
	}
	if err := validateAgreedBatchConfig(plan.Snapshot.Batch); err != nil {
		return s.failedOutcome(ctx, claim, "INVALID_BATCH_CONFIG", err)
	}
	sqlText, err := selectSQL(plan, claim.Action, claim.Direction)
	if err != nil || validateAgreedBatchSQL(claim.Direction, sqlText) != nil {
		return s.failedOutcome(ctx, claim, "SQL_UNSUPPORTED", errors.New("plan SQL does not match agreed batch pattern"))
	}
	approvedSnapBytes, err := json.Marshal(plan.Snapshot)
	if err != nil {
		return nil, fmt.Errorf("encode approved snapshot: %w", err)
	}
	approvedBatchConfig := *plan.Snapshot.Batch
	approvedPlanVersion := plan.Version
	approvedSQLText := sqlText

	var commitTimeAvailable bool
	err = session.Preparer().QueryRowContext(ctx, `SELECT current_setting('track_commit_timestamp') = 'on'
		AND pg_catalog.has_function_privilege(current_user, 'pg_catalog.pg_xact_commit_timestamp(xid)', 'EXECUTE')
		AND pg_catalog.has_function_privilege(current_user, 'pg_catalog.pg_xact_status(xid8)', 'EXECUTE')`).Scan(&commitTimeAvailable)
	if err != nil || !commitTimeAvailable {
		return s.failedOutcome(ctx, claim, "COMMIT_TIME_UNAVAILABLE",
			errors.Join(errors.New("exact target commit timestamp evidence is not supported"), err))
	}

	var scopeExecID string
	var batchScope *BatchScope
	batchSize := plan.Snapshot.Batch.BatchSize

	if claim.Action == executioncontract.Resume {
		scopeExecID = *claim.SourceExecutionID
		var (
			srcTargetID      string
			srcDatabaseID    string
			srcMode          string
			srcStatus        string
			srcSnapshotBytes []byte
		)
		err = s.db.QueryRowContext(ctx, `
			SELECT target_id::text, database_id::text, mode, status, snapshot
			FROM app.jobs
			WHERE execution_id = $1::uuid AND database_id = $2::uuid
			ORDER BY created_at DESC
			LIMIT 1
		`, scopeExecID, claim.DatabaseID).Scan(&srcTargetID, &srcDatabaseID, &srcMode, &srcStatus, &srcSnapshotBytes)
		if err != nil {
			return s.failedOutcome(ctx, claim, "SOURCE_JOB_NOT_FOUND", fmt.Errorf("source execution job: %w", err))
		}
		if srcTargetID != claim.TargetID {
			return s.failedOutcome(ctx, claim, "TARGET_MISMATCH", errors.New("resume target does not match source execution"))
		}
		if srcMode != "batch" {
			return s.failedOutcome(ctx, claim, "UNSUPPORTED_MODE", errors.New("source execution is not a batch job"))
		}
		if srcStatus == "succeeded" {
			return s.failedOutcome(ctx, claim, "REQUEST_CONFLICT", errors.New("source execution has already succeeded"))
		}
		var srcSnapshot executioncontract.Draft
		if err := json.Unmarshal(srcSnapshotBytes, &srcSnapshot); err != nil {
			return s.failedOutcome(ctx, claim, "INVALID_BATCH_CONFIG", fmt.Errorf("decode source snapshot: %w", err))
		}
		if srcSnapshot.MigrationID != claim.MigrationID {
			return s.failedOutcome(ctx, claim, "MIGRATION_MISMATCH", errors.New("resume migration does not match source execution"))
		}
		if srcSnapshot.Batch == nil || srcSnapshot.Batch.BatchSize != batchSize ||
			srcSnapshot.Batch.Schema != plan.Snapshot.Batch.Schema ||
			srcSnapshot.Batch.Table != plan.Snapshot.Batch.Table ||
			srcSnapshot.Batch.CursorColumn != plan.Snapshot.Batch.CursorColumn {
			return s.failedOutcome(ctx, claim, "BATCH_CONFIG_MISMATCH",
				errors.New("resume batch configuration does not match source execution"))
		}

		ident := BatchScopeIdentity{
			ExecutionID: scopeExecID,
			TargetID:    claim.TargetID,
			DatabaseID:  claim.DatabaseID,
			ActorID:     claim.ActorID,
		}
		batchScope, err = s.GetBatchScope(ctx, session.Preparer(), ident)
		if err != nil {
			return s.failedOutcome(ctx, claim, "SCOPE_UNAVAILABLE", fmt.Errorf("get source batch scope: %w", err))
		}
	} else {
		scopeExecID = claim.ExecutionID
		ident := BatchScopeIdentity{
			ExecutionID: scopeExecID,
			TargetID:    claim.TargetID,
			DatabaseID:  claim.DatabaseID,
			ActorID:     claim.ActorID,
		}
		batchScope, err = s.FreezeBatchScope(ctx, session.Preparer(), ident)
		if err != nil {
			return s.failedOutcome(ctx, claim, "TARGET_PREPARATION_FAILED", fmt.Errorf("freeze batch scope: %w", err))
		}
	}

	totalBatches := 0
	if batchScope.TotalCount > 0 {
		totalBatches = int((batchScope.TotalCount + int64(batchSize) - 1) / int64(batchSize))
	}

	var (
		totalCommittedRows int64
		nextBatchIndex     int
		lastSeenItemID     *int64
		pendingFinal       *pendingFinalStep
	)

	scanRes, outc, err := s.scanAndReconcileBatchCheckpoints(
		ctx, claim, session, scopeExecID, totalBatches, batchSize,
		batchScope, approvedSQLText, approvedBatchConfig, deps,
	)
	if err != nil || outc != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		if outc != nil {
			return outc, err
		}
		return s.unknownOutcome(ctx, claim, err)
	}

	nextBatchIndex = scanRes.nextBatchIndex
	totalCommittedRows = scanRes.totalCommittedRows
	lastSeenItemID = scanRes.lastSeenItemID
	pendingFinal = scanRes.pendingFinal
	if totalBatches == 0 {
		totalRowsStr := "0"
		emptyProg := executioncontract.Progress{
			Direction:        claim.Direction,
			BatchesCommitted: "0",
			RowsCommitted:    "0",
			BatchesReverted:  "0",
			RowsReverted:     "0",
			TotalRows:        &totalRowsStr,
		}
		emptyProgJSON, _ := json.Marshal(emptyProg)

		emptyErr := func() error {
			ptx, err := s.db.BeginTx(ctx, nil)
			if err != nil {
				return fmt.Errorf("begin empty scope finalization tx: %w", err)
			}
			defer ptx.Rollback()

			if err := s.applyOriginatingClosuresTx(ctx, ptx, claim, session, scopeExecID, totalBatches, nil, claim.JobID, claim.AttemptID, true, deps); err != nil {
				return err
			}
			if err := s.completeBatchSuccessTx(ctx, ptx, claim, scopeExecID, emptyProgJSON); err != nil {
				return err
			}
			return ptx.Commit()
		}()
		if emptyErr != nil {
			return s.handleClosureTxError(ctx, claim, emptyErr)
		}
		return &ExecutionOutcome{Status: executioncontract.Succeeded, Receipt: ""}, nil
	}

	if nextBatchIndex >= totalBatches {
		if pendingFinal != nil {
			platErr := func() error {
				ptx, err := s.db.BeginTx(ctx, nil)
				if err != nil {
					return fmt.Errorf("begin platform finalization tx: %w", err)
				}
				defer ptx.Rollback()
				finalJobID := scanRes.finalizerJobID
				finalAttemptID := scanRes.finalizerAttemptID
				if finalJobID == "" {
					finalJobID = claim.JobID
					finalAttemptID = claim.AttemptID
				}
				res, err := ptx.ExecContext(ctx, `
					UPDATE app.job_statement_evidence
					SET outcome = 'succeeded',
					    row_count = $1,
					    executed_at = clock_timestamp()
					WHERE attempt_id = $2::uuid AND ordinal = $3 AND batch_index = $4
				`, pendingFinal.chk.rows, pendingFinal.step.bsaAttemptID, pendingFinal.step.bsaOrdinal, pendingFinal.step.bsaBatchIdx)
				if err != nil {
					return fmt.Errorf("reconcile statement evidence: %w", err)
				}
				if err := requireOne(res, err, "reconcile statement evidence"); err != nil {
					return err
				}

				_, err = ptx.ExecContext(ctx, `
					INSERT INTO app.job_batch_commit_proofs
					    (step_auth_id, job_id, attempt_id, scope_execution_id, physical_target_id, direction, batch_index, target_xid, row_count, committed_at)
					VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
					ON CONFLICT (scope_execution_id, direction, batch_index) DO NOTHING
				`, pendingFinal.step.bsaID, pendingFinal.step.bsaJobID, pendingFinal.step.bsaAttemptID, scopeExecID, claim.TargetID, string(claim.Direction), pendingFinal.chk.index, pendingFinal.chk.xid, pendingFinal.chk.rows, pendingFinal.committedAt)
				if err != nil {
					return fmt.Errorf("reconcile batch commit proof: %w", err)
				}

				_, err = ptx.ExecContext(ctx, `
					INSERT INTO app.job_commit_proofs (job_id, attempt_id, target_xid, committed_at)
					VALUES ($1, $2, $3, $4)
					ON CONFLICT (job_id) DO NOTHING
				`, pendingFinal.step.bsaJobID, pendingFinal.step.bsaAttemptID, pendingFinal.chk.xid, pendingFinal.committedAt)
				if err != nil {
					return fmt.Errorf("reconcile job commit proof: %w", err)
				}

				if err := deps.Gate.RecordCommitTx(ctx, ptx, pendingFinal.evidence); err != nil {
					return fmt.Errorf("gate reconcile record commit for batch %d: %w", pendingFinal.chk.index, err)
				}

				if err := s.applyOriginatingClosuresTx(ctx, ptx, claim, session, scopeExecID, totalBatches, nil, finalJobID, finalAttemptID, true, deps); err != nil {
					return err
				}

				if err := s.completeBatchSuccessTx(ctx, ptx, claim, scopeExecID, pendingFinal.progJSON); err != nil {
					return err
				}
				return ptx.Commit()
			}()
			if platErr != nil {
				return s.handleClosureTxError(ctx, claim, platErr)
			}

			if deps.afterTargetCommit != nil {
				if err := deps.afterTargetCommit(ctx); err != nil {
					return s.unknownOutcome(ctx, claim, err)
				}
			}
			return &ExecutionOutcome{Status: executioncontract.Succeeded, Receipt: claim.ExecutionID, Reconciled: true}, nil
		}

		var isSucceeded bool
		_ = s.db.QueryRowContext(ctx, `SELECT status = 'succeeded' FROM app.jobs WHERE id = $1 AND attempt_no = $2`, claim.JobID, claim.AttemptNo).Scan(&isSucceeded)
		if !isSucceeded {
			totalRowsStr := strconv.FormatInt(batchScope.TotalCount, 10)
			prog := executioncontract.Progress{
				Direction:        claim.Direction,
				BatchesCommitted: strconv.Itoa(nextBatchIndex),
				RowsCommitted:    strconv.FormatInt(totalCommittedRows, 10),
				BatchesReverted:  "0",
				RowsReverted:     "0",
				TotalRows:        &totalRowsStr,
			}
			progJSON, _ := json.Marshal(prog)

			finalizeErr := func() error {
				ptx, err := s.db.BeginTx(ctx, nil)
				if err != nil {
					return fmt.Errorf("begin finalization tx: %w", err)
				}
				defer ptx.Rollback()
				finalJobID := scanRes.finalizerJobID
				finalAttemptID := scanRes.finalizerAttemptID
				if finalJobID == "" {
					finalJobID = claim.JobID
					finalAttemptID = claim.AttemptID
				}

				if err := s.applyOriginatingClosuresTx(ctx, ptx, claim, session, scopeExecID, totalBatches, nil, finalJobID, finalAttemptID, true, deps); err != nil {
					return err
				}
				if err := s.completeBatchSuccessTx(ctx, ptx, claim, scopeExecID, progJSON); err != nil {
					return err
				}
				return ptx.Commit()
			}()
			if finalizeErr != nil {
				return s.handleClosureTxError(ctx, claim, finalizeErr)
			}
		}
		return &ExecutionOutcome{Status: executioncontract.Succeeded, Receipt: claim.ExecutionID, Reconciled: true}, nil
	} else {
		recoverErr := func() error {
			ptx, err := s.db.BeginTx(ctx, nil)
			if err != nil {
				return fmt.Errorf("begin intermediate recovery tx: %w", err)
			}
			defer ptx.Rollback()

			if err := s.applyOriginatingClosuresTx(ctx, ptx, claim, session, scopeExecID, totalBatches, nil, "", "", false, deps); err != nil {
				return err
			}
			return ptx.Commit()
		}()
		if recoverErr != nil {
			return s.handleClosureTxError(ctx, claim, recoverErr)
		}

		if nextBatchIndex > 0 {
			totalRowsStr := strconv.FormatInt(batchScope.TotalCount, 10)
			prog := executioncontract.Progress{
				Direction:        claim.Direction,
				BatchesCommitted: strconv.Itoa(nextBatchIndex),
				RowsCommitted:    strconv.FormatInt(totalCommittedRows, 10),
				BatchesReverted:  "0",
				RowsReverted:     "0",
				TotalRows:        &totalRowsStr,
			}
			progJSON, _ := json.Marshal(prog)

			res, err := s.db.ExecContext(ctx, `
				UPDATE app.jobs
				SET progress = $1::jsonb,
				    updated_at = clock_timestamp()
				WHERE id = $2 AND attempt_no = $3
				  AND status = 'running' AND execution_status = 'running'
				  AND lease_token = $4::uuid
				  AND lease_expires_at > clock_timestamp()
			`, string(progJSON), claim.JobID, claim.AttemptNo, claim.LeaseToken)
			if err != nil || requireOne(res, err, "update progress for resume") != nil {
				return s.unknownOutcome(ctx, claim, fmt.Errorf("update progress for resume: %w", err))
			}
		}
	}

	version, err := strconv.ParseInt(plan.Version, 10, 64)
	if err != nil || version < 1 {
		return nil, executioncontract.ContractError{Code: "INVALID_VERSION", Message: "workflow plan has invalid target version"}
	}

	// Execute remaining chunks sequentially
	for batchIdx := nextBatchIndex; batchIdx < totalBatches; batchIdx++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		offset := int64(batchIdx) * int64(batchSize)
		limit := int64(batchSize)
		if rem := batchScope.TotalCount - offset; rem < limit {
			limit = rem
		}

		chunkIDs, err := fetchScopeChunk(ctx, session, scopeExecID, lastSeenItemID, limit)
		if err != nil {
			return nil, fmt.Errorf("read chunk items for batch %d: %w", batchIdx, err)
		}
		if int64(len(chunkIDs)) != limit {
			return s.unknownOutcome(ctx, claim, fmt.Errorf("chunk %d expected %d items, got %d", batchIdx, limit, len(chunkIDs)))
		}
		lastSeenItemID = &chunkIDs[len(chunkIDs)-1]

		earlierCommitted := (totalCommittedRows > 0 || batchIdx > 0 || nextBatchIndex > 0)

		// Optional test hooks
		if deps.beforeBatchDispatch != nil {
			if err := deps.beforeBatchDispatch(ctx, batchIdx); err != nil {
				if earlierCommitted {
					if failErr := s.failPartialJob(ctx, claim, "DISPATCH_INTERRUPTED", err.Error()); failErr != nil {
						return nil, errors.Join(err, failErr)
					}
					return &ExecutionOutcome{Status: executioncontract.Partial, Error: err}, nil
				}
				if failErr := s.failCleanJob(ctx, claim, "DISPATCH_INTERRUPTED", err.Error()); failErr != nil {
					return nil, errors.Join(err, failErr)
				}
				return &ExecutionOutcome{Status: executioncontract.Failed, Error: err}, nil
			}
		}
		if deps.beforeDispatch != nil && batchIdx == 0 {
			if err := deps.beforeDispatch(ctx); err != nil {
				if earlierCommitted {
					if failErr := s.failPartialJob(ctx, claim, "DISPATCH_INTERRUPTED", err.Error()); failErr != nil {
						return nil, errors.Join(err, failErr)
					}
					return &ExecutionOutcome{Status: executioncontract.Partial, Error: err}, nil
				}
				if failErr := s.failCleanJob(ctx, claim, "DISPATCH_INTERRUPTED", err.Error()); failErr != nil {
					return nil, errors.Join(err, failErr)
				}
				return &ExecutionOutcome{Status: executioncontract.Failed, Error: err}, nil
			}
		}

		if err := s.checkBatchLeaseAndTargetBlock(ctx, claim); err != nil {
			var cErr executioncontract.ContractError
			if errors.As(err, &cErr) && cErr.Code == "LEASE_MISMATCH" {
				return nil, err
			}
			if earlierCommitted {
				_ = s.failPartialJob(ctx, claim, "TARGET_BLOCKED", err.Error())
				return &ExecutionOutcome{Status: executioncontract.Partial, Error: err}, nil
			}
			return nil, err
		}

		// Gate Revalidate before EACH new chunk while holding target physical lock
		chunkPlan, err := deps.Gate.Revalidate(ctx, claim.RevalidationRequest())
		if err != nil {
			var cErr executioncontract.ContractError
			if errors.As(err, &cErr) && cErr.Code == "REVIEW_STALE" {
				if earlierCommitted {
					if failErr := s.failPartialJob(ctx, claim, "REVIEW_STALE", err.Error()); failErr != nil {
						return nil, errors.Join(err, failErr)
					}
					return &ExecutionOutcome{Status: executioncontract.Partial, Error: err}, nil
				}
				return nil, fmt.Errorf("REVIEW_STALE requires WorkflowGate.ReturnToReviewTx and atomic job finalization: %w", err)
			}
			if earlierCommitted {
				if failErr := s.failPartialJob(ctx, claim, "GATE_DENIED", err.Error()); failErr != nil {
					return nil, errors.Join(err, failErr)
				}
				return &ExecutionOutcome{Status: executioncontract.Partial, Error: err}, nil
			}
			return nil, fmt.Errorf("workflow revalidation failed for batch %d: %w", batchIdx, err)
		}

		// Reject changed plan/request/snapshot/version/SQL/batch config against exact approved execution
		if !reflect.DeepEqual(chunkPlan.Request, claim.RevalidationRequest()) {
			err := executioncontract.ContractError{Code: "PLAN_MISMATCH", Message: "workflow plan request mismatch"}
			if earlierCommitted {
				_ = s.failPartialJob(ctx, claim, "PLAN_MISMATCH", err.Message)
				return &ExecutionOutcome{Status: executioncontract.Partial, Error: err}, nil
			}
			return nil, err
		}
		if chunkPlan.Version != approvedPlanVersion {
			err := executioncontract.ContractError{Code: "PLAN_MISMATCH", Message: "workflow plan target version changed"}
			if earlierCommitted {
				_ = s.failPartialJob(ctx, claim, "PLAN_MISMATCH", err.Message)
				return &ExecutionOutcome{Status: executioncontract.Partial, Error: err}, nil
			}
			return nil, err
		}
		chunkSnapBytes, err := json.Marshal(chunkPlan.Snapshot)
		if err != nil || !bytes.Equal(chunkSnapBytes, approvedSnapBytes) {
			err := executioncontract.ContractError{Code: "PLAN_MISMATCH", Message: "workflow plan snapshot changed"}
			if earlierCommitted {
				_ = s.failPartialJob(ctx, claim, "PLAN_MISMATCH", err.Message)
				return &ExecutionOutcome{Status: executioncontract.Partial, Error: err}, nil
			}
			return nil, err
		}
		if err := validateAgreedBatchConfig(chunkPlan.Snapshot.Batch); err != nil ||
			chunkPlan.Snapshot.Batch.BatchSize != approvedBatchConfig.BatchSize ||
			chunkPlan.Snapshot.Batch.Schema != approvedBatchConfig.Schema ||
			chunkPlan.Snapshot.Batch.Table != approvedBatchConfig.Table ||
			chunkPlan.Snapshot.Batch.CursorColumn != approvedBatchConfig.CursorColumn {
			err := executioncontract.ContractError{Code: "BATCH_CONFIG_MISMATCH", Message: "workflow batch configuration changed"}
			if earlierCommitted {
				_ = s.failPartialJob(ctx, claim, "BATCH_CONFIG_MISMATCH", err.Message)
				return &ExecutionOutcome{Status: executioncontract.Partial, Error: err}, nil
			}
			return nil, err
		}
		chunkSQL, err := selectSQL(chunkPlan, claim.Action, claim.Direction)
		if err != nil || chunkSQL != approvedSQLText || validateAgreedBatchSQL(claim.Direction, chunkSQL) != nil {
			err := executioncontract.ContractError{Code: "SQL_MISMATCH", Message: "workflow plan SQL changed"}
			if earlierCommitted {
				_ = s.failPartialJob(ctx, claim, "SQL_MISMATCH", err.Message)
				return &ExecutionOutcome{Status: executioncontract.Partial, Error: err}, nil
			}
			return nil, err
		}

		// Begin target transaction
		if err := session.Begin(ctx, deps.Timeout); err != nil {
			return nil, fmt.Errorf("begin target tx for batch %d: %w", batchIdx, err)
		}

		var xid string
		if err := session.Tx().QueryRowContext(ctx, `SELECT pg_catalog.pg_current_xact_id()::text`).Scan(&xid); err != nil {
			return s.rollbackAndFailBatch(ctx, claim, session, "", batchIdx, 0, earlierCommitted, "TARGET_PREPARATION_FAILED", err, deps)
		}

		// Potentially blocking target safety preflight (relation lock)
		if err := migration.PreflightBatch(ctx, session.Tx(), "managed", "batch_items"); err != nil {
			code := "SQL_UNSUPPORTED"
			var apiErr *migration.APIError
			if errors.As(err, &apiErr) {
				code = apiErr.Code
			}
			return s.rollbackAndFailBatch(ctx, claim, session, xid, batchIdx, 0, earlierCommitted, code, err, deps)
		}

		// Potentially blocking row lock & check external writer modifications
		lockedRows, err := session.Tx().QueryContext(ctx, `
			SELECT id, processed
			FROM ONLY managed.batch_items
			WHERE id = ANY($1::bigint[])
			FOR UPDATE
		`, chunkIDs)
		if err != nil {
			return s.rollbackAndFailBatch(ctx, claim, session, xid, batchIdx, 0, earlierCommitted, "TARGET_PREPARATION_FAILED", err, deps)
		}

		seenIDs := make(map[int64]bool, len(chunkIDs))
		var unsafeRow bool
		for lockedRows.Next() {
			var idVal int64
			var procVal bool
			if err := lockedRows.Scan(&idVal, &procVal); err != nil {
				lockedRows.Close()
				return s.rollbackAndFailBatch(ctx, claim, session, xid, batchIdx, 0, earlierCommitted, "TARGET_PREPARATION_FAILED", err, deps)
			}
			seenIDs[idVal] = true
			if procVal != false {
				unsafeRow = true
			}
		}
		lockedRows.Close()

		if len(seenIDs) != len(chunkIDs) || unsafeRow {
			unsafeErr := errors.New("external row modification or deletion detected in batch items")
			return s.rollbackAndFailBatch(ctx, claim, session, xid, batchIdx, 0, earlierCommitted, "BATCH_UNSAFE", unsafeErr, deps)
		}

		// Ticket preparation and reservation on target via Preparer
		capability := make([]byte, 32)
		if _, err := rand.Read(capability); err != nil {
			return s.rollbackAndFailBatch(ctx, claim, session, xid, batchIdx, 0, earlierCommitted, "TARGET_PREPARATION_FAILED", err, deps)
		}

		if err := s.reserveBatchTicket(ctx, session, scopeExecID, claim.TargetID, xid, batchIdx, claim.Direction, capability); err != nil {
			clear(capability)
			return s.rollbackAndFailBatch(ctx, claim, session, xid, batchIdx, 0, earlierCommitted, "TARGET_PREPARATION_FAILED", err, deps)
		}

		// Test hook after ticket reservation (before final atomic fence)
		if deps.afterTicketReservation != nil {
			if err := deps.afterTicketReservation(ctx, batchIdx); err != nil {
				clear(capability)
				return s.rollbackAndFailBatch(ctx, claim, session, xid, batchIdx, 0, earlierCommitted, "DISPATCH_INTERRUPTED", err, deps)
			}
		}

		// Lease-fenced atomic persisted authorization and target block immediately before SQL
		ordinal := batchIdx + 1
		if err := s.authorizeBatchDispatch(ctx, claim, scopeExecID, chunkPlan, approvedSQLText, xid, version, ordinal, batchIdx, chunkIDs); err != nil {
			clear(capability)
			_ = session.Rollback()
			var cErr executioncontract.ContractError
			if errors.As(err, &cErr) && cErr.Code == "LEASE_MISMATCH" {
				return nil, err
			}
			return nil, err
		}
		if deps.crashAfterBatchAuthorize != nil {
			if crashErr := deps.crashAfterBatchAuthorize(ctx, batchIdx); crashErr != nil {
				clear(capability)
				_ = session.Rollback()
				return nil, crashErr
			}
		}
		// Execute the agreed immutable parameterized UPDATE immediately after final fence
		res, execErr := session.Tx().ExecContext(ctx, approvedSQLText, chunkIDs)
		if execErr != nil {
			clear(capability)
			return s.rollbackAndFailBatch(ctx, claim, session, xid, batchIdx, ordinal, earlierCommitted, "SQL_FAILED", execErr, deps)
		}
		ra, err := res.RowsAffected()
		if err != nil || ra != int64(len(chunkIDs)) {
			clear(capability)
			return s.rollbackAndFailBatch(ctx, claim, session, xid, batchIdx, ordinal, earlierCommitted, "BATCH_UNSAFE", errors.New("row count mismatch"), deps)
		}

		if deps.afterBatchUpdate != nil {
			if err := deps.afterBatchUpdate(ctx, batchIdx); err != nil {
				clear(capability)
				return s.rollbackAndFailBatch(ctx, claim, session, xid, batchIdx, ordinal, earlierCommitted, "UPDATE_HOOK_FAILED", err, deps)
			}
		}
		// Save needed row identity/version evidence atomically with up before commit in additive target profile
		_, err = session.Tx().ExecContext(ctx, `
			INSERT INTO wbd_auth.batch_row_versions
				(execution_id, physical_target_id, batch_index, item_id, target_xid, row_xmin, row_ctid)
			SELECT $1::uuid, $2::uuid, $3, b.id, $4::text::xid8, b.xmin::text, b.ctid::text
			FROM ONLY managed.batch_items b
			WHERE b.id = ANY($5::bigint[])
		`, scopeExecID, claim.TargetID, batchIdx, xid, chunkIDs)
		if err != nil {
			clear(capability)
			return s.rollbackAndFailBatch(ctx, claim, session, xid, batchIdx, ordinal, earlierCommitted, "TARGET_PREPARATION_FAILED", fmt.Errorf("record batch row versions: %w", err), deps)
		}

		// Record checkpoint on target in same transaction
		_, err = session.Tx().ExecContext(ctx, `
			SELECT wbd_auth.record_checkpoint($1::uuid, $2, $3, $4, $5::bytea)
		`, scopeExecID, batchIdx, string(claim.Direction), ra, capability)
		clear(capability)
		if err != nil {
			return s.rollbackAndFailBatch(ctx, claim, session, xid, batchIdx, ordinal, earlierCommitted, "RECEIPT_FAILED", fmt.Errorf("record checkpoint: %w", err), deps)
		}
		// Commit target transaction
		if err := session.Commit(); err != nil {
			return s.unknownOutcome(ctx, claim, fmt.Errorf("target commit for batch %d: %w", batchIdx, err))
		}

		// Verify checkpoint on target
		var recordedAt time.Time
		var chkXID string
		var chkRowsAffected int64
		err = session.Preparer().QueryRowContext(ctx, `
			SELECT recorded_at, target_xid::text, rows_affected
			FROM wbd_auth.batch_checkpoints
			WHERE execution_id = $1::uuid AND direction = $2 AND batch_index = $3
		`, scopeExecID, string(claim.Direction), batchIdx).Scan(&recordedAt, &chkXID, &chkRowsAffected)
		if err != nil || chkXID != xid || chkRowsAffected != ra {
			return s.unknownOutcome(ctx, claim, errors.New("unconfirmed or mismatched batch checkpoint after target commit"))
		}

		// Query truthful commit timestamp
		committedAt, err := queryBatchCommitTimestamp(ctx, session, scopeExecID, string(claim.Direction), batchIdx, xid)
		if err != nil {
			return s.unknownOutcome(ctx, claim, fmt.Errorf("query truthful commit timestamp: %w", err))
		}

		totalCommittedRows += ra
		isFinal := (batchIdx == totalBatches-1)

		// Test hook between target commit and platform finalization (e.g. crash simulation)
		if deps.afterBatchCommit != nil {
			if err := deps.afterBatchCommit(ctx, batchIdx); err != nil {
				return s.unknownOutcome(ctx, claim, err)
			}
		}
		if deps.crashAfterBatchCommit != nil {
			if crashErr := deps.crashAfterBatchCommit(ctx, batchIdx); crashErr != nil {
				return nil, crashErr
			}
		}
		totalRowsStr := strconv.FormatInt(batchScope.TotalCount, 10)
		batchesCommittedStr := strconv.Itoa(batchIdx + 1)
		rowsCommittedStr := strconv.FormatInt(totalCommittedRows, 10)
		prog := executioncontract.Progress{
			Direction:        claim.Direction,
			BatchesCommitted: batchesCommittedStr,
			RowsCommitted:    rowsCommittedStr,
			BatchesReverted:  "0",
			RowsReverted:     "0",
			TotalRows:        &totalRowsStr,
		}
		progJSON, _ := json.Marshal(prog)

		startIDStr := strconv.FormatInt(chunkIDs[0], 10)
		endIDStr := strconv.FormatInt(chunkIDs[len(chunkIDs)-1], 10)
		evidence := executioncontract.CommitEvidence{
			JobID:             claim.JobID,
			ExecutionID:       claim.ExecutionID,
			ProposalID:        claim.ProposalID,
			MigrationID:       claim.MigrationID,
			ActorID:           claim.ActorID,
			DatabaseID:        claim.DatabaseID,
			TargetID:          claim.TargetID,
			Revision:          claim.Revision,
			Version:           plan.Version,
			Action:            claim.Action,
			Direction:         claim.Direction,
			SQLSnapshot:       sqlText,
			SafeParameters:    map[string]any{"ids": chunkIDs},
			SnapshotHash:      claim.SnapshotHash,
			Receipt:           claim.ExecutionID,
			CommittedAt:       committedAt,
			RowCount:          strconv.FormatInt(ra, 10),
			SourceExecutionID: claim.SourceExecutionID,
			Batch: &executioncontract.BatchCommit{
				JobID:          claim.JobID,
				ExecutedBy:     executioncontract.Actor{ID: claim.ActorID},
				RangeStart:     &startIDStr,
				RangeEnd:       &endIDStr,
				SQLSnapshot:    sqlText,
				SafeParameters: map[string]any{"ids": chunkIDs},
				BatchIndex:     strconv.Itoa(batchIdx),
				RowCount:       strconv.FormatInt(ra, 10),
				CommittedAt:    committedAt,
				Receipt:        claim.ExecutionID,
			},
			FinalStep: isFinal,
		}

		// Platform finalization for this batch in prompt closure without deferred accumulation
		platErr := func() error {
			ptx, err := s.db.BeginTx(ctx, nil)
			if err != nil {
				return fmt.Errorf("begin platform batch commit tx: %w", err)
			}
			defer ptx.Rollback()

			_, err = ptx.ExecContext(ctx, `
				UPDATE app.job_statement_evidence
				SET outcome = 'succeeded', row_count = $3
				WHERE attempt_id = $1 AND ordinal = $2
			`, claim.AttemptID, ordinal, ra)
			if err != nil {
				return fmt.Errorf("update statement evidence: %w", err)
			}

			_, err = ptx.ExecContext(ctx, `
				INSERT INTO app.job_batch_commit_proofs
				    (step_auth_id, job_id, attempt_id, scope_execution_id, physical_target_id, direction, batch_index, target_xid, row_count, committed_at)
				SELECT id, $1, $2, $3, physical_target_id, $4, $5, $6, $7, $8
				FROM app.job_batch_step_authorizations
				WHERE scope_execution_id = $3 AND physical_target_id = $9::uuid AND direction = $4 AND batch_index = $5 AND target_xid = $6
				ON CONFLICT (scope_execution_id, direction, batch_index) DO NOTHING
			`, claim.JobID, claim.AttemptID, scopeExecID, string(claim.Direction), batchIdx, xid, ra, committedAt, claim.TargetID)
			if err != nil {
				return fmt.Errorf("persist batch step commit proof: %w", err)
			}

			_, err = ptx.ExecContext(ctx, `
				INSERT INTO app.job_commit_proofs (job_id, attempt_id, target_xid, committed_at)
				VALUES ($1, $2, $3, $4)
				ON CONFLICT (job_id) DO NOTHING
			`, claim.JobID, claim.AttemptID, xid, committedAt)
			if err != nil {
				return fmt.Errorf("persist commit proof: %w", err)
			}

			if isFinal {
				var (
					finalChkCount int
					finalMaxIdx   sql.NullInt64
				)
				tipErr := session.Preparer().QueryRowContext(ctx, `
					SELECT count(*), max(batch_index)
					FROM wbd_auth.batch_checkpoints
					WHERE execution_id = $1::uuid AND direction = $2
				`, scopeExecID, string(claim.Direction)).Scan(&finalChkCount, &finalMaxIdx)
				if tipErr != nil {
					return fmt.Errorf("validate checkpoint tip: %w", tipErr)
				}
				if finalChkCount != totalBatches || (finalMaxIdx.Valid && int(finalMaxIdx.Int64) != totalBatches-1) {
					return fmt.Errorf("observed checkpoint tip mismatch: count=%d, max=%v, expected=%d", finalChkCount, finalMaxIdx, totalBatches)
				}
			}

			if err := deps.Gate.RecordCommitTx(ctx, ptx, evidence); err != nil {
				return fmt.Errorf("gate record commit for batch %d: %w", batchIdx, err)
			}

			if isFinal {
				if err := s.completeBatchSuccessTx(ctx, ptx, claim, scopeExecID, progJSON); err != nil {
					return err
				}
			} else {
				res, err := ptx.ExecContext(ctx, `
					UPDATE app.jobs
					SET progress = $1::jsonb,
					    updated_at = clock_timestamp()
					WHERE id = $2 AND attempt_no = $3
					  AND status = 'running' AND execution_status = 'running'
					  AND lease_token = $4::uuid
					  AND lease_expires_at > clock_timestamp()
				`, string(progJSON), claim.JobID, claim.AttemptNo, claim.LeaseToken)
				if err != nil || requireOne(res, err, "update job progress") != nil {
					return fmt.Errorf("update job progress: %w", err)
				}
			}

			return ptx.Commit()
		}()
		if platErr != nil {
			return s.unknownOutcome(ctx, claim, platErr)
		}

		if isFinal && deps.afterTargetCommit != nil {
			if err := deps.afterTargetCommit(ctx); err != nil {
				return s.unknownOutcome(ctx, claim, err)
			}
		}
	}

	return &ExecutionOutcome{
		Status:  executioncontract.Succeeded,
		Receipt: claim.ExecutionID,
	}, nil
}

// ReconcileBatchJob recovers an authorized batch job from persisted state and target checkpoints
// without replay, without incoming resume job, and without approval revalidation.
func (s *Service) ReconcileBatchJob(ctx context.Context, claim *Claim, deps ExecutionDeps) (*ExecutionOutcome, error) {
	if s.db == nil {
		return nil, errors.New("db is required")
	}
	if claim == nil {
		return nil, executioncontract.ContractError{Code: "INVALID_REQUEST", Message: "claim is required"}
	}
	if claim.Mode != executioncontract.Batch {
		return nil, executioncontract.ContractError{Code: "UNSUPPORTED_MODE", Message: "batch execution only"}
	}
	if claim.Action != executioncontract.Apply &&
		claim.Action != executioncontract.Forward &&
		claim.Action != executioncontract.Resume &&
		claim.Action != executioncontract.Compensate {
		return nil, executioncontract.ContractError{Code: "UNSUPPORTED_ACTION", Message: "unsupported action for batch"}
	}
	if claim.Direction != executioncontract.Up && claim.Direction != executioncontract.Down {
		return nil, executioncontract.ContractError{Code: "INVALID_DIRECTION", Message: "batch execution supports up or down direction"}
	}
	if (claim.Action == executioncontract.Resume || claim.Action == executioncontract.Compensate) && (claim.SourceExecutionID == nil || *claim.SourceExecutionID == "") {
		return nil, executioncontract.ContractError{Code: "INVALID_REQUEST", Message: fmt.Sprintf("%s requires source_execution_id", claim.Action)}
	}
	if deps.Gate == nil {
		return nil, executioncontract.ContractError{Code: "GATE_REQUIRED", Message: "workflow gate is required"}
	}
	if deps.Acquire == nil {
		return nil, executioncontract.ContractError{Code: "ACQUIRE_REQUIRED", Message: "target lease acquirer is required"}
	}
	if deps.Timeout <= 0 {
		deps.Timeout = 30 * time.Second
	}
	if claim.Direction == executioncontract.Down {
		return s.reconcileBatchJobDown(ctx, claim, deps)
	}

	lease, err := deps.Acquire(ctx, claim.DatabaseID)
	if err != nil {
		discardLease(lease)
		return nil, fmt.Errorf("acquire target lease; outcome unconfirmed: %w", err)
	}
	session, err := migration.OpenTargetSession(ctx, lease, claim.TargetID, deps.Timeout)
	if err != nil {
		return nil, fmt.Errorf("open target session; outcome unconfirmed: %w", err)
	}
	defer session.Close(deps.Timeout)
	if err := session.VerifyExecutorTarget(ctx, claim.TargetID); err != nil {
		return nil, fmt.Errorf("verify executor target; outcome unconfirmed: %w", err)
	}

	var commitTimeAvailable bool
	err = session.Preparer().QueryRowContext(ctx, `SELECT current_setting('track_commit_timestamp') = 'on'
		AND pg_catalog.has_function_privilege(current_user, 'pg_catalog.pg_xact_commit_timestamp(xid)', 'EXECUTE')
		AND pg_catalog.has_function_privilege(current_user, 'pg_catalog.pg_xact_status(xid8)', 'EXECUTE')`).Scan(&commitTimeAvailable)
	if err != nil || !commitTimeAvailable {
		return s.failedBlockedOutcome(ctx, claim, "COMMIT_TIME_UNAVAILABLE",
			errors.Join(errors.New("exact target commit timestamp evidence is not supported"), err))
	}

	var snapshotBytes []byte
	err = s.db.QueryRowContext(ctx, `
		SELECT snapshot
		FROM app.jobs
		WHERE id = $1
	`, claim.JobID).Scan(&snapshotBytes)
	if err != nil {
		return nil, fmt.Errorf("read job snapshot for recovery: %w", err)
	}

	var draft executioncontract.Draft
	if err := json.Unmarshal(snapshotBytes, &draft); err != nil {
		return s.failedBlockedOutcome(ctx, claim, "INVALID_BATCH_CONFIG", fmt.Errorf("decode snapshot: %w", err))
	}
	if draft.Batch == nil {
		return s.failedBlockedOutcome(ctx, claim, "INVALID_BATCH_CONFIG", errors.New("draft snapshot missing batch configuration"))
	}
	if err := validateAgreedBatchConfig(draft.Batch); err != nil {
		return s.failedBlockedOutcome(ctx, claim, "INVALID_BATCH_CONFIG", err)
	}
	batchSize := draft.Batch.BatchSize
	approvedBatchConfig := *draft.Batch

	var approvedSQLText string
	err = s.db.QueryRowContext(ctx, `
		SELECT sql_text
		FROM app.job_dispatch_authorizations
		WHERE job_id = $1
		LIMIT 1
	`, claim.JobID).Scan(&approvedSQLText)
	if err != nil {
		err = s.db.QueryRowContext(ctx, `
			SELECT sql_text
			FROM app.job_batch_step_authorizations
			WHERE job_id = $1
			ORDER BY ordinal ASC
			LIMIT 1
		`, claim.JobID).Scan(&approvedSQLText)
		if err != nil {
			return s.failedBlockedOutcome(ctx, claim, "NO_PERSISTED_AUTHORITY", fmt.Errorf("read persisted SQL text: %w", err))
		}
	}

	scopeExecID := claim.ExecutionID
	if claim.Action == executioncontract.Resume {
		scopeExecID = *claim.SourceExecutionID
	}

	ident := BatchScopeIdentity{
		ExecutionID: scopeExecID,
		TargetID:    claim.TargetID,
		DatabaseID:  claim.DatabaseID,
		ActorID:     claim.ActorID,
	}
	batchScope, err := s.GetBatchScope(ctx, session.Preparer(), ident)
	if err != nil {
		return s.failedBlockedOutcome(ctx, claim, "SCOPE_UNAVAILABLE", fmt.Errorf("get batch scope: %w", err))
	}

	totalBatches := 0
	if batchScope.TotalCount > 0 {
		totalBatches = int((batchScope.TotalCount + int64(batchSize) - 1) / int64(batchSize))
	}

	scanRes, outc, err := s.scanAndReconcileBatchCheckpoints(
		ctx, claim, session, scopeExecID, totalBatches, batchSize,
		batchScope, approvedSQLText, approvedBatchConfig, deps,
	)
	if err != nil || outc != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		if outc != nil {
			return outc, err
		}
		return s.unknownOutcome(ctx, claim, err)
	}

	if totalBatches == 0 {
		totalRowsStr := "0"
		emptyProg := executioncontract.Progress{
			Direction:        claim.Direction,
			BatchesCommitted: "0",
			RowsCommitted:    "0",
			BatchesReverted:  "0",
			RowsReverted:     "0",
			TotalRows:        &totalRowsStr,
		}
		emptyProgJSON, _ := json.Marshal(emptyProg)

		emptyErr := func() error {
			ptx, err := s.db.BeginTx(ctx, nil)
			if err != nil {
				return fmt.Errorf("begin empty scope finalization tx: %w", err)
			}
			defer ptx.Rollback()

			if err := s.applyOriginatingClosuresTx(ctx, ptx, claim, session, scopeExecID, totalBatches, nil, claim.JobID, claim.AttemptID, true, deps); err != nil {
				return err
			}
			_, err = ptx.ExecContext(ctx, `
				UPDATE app.jobs
				SET progress = $1::jsonb,
				    updated_at = clock_timestamp()
				WHERE id = $2
			`, string(emptyProgJSON), claim.JobID)
			if err != nil {
				return fmt.Errorf("update empty scope progress: %w", err)
			}
			if err := s.releaseVerifiedBatchLineageBlockersTx(ctx, ptx, scopeExecID, claim.TargetID, claim.DatabaseID, claim.MigrationID, claim.Revision, claim.Direction, claim.SnapshotHash, claim.JobID, emptyProgJSON); err != nil {
				return err
			}
			return ptx.Commit()
		}()
		if emptyErr != nil {
			return s.handleClosureTxError(ctx, claim, emptyErr)
		}
		return &ExecutionOutcome{Status: executioncontract.Succeeded, Receipt: claim.ExecutionID, Reconciled: true}, nil
	}

	if scanRes.nextBatchIndex >= totalBatches {
		totalRowsStr := strconv.FormatInt(batchScope.TotalCount, 10)
		prog := executioncontract.Progress{
			Direction:        claim.Direction,
			BatchesCommitted: strconv.Itoa(scanRes.nextBatchIndex),
			RowsCommitted:    strconv.FormatInt(scanRes.totalCommittedRows, 10),
			BatchesReverted:  "0",
			RowsReverted:     "0",
			TotalRows:        &totalRowsStr,
		}
		progJSON, _ := json.Marshal(prog)

		platErr := func() error {
			ptx, err := s.db.BeginTx(ctx, nil)
			if err != nil {
				return fmt.Errorf("begin platform finalization tx: %w", err)
			}
			defer ptx.Rollback()

			if scanRes.pendingFinal != nil {
				res, err := ptx.ExecContext(ctx, `
					UPDATE app.job_statement_evidence
					SET outcome = 'succeeded',
					    row_count = $1,
					    executed_at = clock_timestamp()
					WHERE attempt_id = $2::uuid AND ordinal = $3 AND batch_index = $4
				`, scanRes.pendingFinal.chk.rows, scanRes.pendingFinal.step.bsaAttemptID, scanRes.pendingFinal.step.bsaOrdinal, scanRes.pendingFinal.step.bsaBatchIdx)
				if err != nil {
					return fmt.Errorf("reconcile statement evidence: %w", err)
				}
				if err := requireOne(res, err, "reconcile statement evidence"); err != nil {
					return err
				}

				_, err = ptx.ExecContext(ctx, `
					INSERT INTO app.job_batch_commit_proofs
					    (step_auth_id, job_id, attempt_id, scope_execution_id, physical_target_id, direction, batch_index, target_xid, row_count, committed_at)
					VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
					ON CONFLICT (scope_execution_id, direction, batch_index) DO NOTHING
				`, scanRes.pendingFinal.step.bsaID, scanRes.pendingFinal.step.bsaJobID, scanRes.pendingFinal.step.bsaAttemptID, scopeExecID, claim.TargetID, string(claim.Direction), scanRes.pendingFinal.chk.index, scanRes.pendingFinal.chk.xid, scanRes.pendingFinal.chk.rows, scanRes.pendingFinal.committedAt)
				if err != nil {
					return fmt.Errorf("reconcile batch commit proof: %w", err)
				}

				_, err = ptx.ExecContext(ctx, `
					INSERT INTO app.job_commit_proofs (job_id, attempt_id, target_xid, committed_at)
					VALUES ($1, $2, $3, $4)
					ON CONFLICT (job_id) DO NOTHING
				`, scanRes.pendingFinal.step.bsaJobID, scanRes.pendingFinal.step.bsaAttemptID, scanRes.pendingFinal.chk.xid, scanRes.pendingFinal.committedAt)
				if err != nil {
					return fmt.Errorf("reconcile job commit proof: %w", err)
				}

				if err := deps.Gate.RecordCommitTx(ctx, ptx, scanRes.pendingFinal.evidence); err != nil {
					return fmt.Errorf("gate reconcile record commit for batch %d: %w", scanRes.pendingFinal.chk.index, err)
				}
			}

			if err := s.applyOriginatingClosuresTx(ctx, ptx, claim, session, scopeExecID, totalBatches, nil, scanRes.finalizerJobID, scanRes.finalizerAttemptID, true, deps); err != nil {
				return err
			}

			finalID := scanRes.finalizerJobID
			if finalID == "" {
				finalID = claim.JobID
			}
			_, err = ptx.ExecContext(ctx, `
				UPDATE app.jobs
				SET progress = $1::jsonb,
				    updated_at = clock_timestamp()
				WHERE id = $2
			`, string(progJSON), finalID)
			if err != nil {
				return fmt.Errorf("update final progress: %w", err)
			}

			if err := s.releaseVerifiedBatchLineageBlockersTx(ctx, ptx, scopeExecID, claim.TargetID, claim.DatabaseID, claim.MigrationID, claim.Revision, claim.Direction, claim.SnapshotHash, finalID, progJSON); err != nil {
				return err
			}

			return ptx.Commit()
		}()
		if platErr != nil {
			return s.handleClosureTxError(ctx, claim, platErr)
		}
		return &ExecutionOutcome{Status: executioncontract.Succeeded, Receipt: claim.ExecutionID, Reconciled: true}, nil

	} else {
		var (
			jobDurableStatus, jobDurableExecStatus string
			jobDurableBlocks                       bool
			jobErrCode, jobErrMsg                  sql.NullString
		)
		partErr := func() error {
			ptx, err := s.db.BeginTx(ctx, nil)
			if err != nil {
				return fmt.Errorf("begin closure tx: %w", err)
			}
			defer ptx.Rollback()

			if err := s.applyOriginatingClosuresTx(ctx, ptx, claim, session, scopeExecID, totalBatches, nil, "", "", false, deps); err != nil {
				return err
			}

			if scanRes.nextBatchIndex > 0 {
				totalRowsStr := strconv.FormatInt(batchScope.TotalCount, 10)
				prog := executioncontract.Progress{
					Direction:        claim.Direction,
					BatchesCommitted: strconv.Itoa(scanRes.nextBatchIndex),
					RowsCommitted:    strconv.FormatInt(scanRes.totalCommittedRows, 10),
					BatchesReverted:  "0",
					RowsReverted:     "0",
					TotalRows:        &totalRowsStr,
				}
				progJSON, _ := json.Marshal(prog)

				_, err = ptx.ExecContext(ctx, `
					UPDATE app.jobs
					SET progress = $1::jsonb,
					    updated_at = clock_timestamp()
					WHERE id = $2
				`, string(progJSON), claim.JobID)
				if err != nil {
					return fmt.Errorf("update partial progress: %w", err)
				}
			}

			if err := ptx.QueryRowContext(ctx, `
				SELECT status, execution_status, blocks_database, error_code, error_message
				FROM app.jobs
				WHERE id = $1
			`, claim.JobID).Scan(&jobDurableStatus, &jobDurableExecStatus, &jobDurableBlocks, &jobErrCode, &jobErrMsg); err != nil {
				return fmt.Errorf("read recovered job disposition: %w", err)
			}

			return ptx.Commit()
		}()
		if partErr != nil {
			return s.handleClosureTxError(ctx, claim, partErr)
		}

		code := "TARGET_BLOCKED"
		if jobErrCode.Valid && jobErrCode.String != "" {
			code = jobErrCode.String
		}
		msg := "lease expired after partial batch execution"
		if scanRes.nextBatchIndex == 0 {
			msg = "lease expired and target transaction aborted"
		}
		if jobErrMsg.Valid && jobErrMsg.String != "" {
			msg = jobErrMsg.String
		}

		outStatus := executioncontract.Failed
		if jobDurableStatus == "partial" {
			outStatus = executioncontract.Partial
		}
		var outErr error
		if jobDurableBlocks || outStatus == executioncontract.Partial {
			outErr = executioncontract.ContractError{Code: code, Message: msg}
		}
		return &ExecutionOutcome{
			Status:     outStatus,
			Error:      outErr,
			Reconciled: true,
		}, nil
	}
}

// ReconcileBatchJobByID reconstructs the authorized batch execution identity and reconciles it.
func (s *Service) ReconcileBatchJobByID(ctx context.Context, jobID string, deps ExecutionDeps) (*ExecutionOutcome, error) {
	if s.db == nil {
		return nil, errors.New("db is required")
	}
	if jobID == "" {
		return nil, executioncontract.ContractError{Code: "INVALID_REQUEST", Message: "job_id is required"}
	}

	var completedReceipt string
	err := s.db.QueryRowContext(ctx, `SELECT j.execution_id::text FROM app.jobs j
		JOIN app.job_attempts a ON a.job_id = j.id AND a.attempt_no = j.attempt_no
		WHERE j.id = $1 AND j.mode = 'batch' AND j.status = 'succeeded'
		  AND NOT j.blocks_database AND a.status = 'succeeded'`, jobID).Scan(&completedReceipt)
	if err == nil {
		return &ExecutionOutcome{Status: executioncontract.Succeeded, Receipt: completedReceipt, Reconciled: true}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("read completed batch job: %w", err)
	}

	var failedReceipt string
	err = s.db.QueryRowContext(ctx, `SELECT j.execution_id::text FROM app.jobs j
		JOIN app.job_attempts a ON a.job_id = j.id AND a.attempt_no = j.attempt_no
		WHERE j.id = $1 AND j.mode = 'batch' AND j.status = 'failed'
		  AND NOT j.blocks_database AND a.status = 'failed'`, jobID).Scan(&failedReceipt)
	if err == nil {
		return &ExecutionOutcome{Status: executioncontract.Failed, Reconciled: true}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("read clean failed batch job: %w", err)
	}

	var partialReceipt string
	var errCode, errMsg sql.NullString
	err = s.db.QueryRowContext(ctx, `SELECT j.execution_id::text, j.error_code, j.error_message FROM app.jobs j
		JOIN app.job_attempts a ON a.job_id = j.id AND a.attempt_no = j.attempt_no
		WHERE j.id = $1 AND j.mode = 'batch' AND j.status = 'partial'
		  AND a.status = 'partial'`, jobID).Scan(&partialReceipt, &errCode, &errMsg)
	if err == nil {
		code := "TARGET_BLOCKED"
		if errCode.Valid && errCode.String != "" {
			code = errCode.String
		}
		msg := "batch job is partially committed and blocked"
		if errMsg.Valid && errMsg.String != "" {
			msg = errMsg.String
		}
		return &ExecutionOutcome{
			Status:     executioncontract.Partial,
			Error:      executioncontract.ContractError{Code: code, Message: msg},
			Reconciled: true,
		}, nil
	}

	var failedBlockedReceipt string
	errCode, errMsg = sql.NullString{}, sql.NullString{}
	err = s.db.QueryRowContext(ctx, `SELECT j.execution_id::text, j.error_code, j.error_message FROM app.jobs j
		JOIN app.job_attempts a ON a.job_id = j.id AND a.attempt_no = j.attempt_no
		WHERE j.id = $1 AND j.mode = 'batch' AND j.status = 'failed'
		  AND j.blocks_database AND a.status = 'failed'`, jobID).Scan(&failedBlockedReceipt, &errCode, &errMsg)
	if err == nil {
		code := "TARGET_BLOCKED"
		if errCode.Valid && errCode.String != "" {
			code = errCode.String
		}
		msg := "batch job failed and target remains blocked"
		if errMsg.Valid && errMsg.String != "" {
			msg = errMsg.String
		}
		return &ExecutionOutcome{
			Status:     executioncontract.Failed,
			Error:      executioncontract.ContractError{Code: code, Message: msg},
			Reconciled: true,
		}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("read failed blocked batch job: %w", err)
	}

	var (
		claim         Claim
		source        sql.NullString
		jobStatus     string
		attemptStatus string
		leaseExpires  sql.NullTime
		isExpired     bool
		traceparent   sql.NullString
		tracestate    sql.NullString
	)
	err = s.db.QueryRowContext(ctx, `
		SELECT j.id::text, j.execution_id::text, j.database_id::text, j.target_id::text,
		       j.proposal_id::text, j.migration_id::text, j.revision, j.actor_id::text,
		       j.action, j.direction, j.mode, j.source_execution_id::text,
		       j.snapshot_hash, COALESCE(j.approval_fingerprint, ''), a.id::text, a.attempt_no,
		       j.status, a.status, j.lease_expires_at,
		       (j.lease_expires_at IS NULL OR j.lease_expires_at <= clock_timestamp()),
		       j.traceparent, j.tracestate
		FROM app.jobs j
		JOIN app.job_attempts a ON a.job_id = j.id AND a.attempt_no = j.attempt_no
		WHERE j.id = $1 AND j.mode = 'batch' AND j.blocks_database
		  AND (
		      EXISTS (SELECT 1 FROM app.job_batch_step_authorizations bsa WHERE bsa.job_id = j.id AND bsa.attempt_id = a.id)
		      OR EXISTS (SELECT 1 FROM app.job_dispatch_authorizations d WHERE d.job_id = j.id AND d.attempt_id = a.id)
		      OR (j.action = 'compensate' AND j.direction = 'down' AND j.source_execution_id IS NOT NULL)
		  )
		  AND (
		      (j.status IN ('running', 'unknown') OR a.status IN ('running', 'unknown'))
		  )`, jobID).Scan(
		&claim.JobID, &claim.ExecutionID, &claim.DatabaseID, &claim.TargetID,
		&claim.ProposalID, &claim.MigrationID, &claim.Revision, &claim.ActorID,
		&claim.Action, &claim.Direction, &claim.Mode, &source, &claim.SnapshotHash,
		&claim.ApprovalFingerprint, &claim.AttemptID, &claim.AttemptNo,
		&jobStatus, &attemptStatus, &leaseExpires, &isExpired,
		&traceparent, &tracestate,
	)
	if err != nil {
		return nil, fmt.Errorf("discover authorized batch job: %w", err)
	}
	if !isExpired {
		return nil, executioncontract.ContractError{
			Code:    "TARGET_BLOCKED",
			Message: fmt.Sprintf("job %s lease has not expired (expires at %v)", jobID, leaseExpires.Time),
		}
	}
	if source.Valid {
		claim.SourceExecutionID = &source.String
	}
	claim.Traceparent = traceparent.String
	claim.Tracestate = tracestate.String
	claim.LeaseToken = ""
	return s.ReconcileBatchJob(ctx, &claim, deps)
}

// ListUnresolvedBatchJobIDs discovers blocked authorized batch jobs for reconciliation after restart.
func (s *Service) ListUnresolvedBatchJobIDs(ctx context.Context, limit int, afterID ...string) ([]string, error) {
	if s.db == nil {
		return nil, errors.New("db is required")
	}
	if limit < 1 || limit > 1000 {
		return nil, errors.New("limit must be between 1 and 1000")
	}
	cursor := ""
	if len(afterID) > 0 {
		cursor = afterID[0]
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT j.id::text
		FROM app.jobs j
		JOIN app.job_attempts a ON a.job_id = j.id AND a.attempt_no = j.attempt_no
		WHERE j.mode = 'batch' AND j.blocks_database
		  AND (j.lease_expires_at IS NULL OR j.lease_expires_at <= clock_timestamp())
		  AND (j.status IN ('running', 'unknown') OR a.status IN ('running', 'unknown'))
		  AND (
		      EXISTS (SELECT 1 FROM app.job_batch_step_authorizations bsa WHERE bsa.job_id = j.id AND bsa.attempt_id = a.id)
		      OR EXISTS (SELECT 1 FROM app.job_dispatch_authorizations d WHERE d.job_id = j.id AND d.attempt_id = a.id)
		      OR (j.action = 'compensate' AND j.direction = 'down' AND j.source_execution_id IS NOT NULL)
		  )
		  AND ($2 = '' OR j.id > $2::uuid)
		ORDER BY j.id
		LIMIT $1`, limit, cursor)
	if err != nil {
		return nil, fmt.Errorf("list unresolved authorized batch jobs: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan unresolved authorized batch job: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list unresolved authorized batch jobs: %w", err)
	}
	return ids, nil
}

// batchDownScopeExecution resolves a resume's evidence execution to the up
// execution that owns the immutable batch scope. It does not rewrite the
// approved source execution carried by the claim.
func (s *Service) batchDownScopeExecution(ctx context.Context, claim *Claim) (string, error) {
	if claim.SourceExecutionID == nil {
		return "", errors.New("down job has no source execution")
	}
	if claim.Action != executioncontract.Resume {
		return *claim.SourceExecutionID, nil
	}
	var scope string
	err := s.db.QueryRowContext(ctx, `
		SELECT up.execution_id::text
		FROM app.jobs down_job
		JOIN app.jobs up ON up.execution_id = down_job.source_execution_id
		WHERE down_job.execution_id = $1::uuid AND down_job.id <> $2::uuid
		  AND down_job.target_id = $3::uuid AND down_job.database_id = $4::uuid
		  AND down_job.migration_id = $5::uuid AND down_job.revision = $6
		  AND down_job.snapshot_hash = $7 AND down_job.mode = 'batch'
		  AND down_job.action = 'compensate' AND down_job.direction = 'down'
		  AND down_job.status = 'partially_reverted'
		  AND up.id <> down_job.id AND up.id <> $2::uuid
		  AND up.target_id = down_job.target_id AND up.database_id = down_job.database_id
		  AND up.migration_id = down_job.migration_id AND up.revision = down_job.revision
		  AND up.snapshot_hash = down_job.snapshot_hash AND up.mode = 'batch'
		  AND up.direction = 'up' AND up.status IN ('succeeded', 'partial', 'partially_reverted')
		  AND EXISTS (
		      SELECT 1 FROM app.job_batch_step_authorizations bsa
		      JOIN app.job_batch_commit_proofs p ON p.step_auth_id = bsa.id
		        AND p.target_xid = bsa.target_xid
		        AND p.physical_target_id = bsa.physical_target_id
		        AND p.direction = bsa.direction
		        AND p.scope_execution_id = bsa.scope_execution_id
		      WHERE bsa.scope_execution_id = up.execution_id
		        AND bsa.physical_target_id = down_job.target_id AND bsa.direction = 'up'
		  )
	`, *claim.SourceExecutionID, claim.JobID, claim.TargetID, claim.DatabaseID,
		claim.MigrationID, claim.Revision, claim.SnapshotHash).Scan(&scope)
	if err != nil {
		return "", fmt.Errorf("resolve interrupted compensation source: %w", err)
	}
	return scope, nil
}

func (s *Service) executeBatchJobDown(ctx context.Context, claim *Claim, deps ExecutionDeps) (*ExecutionOutcome, error) {
	lease, err := deps.Acquire(ctx, claim.DatabaseID)
	if err != nil {
		discardLease(lease)
		return nil, fmt.Errorf("acquire target lease: %w", err)
	}
	session, err := migration.OpenTargetSession(ctx, lease, claim.TargetID, deps.Timeout)
	if err != nil {
		return nil, fmt.Errorf("open target session: %w", err)
	}
	defer session.Close(deps.Timeout)
	if err := session.VerifyExecutorTarget(ctx, claim.TargetID); err != nil {
		return nil, fmt.Errorf("verify executor target: %w", err)
	}

	plan, err := deps.Gate.Revalidate(ctx, claim.RevalidationRequest())
	if err != nil {
		var cErr executioncontract.ContractError
		if errors.As(err, &cErr) && cErr.Code == "REVIEW_STALE" {
			return nil, fmt.Errorf("REVIEW_STALE requires WorkflowGate.ReturnToReviewTx and atomic job finalization: %w", err)
		}
		return nil, fmt.Errorf("workflow revalidation failed: %w", err)
	}
	if !reflect.DeepEqual(plan.Request, claim.RevalidationRequest()) {
		return nil, executioncontract.ContractError{Code: "PLAN_MISMATCH", Message: "workflow plan request mismatch"}
	}
	if err := validateAgreedBatchConfig(plan.Snapshot.Batch); err != nil {
		return s.failedOutcome(ctx, claim, "INVALID_BATCH_CONFIG", err)
	}
	sqlText, err := selectSQL(plan, claim.Action, claim.Direction)
	if err != nil || validateAgreedBatchSQL(claim.Direction, sqlText) != nil {
		return s.failedOutcome(ctx, claim, "SQL_UNSUPPORTED", errors.New("plan SQL does not match agreed batch pattern"))
	}
	approvedPlanVersion := plan.Version
	approvedSQLText := sqlText

	var commitTimeAvailable bool
	err = session.Preparer().QueryRowContext(ctx, `SELECT current_setting('track_commit_timestamp') = 'on'
		AND pg_catalog.has_function_privilege(current_user, 'pg_catalog.pg_xact_commit_timestamp(xid)', 'EXECUTE')
		AND pg_catalog.has_function_privilege(current_user, 'pg_catalog.pg_xact_status(xid8)', 'EXECUTE')`).Scan(&commitTimeAvailable)
	if err != nil || !commitTimeAvailable {
		return s.failedOutcome(ctx, claim, "COMMIT_TIME_UNAVAILABLE",
			errors.Join(errors.New("exact target commit timestamp evidence is not supported"), err))
	}

	scopeExecID, err := s.batchDownScopeExecution(ctx, claim)
	if err != nil {
		return s.failedOutcome(ctx, claim, "SOURCE_JOB_NOT_FOUND", err)
	}
	var (
		srcTargetID      string
		srcDatabaseID    string
		srcMigrationID   string
		srcRevision      int
		srcSnapshotHash  string
		srcDirection     string
		srcMode          string
		srcStatus        string
		srcSnapshotBytes []byte
	)
	err = s.db.QueryRowContext(ctx, `
		SELECT target_id::text, database_id::text, migration_id::text, revision,
		       snapshot_hash, direction, mode, status, snapshot
		FROM app.jobs
		WHERE execution_id = $1::uuid AND database_id = $2::uuid
		ORDER BY created_at DESC
		LIMIT 1
	`, scopeExecID, claim.DatabaseID).Scan(
		&srcTargetID, &srcDatabaseID, &srcMigrationID, &srcRevision,
		&srcSnapshotHash, &srcDirection, &srcMode, &srcStatus, &srcSnapshotBytes,
	)
	if err != nil {
		return s.failedOutcome(ctx, claim, "SOURCE_JOB_NOT_FOUND", fmt.Errorf("source execution job: %w", err))
	}
	if srcTargetID != claim.TargetID {
		return s.failedOutcome(ctx, claim, "TARGET_MISMATCH", errors.New("compensation target does not match source execution"))
	}
	if srcDatabaseID != claim.DatabaseID {
		return s.failedOutcome(ctx, claim, "DATABASE_MISMATCH", errors.New("compensation database does not match source execution"))
	}
	if srcMigrationID != claim.MigrationID {
		return s.failedOutcome(ctx, claim, "MIGRATION_MISMATCH", errors.New("compensation migration does not match source execution"))
	}
	if srcRevision != claim.Revision {
		return s.failedOutcome(ctx, claim, "REVISION_MISMATCH", errors.New("compensation revision does not match source execution"))
	}
	if srcSnapshotHash != claim.SnapshotHash {
		return s.failedOutcome(ctx, claim, "SNAPSHOT_HASH_MISMATCH", errors.New("compensation snapshot hash does not match source execution"))
	}
	if srcDirection != "up" {
		return s.failedOutcome(ctx, claim, "DIRECTION_MISMATCH", errors.New("source execution direction must be up"))
	}
	if srcMode != "batch" {
		return s.failedOutcome(ctx, claim, "UNSUPPORTED_MODE", errors.New("source execution is not a batch job"))
	}
	if srcStatus != "succeeded" && srcStatus != "partial" && srcStatus != "partially_reverted" {
		return s.failedOutcome(ctx, claim, "STATUS_CONFLICT", errors.New("source execution is not in an eligible committed state"))
	}
	var srcSnapshot executioncontract.Draft
	if err := json.Unmarshal(srcSnapshotBytes, &srcSnapshot); err != nil {
		return s.failedOutcome(ctx, claim, "INVALID_BATCH_CONFIG", fmt.Errorf("decode source snapshot: %w", err))
	}
	if srcSnapshot.MigrationID != claim.MigrationID {
		return s.failedOutcome(ctx, claim, "MIGRATION_MISMATCH", errors.New("compensation migration does not match source execution"))
	}
	batchSize := plan.Snapshot.Batch.BatchSize
	if srcSnapshot.Batch == nil || srcSnapshot.Batch.BatchSize != batchSize ||
		srcSnapshot.Batch.Schema != plan.Snapshot.Batch.Schema ||
		srcSnapshot.Batch.Table != plan.Snapshot.Batch.Table ||
		srcSnapshot.Batch.CursorColumn != plan.Snapshot.Batch.CursorColumn {
		return s.failedOutcome(ctx, claim, "BATCH_CONFIG_MISMATCH",
			errors.New("compensation batch configuration does not match source execution"))
	}

	ident := BatchScopeIdentity{
		ExecutionID: scopeExecID,
		TargetID:    claim.TargetID,
		DatabaseID:  claim.DatabaseID,
		ActorID:     claim.ActorID,
	}
	batchScope, err := s.GetBatchScope(ctx, session.Preparer(), ident)
	if err != nil {
		return s.failedOutcome(ctx, claim, "SCOPE_UNAVAILABLE", fmt.Errorf("get source batch scope: %w", err))
	}

	type upChk struct {
		index int
		rows  int64
		xid   string
	}
	var committedUpBatches []upChk
	var totalCommittedUpRows int64

	rows, err := session.Preparer().QueryContext(ctx, `
		SELECT batch_index, rows_affected, target_xid::text
		FROM wbd_auth.batch_checkpoints
		WHERE execution_id = $1::uuid AND direction = 'up'
		ORDER BY batch_index ASC
	`, scopeExecID)
	if err != nil {
		return nil, fmt.Errorf("query committed up checkpoints: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var uc upChk
		if err := rows.Scan(&uc.index, &uc.rows, &uc.xid); err != nil {
			return nil, fmt.Errorf("scan up checkpoint: %w", err)
		}
		committedUpBatches = append(committedUpBatches, uc)
	}
	rows.Close()

	for i, uc := range committedUpBatches {
		if uc.index != i {
			return s.failedBlockedOutcome(ctx, claim, "TARGET_BLOCKED",
				fmt.Errorf("non-consecutive up checkpoints: expected %d, got %d", i, uc.index))
		}
		var xactStatus string
		if err := session.Preparer().QueryRowContext(ctx, `SELECT pg_catalog.pg_xact_status($1::xid8)`, uc.xid).Scan(&xactStatus); err != nil || xactStatus != "committed" {
			return s.unknownOutcome(ctx, claim, fmt.Errorf("up checkpoint %d target xid %s status %s", uc.index, uc.xid, xactStatus))
		}

		var upProofExists bool
		err = s.db.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM app.job_batch_step_authorizations bsa
				JOIN app.job_batch_commit_proofs p ON p.step_auth_id = bsa.id
				  AND p.target_xid = bsa.target_xid
				  AND p.physical_target_id = bsa.physical_target_id
				  AND p.direction = bsa.direction
				  AND p.scope_execution_id = bsa.scope_execution_id
				WHERE bsa.scope_execution_id = $1::uuid
				  AND bsa.physical_target_id = $2::uuid
				  AND bsa.direction = 'up'
				  AND bsa.batch_index = $3
				  AND bsa.target_xid = $4
			)
		`, scopeExecID, claim.TargetID, uc.index, uc.xid).Scan(&upProofExists)
		if err != nil || !upProofExists {
			return s.failedBlockedOutcome(ctx, claim, "TARGET_BLOCKED",
				fmt.Errorf("up checkpoint %d target xid %s lacks matching platform authorization/commit proof", uc.index, uc.xid))
		}
		totalCommittedUpRows += uc.rows
	}

	N_up := len(committedUpBatches)
	totalRowsStr := strconv.FormatInt(batchScope.TotalCount, 10)
	if N_up == 0 {
		return s.failedOutcome(ctx, claim, "NO_COMMITTED_UP_BATCHES",
			errors.New("cannot compensate: no committed up checkpoints proved for source execution"))
	}

	type downChk struct {
		index int
		rows  int64
		xid   string
	}
	var revertedDownBatches []downChk
	var rowsRevertedCount int64

	dRows, err := session.Preparer().QueryContext(ctx, `
		SELECT batch_index, rows_affected, target_xid::text
		FROM wbd_auth.batch_checkpoints
		WHERE execution_id = $1::uuid AND direction = 'down'
		ORDER BY batch_index DESC
	`, scopeExecID)
	if err != nil {
		return nil, fmt.Errorf("query down checkpoints: %w", err)
	}
	defer dRows.Close()
	for dRows.Next() {
		var dc downChk
		if err := dRows.Scan(&dc.index, &dc.rows, &dc.xid); err != nil {
			return nil, fmt.Errorf("scan down checkpoint: %w", err)
		}
		revertedDownBatches = append(revertedDownBatches, dc)
	}
	dRows.Close()

	for i, dc := range revertedDownBatches {
		expectedIndex := N_up - 1 - i
		if dc.index != expectedIndex {
			return s.failedBlockedOutcome(ctx, claim, "TARGET_BLOCKED",
				fmt.Errorf("non-reverse down checkpoint: expected %d, got %d", expectedIndex, dc.index))
		}
		var xactStatus string
		if err := session.Preparer().QueryRowContext(ctx, `SELECT pg_catalog.pg_xact_status($1::xid8)`, dc.xid).Scan(&xactStatus); err != nil || xactStatus != "committed" {
			return s.unknownOutcome(ctx, claim, fmt.Errorf("down checkpoint %d target xid %s status %s", dc.index, dc.xid, xactStatus))
		}
		rowsRevertedCount += dc.rows

		committedAt, err := queryBatchCommitTimestamp(ctx, session, scopeExecID, "down", dc.index, dc.xid)
		if err != nil || committedAt.IsZero() {
			return s.failedBlockedOutcome(ctx, claim, "TARGET_BLOCKED",
				fmt.Errorf("down checkpoint %d truthful commit timestamp unavailable: %w", dc.index, err))
		}

		var bsaID, bsaJobID, bsaAttemptID, bsaActorID, bsaOrdinal string
		var bsaPlanJSON, bsaChunkIDsJSON, bsaSQLText, bsaTargetVersion string
		var bsaSQLSha256 []byte
		var origJobID, origExecID, origProposalID, origMigrationID, origActorID, origAction, origDirection, origSnapHash string
		var origRevision int
		var origSourceExecID sql.NullString
		err = s.db.QueryRowContext(ctx, `
			SELECT bsa.id::text, bsa.job_id::text, bsa.attempt_id::text, bsa.actor_id::text, bsa.ordinal::text,
			       bsa.plan::text, bsa.chunk_ids::text, bsa.sql_text, bsa.sql_sha256, bsa.target_version::text,
			       j.id::text, j.execution_id::text, j.proposal_id::text, j.migration_id::text,
			       j.actor_id::text, j.action, j.direction, j.snapshot_hash, j.revision, j.source_execution_id::text
			FROM app.job_batch_step_authorizations bsa
			JOIN app.jobs j ON j.id = bsa.job_id
			WHERE bsa.scope_execution_id = $1::uuid
			  AND bsa.physical_target_id = $2::uuid
			  AND bsa.direction = 'down'
			  AND bsa.batch_index = $3
			  AND bsa.target_xid = $4
		`, scopeExecID, claim.TargetID, dc.index, dc.xid).Scan(
			&bsaID, &bsaJobID, &bsaAttemptID, &bsaActorID, &bsaOrdinal,
			&bsaPlanJSON, &bsaChunkIDsJSON, &bsaSQLText, &bsaSQLSha256, &bsaTargetVersion,
			&origJobID, &origExecID, &origProposalID, &origMigrationID, &origActorID, &origAction, &origDirection, &origSnapHash, &origRevision, &origSourceExecID,
		)
		if err != nil {
			return s.failedBlockedOutcome(ctx, claim, "TARGET_BLOCKED",
				fmt.Errorf("match authorized down step for batch %d: %w", dc.index, err))
		}

		var evOutcome string
		var evRowCount sql.NullInt64
		err = s.db.QueryRowContext(ctx, `
			SELECT outcome, row_count
			FROM app.job_statement_evidence
			WHERE attempt_id = $1::uuid AND batch_index = $2
		`, bsaAttemptID, dc.index).Scan(&evOutcome, &evRowCount)
		if err != nil {
			return s.failedBlockedOutcome(ctx, claim, "TARGET_BLOCKED",
				fmt.Errorf("statement evidence for down batch %d: %w", dc.index, err))
		}

		var proofExists bool
		err = s.db.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM app.job_batch_commit_proofs
				WHERE scope_execution_id = $1::uuid AND direction = 'down' AND batch_index = $2
			)
		`, scopeExecID, dc.index).Scan(&proofExists)
		if err != nil {
			return s.unknownOutcome(ctx, claim, fmt.Errorf("check down commit proof: %w", err))
		}

		if !proofExists {
			var stepChunkIDs []int64
			_ = json.Unmarshal([]byte(bsaChunkIDsJSON), &stepChunkIDs)
			startIDStr := strconv.FormatInt(stepChunkIDs[0], 10)
			endIDStr := strconv.FormatInt(stepChunkIDs[len(stepChunkIDs)-1], 10)
			var sExecID *string
			if origSourceExecID.Valid && origSourceExecID.String != "" {
				sVal := origSourceExecID.String
				sExecID = &sVal
			}
			isFinalStep := (dc.index == 0)
			evidence := executioncontract.CommitEvidence{
				JobID:             origJobID,
				ExecutionID:       origExecID,
				ProposalID:        origProposalID,
				MigrationID:       origMigrationID,
				ActorID:           origActorID,
				DatabaseID:        claim.DatabaseID,
				TargetID:          claim.TargetID,
				Revision:          origRevision,
				Version:           bsaTargetVersion,
				Action:            executioncontract.Action(origAction),
				Direction:         executioncontract.Down,
				SQLSnapshot:       bsaSQLText,
				SafeParameters:    map[string]any{"ids": stepChunkIDs},
				SnapshotHash:      origSnapHash,
				Receipt:           origExecID,
				CommittedAt:       committedAt,
				RowCount:          strconv.FormatInt(dc.rows, 10),
				SourceExecutionID: sExecID,
				Batch: &executioncontract.BatchCommit{
					JobID:          origJobID,
					ExecutedBy:     executioncontract.Actor{ID: origActorID},
					RangeStart:     &startIDStr,
					RangeEnd:       &endIDStr,
					SQLSnapshot:    bsaSQLText,
					SafeParameters: map[string]any{"ids": stepChunkIDs},
					BatchIndex:     strconv.Itoa(dc.index),
					RowCount:       strconv.FormatInt(dc.rows, 10),
					CommittedAt:    committedAt,
					Receipt:        origExecID,
				},
				FinalStep: isFinalStep,
			}

			reconErr := func() error {
				ptx, err := s.db.BeginTx(ctx, nil)
				if err != nil {
					return fmt.Errorf("begin platform tx for down reconcile: %w", err)
				}
				defer ptx.Rollback()

				res, err := ptx.ExecContext(ctx, `
					UPDATE app.job_statement_evidence
					SET outcome = 'succeeded',
					    row_count = $1,
					    executed_at = clock_timestamp()
					WHERE attempt_id = $2::uuid AND batch_index = $3
				`, dc.rows, bsaAttemptID, dc.index)
				if err != nil {
					return fmt.Errorf("reconcile down statement evidence: %w", err)
				}
				if err := requireOne(res, err, "reconcile down statement evidence"); err != nil {
					return err
				}

				_, err = ptx.ExecContext(ctx, `
					INSERT INTO app.job_batch_commit_proofs
						(step_auth_id, job_id, attempt_id, scope_execution_id, physical_target_id, direction, batch_index, target_xid, row_count, committed_at)
					VALUES ($1, $2, $3, $4, $5, 'down', $6, $7, $8, $9)
					ON CONFLICT (scope_execution_id, direction, batch_index) DO NOTHING
				`, bsaID, origJobID, bsaAttemptID, scopeExecID, claim.TargetID, dc.index, dc.xid, dc.rows, committedAt)
				if err != nil {
					return fmt.Errorf("record down commit proof: %w", err)
				}

				if err := deps.Gate.RecordCommitTx(ctx, ptx, evidence); err != nil {
					return fmt.Errorf("gate record commit for recovered down batch %d: %w", dc.index, err)
				}

				return ptx.Commit()
			}()
			if reconErr != nil {
				return s.failedBlockedOutcome(ctx, claim, "TARGET_BLOCKED", reconErr)
			}
		}
	}
	batchesRevertedCount := len(revertedDownBatches)

	if batchesRevertedCount >= N_up {
		prog := executioncontract.Progress{
			Direction:        claim.Direction,
			BatchesCommitted: strconv.Itoa(N_up),
			RowsCommitted:    strconv.FormatInt(totalCommittedUpRows, 10),
			BatchesReverted:  strconv.Itoa(batchesRevertedCount),
			RowsReverted:     strconv.FormatInt(rowsRevertedCount, 10),
			TotalRows:        &totalRowsStr,
		}
		progJSON, _ := json.Marshal(prog)
		finalizeErr := func() error {
			ptx, err := s.db.BeginTx(ctx, nil)
			if err != nil {
				return fmt.Errorf("begin finalization tx: %w", err)
			}
			defer ptx.Rollback()
			if err := s.completeBatchSuccessTx(ctx, ptx, claim, scopeExecID, progJSON); err != nil {
				return err
			}
			if err := s.releaseVerifiedBatchDownLineageBlockersTx(ctx, ptx, scopeExecID, claim.TargetID, claim.DatabaseID, claim.MigrationID, claim.Revision, claim.SnapshotHash, claim.JobID); err != nil {
				return fmt.Errorf("release verified down lineage blockers: %w", err)
			}
			return ptx.Commit()
		}()
		if finalizeErr != nil {
			return s.handleClosureTxError(ctx, claim, finalizeErr)
		}
		return &ExecutionOutcome{Status: executioncontract.Succeeded, Receipt: claim.ExecutionID, Reconciled: true}, nil
	}

	version, err := strconv.ParseInt(plan.Version, 10, 64)
	if err != nil || version < 1 {
		return nil, executioncontract.ContractError{Code: "INVALID_VERSION", Message: "workflow plan has invalid target version"}
	}

	startBatchIdx := N_up - 1 - batchesRevertedCount
	for batchIdx := startBatchIdx; batchIdx >= 0; batchIdx-- {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		offset := int64(batchIdx) * int64(batchSize)
		limit := int64(batchSize)
		if rem := batchScope.TotalCount - offset; rem < limit {
			limit = rem
		}

		var chunkIDs []int64
		cRows, err := session.Preparer().QueryContext(ctx, `
			SELECT item_id
			FROM wbd_auth.batch_scope_items
			WHERE execution_id = $1::uuid
			ORDER BY item_id ASC
			OFFSET $2 LIMIT $3
		`, scopeExecID, offset, limit)
		if err != nil {
			return nil, fmt.Errorf("read chunk items for batch %d: %w", batchIdx, err)
		}
		for cRows.Next() {
			var idVal int64
			if err := cRows.Scan(&idVal); err != nil {
				cRows.Close()
				return nil, fmt.Errorf("scan chunk item: %w", err)
			}
			chunkIDs = append(chunkIDs, idVal)
		}
		cRows.Close()

		if int64(len(chunkIDs)) != limit {
			return s.unknownOutcome(ctx, claim, fmt.Errorf("chunk %d expected %d items, got %d", batchIdx, limit, len(chunkIDs)))
		}

		earlierReverted := (batchesRevertedCount > 0)

		if deps.beforeBatchDispatch != nil {
			if err := deps.beforeBatchDispatch(ctx, batchIdx); err != nil {
				if earlierReverted {
					_ = s.failPartiallyRevertedJob(ctx, claim, "DISPATCH_INTERRUPTED", err.Error())
					return &ExecutionOutcome{Status: executioncontract.PartiallyReverted, Error: err}, nil
				}
				_ = s.failBlockedBatchJob(ctx, claim, "DISPATCH_INTERRUPTED", err.Error())
				return &ExecutionOutcome{Status: executioncontract.Failed, Error: err}, nil
			}
		}

		if err := s.checkBatchLeaseAndTargetBlock(ctx, claim); err != nil {
			var cErr executioncontract.ContractError
			if errors.As(err, &cErr) && cErr.Code == "LEASE_MISMATCH" {
				return nil, err
			}
			if earlierReverted {
				_ = s.failPartiallyRevertedJob(ctx, claim, "TARGET_BLOCKED", err.Error())
				return &ExecutionOutcome{Status: executioncontract.PartiallyReverted, Error: err}, nil
			}
			return nil, err
		}

		chunkPlan, err := deps.Gate.Revalidate(ctx, claim.RevalidationRequest())
		if err != nil {
			var cErr executioncontract.ContractError
			if errors.As(err, &cErr) && cErr.Code == "REVIEW_STALE" {
				if earlierReverted {
					_ = s.failPartiallyRevertedJob(ctx, claim, "REVIEW_STALE", err.Error())
					return &ExecutionOutcome{Status: executioncontract.PartiallyReverted, Error: err}, nil
				}
				return nil, fmt.Errorf("REVIEW_STALE requires WorkflowGate.ReturnToReviewTx and atomic job finalization: %w", err)
			}
			if earlierReverted {
				_ = s.failPartiallyRevertedJob(ctx, claim, "GATE_DENIED", err.Error())
				return &ExecutionOutcome{Status: executioncontract.PartiallyReverted, Error: err}, nil
			}
			return nil, fmt.Errorf("workflow revalidation failed for down batch %d: %w", batchIdx, err)
		}

		if !reflect.DeepEqual(chunkPlan.Request, claim.RevalidationRequest()) ||
			chunkPlan.Version != approvedPlanVersion ||
			validateAgreedBatchConfig(chunkPlan.Snapshot.Batch) != nil {
			err := executioncontract.ContractError{Code: "PLAN_MISMATCH", Message: "workflow plan changed"}
			if earlierReverted {
				_ = s.failPartiallyRevertedJob(ctx, claim, "PLAN_MISMATCH", err.Message)
				return &ExecutionOutcome{Status: executioncontract.PartiallyReverted, Error: err}, nil
			}
			return nil, err
		}
		chunkSQL, err := selectSQL(chunkPlan, claim.Action, claim.Direction)
		if err != nil || chunkSQL != approvedSQLText || validateAgreedBatchSQL(claim.Direction, chunkSQL) != nil {
			err := executioncontract.ContractError{Code: "SQL_MISMATCH", Message: "workflow plan SQL changed"}
			if earlierReverted {
				_ = s.failPartiallyRevertedJob(ctx, claim, "SQL_MISMATCH", err.Message)
				return &ExecutionOutcome{Status: executioncontract.PartiallyReverted, Error: err}, nil
			}
			return nil, err
		}

		if err := session.Begin(ctx, deps.Timeout); err != nil {
			return nil, fmt.Errorf("begin target tx for down batch %d: %w", batchIdx, err)
		}

		var xid string
		if err := session.Tx().QueryRowContext(ctx, `SELECT pg_catalog.pg_current_xact_id()::text`).Scan(&xid); err != nil {
			return s.rollbackAndFailBatch(ctx, claim, session, "", batchIdx, 0, earlierReverted, "TARGET_PREPARATION_FAILED", err, deps)
		}

		if err := migration.PreflightBatch(ctx, session.Tx(), "managed", "batch_items"); err != nil {
			code := "SQL_UNSUPPORTED"
			var apiErr *migration.APIError
			if errors.As(err, &apiErr) {
				code = apiErr.Code
			}
			return s.rollbackAndFailBatch(ctx, claim, session, xid, batchIdx, 0, earlierReverted, code, err, deps)
		}

		lRows, err := session.Tx().QueryContext(ctx, `
			SELECT b.id, b.processed, b.xmin::text, b.ctid::text,
			       v.row_xmin, v.row_ctid
			FROM ONLY managed.batch_items b
			LEFT JOIN wbd_auth.batch_row_versions v
			  ON v.execution_id = $1::uuid
			 AND v.batch_index = $2
			 AND v.item_id = b.id
			WHERE b.id = ANY($3::bigint[])
			FOR UPDATE OF b
		`, scopeExecID, batchIdx, chunkIDs)
		if err != nil {
			return s.rollbackAndFailBatch(ctx, claim, session, xid, batchIdx, 0, earlierReverted, "TARGET_PREPARATION_FAILED", err, deps)
		}

		seenIDs := make(map[int64]bool, len(chunkIDs))
		var unsafeRow bool
		for lRows.Next() {
			var idVal int64
			var procVal bool
			var currXmin, currCtid string
			var origXmin, origCtid sql.NullString
			if err := lRows.Scan(&idVal, &procVal, &currXmin, &currCtid, &origXmin, &origCtid); err != nil {
				lRows.Close()
				return s.rollbackAndFailBatch(ctx, claim, session, xid, batchIdx, 0, earlierReverted, "TARGET_PREPARATION_FAILED", err, deps)
			}
			seenIDs[idVal] = true
			if !procVal {
				unsafeRow = true
			}
			if !origXmin.Valid || !origCtid.Valid || origXmin.String == "" || origCtid.String == "" {
				unsafeRow = true
			} else if currXmin != origXmin.String || currCtid != origCtid.String {
				unsafeRow = true
			}
		}
		lRows.Close()

		if len(seenIDs) != len(chunkIDs) || unsafeRow {
			unsafeErr := executioncontract.ContractError{
				Code:    "BATCH_UNSAFE",
				Message: "external row modification or deletion detected in batch items",
			}
			return s.rollbackAndFailBatch(ctx, claim, session, xid, batchIdx, 0, earlierReverted, "BATCH_UNSAFE", unsafeErr, deps)
		}

		capability := make([]byte, 32)
		if _, err := rand.Read(capability); err != nil {
			return s.rollbackAndFailBatch(ctx, claim, session, xid, batchIdx, 0, earlierReverted, "TARGET_PREPARATION_FAILED", err, deps)
		}

		if err := s.reserveBatchTicket(ctx, session, scopeExecID, claim.TargetID, xid, batchIdx, executioncontract.Down, capability); err != nil {
			clear(capability)
			return s.rollbackAndFailBatch(ctx, claim, session, xid, batchIdx, 0, earlierReverted, "TARGET_PREPARATION_FAILED", err, deps)
		}

		ordinal := batchesRevertedCount + 1
		if err := s.authorizeBatchDispatch(ctx, claim, scopeExecID, chunkPlan, approvedSQLText, xid, version, ordinal, batchIdx, chunkIDs); err != nil {
			clear(capability)
			_ = session.Rollback()
			var cErr executioncontract.ContractError
			if errors.As(err, &cErr) && cErr.Code == "LEASE_MISMATCH" {
				return nil, err
			}
			return nil, err
		}

		res, execErr := session.Tx().ExecContext(ctx, approvedSQLText, chunkIDs)
		if execErr != nil {
			clear(capability)
			return s.rollbackAndFailBatch(ctx, claim, session, xid, batchIdx, ordinal, earlierReverted, "SQL_FAILED", execErr, deps)
		}
		ra, err := res.RowsAffected()
		if err != nil || ra != int64(len(chunkIDs)) {
			clear(capability)
			return s.rollbackAndFailBatch(ctx, claim, session, xid, batchIdx, ordinal, earlierReverted, "BATCH_UNSAFE", errors.New("row count mismatch"), deps)
		}

		if deps.afterBatchUpdate != nil {
			if err := deps.afterBatchUpdate(ctx, batchIdx); err != nil {
				clear(capability)
				return s.rollbackAndFailBatch(ctx, claim, session, xid, batchIdx, ordinal, earlierReverted, "UPDATE_HOOK_FAILED", err, deps)
			}
		}

		_, err = session.Tx().ExecContext(ctx, `
			SELECT wbd_auth.record_checkpoint($1::uuid, $2, $3, $4, $5::bytea)
		`, scopeExecID, batchIdx, string(claim.Direction), ra, capability)
		clear(capability)
		if err != nil {
			return s.rollbackAndFailBatch(ctx, claim, session, xid, batchIdx, ordinal, earlierReverted, "RECEIPT_FAILED", fmt.Errorf("record checkpoint: %w", err), deps)
		}

		if err := session.Commit(); err != nil {
			return s.unknownOutcome(ctx, claim, fmt.Errorf("target commit for down batch %d: %w", batchIdx, err))
		}

		var recordedAt time.Time
		var chkXID string
		var chkRowsAffected int64
		err = session.Preparer().QueryRowContext(ctx, `
			SELECT recorded_at, target_xid::text, rows_affected
			FROM wbd_auth.batch_checkpoints
			WHERE execution_id = $1::uuid AND direction = $2 AND batch_index = $3
		`, scopeExecID, string(claim.Direction), batchIdx).Scan(&recordedAt, &chkXID, &chkRowsAffected)
		if err != nil || chkXID != xid || chkRowsAffected != ra {
			return s.unknownOutcome(ctx, claim, errors.New("unconfirmed or mismatched batch checkpoint after target commit"))
		}

		committedAt, err := queryBatchCommitTimestamp(ctx, session, scopeExecID, string(claim.Direction), batchIdx, xid)
		if err != nil {
			return s.unknownOutcome(ctx, claim, fmt.Errorf("query truthful commit timestamp: %w", err))
		}

		batchesRevertedCount++
		rowsRevertedCount += ra

		if deps.afterBatchCommit != nil {
			if err := deps.afterBatchCommit(ctx, batchIdx); err != nil {
				return s.unknownOutcome(ctx, claim, err)
			}
		}
		if deps.crashAfterBatchCommit != nil {
			if crashErr := deps.crashAfterBatchCommit(ctx, batchIdx); crashErr != nil {
				return nil, crashErr
			}
		}

		prog := executioncontract.Progress{
			Direction:        claim.Direction,
			BatchesCommitted: strconv.Itoa(N_up),
			RowsCommitted:    strconv.FormatInt(totalCommittedUpRows, 10),
			BatchesReverted:  strconv.Itoa(batchesRevertedCount),
			RowsReverted:     strconv.FormatInt(rowsRevertedCount, 10),
			TotalRows:        &totalRowsStr,
		}
		progJSON, _ := json.Marshal(prog)

		startIDStr := strconv.FormatInt(chunkIDs[0], 10)
		endIDStr := strconv.FormatInt(chunkIDs[len(chunkIDs)-1], 10)
		isFinal := (batchIdx == 0)
		evidence := executioncontract.CommitEvidence{
			JobID:             claim.JobID,
			ExecutionID:       claim.ExecutionID,
			ProposalID:        claim.ProposalID,
			MigrationID:       claim.MigrationID,
			ActorID:           claim.ActorID,
			DatabaseID:        claim.DatabaseID,
			TargetID:          claim.TargetID,
			Revision:          claim.Revision,
			Version:           plan.Version,
			Action:            claim.Action,
			Direction:         claim.Direction,
			SQLSnapshot:       sqlText,
			SafeParameters:    map[string]any{"ids": chunkIDs},
			SnapshotHash:      claim.SnapshotHash,
			Receipt:           claim.ExecutionID,
			CommittedAt:       committedAt,
			RowCount:          strconv.FormatInt(ra, 10),
			SourceExecutionID: claim.SourceExecutionID,
			Batch: &executioncontract.BatchCommit{
				JobID:          claim.JobID,
				ExecutedBy:     executioncontract.Actor{ID: claim.ActorID},
				RangeStart:     &startIDStr,
				RangeEnd:       &endIDStr,
				SQLSnapshot:    sqlText,
				SafeParameters: map[string]any{"ids": chunkIDs},
				BatchIndex:     strconv.Itoa(batchIdx),
				RowCount:       strconv.FormatInt(ra, 10),
				CommittedAt:    committedAt,
				Receipt:        claim.ExecutionID,
			},
			FinalStep: isFinal,
		}

		platErr := func() error {
			ptx, err := s.db.BeginTx(ctx, nil)
			if err != nil {
				return fmt.Errorf("begin platform batch commit tx: %w", err)
			}
			defer ptx.Rollback()

			res, err := ptx.ExecContext(ctx, `
				UPDATE app.job_statement_evidence
				SET outcome = 'succeeded',
				    row_count = $1,
				    executed_at = clock_timestamp()
				WHERE attempt_id = $2::uuid AND ordinal = $3 AND batch_index = $4
			`, ra, claim.AttemptID, ordinal, batchIdx)
			if err != nil {
				return fmt.Errorf("update statement evidence: %w", err)
			}
			if err := requireOne(res, err, "update statement evidence"); err != nil {
				return err
			}

			var bsaID string
			err = ptx.QueryRowContext(ctx, `
				SELECT id::text FROM app.job_batch_step_authorizations
				WHERE job_id = $1 AND attempt_id = $2 AND batch_index = $3 AND direction = $4
			`, claim.JobID, claim.AttemptID, batchIdx, string(claim.Direction)).Scan(&bsaID)
			if err != nil {
				return fmt.Errorf("find step auth for batch proof: %w", err)
			}

			_, err = ptx.ExecContext(ctx, `
				INSERT INTO app.job_batch_commit_proofs
					(step_auth_id, job_id, attempt_id, scope_execution_id, physical_target_id, direction, batch_index, target_xid, row_count, committed_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
				ON CONFLICT (scope_execution_id, direction, batch_index) DO NOTHING
			`, bsaID, claim.JobID, claim.AttemptID, scopeExecID, claim.TargetID, string(claim.Direction), batchIdx, xid, ra, committedAt)
			if err != nil {
				return fmt.Errorf("record batch commit proof: %w", err)
			}

			if err := deps.Gate.RecordCommitTx(ctx, ptx, evidence); err != nil {
				return fmt.Errorf("gate record commit for down batch %d: %w", batchIdx, err)
			}

			if isFinal {
				if err := s.completeBatchSuccessTx(ctx, ptx, claim, scopeExecID, progJSON); err != nil {
					return err
				}
				if err := s.releaseVerifiedBatchDownLineageBlockersTx(ctx, ptx, scopeExecID, claim.TargetID, claim.DatabaseID, claim.MigrationID, claim.Revision, claim.SnapshotHash, claim.JobID); err != nil {
					return fmt.Errorf("release verified down lineage blockers: %w", err)
				}
			} else {
				res, err := ptx.ExecContext(ctx, `
					UPDATE app.jobs
					SET progress = $1::jsonb,
					    updated_at = clock_timestamp()
					WHERE id = $2 AND attempt_no = $3
					  AND status = 'running' AND execution_status = 'running'
					  AND lease_token = $4::uuid
					  AND lease_expires_at > clock_timestamp()
				`, string(progJSON), claim.JobID, claim.AttemptNo, claim.LeaseToken)
				if err != nil || requireOne(res, err, "update down batch progress") != nil {
					return fmt.Errorf("update down batch progress: %w", err)
				}
			}

			return ptx.Commit()
		}()
		if platErr != nil {
			return s.handleClosureTxError(ctx, claim, platErr)
		}
	}

	return &ExecutionOutcome{Status: executioncontract.Succeeded, Receipt: claim.ExecutionID}, nil
}

func (s *Service) reconcileBatchJobDown(ctx context.Context, claim *Claim, deps ExecutionDeps) (*ExecutionOutcome, error) {
	lease, err := deps.Acquire(ctx, claim.DatabaseID)
	if err != nil {
		discardLease(lease)
		return nil, fmt.Errorf("acquire target lease; outcome unconfirmed: %w", err)
	}
	session, err := migration.OpenTargetSession(ctx, lease, claim.TargetID, deps.Timeout)
	if err != nil {
		return nil, fmt.Errorf("open target session; outcome unconfirmed: %w", err)
	}
	defer session.Close(deps.Timeout)
	if err := session.VerifyExecutorTarget(ctx, claim.TargetID); err != nil {
		return nil, fmt.Errorf("verify executor target; outcome unconfirmed: %w", err)
	}

	var commitTimeAvailable bool
	err = session.Preparer().QueryRowContext(ctx, `SELECT current_setting('track_commit_timestamp') = 'on'
		AND pg_catalog.has_function_privilege(current_user, 'pg_catalog.pg_xact_commit_timestamp(xid)', 'EXECUTE')
		AND pg_catalog.has_function_privilege(current_user, 'pg_catalog.pg_xact_status(xid8)', 'EXECUTE')`).Scan(&commitTimeAvailable)
	if err != nil || !commitTimeAvailable {
		return s.failedBlockedOutcome(ctx, claim, "COMMIT_TIME_UNAVAILABLE",
			errors.Join(errors.New("exact target commit timestamp evidence is not supported"), err))
	}

	var snapshotBytes []byte
	err = s.db.QueryRowContext(ctx, `
		SELECT snapshot
		FROM app.jobs
		WHERE id = $1
	`, claim.JobID).Scan(&snapshotBytes)
	if err != nil {
		return nil, fmt.Errorf("read job snapshot for recovery: %w", err)
	}

	var draft executioncontract.Draft
	if err := json.Unmarshal(snapshotBytes, &draft); err != nil {
		return s.failedBlockedOutcome(ctx, claim, "INVALID_BATCH_CONFIG", fmt.Errorf("decode snapshot: %w", err))
	}
	if draft.Batch == nil {
		return s.failedBlockedOutcome(ctx, claim, "INVALID_BATCH_CONFIG", errors.New("draft snapshot missing batch configuration"))
	}
	if err := validateAgreedBatchConfig(draft.Batch); err != nil {
		return s.failedBlockedOutcome(ctx, claim, "INVALID_BATCH_CONFIG", err)
	}
	batchSize := draft.Batch.BatchSize

	scopeExecID, err := s.batchDownScopeExecution(ctx, claim)
	if err != nil {
		return s.failedBlockedOutcome(ctx, claim, "SOURCE_JOB_NOT_FOUND", err)
	}
	var (
		srcTargetID      string
		srcDatabaseID    string
		srcMigrationID   string
		srcRevision      int
		srcSnapshotHash  string
		srcDirection     string
		srcMode          string
		srcStatus        string
		srcSnapshotBytes []byte
	)
	err = s.db.QueryRowContext(ctx, `
		SELECT target_id::text, database_id::text, migration_id::text, revision,
		       snapshot_hash, direction, mode, status, snapshot
		FROM app.jobs
		WHERE execution_id = $1::uuid AND database_id = $2::uuid
		ORDER BY created_at DESC
		LIMIT 1
	`, scopeExecID, claim.DatabaseID).Scan(
		&srcTargetID, &srcDatabaseID, &srcMigrationID, &srcRevision,
		&srcSnapshotHash, &srcDirection, &srcMode, &srcStatus, &srcSnapshotBytes,
	)
	if err != nil {
		return s.failedBlockedOutcome(ctx, claim, "SOURCE_JOB_NOT_FOUND", fmt.Errorf("source execution job: %w", err))
	}
	if srcTargetID != claim.TargetID {
		return s.failedBlockedOutcome(ctx, claim, "TARGET_MISMATCH", errors.New("compensation target does not match source execution"))
	}
	if srcDatabaseID != claim.DatabaseID {
		return s.failedBlockedOutcome(ctx, claim, "DATABASE_MISMATCH", errors.New("compensation database does not match source execution"))
	}
	if srcMigrationID != claim.MigrationID {
		return s.failedBlockedOutcome(ctx, claim, "MIGRATION_MISMATCH", errors.New("compensation migration does not match source execution"))
	}
	if srcRevision != claim.Revision {
		return s.failedBlockedOutcome(ctx, claim, "REVISION_MISMATCH", errors.New("compensation revision does not match source execution"))
	}
	if srcSnapshotHash != claim.SnapshotHash {
		return s.failedBlockedOutcome(ctx, claim, "SNAPSHOT_HASH_MISMATCH", errors.New("compensation snapshot hash does not match source execution"))
	}
	if srcDirection != "up" {
		return s.failedBlockedOutcome(ctx, claim, "DIRECTION_MISMATCH", errors.New("source execution direction must be up"))
	}
	if srcMode != "batch" {
		return s.failedBlockedOutcome(ctx, claim, "UNSUPPORTED_MODE", errors.New("source execution is not a batch job"))
	}
	if srcStatus != "succeeded" && srcStatus != "partial" && srcStatus != "partially_reverted" {
		return s.failedBlockedOutcome(ctx, claim, "STATUS_CONFLICT", errors.New("source execution is not in an eligible committed state"))
	}

	var srcSnapshot executioncontract.Draft
	if err := json.Unmarshal(srcSnapshotBytes, &srcSnapshot); err != nil {
		return s.failedBlockedOutcome(ctx, claim, "INVALID_BATCH_CONFIG", fmt.Errorf("decode source snapshot: %w", err))
	}
	if srcSnapshot.Batch == nil || srcSnapshot.Batch.BatchSize != batchSize ||
		srcSnapshot.Batch.Schema != draft.Batch.Schema ||
		srcSnapshot.Batch.Table != draft.Batch.Table ||
		srcSnapshot.Batch.CursorColumn != draft.Batch.CursorColumn {
		return s.failedBlockedOutcome(ctx, claim, "BATCH_CONFIG_MISMATCH",
			errors.New("compensation batch configuration does not match source execution"))
	}

	ident := BatchScopeIdentity{
		ExecutionID: scopeExecID,
		TargetID:    claim.TargetID,
		DatabaseID:  claim.DatabaseID,
		ActorID:     claim.ActorID,
	}
	batchScope, err := s.GetBatchScope(ctx, session.Preparer(), ident)
	if err != nil {
		return s.failedBlockedOutcome(ctx, claim, "SCOPE_UNAVAILABLE", fmt.Errorf("get source batch scope: %w", err))
	}

	type upChk struct {
		index int
		rows  int64
		xid   string
	}
	var committedUpBatches []upChk
	var totalCommittedUpRows int64

	rows, err := session.Preparer().QueryContext(ctx, `
		SELECT batch_index, rows_affected, target_xid::text
		FROM wbd_auth.batch_checkpoints
		WHERE execution_id = $1::uuid AND direction = 'up'
		ORDER BY batch_index ASC
	`, scopeExecID)
	if err != nil {
		return nil, fmt.Errorf("query committed up checkpoints: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var uc upChk
		if err := rows.Scan(&uc.index, &uc.rows, &uc.xid); err != nil {
			return nil, fmt.Errorf("scan up checkpoint: %w", err)
		}
		committedUpBatches = append(committedUpBatches, uc)
	}
	rows.Close()

	N_up := len(committedUpBatches)
	if N_up == 0 {
		return s.failedBlockedOutcome(ctx, claim, "NO_COMMITTED_UP_BATCHES",
			errors.New("cannot compensate: no committed up checkpoints proved for source execution"))
	}

	for i, uc := range committedUpBatches {
		if uc.index != i {
			return s.failedBlockedOutcome(ctx, claim, "TARGET_BLOCKED",
				fmt.Errorf("non-consecutive up checkpoints: expected %d, got %d", i, uc.index))
		}
		var xactStatus string
		if err := session.Preparer().QueryRowContext(ctx, `SELECT pg_catalog.pg_xact_status($1::xid8)`, uc.xid).Scan(&xactStatus); err != nil || xactStatus != "committed" {
			return s.unknownOutcome(ctx, claim, fmt.Errorf("up checkpoint %d target xid %s status %s", uc.index, uc.xid, xactStatus))
		}

		var upProofExists bool
		err = s.db.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM app.job_batch_step_authorizations bsa
				JOIN app.job_batch_commit_proofs p ON p.step_auth_id = bsa.id
				  AND p.target_xid = bsa.target_xid
				  AND p.physical_target_id = bsa.physical_target_id
				  AND p.direction = bsa.direction
				  AND p.scope_execution_id = bsa.scope_execution_id
				WHERE bsa.scope_execution_id = $1::uuid
				  AND bsa.physical_target_id = $2::uuid
				  AND bsa.direction = 'up'
				  AND bsa.batch_index = $3
				  AND bsa.target_xid = $4
			)
		`, scopeExecID, claim.TargetID, uc.index, uc.xid).Scan(&upProofExists)
		if err != nil || !upProofExists {
			return s.failedBlockedOutcome(ctx, claim, "TARGET_BLOCKED",
				fmt.Errorf("up checkpoint %d target xid %s lacks matching platform authorization/commit proof", uc.index, uc.xid))
		}
		totalCommittedUpRows += uc.rows
	}

	type downChk struct {
		index int
		rows  int64
		xid   string
	}
	var revertedDownBatches []downChk
	var rowsRevertedCount int64

	dRows, err := session.Preparer().QueryContext(ctx, `
		SELECT batch_index, rows_affected, target_xid::text
		FROM wbd_auth.batch_checkpoints
		WHERE execution_id = $1::uuid AND direction = 'down'
		ORDER BY batch_index DESC
	`, scopeExecID)
	if err != nil {
		return nil, fmt.Errorf("query down checkpoints: %w", err)
	}
	defer dRows.Close()
	for dRows.Next() {
		var dc downChk
		if err := dRows.Scan(&dc.index, &dc.rows, &dc.xid); err != nil {
			return nil, fmt.Errorf("scan down checkpoint: %w", err)
		}
		revertedDownBatches = append(revertedDownBatches, dc)
	}
	dRows.Close()

	for i, dc := range revertedDownBatches {
		expectedIndex := N_up - 1 - i
		if dc.index != expectedIndex {
			return s.failedBlockedOutcome(ctx, claim, "TARGET_BLOCKED",
				fmt.Errorf("non-reverse down checkpoint: expected %d, got %d", expectedIndex, dc.index))
		}
		var xactStatus string
		if err := session.Preparer().QueryRowContext(ctx, `SELECT pg_catalog.pg_xact_status($1::xid8)`, dc.xid).Scan(&xactStatus); err != nil || xactStatus != "committed" {
			return s.unknownOutcome(ctx, claim, fmt.Errorf("down checkpoint %d target xid %s status %s", dc.index, dc.xid, xactStatus))
		}
		rowsRevertedCount += dc.rows

		committedAt, err := queryBatchCommitTimestamp(ctx, session, scopeExecID, "down", dc.index, dc.xid)
		if err != nil || committedAt.IsZero() {
			return s.failedBlockedOutcome(ctx, claim, "TARGET_BLOCKED",
				fmt.Errorf("down checkpoint %d truthful commit timestamp unavailable: %w", dc.index, err))
		}

		var bsaID, bsaJobID, bsaAttemptID, bsaActorID, bsaOrdinal string
		var bsaPlanJSON, bsaChunkIDsJSON, bsaSQLText, bsaTargetVersion string
		var bsaSQLSha256 []byte
		var origJobID, origExecID, origProposalID, origMigrationID, origActorID, origAction, origDirection, origSnapHash string
		var origRevision int
		var origSourceExecID sql.NullString
		err = s.db.QueryRowContext(ctx, `
			SELECT bsa.id::text, bsa.job_id::text, bsa.attempt_id::text, bsa.actor_id::text, bsa.ordinal::text,
			       bsa.plan::text, bsa.chunk_ids::text, bsa.sql_text, bsa.sql_sha256, bsa.target_version::text,
			       j.id::text, j.execution_id::text, j.proposal_id::text, j.migration_id::text,
			       j.actor_id::text, j.action, j.direction, j.snapshot_hash, j.revision, j.source_execution_id::text
			FROM app.job_batch_step_authorizations bsa
			JOIN app.jobs j ON j.id = bsa.job_id
			WHERE bsa.scope_execution_id = $1::uuid
			  AND bsa.physical_target_id = $2::uuid
			  AND bsa.direction = 'down'
			  AND bsa.batch_index = $3
			  AND bsa.target_xid = $4
		`, scopeExecID, claim.TargetID, dc.index, dc.xid).Scan(
			&bsaID, &bsaJobID, &bsaAttemptID, &bsaActorID, &bsaOrdinal,
			&bsaPlanJSON, &bsaChunkIDsJSON, &bsaSQLText, &bsaSQLSha256, &bsaTargetVersion,
			&origJobID, &origExecID, &origProposalID, &origMigrationID, &origActorID, &origAction, &origDirection, &origSnapHash, &origRevision, &origSourceExecID,
		)
		if err != nil {
			return s.failedBlockedOutcome(ctx, claim, "TARGET_BLOCKED",
				fmt.Errorf("match authorized down step for batch %d: %w", dc.index, err))
		}

		offset := int64(dc.index) * int64(batchSize)
		limit := int64(batchSize)
		if rem := batchScope.TotalCount - offset; rem < limit {
			limit = rem
		}
		var targetChunkIDs []int64
		cRows, err := session.Preparer().QueryContext(ctx, `
			SELECT item_id
			FROM wbd_auth.batch_scope_items
			WHERE execution_id = $1::uuid
			ORDER BY item_id ASC
			OFFSET $2 LIMIT $3
		`, scopeExecID, offset, limit)
		if err != nil {
			return nil, fmt.Errorf("query chunk items for down batch %d: %w", dc.index, err)
		}
		for cRows.Next() {
			var idVal int64
			if err := cRows.Scan(&idVal); err != nil {
				cRows.Close()
				return nil, fmt.Errorf("scan chunk item: %w", err)
			}
			targetChunkIDs = append(targetChunkIDs, idVal)
		}
		cRows.Close()

		var stepChunkIDs []int64
		if err := json.Unmarshal([]byte(bsaChunkIDsJSON), &stepChunkIDs); err != nil || len(stepChunkIDs) != len(targetChunkIDs) {
			return s.failedBlockedOutcome(ctx, claim, "TARGET_BLOCKED", errors.New("step chunk ids invalid or length mismatch"))
		}
		for idx := range targetChunkIDs {
			if stepChunkIDs[idx] != targetChunkIDs[idx] {
				return s.failedBlockedOutcome(ctx, claim, "TARGET_BLOCKED", errors.New("step chunk id mismatch"))
			}
		}

		var evOutcome string
		var evRowCount sql.NullInt64
		err = s.db.QueryRowContext(ctx, `
			SELECT outcome, row_count
			FROM app.job_statement_evidence
			WHERE attempt_id = $1::uuid AND batch_index = $2
		`, bsaAttemptID, dc.index).Scan(&evOutcome, &evRowCount)
		if err != nil {
			return s.failedBlockedOutcome(ctx, claim, "TARGET_BLOCKED",
				fmt.Errorf("statement evidence for down batch %d: %w", dc.index, err))
		}

		var proofExists bool
		err = s.db.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM app.job_batch_commit_proofs
				WHERE scope_execution_id = $1::uuid AND direction = 'down' AND batch_index = $2
			)
		`, scopeExecID, dc.index).Scan(&proofExists)
		if err != nil {
			return s.unknownOutcome(ctx, claim, fmt.Errorf("check down commit proof: %w", err))
		}

		if !proofExists {
			startIDStr := strconv.FormatInt(stepChunkIDs[0], 10)
			endIDStr := strconv.FormatInt(stepChunkIDs[len(stepChunkIDs)-1], 10)
			var sExecID *string
			if origSourceExecID.Valid && origSourceExecID.String != "" {
				sVal := origSourceExecID.String
				sExecID = &sVal
			}
			isFinalStep := (dc.index == 0)
			evidence := executioncontract.CommitEvidence{
				JobID:             origJobID,
				ExecutionID:       origExecID,
				ProposalID:        origProposalID,
				MigrationID:       origMigrationID,
				ActorID:           origActorID,
				DatabaseID:        claim.DatabaseID,
				TargetID:          claim.TargetID,
				Revision:          origRevision,
				Version:           bsaTargetVersion,
				Action:            executioncontract.Action(origAction),
				Direction:         executioncontract.Down,
				SQLSnapshot:       bsaSQLText,
				SafeParameters:    map[string]any{"ids": stepChunkIDs},
				SnapshotHash:      origSnapHash,
				Receipt:           origExecID,
				CommittedAt:       committedAt,
				RowCount:          strconv.FormatInt(dc.rows, 10),
				SourceExecutionID: sExecID,
				Batch: &executioncontract.BatchCommit{
					JobID:          origJobID,
					ExecutedBy:     executioncontract.Actor{ID: origActorID},
					RangeStart:     &startIDStr,
					RangeEnd:       &endIDStr,
					SQLSnapshot:    bsaSQLText,
					SafeParameters: map[string]any{"ids": stepChunkIDs},
					BatchIndex:     strconv.Itoa(dc.index),
					RowCount:       strconv.FormatInt(dc.rows, 10),
					CommittedAt:    committedAt,
					Receipt:        origExecID,
				},
				FinalStep: isFinalStep,
			}

			reconErr := func() error {
				ptx, err := s.db.BeginTx(ctx, nil)
				if err != nil {
					return fmt.Errorf("begin platform tx for down reconcile: %w", err)
				}
				defer ptx.Rollback()

				res, err := ptx.ExecContext(ctx, `
					UPDATE app.job_statement_evidence
					SET outcome = 'succeeded',
					    row_count = $1,
					    executed_at = clock_timestamp()
					WHERE attempt_id = $2::uuid AND batch_index = $3
				`, dc.rows, bsaAttemptID, dc.index)
				if err != nil {
					return fmt.Errorf("reconcile down statement evidence: %w", err)
				}
				if err := requireOne(res, err, "reconcile down statement evidence"); err != nil {
					return err
				}

				_, err = ptx.ExecContext(ctx, `
					INSERT INTO app.job_batch_commit_proofs
						(step_auth_id, job_id, attempt_id, scope_execution_id, physical_target_id, direction, batch_index, target_xid, row_count, committed_at)
					VALUES ($1, $2, $3, $4, $5, 'down', $6, $7, $8, $9)
					ON CONFLICT (scope_execution_id, direction, batch_index) DO NOTHING
				`, bsaID, origJobID, bsaAttemptID, scopeExecID, claim.TargetID, dc.index, dc.xid, dc.rows, committedAt)
				if err != nil {
					return fmt.Errorf("record down commit proof: %w", err)
				}

				if err := deps.Gate.RecordCommitTx(ctx, ptx, evidence); err != nil {
					return fmt.Errorf("gate record commit for recovered down batch %d: %w", dc.index, err)
				}

				return ptx.Commit()
			}()
			if reconErr != nil {
				return s.failedBlockedOutcome(ctx, claim, "TARGET_BLOCKED", reconErr)
			}
		}
	}

	batchesRevertedCount := len(revertedDownBatches)
	totalRowsStr := strconv.FormatInt(batchScope.TotalCount, 10)
	prog := executioncontract.Progress{
		Direction:        claim.Direction,
		BatchesCommitted: strconv.Itoa(N_up),
		RowsCommitted:    strconv.FormatInt(totalCommittedUpRows, 10),
		BatchesReverted:  strconv.Itoa(batchesRevertedCount),
		RowsReverted:     strconv.FormatInt(rowsRevertedCount, 10),
		TotalRows:        &totalRowsStr,
	}
	progJSON, _ := json.Marshal(prog)

	if batchesRevertedCount >= N_up {
		finalizeErr := func() error {
			ptx, err := s.db.BeginTx(ctx, nil)
			if err != nil {
				return fmt.Errorf("begin finalization tx: %w", err)
			}
			defer ptx.Rollback()

			_, err = ptx.ExecContext(ctx, `
				UPDATE app.jobs
				SET status = 'succeeded',
				    execution_status = 'succeeded',
				    blocks_database = false,
				    progress = $1::jsonb,
				    error_code = NULL,
				    error_message = NULL,
				    error_retryable = NULL,
				    finished_at = COALESCE(finished_at, clock_timestamp()),
				    updated_at = clock_timestamp()
				WHERE id = $2
			`, string(progJSON), claim.JobID)
			if err != nil {
				return fmt.Errorf("finalize down job: %w", err)
			}

			_, err = ptx.ExecContext(ctx, `
				UPDATE app.job_attempts
				SET status = 'succeeded',
				    error_code = NULL,
				    error_message = NULL,
				    error_retryable = NULL,
				    finished_at = COALESCE(finished_at, clock_timestamp())
				WHERE job_id = $1 AND status IN ('running', 'unknown')
			`, claim.JobID)
			if err != nil {
				return fmt.Errorf("finalize down attempt: %w", err)
			}

			if err := s.releaseVerifiedBatchDownLineageBlockersTx(ctx, ptx, scopeExecID, claim.TargetID, claim.DatabaseID, claim.MigrationID, claim.Revision, claim.SnapshotHash, claim.JobID); err != nil {
				return fmt.Errorf("release verified down lineage blockers: %w", err)
			}

			return ptx.Commit()
		}()
		if finalizeErr != nil {
			return s.handleClosureTxError(ctx, claim, finalizeErr)
		}
		return &ExecutionOutcome{Status: executioncontract.Succeeded, Receipt: claim.ExecutionID, Reconciled: true}, nil
	}

	ptx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin incomplete progress tx: %w", err)
	}
	defer ptx.Rollback()
	_, err = ptx.ExecContext(ctx, `
		UPDATE app.jobs
		SET progress = $1::jsonb,
		    updated_at = clock_timestamp()
		WHERE id = $2
	`, string(progJSON), claim.JobID)
	if err != nil {
		return nil, fmt.Errorf("update incomplete down progress: %w", err)
	}
	if err := ptx.Commit(); err != nil {
		return nil, fmt.Errorf("commit incomplete down progress tx: %w", err)
	}

	return &ExecutionOutcome{
		Status: executioncontract.PartiallyReverted,
		Error: executioncontract.ContractError{
			Code:    "TARGET_BLOCKED",
			Message: "down execution incomplete",
		},
		Reconciled: true,
	}, nil
}
