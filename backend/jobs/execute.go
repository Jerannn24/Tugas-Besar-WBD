package jobs

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/Jerannn24/Tugas-Besar-WBD.git/executioncontract"
	"github.com/Jerannn24/Tugas-Besar-WBD.git/migration"
	"github.com/Jerannn24/Tugas-Besar-WBD.git/sqlpolicy"
)

type ExecutionDeps struct {
	Gate    executioncontract.WorkflowGate
	Acquire func(ctx context.Context, databaseID string) (*migration.TargetLease, error)
	Timeout time.Duration

	beforeDispatch    func(ctx context.Context) error
	afterTargetCommit func(ctx context.Context) error
}

type ExecutionOutcome struct {
	Status     executioncontract.JobStatus `json:"status"`
	Receipt    string                      `json:"receipt,omitempty"`
	Reconciled bool                        `json:"reconciled,omitempty"`
	Error      error                       `json:"-"`
}

func discardLease(lease *migration.TargetLease) {
	if lease == nil {
		return
	}
	if lease.Executor != nil {
		_ = lease.Executor.Raw(func(any) error { return driver.ErrBadConn })
		_ = lease.Executor.Close()
	}
	if lease.Release != nil {
		lease.Release()
	}
}

func selectSQL(plan executioncontract.ExecutionPlan, action executioncontract.Action, direction executioncontract.Direction) (string, error) {
	if !((action == executioncontract.Apply || action == executioncontract.Forward) && direction == executioncontract.Up ||
		action == executioncontract.Rollback && direction == executioncontract.Down) {
		return "", executioncontract.ContractError{Code: "INVALID_DIRECTION", Message: "action and direction do not match"}
	}
	if direction == executioncontract.Down {
		if plan.Snapshot.SQLDown == nil || strings.TrimSpace(*plan.Snapshot.SQLDown) == "" {
			return "", executioncontract.ContractError{
				Code:    "IRREVERSIBLE_MIGRATION",
				Message: "Rollback SQL was not recorded or migration is irreversible",
			}
		}
		return *plan.Snapshot.SQLDown, nil
	}
	return plan.Snapshot.SQLUp, nil
}

func (s *Service) failJob(ctx context.Context, claim *Claim, code string, msg string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin failure update: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `
		UPDATE app.jobs
		SET status = 'failed',
		    execution_status = 'failed',
		    error_code = $1,
		    error_message = $2,
		    error_retryable = false,
		    finished_at = clock_timestamp(),
		    updated_at = clock_timestamp()
		WHERE id = $3 AND status = 'running' AND execution_status = 'running'
		  AND lease_token = $4::uuid AND lease_expires_at > clock_timestamp() AND attempt_no = $5
		  AND NOT blocks_database
		  AND NOT EXISTS (SELECT 1 FROM app.job_dispatch_authorizations WHERE job_id = $3)
		  AND NOT EXISTS (SELECT 1 FROM app.job_attempts WHERE job_id = $3 AND sql_entered)
		  AND execution_id = $6 AND database_id = $7 AND target_id = $8
		  AND proposal_id = $9 AND migration_id = $10 AND revision = $11 AND actor_id = $12
		  AND action = $13 AND direction = $14 AND mode = $15
		  AND source_execution_id IS NOT DISTINCT FROM $16::uuid
		  AND snapshot_hash = $17 AND COALESCE(approval_fingerprint, '') = $18
	`, code, msg, claim.JobID, claim.LeaseToken, claim.AttemptNo,
		claim.ExecutionID, claim.DatabaseID, claim.TargetID, claim.ProposalID, claim.MigrationID,
		claim.Revision, claim.ActorID, claim.Action, claim.Direction, claim.Mode,
		claim.SourceExecutionID, claim.SnapshotHash, claim.ApprovalFingerprint)
	if err := requireOne(res, err, "fail job"); err != nil {
		return err
	}

	res, err = tx.ExecContext(ctx, `
		UPDATE app.job_attempts
		SET status = 'failed',
		    error_code = $1,
		    error_message = $2,
		    error_retryable = false,
		    finished_at = clock_timestamp()
		WHERE id = $3 AND job_id = $4 AND status = 'running'
		  AND EXISTS (SELECT 1 FROM app.jobs WHERE id = $4 AND status = 'failed'
		              AND lease_token = $5::uuid AND attempt_no = $6)
	`, code, msg, claim.AttemptID, claim.JobID, claim.LeaseToken, claim.AttemptNo)
	if err := requireOne(res, err, "fail attempt"); err != nil {
		return err
	}
	return tx.Commit()
}

