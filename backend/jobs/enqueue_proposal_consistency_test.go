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

func TestEnqueueTxBlocksDuplicateProposalExecution(t *testing.T) {
	db, owner := testDBs(t)
	f := createFixture(t, db, owner)
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	group := uuid.NewString()
	exec(`INSERT INTO app.groups(id, name, leader_user_id) VALUES ($1, $2, $3)`, group, "enqueue-"+group, f.actorID)
	exec(`UPDATE app.migration_proposals SET status='pending_review' WHERE id=$1`, f.proposalID)
	exec(`INSERT INTO app.migration_proposal_approvals(proposal_id,group_id,approval_round,leader_user_id) VALUES ($1,$2,1,$3)`, f.proposalID, group, f.actorID)
	exec(`UPDATE app.migration_proposal_approvals SET status='approved' WHERE proposal_id=$1 AND group_id=$2 AND approval_round=1`, f.proposalID, group)
	exec(`UPDATE app.migration_proposals SET status='approved' WHERE id=$1`, f.proposalID)
	req := executioncontract.EnqueueRequest{
		DatabaseID: f.dbID, TargetID: f.targetID, RequestID: uuid.NewString(),
		ProposalID: f.proposalID, MigrationID: f.draftID, Revision: 1,
		ActorID: f.actorID, Action: executioncontract.Apply, Snapshot: f.snapshot, SnapshotHash: f.hash,
	}
	svc := New(db)
	first, err := svc.EnqueueTx(ctx, tx, req)
	if err != nil {
		t.Fatal(err)
	}
	exec(`UPDATE app.migration_proposals SET status='queued' WHERE id=$1`, f.proposalID)
	other := req
	other.RequestID = uuid.NewString()
	for _, status := range []string{"queued", "running", "retry_wait", "unknown", "partial", "partially_reverted", "succeeded", "failed"} {
		exec(`UPDATE app.jobs SET status=$2, execution_status=$2, error_code='OTHER' WHERE id=$1`, first.JobID, status)
		if _, err := svc.EnqueueTx(ctx, tx, other); !errors.Is(err, executioncontract.ErrRequestConflict) {
			t.Fatalf("status %s: expected conflict, got %v", status, err)
		}
	}
	exec(`UPDATE app.jobs SET error_code='REVIEW_STALE' WHERE id=$1`, first.JobID)
	if _, err := svc.EnqueueTx(ctx, tx, other); !errors.Is(err, executioncontract.ErrRequestConflict) {
		t.Fatalf("stale without new round: expected conflict, got %v", err)
	}
	exec(`UPDATE app.migration_proposals SET status='pending_review' WHERE id=$1`, f.proposalID)
	exec(`INSERT INTO app.migration_proposal_approvals(proposal_id,group_id,approval_round,leader_user_id) VALUES ($1,$2,2,$3)`, f.proposalID, group, f.actorID)
	exec(`UPDATE app.migration_proposal_approvals SET status='approved' WHERE proposal_id=$1 AND group_id=$2 AND approval_round=2`, f.proposalID, group)
	exec(`UPDATE app.migration_proposals SET status='approved' WHERE id=$1`, f.proposalID)
	second, err := svc.EnqueueTx(ctx, tx, other)
	if err != nil || second.Replayed || second.JobID == first.JobID {
		t.Fatalf("new approval round: result=%+v err=%v", second, err)
	}
	replay, err := svc.EnqueueTx(ctx, tx, req)
	if err != nil || !replay.Replayed || replay.JobID != first.JobID {
		t.Fatalf("original replay: result=%+v err=%v", replay, err)
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM app.jobs WHERE proposal_id=$1`, f.proposalID).Scan(&count); err != nil || count != 2 {
		t.Fatalf("job count=%d, err=%v", count, err)
	}
}
