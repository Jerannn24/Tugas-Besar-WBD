package workflow

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/Jerannn24/Tugas-Besar-WBD.git/executioncontract"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Gate struct {
	db *pgxpool.Pool
}

func NewGate(db *pgxpool.Pool) *Gate {
	return &Gate{db: db}
}

func NewWorkflowGate(db *pgxpool.Pool) *Gate {
	return NewGate(db)
}

func (g *Gate) Revalidate(ctx context.Context, in executioncontract.RevalidationRequest) (executioncontract.ExecutionPlan, error) {
	if g == nil || g.db == nil {
		return executioncontract.ExecutionPlan{}, contractError("INTERNAL_ERROR", "Workflow gate is not configured", false)
	}
	proposalID, err := uuid.Parse(in.ProposalID)
	if err != nil {
		return executioncontract.ExecutionPlan{}, contractError("INVALID_REQUEST", "Invalid proposal id", false)
	}
	migrationID, err := uuid.Parse(in.MigrationID)
	if err != nil {
		return executioncontract.ExecutionPlan{}, contractError("INVALID_REQUEST", "Invalid migration id", false)
	}
	databaseID, err := uuid.Parse(in.DatabaseID)
	if err != nil {
		return executioncontract.ExecutionPlan{}, contractError("INVALID_REQUEST", "Invalid database id", false)
	}

	var status, action string
	var draftRevision int
	var storedHash, storedFingerprint []byte
	var snapshotJSON []byte
	var storedDatabaseID, storedMigrationID uuid.UUID
	err = g.db.QueryRow(ctx, `SELECT database_id,migration_id,revision,status,action,snapshot,snapshot_hash,approval_fingerprint
		FROM app.migration_proposals WHERE id=$1 FOR UPDATE`, proposalID).
		Scan(&storedDatabaseID, &storedMigrationID, &draftRevision, &status, &action, &snapshotJSON, &storedHash, &storedFingerprint)
	if errors.Is(err, pgx.ErrNoRows) {
		return executioncontract.ExecutionPlan{}, contractError("NOT_FOUND", "Proposal not found", false)
	}
	if err != nil {
		return executioncontract.ExecutionPlan{}, contractError("INTERNAL_ERROR", "Unable to revalidate proposal", true)
	}
	if storedDatabaseID != databaseID || storedMigrationID != migrationID {
		return executioncontract.ExecutionPlan{}, contractError("REQUEST_CONFLICT", "Proposal identity mismatch", false)
	}
	if status != "approved" && status != "queued" {
		return executioncontract.ExecutionPlan{}, contractError("APPROVAL_REQUIRED", "Proposal is not approved for execution", false)
	}
	if action != string(in.Action) || draftRevision != in.Revision {
		return executioncontract.ExecutionPlan{}, contractError("PROPOSAL_STALE", "Proposal revision or action changed", false)
	}
	currentRevision := 0
	err = g.db.QueryRow(ctx, `SELECT revision FROM app.migration_drafts
		WHERE id=$1 AND database_id=$2`, migrationID, databaseID).Scan(&currentRevision)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && currentRevision != draftRevision {
		return executioncontract.ExecutionPlan{}, contractError("PROPOSAL_STALE", "Proposal is stale", false)
	}
	if err != nil {
		return executioncontract.ExecutionPlan{}, contractError("INTERNAL_ERROR", "Unable to read migration revision", true)
	}
	if in.SnapshotHash == "" || !bytesEqualHex(storedHash, in.SnapshotHash) {
		return executioncontract.ExecutionPlan{}, contractError("SNAPSHOT_HASH_MISMATCH", "Snapshot hash mismatch", false)
	}
	if in.ApprovalFingerprint == "" || !bytesEqualHex(storedFingerprint, in.ApprovalFingerprint) {
		return executioncontract.ExecutionPlan{}, contractError("APPROVAL_FINGERPRINT_MISMATCH", "Approval fingerprint mismatch", false)
	}
	currentFingerprint, err := gateApprovalFingerprint(ctx, g.db, databaseID)
	if err != nil {
		return executioncontract.ExecutionPlan{}, contractError("INTERNAL_ERROR", "Unable to verify approval roster", true)
	}
	if len(storedFingerprint) != 32 || !equalBytes(storedFingerprint, currentFingerprint[:]) {
		return executioncontract.ExecutionPlan{}, contractError("APPROVALS_INVALIDATED", "Roster or leader has changed", false)
	}

	var snapshot executioncontract.Draft
	if err := json.Unmarshal(snapshotJSON, &snapshot); err != nil {
		return executioncontract.ExecutionPlan{}, contractError("INVALID_SNAPSHOT", "Stored snapshot is invalid", false)
	}
	if err := executioncontract.ValidateDraft(snapshot); err != nil {
		return executioncontract.ExecutionPlan{}, contractError("VALIDATION_FAILED", err.Error(), false)
	}

	var actorUsername string
	actorID, err := uuid.Parse(in.ActorID)
	if err != nil {
		return executioncontract.ExecutionPlan{}, contractError("INVALID_REQUEST", "Invalid actor id", false)
	}
	err = g.db.QueryRow(ctx, `SELECT u.username
		FROM app.users u
		WHERE u.id=$1 AND u.is_active`, actorID).Scan(&actorUsername)
	if errors.Is(err, pgx.ErrNoRows) {
		return executioncontract.ExecutionPlan{}, contractError("FORBIDDEN", "Actor is not active", false)
	}
	if err != nil {
		return executioncontract.ExecutionPlan{}, contractError("INTERNAL_ERROR", "Unable to resolve actor", true)
	}
	return executioncontract.ExecutionPlan{
		Request:       in,
		Snapshot:      snapshot,
		ValidatedAt:   time.Now().UTC(),
		ActorUsername: actorUsername,
	}, nil
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

func (g *Gate) RecordCommitTx(ctx context.Context, tx *sql.Tx, in executioncontract.CommitEvidence) error {
	if tx == nil {
		return contractError("INTERNAL_ERROR", "Commit transaction is required", false)
	}
	if _, err := uuid.Parse(in.ProposalID); err != nil {
		return contractError("INVALID_REQUEST", "Invalid proposal id", false)
	}
	var databaseID, migrationID, proposalDatabaseID, proposalMigrationID uuid.UUID
	var proposalRevision int
	var proposalStatus string
	var storedHash, storedFingerprint []byte
	err := tx.QueryRowContext(ctx, `SELECT database_id,migration_id,revision,status,snapshot_hash,approval_fingerprint
		FROM app.migration_proposals WHERE id=$1`, in.ProposalID).
		Scan(&proposalDatabaseID, &proposalMigrationID, &proposalRevision, &proposalStatus, &storedHash, &storedFingerprint)
	if errors.Is(err, sql.ErrNoRows) {
		return contractError("NOT_FOUND", "Proposal not found", false)
	}
	if err != nil {
		return contractError("INTERNAL_ERROR", "Unable to read proposal", true)
	}
	databaseID, err = uuid.Parse(in.DatabaseID)
	if err != nil {
		return contractError("INVALID_REQUEST", "Invalid database id", false)
	}
	migrationID, err = uuid.Parse(in.MigrationID)
	if err != nil {
		return contractError("INVALID_REQUEST", "Invalid migration id", false)
	}
	if proposalDatabaseID != databaseID || proposalMigrationID != migrationID || proposalRevision != in.Revision {
		return contractError("PROPOSAL_STALE", "Proposal identity or revision mismatch", false)
	}
	if proposalStatus != "approved" && proposalStatus != "queued" {
		return contractError("APPROVAL_REQUIRED", "Proposal is not approved", false)
	}
	if in.SnapshotHash == "" || !bytesEqualHex(storedHash, in.SnapshotHash) {
		return contractError("SNAPSHOT_HASH_MISMATCH", "Snapshot hash mismatch", false)
	}
	if len(storedFingerprint) == 0 {
		return contractError("APPROVAL_REQUIRED", "Approval fingerprint is missing", false)
	}
	var draftRevision int
	err = tx.QueryRowContext(ctx, `SELECT revision FROM app.migration_drafts WHERE id=$1 AND database_id=$2`, in.MigrationID, in.DatabaseID).Scan(&draftRevision)
	if errors.Is(err, sql.ErrNoRows) || err == nil && draftRevision != in.Revision {
		return contractError("PROPOSAL_STALE", "Proposal is stale", false)
	}
	if err != nil {
		return contractError("INTERNAL_ERROR", "Unable to read migration revision", true)
	}

	attemptID := in.ExecutionID
	batchIndex := -1
	var rowCount any
	var rangeStart, rangeEnd, receipt sql.NullString
	safeParameters := in.SafeParameters
	if safeParameters == nil {
		safeParameters = map[string]any{}
	}
	if in.Batch != nil {
		parsed, parseErr := strconv.Atoi(in.Batch.BatchIndex)
		if parseErr == nil {
			batchIndex = parsed
		}
		rangeStart = toNullString(in.Batch.RangeStart)
		rangeEnd = toNullString(in.Batch.RangeEnd)
		receipt = toNullString(&in.Batch.Receipt)
		rowCount = in.Batch.RowCount
		if in.Batch.SQLSnapshot != "" {
			in.SQLSnapshot = in.Batch.SQLSnapshot
		}
	}
	safeJSON, err := json.Marshal(safeParameters)
	if err != nil {
		return contractError("INVALID_REQUEST", "Invalid safe parameters", false)
	}
	var duplicate bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM app.migration_history_entries
		WHERE attempt_id=$1 AND COALESCE(batch_index,-1)=COALESCE($2,-1)
	)`, attemptID, nullBatchIndex(batchIndex)).Scan(&duplicate)
	if err != nil {
		return contractError("INTERNAL_ERROR", "Unable to check history deduplication", true)
	}
	if duplicate {
		return nil
	}

	var counter int64
	err = tx.QueryRowContext(ctx, `SELECT last_version
		FROM app.migration_history_counters WHERE database_id=$1 FOR UPDATE`, in.DatabaseID).Scan(&counter)
	if errors.Is(err, sql.ErrNoRows) {
		_, err = tx.ExecContext(ctx, `INSERT INTO app.migration_history_counters(database_id,last_version)
			VALUES($1,0)`, in.DatabaseID)
		if err != nil {
			return contractError("INTERNAL_ERROR", "Unable to initialize history counter", true)
		}
		counter = 0
	} else if err != nil {
		return contractError("INTERNAL_ERROR", "Unable to lock history counter", true)
	}
	nextVersion := counter + 1
	_, err = tx.ExecContext(ctx, `UPDATE app.migration_history_counters
		SET last_version=$2 WHERE database_id=$1`, in.DatabaseID, nextVersion)
	if err != nil {
		return contractError("INTERNAL_ERROR", "Unable to advance history counter", true)
	}

	var isComplete bool
	if in.Batch != nil {
		isComplete = in.FinalStep
	} else {
		isComplete = true
	}
	var rowCountValue any
	if rowCount != nil {
		rowCountValue = rowCount
	} else if in.RowCount != "" {
		rowCountValue = in.RowCount
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO app.migration_history_entries
		(commit_id,attempt_id,execution_id,database_id,migration_id,revision,version,proposal_id,action,direction,
		 batch_index,row_count,receipt,range_start,range_end,safe_parameters,executed_by,committed_at,
		 sql_snapshot,source_execution_id,is_complete)
		VALUES(gen_random_uuid(),$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)`,
		attemptID, in.ExecutionID, in.DatabaseID, in.MigrationID, in.Revision, nextVersion,
		in.ProposalID, in.Action, in.Direction, nullBatchIndex(batchIndex), rowCountValue,
		nullString(receipt), nullString(rangeStart), nullString(rangeEnd), safeJSON, in.ActorID,
		in.CommittedAt, in.SQLSnapshot, in.SourceExecutionID, isComplete)
	if err != nil {
		if isDuplicateKey(err) {
			return nil
		}
		return contractError("INTERNAL_ERROR", "Unable to record history commit", true)
	}

	if in.Direction == executioncontract.Down || in.FinalStep {
		_, err = tx.ExecContext(ctx, `UPDATE app.migration_active_heads
			SET active_head_execution_id=$2 WHERE database_id=$1`, in.DatabaseID, in.ExecutionID)
		if err != nil {
			return contractError("INTERNAL_ERROR", "Unable to update active head", true)
		}
	}
	return nil
}