func requireOne(res sql.Result, err error, operation string) error {
	if err != nil {
		return fmt.Errorf("%s: %w", operation, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s rows affected: %w", operation, err)
	}
	if n != 1 {
		return fmt.Errorf("%s: expected one affected row, got %d", operation, n)
	}
	return nil
}

func (s *Service) markJobUnknown(ctx context.Context, claim *Claim, cause error) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	code := "UNKNOWN_OUTCOME"
	msg := ""
	if cause != nil {
		msg = cause.Error()
	}

	tx, txErr := s.db.BeginTx(ctx, nil)
	if txErr != nil {
		return fmt.Errorf("begin unknown update: %w", txErr)
	}
	defer tx.Rollback()

	res, txErr := tx.ExecContext(ctx, `
		UPDATE app.jobs
		SET status = 'unknown',
		    execution_status = 'unknown',
		    blocks_database = true,
		    error_code = $1,
		    error_message = $2,
		    error_retryable = false,
		    updated_at = clock_timestamp()
		WHERE id = $3 AND status = 'running' AND execution_status = 'running'
		  AND lease_token = $4::uuid AND lease_expires_at > clock_timestamp() AND attempt_no = $5
	`, code, msg, claim.JobID, claim.LeaseToken, claim.AttemptNo)
	if err := requireOne(res, txErr, "mark job unknown"); err != nil {
		return err
	}

	res, txErr = tx.ExecContext(ctx, `
		UPDATE app.job_attempts
		SET status = 'unknown',
		    error_code = $1,
		    error_message = $2,
		    error_retryable = false
		WHERE id = $3 AND job_id = $4 AND status = 'running'
		  AND EXISTS (SELECT 1 FROM app.jobs WHERE id = $4 AND status = 'unknown'
		              AND lease_token = $5::uuid AND attempt_no = $6)
	`, code, msg, claim.AttemptID, claim.JobID, claim.LeaseToken, claim.AttemptNo)
	if err := requireOne(res, txErr, "mark attempt unknown"); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Service) failedOutcome(ctx context.Context, claim *Claim, code string, cause error) (*ExecutionOutcome, error) {
	if err := s.failJob(ctx, claim, code, cause.Error()); err != nil {
		return nil, errors.Join(cause, err)
	}
	return &ExecutionOutcome{Status: executioncontract.Failed, Error: cause}, nil
}

func (s *Service) unknownOutcome(ctx context.Context, claim *Claim, cause error) (*ExecutionOutcome, error) {
	if err := s.markJobUnknown(ctx, claim, cause); err != nil {
		return nil, errors.Join(cause, err)
	}
	return &ExecutionOutcome{Status: executioncontract.Unknown, Error: cause}, nil
}

