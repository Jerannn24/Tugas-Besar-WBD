package workflow

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/Jerannn24/Tugas-Besar-WBD.git/executioncontract"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
)

type Gate struct {
	db       *pgxpool.Pool
	platform *sql.DB
}

func NewGate(db *pgxpool.Pool) *Gate {
	g := &Gate{db: db}
	if db != nil {
		g.platform = stdlib.OpenDBFromPool(db)
	}
	return g
}

func NewWorkflowGate(db *pgxpool.Pool) *Gate {
	return NewGate(db)
}

func (g *Gate) Revalidate(ctx context.Context, in executioncontract.RevalidationRequest) (executioncontract.ExecutionPlan, error) {
	if g == nil || g.platform == nil {
		return executioncontract.ExecutionPlan{}, contractError("INTERNAL_ERROR", "Workflow gate is not configured", false)
	}
	tx, err := g.platform.BeginTx(ctx, nil)
	if err != nil {
		return executioncontract.ExecutionPlan{}, err
	}
	defer tx.Rollback()
	plan, err := g.RevalidateTx(ctx, tx, in)
	if err != nil {
		return executioncontract.ExecutionPlan{}, err
	}
	if err := tx.Commit(); err != nil {
		return executioncontract.ExecutionPlan{}, err
	}
	return plan, nil
}

