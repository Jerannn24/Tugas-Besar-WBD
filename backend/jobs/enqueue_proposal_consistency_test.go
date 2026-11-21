package jobs

import (
	"context"
	"errors"
	"testing"

	"github.com/Jerannn24/Tugas-Besar-WBD.git/executioncontract"
	"github.com/google/uuid"
)

func TestEnqueueTxProposalIdentity(t *testing.T) {
	db, owner := testDBs(t)
	f := createFixture(t, db, owner)
	other := createFixture(t, db, owner)
	ctx := context.Background()
	base := executioncontract.EnqueueRequest{
		DatabaseID: f.dbID, TargetID: f.targetID, RequestID: uuid.NewString(),
		ProposalID: f.proposalID, MigrationID: f.draftID, Revision: 1,
		ActorID: f.actorID, Action: executioncontract.Apply, Snapshot: f.snapshot,
		SnapshotHash: f.hash,
	}
	cases := []struct {
		name string
		change func(*executioncontract.EnqueueRequest)
		missing bool
	}{
		{"missing proposal", func(r *executioncontract.EnqueueRequest) { r.ProposalID = uuid.NewString() }, true},
		{"database", func(r *executioncontract.EnqueueRequest) { r.ProposalID = other.proposalID }, false},
		{"migration", func(r *executioncontract.EnqueueRequest) { r.MigrationID = other.draftID }, false},
		{"revision", func(r *executioncontract.EnqueueRequest) { r.Revision++ }, false},
		{"action", func(r *executioncontract.EnqueueRequest) { r.Action = executioncontract.Forward }, false},
		{"source", func(r *executioncontract.EnqueueRequest) { id := uuid.NewString(); r.SourceExecutionID = &id }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tx, err := db.BeginTx(ctx, nil)
			if err != nil { t.Fatal(err) }
			defer tx.Rollback()
			req := base
			req.RequestID = uuid.NewString()
			tc.change(&req)
			_, err = New(db).EnqueueTx(ctx, tx, req)
			if tc.missing {
				var contractErr executioncontract.ContractError
				if !errors.As(err, &contractErr) || contractErr.Code != "NOT_FOUND" { t.Fatalf("expected NOT_FOUND, got %v", err) }
			} else if !errors.Is(err, executioncontract.ErrRequestConflict) {
				t.Fatalf("expected request conflict, got %v", err)
			}
			var count int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM app.jobs WHERE database_id = $1 AND request_id = $2`, req.DatabaseID, req.RequestID).Scan(&count); err != nil { t.Fatal(err) }
			if count != 0 { t.Fatalf("inserted %d jobs for inconsistent proposal", count) }
		})
	}
	t.Run("matching and replay", func(t *testing.T) {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil { t.Fatal(err) }
		defer tx.Rollback()
		first, err := New(db).EnqueueTx(ctx, tx, base)
		if err != nil { t.Fatal(err) }
		again, err := New(db).EnqueueTx(ctx, tx, base)
		if err != nil { t.Fatal(err) }
		if first.Replayed || !again.Replayed || first.JobID != again.JobID { t.Fatalf("first=%+v replay=%+v", first, again) }
	})
}

func TestEnqueueTxProposalSourceIdentity(t *testing.T) {
	db, owner := testDBs(t)
	f := createFixture(t, db, owner)
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil { t.Fatal(err) }
	defer tx.Rollback()
	source := uuid.NewString()
	if _, err := tx.ExecContext(ctx, `UPDATE app.migration_target_state SET next_version = next_version + 1 WHERE id = $1`, f.targetID); err != nil { t.Fatal(err) }
	_, err = tx.ExecContext(ctx, `INSERT INTO app.migration_executions
		(id, database_id, execution_target_id, draft_id, request_id, draft_revision, version,
		 title, sql, sql_sha256, executed_by, executed_username, started_at, status)
		SELECT $1, $2, $3, d.id, $5, d.revision, 1, d.title, d.sql, sha256(convert_to(d.sql, 'UTF8')), $6, u.username, clock_timestamp(), 'running'
		FROM app.migration_drafts d JOIN app.users u ON u.id = $6 WHERE d.id = $4`,
		source, f.dbID, f.targetID, f.draftID, uuid.NewString(), f.actorID)
	if err != nil { t.Fatal(err) }
	proposal := uuid.NewString()
	_, err = tx.ExecContext(ctx, `INSERT INTO app.migration_proposals
		(id, database_id, migration_id, revision, action, source_execution_id, snapshot, snapshot_hash, created_by)
		VALUES ($1, $2, $3, 1, 'resume', $4, '{}'::jsonb, sha256('different encoding'::bytea), $5)`, proposal, f.dbID, f.draftID, source, f.actorID)
	if err != nil { t.Fatal(err) }
	req := executioncontract.EnqueueRequest{DatabaseID: f.dbID, TargetID: f.targetID, RequestID: uuid.NewString(), ProposalID: proposal,
		MigrationID: f.draftID, Revision: 1, ActorID: f.actorID, Action: executioncontract.Resume,
		SourceExecutionID: &source, Snapshot: f.snapshot, SnapshotHash: f.hash}
	wrong := uuid.NewString()
	req.SourceExecutionID = &wrong
	if _, err := New(db).EnqueueTx(ctx, tx, req); !errors.Is(err, executioncontract.ErrRequestConflict) { t.Fatalf("expected source conflict, got %v", err) }
	req.SourceExecutionID = &source
	result, err := New(db).EnqueueTx(ctx, tx, req)
	if err != nil { t.Fatal(err) }
	if result.ExecutionID != source { t.Fatalf("execution ID = %s, want %s", result.ExecutionID, source) }
}