// rollbackAuthorizedOutcome clears the target block only after a confirmed rollback
// and an atomic, lease-fenced update of the authorized job and attempt.
func (s *Service) rollbackAuthorizedOutcome(ctx context.Context, claim *Claim, session *migration.TargetSession, xid, code string, cause error) (*ExecutionOutcome, error) {
	rollbackErr := session.Rollback()
	if ctx.Err() != nil || rollbackErr != nil {
		return s.unknownOutcome(ctx, claim, errors.Join(cause, ctx.Err(), rollbackErr))
	}
	updateCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(updateCtx, nil)
	if err != nil {
		return nil, errors.Join(cause, fmt.Errorf("begin rolled-back failure update: %w", err))
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(updateCtx, `
		UPDATE app.jobs
		SET status = 'failed', execution_status = 'failed', blocks_database = false,
		    error_code = $1, error_message = $2, error_retryable = false,
		    finished_at = clock_timestamp(), updated_at = clock_timestamp()
		WHERE id = $3 AND status = 'running' AND execution_status = 'running'
		  AND blocks_database AND lease_token = $4::uuid
		  AND lease_expires_at > clock_timestamp() AND attempt_no = $5
		  AND execution_id = $6 AND database_id = $7 AND target_id = $8
		  AND proposal_id = $9 AND migration_id = $10 AND revision = $11 AND actor_id = $12
		  AND action = $13 AND direction = $14 AND mode = $15
		  AND source_execution_id IS NOT DISTINCT FROM $16::uuid
		  AND snapshot_hash = $17 AND COALESCE(approval_fingerprint, '') = $18
		  AND EXISTS (SELECT 1 FROM app.job_dispatch_authorizations d
		              JOIN app.job_attempts a ON a.id = d.attempt_id AND a.job_id = d.job_id
		              WHERE d.job_id = $3 AND d.attempt_id = $19 AND d.target_xid = $20
		                AND a.attempt_no = $5 AND a.status = 'running' AND a.sql_entered)
	`, code, cause.Error(), claim.JobID, claim.LeaseToken, claim.AttemptNo,
		claim.ExecutionID, claim.DatabaseID, claim.TargetID, claim.ProposalID, claim.MigrationID,
		claim.Revision, claim.ActorID, claim.Action, claim.Direction, claim.Mode,
		claim.SourceExecutionID, claim.SnapshotHash, claim.ApprovalFingerprint, claim.AttemptID, xid)
	if err = requireOne(res, err, "fail rolled-back job"); err != nil {
		return nil, errors.Join(cause, err)
	}
	res, err = tx.ExecContext(updateCtx, `
		UPDATE app.job_attempts SET status = 'failed', error_code = $1,
		    error_message = $2, error_retryable = false, finished_at = clock_timestamp()
		WHERE id = $3 AND job_id = $4 AND attempt_no = $5 AND status = 'running' AND sql_entered
		  AND EXISTS (SELECT 1 FROM app.jobs WHERE id = $4 AND status = 'failed'
		              AND lease_token = $6::uuid AND lease_expires_at > clock_timestamp() AND attempt_no = $5)
	`, code, cause.Error(), claim.AttemptID, claim.JobID, claim.AttemptNo, claim.LeaseToken)
	if err = requireOne(res, err, "fail rolled-back attempt"); err != nil {
		return nil, errors.Join(cause, err)
	}
	if err := ctx.Err(); err != nil {
		if rollbackErr := tx.Rollback(); rollbackErr != nil {
			return nil, errors.Join(cause, err, fmt.Errorf("rollback failure update: %w", rollbackErr))
		}
		return s.unknownOutcome(ctx, claim, errors.Join(cause, err))
	}
	if err = tx.Commit(); err != nil {
		return nil, errors.Join(cause, fmt.Errorf("commit rolled-back failure update: %w", err))
	}
	return &ExecutionOutcome{Status: executioncontract.Failed, Error: cause}, nil
}

func (s *Service) authorizeDispatch(ctx context.Context, claim *Claim, plan executioncontract.ExecutionPlan, sqlText, xid string, version int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin dispatch auth tx: %w", err)
	}
	defer tx.Rollback()

	var (
		status     string
		execStatus string
		token      sql.NullString
		unexpired  sql.NullBool
		attemptNo  int
	)

	snapshotJSON, err := json.Marshal(plan.Snapshot)
	if err != nil {
		return fmt.Errorf("encode plan snapshot: %w", err)
	}
	if plan.Snapshot.MigrationID != claim.MigrationID || plan.Snapshot.DatabaseID != claim.DatabaseID ||
		plan.Snapshot.Revision != claim.Revision || plan.Snapshot.Mode != claim.Mode {
		return errors.New("plan snapshot identity mismatch")
	}
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
			  AND (j.blocks_database OR j.status IN ('unknown', 'partial', 'partially_reverted')
			       OR j.execution_status IN ('unknown', 'partial', 'partially_reverted')
			       OR EXISTS (SELECT 1 FROM app.job_attempts a WHERE a.job_id = j.id
			                  AND (a.sql_entered OR EXISTS (
			                      SELECT 1 FROM app.job_statement_evidence se WHERE se.attempt_id = a.id))
			                  AND (a.status = 'running' OR NOT EXISTS (
			                      SELECT 1 FROM app.job_dispatch_authorizations d WHERE d.attempt_id = a.id))))
		)`, claim.TargetID, claim.JobID).Scan(&blocked); err != nil {
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
	_, err = tx.ExecContext(ctx, `
		INSERT INTO app.job_dispatch_authorizations
		    (job_id, attempt_id, plan, sql_text, sql_sha256, target_xid, target_version)
		VALUES ($1, $2, $3::jsonb, $4, $5, $6, $7)
	`, claim.JobID, claim.AttemptID, string(planJSON), sqlText, hash[:], xid, version)
	if err != nil {
		return fmt.Errorf("record dispatch identity: %w", err)
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
	res, err = tx.ExecContext(ctx, `UPDATE app.jobs SET blocks_database = true, updated_at = clock_timestamp()
		WHERE id = $1 AND status = 'running' AND execution_status = 'running'
		  AND lease_token = $2::uuid AND lease_expires_at > clock_timestamp() AND attempt_no = $3`,
		claim.JobID, claim.LeaseToken, claim.AttemptNo)
	if err := requireOne(res, err, "block authorized job"); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit dispatch auth tx: %w", err)
	}
	return nil
}

func (s *Service) finalizeReceipt(ctx context.Context, claim *Claim, r migration.TargetReceipt, session *migration.TargetSession, deps ExecutionDeps) (*ExecutionOutcome, error) {
	var raw []byte
	var sqlText, xid, attemptID string
	var hash []byte
	var version int64
	err := s.db.QueryRowContext(ctx, `
		SELECT d.plan, d.sql_text, d.sql_sha256, d.target_xid, d.target_version, d.attempt_id::text
		FROM app.job_dispatch_authorizations d JOIN app.jobs j ON j.id = d.job_id
		WHERE j.id = $1 AND j.execution_id = $2 AND j.database_id = $3 AND j.target_id = $4
		  AND j.proposal_id = $5 AND j.migration_id = $6 AND j.revision = $7 AND j.actor_id = $8
		  AND j.action = $9 AND j.direction = $10 AND j.mode = $11
		  AND j.source_execution_id IS NOT DISTINCT FROM $12::uuid
		  AND j.snapshot_hash = $13 AND COALESCE(j.approval_fingerprint, '') = $14
	`, claim.JobID, claim.ExecutionID, claim.DatabaseID, claim.TargetID, claim.ProposalID,
		claim.MigrationID, claim.Revision, claim.ActorID, claim.Action, claim.Direction, claim.Mode,
		claim.SourceExecutionID, claim.SnapshotHash, claim.ApprovalFingerprint).
		Scan(&raw, &sqlText, &hash, &xid, &version, &attemptID)
	if err != nil {
		return &ExecutionOutcome{Status: executioncontract.Unknown, Error: fmt.Errorf("authorized identity unavailable: %w", err)}, nil
	}
	var plan executioncontract.ExecutionPlan
	sum := sha256.Sum256([]byte(sqlText))
	if err := json.Unmarshal(raw, &plan); err != nil || !reflect.DeepEqual(plan.Request, claim.RevalidationRequest()) ||
		attemptID != claim.AttemptID || !bytes.Equal(hash, sum[:]) ||
		r.ID != claim.ExecutionID || r.TargetID != claim.TargetID || r.DatabaseID != claim.DatabaseID ||
		r.ActorID != claim.ActorID || r.Version != strconv.FormatInt(version, 10) || r.XID != xid || !bytes.Equal(r.Hash, hash) {
		return &ExecutionOutcome{Status: executioncontract.Unknown, Error: errors.New("authorized receipt identity mismatch")}, nil
	}
	var completed bool
	err = s.db.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM app.jobs j
		JOIN app.job_dispatch_authorizations d ON d.job_id = j.id
		JOIN app.job_attempts a ON a.id = d.attempt_id AND a.job_id = j.id
		JOIN app.job_commit_proofs p ON p.job_id = j.id AND p.attempt_id = a.id AND p.target_xid = d.target_xid
		WHERE j.id = $1 AND j.status = 'succeeded' AND j.execution_status = 'succeeded'
		  AND NOT j.blocks_database AND j.attempt_no = $2 AND a.attempt_no = $2
		  AND a.id = $3 AND a.status = 'succeeded' AND d.target_xid = $4
	)`, claim.JobID, claim.AttemptNo, claim.AttemptID, xid).Scan(&completed)
	if err != nil {
		return nil, fmt.Errorf("read completed job: %w", err)
	}
	if completed {
		return &ExecutionOutcome{Status: executioncontract.Succeeded, Receipt: r.ID, Reconciled: true}, nil
	}
	var committedAt time.Time
	err = s.db.QueryRowContext(ctx, `SELECT committed_at FROM app.job_commit_proofs
		WHERE job_id = $1 AND attempt_id = $2 AND target_xid = $3`, claim.JobID, claim.AttemptID, xid).Scan(&committedAt)
	if errors.Is(err, sql.ErrNoRows) {
		var timestamp sql.NullTime
		err = session.Preparer().QueryRowContext(ctx, `
			SELECT CASE WHEN current_setting('track_commit_timestamp') = 'on'
			  AND pg_catalog.pg_xact_status($1::text::xid8) = 'committed'
			  AND $1::text::numeric <= pg_catalog.pg_current_xact_id()::text::numeric
			  AND pg_catalog.pg_current_xact_id()::text::numeric - $1::text::numeric < 2147483648
			THEN pg_catalog.pg_xact_commit_timestamp((($1::text::numeric % 4294967296)::bigint::text)::xid)
			END
			FROM wbd_auth.execution_receipts
			WHERE execution_id = $2::uuid AND target_xid = $1::text::xid8
			  AND physical_target_id = $3::uuid AND database_id = $4::uuid
			  AND actor_id = $5::uuid AND version = $6 AND sql_sha256 = $7`,
			xid, r.ID, r.TargetID, r.DatabaseID, r.ActorID, version, hash).Scan(&timestamp)
		if err != nil || !timestamp.Valid {
			return nil, errors.Join(errors.New("cannot finalize: target commit timestamp is unavailable"), err)
		}
		var valid bool
		err = session.Preparer().QueryRowContext(ctx, `SELECT $2::timestamptz >= recorded_at
			FROM wbd_auth.execution_receipts WHERE execution_id = $1::uuid AND target_xid = $3::xid8`,
			r.ID, timestamp.Time, xid).Scan(&valid)
		if err != nil || !valid {
			return nil, errors.Join(errors.New("target commit timestamp precedes receipt"), err)
		}
		_, err = s.db.ExecContext(ctx, `INSERT INTO app.job_commit_proofs
			(job_id, attempt_id, target_xid, committed_at) VALUES ($1, $2, $3, $4)
			ON CONFLICT (job_id) DO NOTHING`, claim.JobID, claim.AttemptID, xid, timestamp.Time)
		if err != nil {
			return nil, fmt.Errorf("persist target commit timestamp: %w", err)
		}
		err = s.db.QueryRowContext(ctx, `SELECT committed_at FROM app.job_commit_proofs
			WHERE job_id = $1 AND attempt_id = $2 AND target_xid = $3`, claim.JobID, claim.AttemptID, xid).Scan(&committedAt)
	}
	if err != nil {
		return nil, fmt.Errorf("read target commit proof: %w", err)
	}
	policy, err := sqlpolicy.New("managed")
	if err != nil {
		return nil, fmt.Errorf("create sql policy: %w", err)
	}
	statements, err := policy.Statements(sqlText)
	if err != nil || len(statements) == 0 {
		return nil, errors.Join(errors.New("authorized SQL has no valid statement evidence"), err)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT ordinal, sql_text, outcome, row_count
		FROM app.job_statement_evidence WHERE attempt_id = $1 ORDER BY ordinal`, claim.AttemptID)
	if err != nil {
		return nil, fmt.Errorf("read statement evidence: %w", err)
	}
	var total, increment big.Int
	index := 0
	for rows.Next() {
		var ordinal int
		var statement, outcome string
		var count sql.NullInt64
		if err := rows.Scan(&ordinal, &statement, &outcome, &count); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan statement evidence: %w", err)
		}
		if index >= len(statements) || ordinal != index+1 || statement != statements[index] ||
			outcome != "succeeded" || !count.Valid || count.Int64 < 0 {
			rows.Close()
			return nil, errors.New("statement evidence is incomplete or inconsistent")
		}
		total.Add(&total, increment.SetInt64(count.Int64))
		index++
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, fmt.Errorf("read statement evidence: %w", err)
	}
	if index != len(statements) {
		return nil, errors.New("statement evidence is incomplete or inconsistent")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin completion: %w", err)
	}
	defer tx.Rollback()
	evidence := executioncontract.CommitEvidence{
		JobID: claim.JobID, ExecutionID: claim.ExecutionID, ProposalID: claim.ProposalID,
		MigrationID: claim.MigrationID, ActorID: claim.ActorID, DatabaseID: claim.DatabaseID,
		TargetID: claim.TargetID, Revision: claim.Revision, Version: r.Version,
		Action: claim.Action, Direction: claim.Direction, SQLSnapshot: sqlText,
		SafeParameters: map[string]any{}, SnapshotHash: claim.SnapshotHash,
		Receipt: r.ID, CommittedAt: committedAt, RowCount: total.String(), SourceExecutionID: claim.SourceExecutionID,
		FinalStep: true,
	}
	if err := deps.Gate.RecordCommitTx(ctx, tx, evidence); err != nil {
		return nil, fmt.Errorf("record target commit: %w", err)
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE app.jobs SET status = 'succeeded', execution_status = 'succeeded',
		  blocks_database = false, error_code = NULL, error_message = NULL, error_retryable = NULL,
		  finished_at = clock_timestamp(), updated_at = clock_timestamp()
		WHERE id = $1 AND blocks_database AND attempt_no = $2
		  AND execution_id = $5 AND database_id = $6 AND target_id = $7
		  AND proposal_id = $8 AND migration_id = $9 AND revision = $10 AND actor_id = $11
		  AND action = $12 AND direction = $13 AND mode = $14
		  AND source_execution_id IS NOT DISTINCT FROM $15::uuid
		  AND snapshot_hash = $16 AND COALESCE(approval_fingerprint, '') = $17
		  AND ((status = 'running' AND execution_status = 'running' AND $3 <> ''
		        AND lease_token = NULLIF($3, '')::uuid AND lease_expires_at > clock_timestamp())
		       OR ($3 = '' AND ((status = 'unknown' AND execution_status = 'unknown')
		           OR (status = 'running' AND execution_status = 'running'))))
		  AND EXISTS (SELECT 1 FROM app.job_dispatch_authorizations d
		    JOIN app.job_attempts a ON a.id = d.attempt_id AND a.job_id = d.job_id
		    JOIN app.job_commit_proofs p ON p.job_id = d.job_id AND p.attempt_id = d.attempt_id
		      AND p.target_xid = d.target_xid
		    WHERE d.job_id = $1 AND d.attempt_id = $4 AND a.attempt_no = $2
		      AND d.target_xid = $18 AND d.target_version = $19 AND d.sql_sha256 = $20
		      AND a.sql_entered AND a.status IN ('running', 'unknown'))`,
		claim.JobID, claim.AttemptNo, claim.LeaseToken, claim.AttemptID,
		claim.ExecutionID, claim.DatabaseID, claim.TargetID, claim.ProposalID, claim.MigrationID,
		claim.Revision, claim.ActorID, claim.Action, claim.Direction, claim.Mode,
		claim.SourceExecutionID, claim.SnapshotHash, claim.ApprovalFingerprint, xid, version, hash)
	if err := requireOne(res, err, "fence completion"); err != nil {
		return nil, err
	}
	res, err = tx.ExecContext(ctx, `UPDATE app.job_attempts SET status = 'succeeded',
		error_code = NULL, error_message = NULL, error_retryable = NULL, finished_at = clock_timestamp()
		WHERE id = $1 AND job_id = $2 AND attempt_no = $3 AND sql_entered
		  AND status IN ('running', 'unknown')`, claim.AttemptID, claim.JobID, claim.AttemptNo)
	if err := requireOne(res, err, "complete attempt"); err != nil {
		return nil, err
	}
	res, err = tx.ExecContext(ctx, `UPDATE app.jobs SET updated_at = clock_timestamp()
		WHERE id = $1 AND status = 'succeeded' AND attempt_no = $2
		  AND ($3 = '' OR (lease_token = NULLIF($3, '')::uuid AND lease_expires_at > clock_timestamp()))`,
		claim.JobID, claim.AttemptNo, claim.LeaseToken)
	if err := requireOne(res, err, "verify completion fence"); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit completion: %w", err)
	}
	return &ExecutionOutcome{Status: executioncontract.Succeeded, Receipt: r.ID, Reconciled: claim.LeaseToken == ""}, nil
}

