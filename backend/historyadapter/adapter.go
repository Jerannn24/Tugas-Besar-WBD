package historyadapter

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/Jerannn24/Tugas-Besar-WBD.git/executioncontract"
)

type Adapter struct {
	db *sql.DB
}

func New(db *sql.DB) *Adapter {
	return &Adapter{db: db}
}

func NewAdapter(db *sql.DB) *Adapter {
	return New(db)
}


func (a *Adapter) ProjectAttemptTx(ctx context.Context, tx *sql.Tx, attempt executioncontract.CommitEvidence) error {
	if a == nil || tx == nil {
		return errors.New("history adapter transaction is required")
	}
	var duplicate bool
	batchIndex := any(nil)
	if attempt.Batch != nil {
		batchIndex = attempt.Batch.BatchIndex
	}
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM app.migration_history_entries
		WHERE attempt_id=$1 AND COALESCE(batch_index::text,'')=COALESCE($2,'')
	)`, attempt.ExecutionID, batchIndex).Scan(&duplicate)
	if err != nil {
		return err
	}
	if duplicate {
		return nil
	}
	return ProjectCommitTx(ctx, tx, attempt)
}

func (a *Adapter) ProjectExecutionTx(ctx context.Context, tx *sql.Tx, attemptID string) error {
	if tx == nil {
		return errors.New("history transaction is required")
	}
	var databaseID, migrationID, actorID string
	var revision int
	var status, sqlSnapshot string
	var committedAt sql.NullTime
	err := tx.QueryRowContext(ctx, `SELECT database_id::text,draft_id::text,draft_revision,
		status,sql,executed_by::text,COALESCE(finished_at,started_at)
		FROM app.migration_executions WHERE id=$1`, attemptID).
		Scan(&databaseID, &migrationID, &revision, &status, &sqlSnapshot, &actorID, &committedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if status != "succeeded" {
		return nil
	}
	evidence := executioncontract.CommitEvidence{
		ExecutionID:    attemptID,
		DatabaseID:     databaseID,
		MigrationID:    migrationID,
		ActorID:        actorID,
		Revision:       revision,
		Action:         executioncontract.Apply,
		Direction:      executioncontract.Up,
		SQLSnapshot:    sqlSnapshot,
		SafeParameters: map[string]any{},
		FinalStep:      true,
	}
	if committedAt.Valid {
		evidence.CommittedAt = committedAt.Time
	}
	return a.ProjectAttemptTx(ctx, tx, evidence)
}

func (a *Adapter) RecordCommitTx(ctx context.Context, tx *sql.Tx, evidence executioncontract.CommitEvidence) error {
	return a.ProjectAttemptTx(ctx, tx, evidence)
}

func ProjectCommitTx(ctx context.Context, tx *sql.Tx, evidence executioncontract.CommitEvidence) error {
	if tx == nil {
		return errors.New("history transaction is required")
	}
	batchIndex := any(nil)
	rowCount := any(nil)
	rangeStart := any(nil)
	rangeEnd := any(nil)
	receipt := any(nil)
	sqlSnapshot := evidence.SQLSnapshot
	isComplete := evidence.FinalStep
	safeParameters := evidence.SafeParameters
	if safeParameters == nil {
		safeParameters = map[string]any{}
	}
	if evidence.Batch != nil {
		batchIndex = evidence.Batch.BatchIndex
		rowCount = evidence.Batch.RowCount
		rangeStart = evidence.Batch.RangeStart
		rangeEnd = evidence.Batch.RangeEnd
		receipt = evidence.Batch.Receipt
		if evidence.Batch.SQLSnapshot != "" {
			sqlSnapshot = evidence.Batch.SQLSnapshot
		}
		isComplete = evidence.FinalStep
	} else {
		isComplete = true
	}
	safeJSON, err := json.Marshal(safeParameters)
	if err != nil {
		return err
	}
	var counter int64
	err = tx.QueryRowContext(ctx, `SELECT last_version
		FROM app.migration_history_counters WHERE database_id=$1 FOR UPDATE`, evidence.DatabaseID).
		Scan(&counter)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err = tx.ExecContext(ctx, `INSERT INTO app.migration_history_counters(database_id,last_version)
			VALUES($1,0)`, evidence.DatabaseID); err != nil {
			return err
		}
		counter = 0
	} else if err != nil {
		return err
	}
	counter++
	if _, err = tx.ExecContext(ctx, `UPDATE app.migration_history_counters
		SET last_version=$2 WHERE database_id=$1`, evidence.DatabaseID, counter); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO app.migration_history_entries
		(commit_id,attempt_id,execution_id,database_id,migration_id,revision,version,proposal_id,action,direction,
		 batch_index,row_count,receipt,range_start,range_end,safe_parameters,executed_by,committed_at,
		 sql_snapshot,source_execution_id,is_complete)
		VALUES(gen_random_uuid(),$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)`,
		evidence.ExecutionID, evidence.ExecutionID, evidence.DatabaseID, evidence.MigrationID,
		evidence.Revision, counter, evidence.ProposalID, evidence.Action, evidence.Direction,
		batchIndex, rowCount, receipt, rangeStart, rangeEnd, safeJSON, evidence.ActorID,
		evidence.CommittedAt, sqlSnapshot, evidence.SourceExecutionID, isComplete)
	if err != nil {
		return err
	}
	if evidence.Direction == "down" || evidence.FinalStep {
		if _, err = tx.ExecContext(ctx, `UPDATE app.migration_active_heads
			SET active_head_execution_id=$2 WHERE database_id=$1`, evidence.DatabaseID, evidence.ExecutionID); err != nil {
			return err
		}
	}
	return nil
}
