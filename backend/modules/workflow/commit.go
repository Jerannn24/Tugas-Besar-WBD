package workflow

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"github.com/Jerannn24/Tugas-Besar-WBD.git/executioncontract"
	"github.com/google/uuid"
)

func (g *Gate) RecordCommitTx(ctx context.Context, tx *sql.Tx, in executioncontract.CommitEvidence) error {
	if tx == nil {
		return contractError("INTERNAL_ERROR", "Commit transaction is required", false)
	}
	for _, id := range []string{in.JobID, in.ExecutionID, in.ProposalID, in.DatabaseID, in.TargetID, in.MigrationID, in.ActorID} {
		if _, err := uuid.Parse(id); err != nil {
			return contractError("INVALID_REQUEST", "Invalid commit identity", false)
		}
	}
	if in.SourceExecutionID != nil {
		if _, err := uuid.Parse(*in.SourceExecutionID); err != nil {
			return contractError("INVALID_REQUEST", "Invalid source execution id", false)
		}
	}
	targetVersion, versionErr := strconv.ParseInt(in.Version, 10, 64)
	rowCount, countErr := strconv.ParseInt(in.RowCount, 10, 64)
	if versionErr != nil || targetVersion <= 0 || countErr != nil || rowCount < 0 || in.Revision <= 0 ||
		in.SnapshotHash == "" || in.Receipt != in.ExecutionID || in.CommittedAt.IsZero() || strings.TrimSpace(in.SQLSnapshot) == "" {
		return contractError("INVALID_REQUEST", "Invalid commit evidence", false)
	}
	var batchIndex, rangeStart, rangeEnd any
	if batch := in.Batch; batch != nil {
		index, err := strconv.ParseInt(batch.BatchIndex, 10, 32)
		if err != nil || index < 0 || batch.JobID != in.JobID || batch.ExecutedBy.ID != in.ActorID ||
			batch.SQLSnapshot != in.SQLSnapshot || batch.RowCount != in.RowCount || batch.Receipt != in.Receipt ||
			!batch.CommittedAt.Equal(in.CommittedAt) || batch.RangeStart == nil || batch.RangeEnd == nil {
			return contractError("INVALID_REQUEST", "Invalid batch commit evidence", false)
		}
		batchIndex, rangeStart, rangeEnd = int32(index), *batch.RangeStart, *batch.RangeEnd
	} else if !in.FinalStep || len(in.SafeParameters) != 0 {
		return contractError("INVALID_REQUEST", "Invalid standard commit evidence", false)
	}
	safeJSON := []byte("{}")
	if in.SafeParameters != nil {
		var err error
		safeJSON, err = json.Marshal(in.SafeParameters)
		if err != nil {
			return contractError("INVALID_REQUEST", "Invalid safe parameters", false)
		}
	}

	// Verify durable dispatch and receipt identities, not authorization or the current draft.
	var attemptID, username string
	err := tx.QueryRowContext(ctx, `
		SELECT a.id::text, COALESCE(NULLIF(proof.plan->>'ActorUsername', ''), u.username)
		FROM app.jobs j
		JOIN app.migration_proposals p ON p.id = j.proposal_id AND p.database_id = j.database_id
		    AND p.migration_id = j.migration_id AND p.revision = j.revision AND p.action = j.action
		    AND p.source_execution_id IS NOT DISTINCT FROM j.source_execution_id
		JOIN app.users u ON u.id = j.actor_id
		JOIN LATERAL (
		    SELECT d.attempt_id, d.plan, c.committed_at
		    FROM app.job_dispatch_authorizations d
		    JOIN app.job_commit_proofs c ON c.job_id = d.job_id AND c.attempt_id = d.attempt_id AND c.target_xid = d.target_xid
		    WHERE d.job_id = j.id AND j.mode = 'standard' AND $13::integer IS NULL
		        AND d.target_version = $14 AND d.sql_text = $15 AND d.sql_sha256 = sha256(convert_to($15, 'UTF8'))
		    UNION ALL
		    SELECT b.attempt_id, b.plan, c.committed_at
		    FROM app.job_batch_step_authorizations b
		    JOIN app.job_batch_commit_proofs c ON c.step_auth_id = b.id AND c.job_id = b.job_id
		        AND c.attempt_id = b.attempt_id AND c.target_xid = b.target_xid
		        AND c.physical_target_id = b.physical_target_id AND c.scope_execution_id = b.scope_execution_id
		        AND c.direction = b.direction AND c.batch_index = b.batch_index
		    WHERE b.job_id = j.id AND j.mode = 'batch' AND b.batch_index = $13
		        AND b.job_execution_id = j.execution_id AND b.physical_target_id = j.target_id
		        AND b.actor_id = j.actor_id AND b.direction = j.direction AND b.target_version = $14
		        AND b.sql_text = $15 AND b.sql_sha256 = sha256(convert_to($15, 'UTF8')) AND c.row_count = $16
		        AND $17::jsonb = jsonb_build_object('ids', b.chunk_ids)
		        AND b.chunk_ids->>0 = $18 AND b.chunk_ids->> -1 = $19
		) proof ON proof.committed_at = $20
		JOIN app.job_attempts a ON a.id = proof.attempt_id AND a.job_id = j.id
		    AND a.execution_id = j.execution_id AND a.database_id = j.database_id
		    AND a.migration_id = j.migration_id AND a.revision = j.revision AND a.actor_id = j.actor_id AND a.sql_entered
		WHERE j.id = $1 AND j.execution_id = $2 AND j.database_id = $3 AND j.target_id = $4
		    AND j.migration_id = $5 AND j.revision = $6 AND j.proposal_id = $7 AND j.actor_id = $8
		    AND j.action = $9 AND j.direction = $10 AND j.source_execution_id IS NOT DISTINCT FROM $11::uuid
		    AND j.snapshot_hash = $12`,
		in.JobID, in.ExecutionID, in.DatabaseID, in.TargetID, in.MigrationID, in.Revision, in.ProposalID, in.ActorID,
		in.Action, in.Direction, in.SourceExecutionID, in.SnapshotHash, batchIndex, targetVersion, in.SQLSnapshot,
		rowCount, safeJSON, rangeStart, rangeEnd, in.CommittedAt).Scan(&attemptID, &username)
	if errors.Is(err, sql.ErrNoRows) {
		return contractError("INVALID_REQUEST", "Commit evidence does not match the persisted job and receipt", false)
	}
	if err != nil {
		return contractError("INTERNAL_ERROR", "Unable to validate job commit evidence", true)
	}

	// Serialize deduplication and version allocation in each database, including its first commit.
	if _, err := tx.ExecContext(ctx, `INSERT INTO app.migration_history_counters(database_id)
		VALUES ($1) ON CONFLICT (database_id) DO NOTHING`, in.DatabaseID); err != nil {
		return contractError("INTERNAL_ERROR", "Unable to initialize history counter", true)
	}
	var counter int64
	if err := tx.QueryRowContext(ctx, `SELECT last_version FROM app.migration_history_counters
		WHERE database_id = $1 FOR UPDATE`, in.DatabaseID).Scan(&counter); err != nil {
		return contractError("INTERNAL_ERROR", "Unable to lock history counter", true)
	}
	var sameCommit bool
	err = tx.QueryRowContext(ctx, `SELECT (database_id, job_id, attempt_id, target_version, sql_snapshot,
		    row_count, receipt, safe_parameters, range_start, range_end, committed_at, is_complete)
		IS NOT DISTINCT FROM ($1::uuid, $5::uuid, $6::uuid, $7::bigint, $8::text, $9::bigint,
		    $10::text, $11::jsonb, $12::text, $13::text, $14::timestamptz, $15::boolean)
		FROM app.migration_history_entries WHERE execution_id = $2 AND direction = $3
		    AND COALESCE(batch_index, -1) = COALESCE($4::integer, -1)`,
		in.DatabaseID, in.ExecutionID, in.Direction, batchIndex, in.JobID, attemptID, targetVersion,
		in.SQLSnapshot, rowCount, in.Receipt, safeJSON, rangeStart, rangeEnd, in.CommittedAt, in.FinalStep).Scan(&sameCommit)
	if err == nil {
		if sameCommit {
			return nil
		}
		return contractError("INVALID_REQUEST", "Conflicting evidence for an existing history commit", false)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return contractError("INTERNAL_ERROR", "Unable to check history deduplication", true)
	}
	var reverted *string
	if in.FinalStep && in.Direction == executioncontract.Down {
		reverted = in.SourceExecutionID
		if in.Action == executioncontract.Resume {
			var original string
			if err := tx.QueryRowContext(ctx, `SELECT source_execution_id::text FROM app.jobs
				WHERE database_id=$1 AND execution_id=$2 AND action='compensate' AND direction='down'`,
				in.DatabaseID, in.SourceExecutionID).Scan(&original); err != nil {
				return contractError("INTERNAL_ERROR", "Unable to resolve the reverted execution", true)
			}
			reverted = &original
		}
	}

	if _, err := tx.ExecContext(ctx, `UPDATE app.migration_history_counters SET last_version = $2
		WHERE database_id = $1`, in.DatabaseID, counter+1); err != nil {
		return contractError("INTERNAL_ERROR", "Unable to advance history counter", true)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO app.migration_history_entries
		(job_id, attempt_id, execution_id, database_id, migration_id, revision, version, target_version, proposal_id,
		 action, direction, batch_index, row_count, receipt, range_start, range_end, safe_parameters,
		 executed_by, executed_username, committed_at, sql_snapshot, source_execution_id, is_complete, reverts_execution_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24)`,
		in.JobID, attemptID, in.ExecutionID, in.DatabaseID, in.MigrationID, in.Revision, counter+1, targetVersion,
		in.ProposalID, in.Action, in.Direction, batchIndex, rowCount, in.Receipt, rangeStart, rangeEnd, safeJSON,
		in.ActorID, username, in.CommittedAt, in.SQLSnapshot, in.SourceExecutionID, in.FinalStep, reverted); err != nil {
		return contractError("INTERNAL_ERROR", "Unable to record history commit", true)
	}
	if in.FinalStep {
		if _, err := tx.ExecContext(ctx, `INSERT INTO app.migration_active_heads(database_id)
			VALUES ($1) ON CONFLICT (database_id) DO NOTHING`, in.DatabaseID); err != nil {
			return contractError("INTERNAL_ERROR", "Unable to initialize active head", true)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE app.migration_active_heads SET active_head_execution_id =
			CASE WHEN $3::uuid IS NOT NULL THEN (
				SELECT h.execution_id FROM app.migration_history_entries h
				WHERE h.database_id = $1 AND h.is_complete AND h.direction = 'up'
				  AND NOT EXISTS (SELECT FROM app.migration_history_entries r
					WHERE r.database_id = h.database_id AND r.is_complete AND r.reverts_execution_id = h.execution_id)
				ORDER BY h.version DESC LIMIT 1
			) ELSE $2::uuid END WHERE database_id = $1`, in.DatabaseID, in.ExecutionID, reverted); err != nil {
			return contractError("INTERNAL_ERROR", "Unable to update active head", true)
		}
	}
	return nil
}