// ExecuteStandardJob executes a claimed standard job against its target using immutable approved SQL,
// verifying workflow approval, enforcing lease fencing and preflight, recording durable statement evidence,
// and coupling target SQL with atomic identity receipt and platform finalization.
func (s *Service) ExecuteStandardJob(ctx context.Context, claim *Claim, deps ExecutionDeps) (*ExecutionOutcome, error) {
	if s.db == nil {
		return nil, errors.New("db is required")
	}
	if claim == nil {
		return nil, executioncontract.ContractError{Code: "INVALID_REQUEST", Message: "claim is required"}
	}
	if claim.Mode != executioncontract.Standard {
		// shortcut: batch mode deferred to P4, upgrade in P4
		return nil, executioncontract.ContractError{Code: "UNSUPPORTED_MODE", Message: "standard execution only, batch mode deferred"}
	}
	if claim.Action != executioncontract.Apply &&
		claim.Action != executioncontract.Rollback &&
		claim.Action != executioncontract.Forward {
		return nil, executioncontract.ContractError{Code: "UNSUPPORTED_ACTION", Message: "unsupported action"}
	}
	if !((claim.Action == executioncontract.Apply || claim.Action == executioncontract.Forward) && claim.Direction == executioncontract.Up ||
		claim.Action == executioncontract.Rollback && claim.Direction == executioncontract.Down) {
		return nil, executioncontract.ContractError{Code: "INVALID_DIRECTION", Message: "action and direction do not match"}
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

	policy, err := sqlpolicy.New("managed")
	if err != nil {
		return nil, fmt.Errorf("create sqlpolicy: %w", err)
	}
	preflight := migration.NewPreflight(policy)

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

	// Step: Check proven receipts before replay (reconcile if target already committed)
	r, found, err := session.ReadReceipt(ctx, claim.ExecutionID)
	if err != nil {
		return nil, fmt.Errorf("check target receipt: %w", err)
	}
	if found {
		outcome, err := s.finalizeReceipt(ctx, claim, r, session, deps)
		if err != nil {
			return s.unknownOutcome(ctx, claim, err)
		}
		if err == nil && outcome.Status == executioncontract.Unknown {
			return s.unknownOutcome(ctx, claim, outcome.Error)
		}
		return outcome, err
	}
	var previouslyAuthorized bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM app.job_dispatch_authorizations WHERE job_id = $1) OR EXISTS (SELECT 1 FROM app.job_attempts WHERE job_id = $1 AND sql_entered)`, claim.JobID).Scan(&previouslyAuthorized); err != nil {
		return nil, err
	}
	if previouslyAuthorized {
		return s.unknownOutcome(ctx, claim, errors.New("authorized dispatch has no receipt"))
	}

	// Step: Revalidate with WorkflowGate
	plan, err := deps.Gate.Revalidate(ctx, claim.RevalidationRequest())
	if err != nil {
		var cErr executioncontract.ContractError
		if errors.As(err, &cErr) && cErr.Code == "REVIEW_STALE" {
			return nil, fmt.Errorf("REVIEW_STALE requires WorkflowGate.ReturnToReviewTx and atomic job finalization: %w", err)
		}
		return nil, fmt.Errorf("workflow revalidation failed: %w", err)
	}
	if !reflect.DeepEqual(plan.Request, claim.RevalidationRequest()) {
		return nil, executioncontract.ContractError{Code: "PLAN_MISMATCH", Message: "workflow plan identity mismatch"}
	}

	sqlText, err := selectSQL(plan, claim.Action, claim.Direction)
	if err != nil {
		return s.failedOutcome(ctx, claim, "INVALID_SQL", err)
	}

	statements, serr := policy.Statements(sqlText)
	if serr != nil || len(statements) == 0 {
		if serr == nil {
			serr = errors.New("no SQL statements")
		}
		return s.failedOutcome(ctx, claim, "TARGET_PREPARATION_FAILED", serr)
	}

	var commitTimeAvailable bool
	err = session.Preparer().QueryRowContext(ctx, `SELECT current_setting('track_commit_timestamp') = 'on'
		AND pg_catalog.has_function_privilege(current_user, 'pg_catalog.pg_xact_commit_timestamp(xid)', 'EXECUTE')
		AND pg_catalog.has_function_privilege(current_user, 'pg_catalog.pg_xact_status(xid8)', 'EXECUTE')`).Scan(&commitTimeAvailable)
	if err != nil || !commitTimeAvailable {
		return s.failedOutcome(ctx, claim, "COMMIT_TIME_UNAVAILABLE",
			errors.Join(errors.New("exact target commit timestamp evidence is not supported"), err))
	}

	// Step: Begin target transaction & Preflight
	if err := session.Begin(ctx, deps.Timeout); err != nil {
		return nil, fmt.Errorf("begin target tx: %w", err)
	}

	if err := preflight(ctx, session.Tx(), sqlText); err != nil {
		session.Abort("SQL_UNSUPPORTED", err.Error())
		code := "SQL_UNSUPPORTED"
		var apiErr *migration.APIError
		if errors.As(err, &apiErr) {
			code = apiErr.Code
		}
		return s.failedOutcome(ctx, claim, code, err)
	}

	version, err := strconv.ParseInt(plan.Version, 10, 64)
	if err != nil || version < 1 {
		session.Abort("INVALID_VERSION", "workflow plan has no valid target version")
		return nil, executioncontract.ContractError{Code: "INVALID_VERSION", Message: "workflow plan has no valid target version"}
	}
	capability := make([]byte, 32)
	if _, err := rand.Read(capability); err != nil {
		session.Abort("TARGET_PREPARATION_FAILED", err.Error())
		return s.failedOutcome(ctx, claim, "TARGET_PREPARATION_FAILED", err)
	}
	defer clear(capability)
	digest := sha256.Sum256(capability)
	sqlHash := sha256.Sum256([]byte(sqlText))

	var xid string
	err = session.Tx().QueryRowContext(ctx, `SELECT pg_catalog.pg_current_xact_id()::text`).Scan(&xid)
	if err != nil {
		session.Abort("TARGET_PREPARATION_FAILED", "target xid query failed")
		return s.failedOutcome(ctx, claim, "TARGET_PREPARATION_FAILED", err)
	}
	if err := s.authorizeDispatch(ctx, claim, plan, sqlText, xid, version); err != nil {
		session.Abort("DISPATCH_NOT_AUTHORIZED", err.Error())
		return nil, err
	}
	if deps.beforeDispatch != nil {
		if err := deps.beforeDispatch(ctx); err != nil {
			return s.rollbackAuthorizedOutcome(ctx, claim, session, xid, "DISPATCH_INTERRUPTED", err)
		}
	}

	_, err = session.Preparer().ExecContext(ctx, `
		INSERT INTO wbd_auth.execution_tickets
			(execution_id, physical_target_id, database_id, target_xid, version, sql_sha256, actor_id, capability_sha256)
		VALUES ($1::uuid, $2::uuid, $3::uuid, $4::xid8, $5, $6, $7::uuid, $8)
	`, claim.ExecutionID, claim.TargetID, claim.DatabaseID, xid, version, sqlHash[:], claim.ActorID, digest[:])
	if err != nil {
		return s.rollbackAuthorizedOutcome(ctx, claim, session, xid, "TARGET_PREPARATION_FAILED", err)
	}

	for idx, stmt := range statements {
		ordinal := idx + 1
		_, err := s.db.ExecContext(ctx, `
			INSERT INTO app.job_statement_evidence (attempt_id, ordinal, sql_text, safe_parameters, outcome, executed_at)
			VALUES ($1::uuid, $2, $3, '{}'::jsonb, 'unknown', clock_timestamp())
		`, claim.AttemptID, ordinal, stmt)
		if err != nil {
			return s.rollbackAuthorizedOutcome(ctx, claim, session, xid, "EVIDENCE_FAILED", fmt.Errorf("record statement dispatch: %w", err))
		}
		res, execErr := session.Tx().ExecContext(ctx, stmt)
		outcome := "succeeded"
		var rowCount *int64
		if execErr != nil {
			outcome = "failed"
		} else if ra, err := res.RowsAffected(); err == nil {
			rowCount = &ra
		}
		evidenceRes, evidenceErr := s.db.ExecContext(ctx, `
			UPDATE app.job_statement_evidence SET outcome = $3, row_count = $4
			WHERE attempt_id = $1 AND ordinal = $2 AND outcome = 'unknown'
		`, claim.AttemptID, ordinal, outcome, rowCount)
		if evidenceErr = requireOne(evidenceRes, evidenceErr, "record statement outcome"); evidenceErr != nil {
			return s.rollbackAuthorizedOutcome(ctx, claim, session, xid, "EVIDENCE_FAILED", errors.Join(execErr, evidenceErr))
		}
		if execErr != nil {
			return s.rollbackAuthorizedOutcome(ctx, claim, session, xid, "SQL_FAILED", execErr)
		}
	}

	// Step: Record receipt on target
	_, err = session.Tx().ExecContext(ctx, `SELECT wbd_auth.record_receipt($1::uuid, $2::bytea)`, claim.ExecutionID, capability)
	if err != nil {
		return s.rollbackAuthorizedOutcome(ctx, claim, session, xid, "RECEIPT_FAILED", err)
	}

	// Step: Commit target transaction
	if err := session.Commit(); err != nil {
		return s.unknownOutcome(ctx, claim, err)
	}

	// Step: Confirm receipt identity
	receiptRecord, found, err := session.ReadReceipt(ctx, claim.ExecutionID)
	if err != nil || !found {
		return s.unknownOutcome(ctx, claim, errors.Join(errors.New("receipt unconfirmed after commit"), err))
	}
	if receiptRecord.ID != claim.ExecutionID || receiptRecord.TargetID != claim.TargetID ||
		receiptRecord.DatabaseID != claim.DatabaseID || receiptRecord.ActorID != claim.ActorID ||
		!bytes.Equal(receiptRecord.Hash, sqlHash[:]) || receiptRecord.XID != xid ||
		receiptRecord.Version != strconv.FormatInt(version, 10) {
		return s.unknownOutcome(ctx, claim, errors.New("receipt identity mismatch"))
	}

	// Step: Test seam after target commit
	if deps.afterTargetCommit != nil {
		if err := deps.afterTargetCommit(ctx); err != nil {
			return s.unknownOutcome(ctx, claim, err)
		}
	}

	outcome, err := s.finalizeReceipt(ctx, claim, receiptRecord, session, deps)
	if err != nil {
		return s.unknownOutcome(ctx, claim, err)
	}
	if outcome.Status == executioncontract.Unknown {
		return s.unknownOutcome(ctx, claim, outcome.Error)
	}
	return outcome, nil
}

// ReconcileStandardJob recovers a job whose target SQL already committed by locking the target,
// validating the proven receipt, and executing platform finalization without replaying target SQL.
func (s *Service) ReconcileStandardJob(ctx context.Context, claim *Claim, deps ExecutionDeps) (*ExecutionOutcome, error) {
	if s.db == nil {
		return nil, errors.New("db is required")
	}
	if claim == nil {
		return nil, executioncontract.ContractError{Code: "INVALID_REQUEST", Message: "claim is required"}
	}
	if claim.Mode != executioncontract.Standard {
		return nil, executioncontract.ContractError{Code: "UNSUPPORTED_MODE", Message: "standard execution only"}
	}
	if claim.Action != executioncontract.Apply && claim.Action != executioncontract.Rollback && claim.Action != executioncontract.Forward {
		return nil, executioncontract.ContractError{Code: "UNSUPPORTED_ACTION", Message: "unsupported action"}
	}
	if !((claim.Action == executioncontract.Apply || claim.Action == executioncontract.Forward) && claim.Direction == executioncontract.Up ||
		claim.Action == executioncontract.Rollback && claim.Direction == executioncontract.Down) {
		return nil, executioncontract.ContractError{Code: "INVALID_DIRECTION", Message: "action and direction do not match"}
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

	r, found, err := session.ReadReceipt(ctx, claim.ExecutionID)
	if err != nil {
		return &ExecutionOutcome{Status: executioncontract.Unknown, Error: fmt.Errorf("target receipt unreadable; outcome unconfirmed: %w", err)}, nil
	}
	if !found {
		return &ExecutionOutcome{Status: executioncontract.Unknown, Error: errors.New("target receipt absent; outcome unconfirmed")}, nil
	}
	recoveryClaim := *claim
	recoveryClaim.LeaseToken = ""
	return s.finalizeReceipt(ctx, &recoveryClaim, r, session, deps)
}

// ReconcileStandardJobByID reconstructs the authorized execution identity after restart.
func (s *Service) ReconcileStandardJobByID(ctx context.Context, jobID string, deps ExecutionDeps) (*ExecutionOutcome, error) {
	if s.db == nil {
		return nil, errors.New("db is required")
	}
	var completedReceipt string
	err := s.db.QueryRowContext(ctx, `SELECT j.execution_id::text FROM app.jobs j
		JOIN app.job_dispatch_authorizations d ON d.job_id = j.id
		JOIN app.job_commit_proofs p ON p.job_id = j.id AND p.attempt_id = d.attempt_id AND p.target_xid = d.target_xid
		JOIN app.job_attempts a ON a.id = d.attempt_id AND a.job_id = j.id AND a.attempt_no = j.attempt_no
		WHERE j.id = $1 AND j.status = 'succeeded' AND j.execution_status = 'succeeded'
		  AND NOT j.blocks_database AND a.status = 'succeeded'`, jobID).Scan(&completedReceipt)
	if err == nil {
		return &ExecutionOutcome{Status: executioncontract.Succeeded, Receipt: completedReceipt, Reconciled: true}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("read completed job: %w", err)
	}
	var claim Claim
	var source sql.NullString
	err = s.db.QueryRowContext(ctx, `
		SELECT j.id::text, j.execution_id::text, j.database_id::text, j.target_id::text,
		       j.proposal_id::text, j.migration_id::text, j.revision, j.actor_id::text,
		       j.action, j.direction, j.mode, j.source_execution_id::text,
		       j.snapshot_hash, COALESCE(j.approval_fingerprint, ''), a.id::text, a.attempt_no
		FROM app.jobs j
		JOIN app.job_dispatch_authorizations d ON d.job_id = j.id
		JOIN app.job_attempts a ON a.id = d.attempt_id AND a.job_id = j.id
		WHERE j.id = $1 AND j.blocks_database AND j.attempt_no = a.attempt_no
		  AND j.status IN ('running', 'unknown')`, jobID).Scan(
		&claim.JobID, &claim.ExecutionID, &claim.DatabaseID, &claim.TargetID,
		&claim.ProposalID, &claim.MigrationID, &claim.Revision, &claim.ActorID,
		&claim.Action, &claim.Direction, &claim.Mode, &source, &claim.SnapshotHash,
		&claim.ApprovalFingerprint, &claim.AttemptID, &claim.AttemptNo)
	if err != nil {
		return nil, fmt.Errorf("discover authorized job: %w", err)
	}
	if source.Valid {
		claim.SourceExecutionID = &source.String
	}
	return s.ReconcileStandardJob(ctx, &claim, deps)
}

// ListUnresolvedStandardJobIDs discovers blocked authorized jobs for reconciliation after restart.
func (s *Service) ListUnresolvedStandardJobIDs(ctx context.Context, limit int) ([]string, error) {
	if s.db == nil {
		return nil, errors.New("db is required")
	}
	if limit < 1 || limit > 1000 {
		return nil, errors.New("limit must be between 1 and 1000")
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT j.id::text
		FROM app.jobs j
		JOIN app.job_dispatch_authorizations d ON d.job_id = j.id
		JOIN app.job_attempts a ON a.id = d.attempt_id AND a.job_id = j.id
		WHERE j.mode = 'standard' AND j.blocks_database AND j.attempt_no = a.attempt_no
		  AND j.status IN ('running', 'unknown')
		ORDER BY j.id
		LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("list unresolved authorized jobs: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan unresolved authorized job: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list unresolved authorized jobs: %w", err)
	}
	return ids, nil
}