func (g *Gate) ReturnToReviewTx(ctx context.Context, tx *sql.Tx, args ...any) error {
	if tx == nil {
		return contractError("INTERNAL_ERROR", "Review transaction is required", false)
	}
	if len(args) == 0 {
		return contractError("INVALID_REQUEST", "Proposal id is required", false)
	}
	proposalID, err := parseUUIDArg(args[0])
	if err != nil {
		return contractError("INVALID_REQUEST", "Invalid proposal id", false)
	}
	tag, err := tx.ExecContext(ctx, `UPDATE app.migration_proposals
		SET status='pending_review' WHERE id=$1 AND status='queued'`, proposalID)
	if err != nil {
		return contractError("INTERNAL_ERROR", "Unable to return proposal to review", true)
	}
	rowsAffected, err := tag.RowsAffected()
	if err != nil {
		return contractError("INTERNAL_ERROR", "Unable to read review transition", true)
	}
	if rowsAffected == 0 {
		_, err = tx.ExecContext(ctx, `UPDATE app.migration_proposals
			SET status='superseded' WHERE id=$1 AND status='approved'`, proposalID)
		if err != nil {
			return contractError("INTERNAL_ERROR", "Unable to invalidate proposal", true)
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO app.migration_proposal_approvals
		(proposal_id,group_id,approval_round,leader_user_id)
		SELECT p.id,gr.group_id,
		       COALESCE((SELECT MAX(a.approval_round) FROM app.migration_proposal_approvals a WHERE a.proposal_id=p.id),0)+1,
		       g.leader_user_id
		FROM app.migration_proposals p
		JOIN app.group_database_grants gr ON gr.database_id=p.database_id
		JOIN app.groups g ON g.id=gr.group_id
		WHERE p.id=$1
		ON CONFLICT DO NOTHING`, proposalID)
	if err != nil {
		// PostgreSQL rows can be absent when approval round reset is outside this gate.
		return contractError("INTERNAL_ERROR", "Unable to create review approval round", true)
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

func toNullString(value *string) sql.NullString {
	if value == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: *value, Valid: true}
}

func nullString(value sql.NullString) any {
	if value.Valid {
		return value.String
	}
	return nil
}

func nullBatchIndex(index int) any {
	if index >= 0 {
		return index
	}
	return nil
}

func parseUUIDArg(value any) (string, error) {
	switch v := value.(type) {
	case string:
		if _, err := uuid.Parse(v); err != nil {
			return "", err
		}
		return v, nil
	case uuid.UUID:
		return v.String(), nil
	default:
		return "", fmt.Errorf("invalid uuid")
	}
}

func isDuplicateKey(err error) bool {
	return err != nil && (contains(err.Error(), "duplicate key") || contains(err.Error(), "23505"))
}

func contains(value, part string) bool {
	for i := 0; i+len(part) <= len(value); i++ {
		if value[i:i+len(part)] == part {
			return true
		}
	}
	return false
}

func gateApprovalFingerprint(ctx context.Context, db *pgxpool.Pool, databaseID uuid.UUID) ([32]byte, error) {
	rows, err := db.Query(ctx, `SELECT g.id,g.leader_user_id,m.user_id
		FROM app.groups g
		JOIN app.group_database_grants gr ON gr.group_id=g.id
		LEFT JOIN app.group_memberships m ON m.group_id=g.id
		WHERE gr.database_id=$1
		ORDER BY g.id,m.user_id`, databaseID)
	if err != nil {
		return [32]byte{}, err
	}
	defer rows.Close()
	type roster struct {
		Group   string   `json:"group"`
		Leader  *string  `json:"leader"`
		Members []string `json:"members"`
	}
	var all []roster
	var current *roster
	for rows.Next() {
		var group uuid.UUID
		var leader, member *uuid.UUID
		if err := rows.Scan(&group, &leader, &member); err != nil {
			return [32]byte{}, err
		}
		if current == nil || current.Group != group.String() {
			all = append(all, roster{Group: group.String()})
			current = &all[len(all)-1]
		}
		if leader != nil {
			value := leader.String()
			current.Leader = &value
		}
		if member != nil {
			current.Members = append(current.Members, member.String())
		}
	}
	if err := rows.Err(); err != nil {
		return [32]byte{}, err
	}
	raw, err := json.Marshal(all)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(raw), nil
}