// RevalidateTx checks approval inside the standard dispatch authorization transaction.
func (g *Gate) RevalidateTx(ctx context.Context, tx *sql.Tx, in executioncontract.RevalidationRequest) (executioncontract.ExecutionPlan, error) {
	if tx == nil {
		return executioncontract.ExecutionPlan{}, contractError("INTERNAL_ERROR", "Dispatch transaction is required", false)
	}
	databaseID, err := uuid.Parse(in.DatabaseID)
	if err != nil || in.SourceExecutionID != nil && uuid.Validate(*in.SourceExecutionID) != nil {
		return executioncontract.ExecutionPlan{}, contractError("INVALID_REQUEST", "Invalid database or source execution id", false)
	}
	if in.Action == executioncontract.Rollback && in.SourceExecutionID == nil {
		return executioncontract.ExecutionPlan{}, contractError("REQUEST_CONFLICT", "Rollback requires a source execution", false)
	}
	for _, id := range [...]string{in.JobID, in.ExecutionID, in.ActorID, in.TargetID, in.ProposalID, in.MigrationID} {
		if uuid.Validate(id) != nil {
			return executioncontract.ExecutionPlan{}, contractError("INVALID_REQUEST", "Invalid dispatch identity", false)
		}
	}

	// Follow enqueue and draft edit lock ordering before reading the proposal and revision.
	var targetID sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT execution_target_id::text FROM app.database_connections
		WHERE id=$1 AND deleted_at IS NULL FOR SHARE`, databaseID).Scan(&targetID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return executioncontract.ExecutionPlan{}, contractError("INTERNAL_ERROR", "Unable to verify target binding", true)
	}
	if !targetID.Valid || targetID.String != in.TargetID {
		return executioncontract.ExecutionPlan{}, contractError("REQUEST_CONFLICT", "Target binding changed", false)
	}

	var status string
	var mode executioncontract.Mode
	var draftRevision int
	var sourceMatches bool
	var snapshotJSON, storedHash, storedFingerprint []byte
	var enqueuedAt time.Time
	err = tx.QueryRowContext(ctx, `
		SELECT p.status,j.mode,d.revision,p.snapshot,p.snapshot_hash,p.approval_fingerprint,j.created_at,
		       COALESCE(app.workflow_source_valid(j.database_id, j.target_id, j.migration_id,
		           j.revision, j.action, j.source_execution_id, j.mode), false)
		FROM app.jobs j
		JOIN app.migration_proposals p ON p.id=j.proposal_id AND p.database_id=j.database_id
		  AND p.migration_id=j.migration_id AND p.revision=j.revision AND p.action=j.action
		  AND p.source_execution_id IS NOT DISTINCT FROM j.source_execution_id
		JOIN app.migration_drafts d ON d.id=j.migration_id AND d.database_id=j.database_id
		WHERE j.id=$1 AND j.execution_id=$2 AND j.actor_id=$3 AND j.database_id=$4 AND j.target_id=$5
		  AND j.proposal_id=$6 AND j.migration_id=$7 AND j.revision=$8 AND j.action=$9 AND j.direction=$10
		  AND j.source_execution_id IS NOT DISTINCT FROM $11::uuid
		  AND j.snapshot_hash=$12 AND COALESCE(j.approval_fingerprint,'')=$13
		FOR SHARE OF p,d
	`, in.JobID, in.ExecutionID, in.ActorID, databaseID, in.TargetID, in.ProposalID, in.MigrationID,
		in.Revision, in.Action, in.Direction, in.SourceExecutionID, in.SnapshotHash, in.ApprovalFingerprint).
		Scan(&status, &mode, &draftRevision, &snapshotJSON, &storedHash, &storedFingerprint, &enqueuedAt, &sourceMatches)
	if errors.Is(err, sql.ErrNoRows) {
		return executioncontract.ExecutionPlan{}, contractError("REQUEST_CONFLICT", "Job or proposal identity mismatch", false)
	}
	if err != nil {
		return executioncontract.ExecutionPlan{}, contractError("INTERNAL_ERROR", "Unable to revalidate proposal", true)
	}
	if !sourceMatches {
		return executioncontract.ExecutionPlan{}, contractError("REQUEST_CONFLICT", "Source execution mismatch", false)
	}
	if !bytesEqualHex(storedHash, in.SnapshotHash) {
		return executioncontract.ExecutionPlan{}, contractError("SNAPSHOT_HASH_MISMATCH", "Snapshot hash mismatch", false)
	}
	if status != "approved" && status != "queued" || draftRevision != in.Revision ||
		len(storedFingerprint) != 32 || !bytesEqualHex(storedFingerprint, in.ApprovalFingerprint) {
		return executioncontract.ExecutionPlan{}, contractError("REVIEW_STALE", "The queued review is no longer current", false)
	}

	var actorUsername string
	err = tx.QueryRowContext(ctx, `
		SELECT u.username FROM app.users u
		JOIN app.group_memberships m ON m.user_id=u.id
		JOIN app.group_database_grants gr ON gr.group_id=m.group_id
		WHERE u.id=$1 AND u.is_active AND gr.database_id=$2 AND gr.access='edit'
		LIMIT 1 FOR SHARE OF u,m,gr
	`, in.ActorID, databaseID).Scan(&actorUsername)
	canEdit := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return executioncontract.ExecutionPlan{}, contractError("INTERNAL_ERROR", "Unable to verify actor access", true)
	}
	currentFingerprint, err := approvalFingerprintTx(ctx, tx, databaseID)
	if err != nil {
		return executioncontract.ExecutionPlan{}, contractError("INTERNAL_ERROR", "Unable to verify approval roster", true)
	}
	if !equalBytes(storedFingerprint, currentFingerprint[:]) {
		return executioncontract.ExecutionPlan{}, contractError("REVIEW_STALE", "The approval roster or leader changed", false)
	}
	if !canEdit {
		return executioncontract.ExecutionPlan{}, contractError("FORBIDDEN", "Actor no longer has edit access", false)
	}
	var required, approved int
	err = tx.QueryRowContext(ctx, currentApprovalCountsQuery, in.ProposalID, databaseID, enqueuedAt).Scan(&required, &approved)
	if err != nil {
		return executioncontract.ExecutionPlan{}, contractError("INTERNAL_ERROR", "Unable to verify current approvals", true)
	}
	if required == 0 || approved != required {
		return executioncontract.ExecutionPlan{}, contractError("REVIEW_STALE", "The queued approval round is incomplete or no longer current", false)
	}

	var snapshot executioncontract.Draft
	if err := json.Unmarshal(snapshotJSON, &snapshot); err != nil {
		return executioncontract.ExecutionPlan{}, contractError("INVALID_SNAPSHOT", "Stored snapshot is invalid", false)
	}
	if snapshot.MigrationID != in.MigrationID || snapshot.DatabaseID != in.DatabaseID ||
		snapshot.Revision != in.Revision || snapshot.Mode != mode {
		return executioncontract.ExecutionPlan{}, contractError("REQUEST_CONFLICT", "Snapshot identity mismatch", false)
	}
	if err := executioncontract.ValidateDraft(snapshot); err != nil {
		return executioncontract.ExecutionPlan{}, contractError("VALIDATION_FAILED", err.Error(), false)
	}
	version, err := reserveJobTargetVersion(ctx, tx, in.ExecutionID, in.TargetID)
	if err != nil {
		return executioncontract.ExecutionPlan{}, err
	}
	return executioncontract.ExecutionPlan{
		Request:       in,
		Snapshot:      snapshot,
		Version:       strconv.FormatInt(version, 10),
		ValidatedAt:   time.Now().UTC(),
		ActorUsername: actorUsername,
	}, nil
}

func reserveJobTargetVersion(ctx context.Context, tx *sql.Tx, executionID, targetID string) (int64, error) {
	var version int64
	var boundTarget string
	lookup := func() error {
		return tx.QueryRowContext(ctx, `SELECT target_id::text,version FROM app.job_execution_versions
			WHERE execution_id=$1`, executionID).Scan(&boundTarget, &version)
	}
	err := lookup()
	if errors.Is(err, sql.ErrNoRows) {
		// Share the physical target counter with legacy executions and other database aliases.
		if err := tx.QueryRowContext(ctx, `SELECT next_version FROM app.migration_target_state
			WHERE id=$1 FOR UPDATE`, targetID).Scan(&version); err != nil {
			return 0, contractError("INTERNAL_ERROR", "Unable to lock target version counter", true)
		}
		err = lookup()
		if errors.Is(err, sql.ErrNoRows) {
			err = tx.QueryRowContext(ctx, `UPDATE app.migration_target_state SET next_version=next_version+1
				WHERE id=$1 AND next_version<9223372036854775807 RETURNING next_version-1`, targetID).Scan(&version)
			if errors.Is(err, sql.ErrNoRows) {
				return 0, contractError("INVALID_VERSION", "Target version counter is exhausted", false)
			}
			if err == nil {
				_, err = tx.ExecContext(ctx, `INSERT INTO app.job_execution_versions (execution_id,target_id,version)
					VALUES ($1,$2,$3)`, executionID, targetID, version)
				boundTarget = targetID
			}
		}
	}
	if err != nil {
		return 0, contractError("INTERNAL_ERROR", "Unable to reserve target version", true)
	}
	if boundTarget != targetID {
		return 0, contractError("REQUEST_CONFLICT", "Execution version belongs to another target", false)
	}
	return version, nil
}

func (g *Gate) AllowedActions(ctx context.Context, actorID, databaseID, executionID string) ([]executioncontract.AllowedAction, error) {
	if g == nil || g.db == nil {
		return nil, contractError("INTERNAL_ERROR", "Workflow gate is not configured", false)
	}
	actor, err := uuid.Parse(actorID)
	if err != nil {
		return nil, contractError("INVALID_REQUEST", "Invalid actor id", false)
	}
	database, err := uuid.Parse(databaseID)
	if err != nil {
		return nil, contractError("INVALID_REQUEST", "Invalid database id", false)
	}
	execution, err := uuid.Parse(executionID)
	if err != nil {
		return nil, contractError("INVALID_REQUEST", "Invalid execution id", false)
	}
	var accessRank int
	err = g.db.QueryRow(ctx, `SELECT COALESCE(MAX(CASE gr.access WHEN 'edit' THEN 2 WHEN 'view' THEN 1 ELSE 0 END),0)
		FROM app.group_memberships m
		JOIN app.group_database_grants gr ON gr.group_id=m.group_id
		JOIN app.users u ON u.id=m.user_id AND u.is_active
		WHERE m.user_id=$1 AND gr.database_id=$2`, actor, database).Scan(&accessRank)
	if err != nil {
		return nil, contractError("INTERNAL_ERROR", "Unable to check access", true)
	}
	if accessRank == 0 {
		return nil, contractError("FORBIDDEN", "Access denied", false)
	}
	var status string
	err = g.db.QueryRow(ctx, `SELECT status FROM app.migration_executions
		WHERE id=$1 AND database_id=$2`, execution, database).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, contractError("NOT_FOUND", "Execution not found", false)
	}
	if err != nil {
		return nil, contractError("INTERNAL_ERROR", "Unable to read execution", true)
	}
	switch status {
	case "failed":
		return []executioncontract.AllowedAction{"resume", "compensate"}, nil
	case "succeeded":
		return []executioncontract.AllowedAction{"rollback", "forward", "compensate"}, nil
	case "unknown":
		return []executioncontract.AllowedAction{"resume", "compensate"}, nil
	default:
		return []executioncontract.AllowedAction{}, nil
	}
}

func (g *Gate) ReturnToReviewTx(ctx context.Context, tx *sql.Tx, in executioncontract.RevalidationRequest) error {
	if tx == nil {
		return contractError("INTERNAL_ERROR", "Review transaction is required", false)
	}
	databaseID, err := uuid.Parse(in.DatabaseID)
	if err != nil {
		return contractError("INVALID_REQUEST", "Invalid database id", false)
	}
	var status string
	var currentRevision int
	err = tx.QueryRowContext(ctx, `
		SELECT p.status,d.revision
		FROM app.migration_proposals p
		JOIN app.migration_drafts d ON d.id=p.migration_id
		JOIN app.jobs j ON j.id=$1 AND j.proposal_id=p.id
		WHERE p.id=$2 AND p.database_id=$3 AND p.migration_id=$4 AND p.revision=$5
		  AND p.status IN ('approved','queued') AND encode(p.approval_fingerprint,'hex')=$6
		  AND NOT EXISTS (
		      SELECT 1 FROM app.migration_proposal_approvals a
		      WHERE a.proposal_id=p.id AND a.created_at>j.created_at
		  )
		  AND NOT EXISTS (
		      SELECT 1 FROM app.jobs other
		      WHERE other.proposal_id=p.id AND other.id<>j.id
		        AND (other.blocks_database OR other.status IN ('succeeded','unknown','partial','partially_reverted')
		             OR EXISTS (SELECT 1 FROM app.job_dispatch_authorizations a WHERE a.job_id=other.id)
		             OR EXISTS (SELECT 1 FROM app.job_batch_step_authorizations a WHERE a.job_id=other.id)
		             OR EXISTS (
		                 SELECT 1 FROM app.job_attempts a WHERE a.job_id=other.id
		                   AND (a.sql_entered OR EXISTS (SELECT 1 FROM app.job_statement_evidence e WHERE e.attempt_id=a.id))
		             ))
		  )
		FOR UPDATE OF p FOR SHARE OF d
	`, in.JobID, in.ProposalID, databaseID, in.MigrationID, in.Revision, in.ApprovalFingerprint).Scan(&status, &currentRevision)
	if errors.Is(err, sql.ErrNoRows) {
		// Another review or existing dispatch evidence owns this proposal now.
		return nil
	}
	if err != nil {
		return contractError("INTERNAL_ERROR", "Unable to lock stale proposal", true)
	}
	if status == "queued" {
		if _, err := tx.ExecContext(ctx, `UPDATE app.migration_proposals SET status='pending_review' WHERE id=$1`, in.ProposalID); err != nil {
			return contractError("INTERNAL_ERROR", "Unable to return proposal to review", true)
		}
	}
	if currentRevision != in.Revision || status == "approved" {
		// A proposal's revision and snapshot cannot be edited in place.
		_, err := tx.ExecContext(ctx, `UPDATE app.migration_proposals SET status='superseded' WHERE id=$1`, in.ProposalID)
		if err != nil {
			return contractError("INTERNAL_ERROR", "Unable to supersede stale proposal", true)
		}
		return nil
	}
	fingerprint, err := approvalFingerprintTx(ctx, tx, databaseID)
	if err != nil {
		return contractError("INTERNAL_ERROR", "Unable to refresh approval roster", true)
	}
	_, err = tx.ExecContext(ctx, `
		UPDATE app.migration_proposals SET approval_fingerprint=$2 WHERE id=$1
	`, in.ProposalID, fingerprint[:])
	if err != nil {
		return contractError("INTERNAL_ERROR", "Unable to refresh approval fingerprint", true)
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO app.migration_proposal_approvals (proposal_id,group_id,approval_round,leader_user_id)
		SELECT $1,gr.group_id,
		       COALESCE((SELECT MAX(a.approval_round) FROM app.migration_proposal_approvals a WHERE a.proposal_id=$1),0)+1,
		       g.leader_user_id
		FROM app.group_database_grants gr JOIN app.groups g ON g.id=gr.group_id
		WHERE gr.database_id=$2 AND gr.access IN ('view','edit')
	`, in.ProposalID, databaseID)
	if err != nil {
		return contractError("INTERNAL_ERROR", "Unable to open a new approval round", true)
	}
	return nil
}

var _ executioncontract.WorkflowGate = (*Gate)(nil)

func contractError(code, message string, retryable bool) executioncontract.ContractError {
	return executioncontract.ContractError{
		Code: code, Message: message, FieldErrors: map[string]string{}, Retryable: retryable,
	}
}

func bytesEqualHex(value []byte, hexValue string) bool {
	decoded, err := hex.DecodeString(hexValue)
	return err == nil && equalBytes(value, decoded)
}
