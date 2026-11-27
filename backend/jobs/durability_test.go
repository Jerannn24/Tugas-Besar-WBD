package jobs

import (
	"context"
	"testing"

	"errors"
	"github.com/Jerannn24/Tugas-Besar-WBD.git/executioncontract"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestJobEvidenceDeletionProtection(t *testing.T) {
	platform, owner := testDBs(t)
	f := createFixture(t, platform, owner)
	ctx := context.Background()
	tx, err := platform.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	result, err := New(platform).EnqueueTx(ctx, tx, executioncontract.EnqueueRequest{
		DatabaseID: f.dbID, TargetID: f.targetID, RequestID: uuid.NewString(), ProposalID: f.proposalID,
		MigrationID: f.draftID, Revision: 1, ActorID: f.actorID, Action: executioncontract.Apply,
		Direction: executioncontract.Up, Snapshot: f.snapshot, SnapshotHash: f.hash,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	attemptID, evidenceID := uuid.NewString(), uuid.NewString()
	_, err = owner.ExecContext(ctx, `INSERT INTO app.job_attempts
		(id, job_id, execution_id, database_id, migration_id, revision, actor_id, attempt_no, status)
		VALUES ($1,$2,$3,$4,$5,1,$6,1,'failed')`, attemptID, result.JobID, result.ExecutionID, f.dbID, f.draftID, f.actorID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = owner.ExecContext(ctx, `INSERT INTO app.job_statement_evidence (id, attempt_id, ordinal, sql_text, outcome)
		VALUES ($1,$2,1,'SELECT 1','failed')`, evidenceID, attemptID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = owner.ExecContext(ctx, `INSERT INTO app.job_dispatch_authorizations
		(job_id, attempt_id, plan, sql_text, sql_sha256, target_xid, target_version)
		VALUES ($1, $2, '{}'::jsonb, 'SELECT 1', sha256('SELECT 1'::bytea), '1', 1)`, result.JobID, attemptID)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		query string
		id    string
	}{
		{"DELETE FROM app.jobs WHERE id = $1", result.JobID},
		{"DELETE FROM app.job_attempts WHERE id = $1", attemptID},
		{"DELETE FROM app.job_statement_evidence WHERE id = $1", evidenceID},
	} {
		_, err := platform.ExecContext(ctx, tc.query, tc.id)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Errorf("runtime delete %q: expected privilege error, got %v", tc.query, err)
		}
	}
	for _, tc := range []struct {
		query string
		id    string
	}{
		{"DELETE FROM app.jobs WHERE id = $1", result.JobID},
		{"DELETE FROM app.job_attempts WHERE id = $1", attemptID},
		{"DELETE FROM app.database_connections WHERE id = $1", f.dbID},
		{"DELETE FROM app.migration_target_state WHERE id = $1", f.targetID},
		{"DELETE FROM app.migration_proposals WHERE id = $1", f.proposalID},
		{"DELETE FROM app.migration_drafts WHERE id = $1", f.draftID},
		{"DELETE FROM app.users WHERE id = $1", f.actorID},
	} {
		_, err := owner.ExecContext(ctx, tc.query, tc.id)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23503" {
			t.Errorf("parent delete %q: expected foreign key error, got %v", tc.query, err)
		}
	}
	var count int
	if err := owner.QueryRowContext(ctx, "SELECT count(*) FROM app.job_statement_evidence WHERE id = $1", evidenceID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("evidence missing after delete attempts: count=%d err=%v", count, err)
	}
	if _, err := owner.ExecContext(ctx, "DELETE FROM app.job_statement_evidence WHERE id = $1", evidenceID); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		query string
		id    string
	}{
		{"DELETE FROM app.jobs WHERE id = $1", result.JobID},
		{"DELETE FROM app.job_attempts WHERE id = $1", attemptID},
	} {
		_, err := owner.ExecContext(ctx, tc.query, tc.id)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23503" {
			t.Errorf("authorization parent delete %q: %v", tc.query, err)
		}
	}
	for _, tc := range []struct {
		query string
		id    string
	}{
		{"DELETE FROM app.job_dispatch_authorizations WHERE job_id = $1", result.JobID},
		{"DELETE FROM app.job_attempts WHERE id = $1", attemptID},
		{"DELETE FROM app.jobs WHERE id = $1", result.JobID},
	} {
		if _, err := owner.ExecContext(ctx, tc.query, tc.id); err != nil {
			t.Fatalf("owner cleanup %q: %v", tc.query, err)
		}
	}
}
