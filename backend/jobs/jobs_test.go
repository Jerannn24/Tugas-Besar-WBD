package jobs

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Jerannn24/Tugas-Besar-WBD.git/executioncontract"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

type fixture struct {
	dbID       string
	targetID   string
	actorID    string
	draftID    string
	proposalID string
	snapshot   executioncontract.Draft
	hash       string
}

func testDBs(t *testing.T) (*sql.DB, *sql.DB) {
	t.Helper()
	platformDSN := os.Getenv("MIGRATION_TEST_PLATFORM_URL")
	if platformDSN == "" {
		platformDSN = os.Getenv("HISTORY_TEST_DATABASE_URL")
	}
	if platformDSN == "" {
		if os.Getenv("TEST_INTEGRATION_REQUIRED") == "1" {
			t.Fatal("missing test database configuration")
		}
		t.Skip("MIGRATION_TEST_PLATFORM_URL not set")
	}
	platformDB, err := sql.Open("pgx", platformDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { platformDB.Close() })

	ownerDSN := os.Getenv("MIGRATION_TEST_OWNER_URL")
	if ownerDSN == "" {
		ownerDSN = os.Getenv("HISTORY_TEST_DATABASE_URL")
	}
	var ownerDB *sql.DB
	if ownerDSN != "" {
		ownerDB, err = sql.Open("pgx", ownerDSN)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { ownerDB.Close() })
	} else {
		ownerDB = platformDB
	}

	return platformDB, ownerDB
}

func createFixture(t *testing.T, db *sql.DB, ownerDB *sql.DB) fixture {
	t.Helper()
	ctx := context.Background()

	dbID := uuid.NewString()
	targetID := uuid.NewString()
	actorID := uuid.NewString()
	draftID := uuid.NewString()
	proposalID := uuid.NewString()

	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, query := range []string{
			"DELETE FROM app.job_commit_proofs WHERE job_id IN (SELECT id FROM app.jobs WHERE database_id = $1)",
			"DELETE FROM app.job_dispatch_authorizations WHERE job_id IN (SELECT id FROM app.jobs WHERE database_id = $1)",
			"DELETE FROM app.job_statement_evidence WHERE attempt_id IN (SELECT id FROM app.job_attempts WHERE database_id = $1)",
			"DELETE FROM app.job_attempts WHERE database_id = $1",
			"DELETE FROM app.jobs WHERE database_id = $1",
			"DELETE FROM app.migration_proposals WHERE database_id = $1",
			"DELETE FROM app.migration_executions WHERE database_id = $1",
			"DELETE FROM app.migration_drafts WHERE database_id = $1",
			"DELETE FROM app.database_connections WHERE id = $1",
		} {
			if _, err := ownerDB.ExecContext(cleanupCtx, query, dbID); err != nil {
				t.Errorf("cleanup %s: %v", query, err)
			}
		}
		if _, err := ownerDB.ExecContext(cleanupCtx, "DELETE FROM app.migration_target_state WHERE id = $1", targetID); err != nil {
			t.Errorf("cleanup target state: %v", err)
		}
		if _, err := ownerDB.ExecContext(cleanupCtx, "DELETE FROM app.users WHERE id = $1", actorID); err != nil {
			t.Errorf("cleanup user: %v", err)
		}
	})

	username := "u_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	_, err := db.ExecContext(ctx, "INSERT INTO app.users (id, username, password_hash) VALUES ($1, $2, 'hash')", actorID, username)
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}

	_, err = db.ExecContext(ctx, "INSERT INTO app.migration_target_state (id, next_version) VALUES ($1, 1)", targetID)
	if err != nil {
		t.Fatalf("insert target state: %v", err)
	}

	targetKey := "tkey_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	connName := "conn_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	_, err = db.ExecContext(ctx, `
		INSERT INTO app.database_connections
			(id, name, host, port, database_name, username, password_ciphertext, credential_key_id, ssl_mode, target_key, execution_target_id)
		VALUES ($1, $2, 'localhost', 5432, 'testdb', 'user', '\x01', 'key1', 'disable', $3, $4)
	`, dbID, connName, targetKey, targetID)
	if err != nil {
		t.Fatalf("insert connection: %v", err)
	}

	title := "Create Items Table"
	_, err = db.ExecContext(ctx, `
		INSERT INTO app.migration_drafts (id, database_id, revision, title, sql, created_by)
		VALUES ($1, $2, 1, $3, 'CREATE TABLE items (id int);', $4)
	`, draftID, dbID, title, actorID)
	if err != nil {
		t.Fatalf("insert draft: %v", err)
	}

	_, err = db.ExecContext(ctx, `
		INSERT INTO app.migration_proposals (
			id, database_id, migration_id, revision, action, status,
			snapshot, snapshot_hash, created_by
		) VALUES (
			$1, $2, $3, 1, 'apply', 'draft',
			'{"title": "Create Items Table", "sql": "CREATE TABLE items (id int);"}'::jsonb,
			sha256('Create Items Table'::bytea),
			$4
		)
	`, proposalID, dbID, draftID, actorID)
	if err != nil {
		t.Fatalf("insert proposal: %v", err)
	}

	snapshot := executioncontract.Draft{
		MigrationID: draftID,
		DatabaseID:  dbID,
		Revision:    1,
		Title:       title,
		SQLUp:       "CREATE TABLE items (id int);",
		Mode:        executioncontract.Standard,
	}

	return fixture{
		dbID:       dbID,
		targetID:   targetID,
		actorID:    actorID,
		draftID:    draftID,
		proposalID: proposalID,
		snapshot:   snapshot,
		hash:       "hash_00000000000000000000000000000001",
	}
}

type fixtureExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func createFixtureSource(t *testing.T, ctx context.Context, db fixtureExecer, f fixture, source string) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `UPDATE app.migration_target_state SET next_version = next_version + 1 WHERE id = $1`, f.targetID); err != nil {
		t.Fatal(err)
	}
	_, err := db.ExecContext(ctx, `INSERT INTO app.migration_executions
		(id, database_id, execution_target_id, draft_id, request_id, draft_revision, version,
		 title, sql, sql_sha256, executed_by, executed_username, started_at, status)
		SELECT $1, $2, $3, d.id, $5, d.revision, 1, d.title, d.sql,
		       sha256(convert_to(d.sql, 'UTF8')), $6, u.username, clock_timestamp(), 'running'
		FROM app.migration_drafts d JOIN app.users u ON u.id = $6 WHERE d.id = $4`,
		source, f.dbID, f.targetID, f.draftID, uuid.NewString(), f.actorID)
	if err != nil {
		t.Fatalf("insert source execution: %v", err)
	}
}

func createFixtureProposal(t *testing.T, ctx context.Context, db fixtureExecer, f fixture, action executioncontract.Action, source *string) string {
	t.Helper()
	id := uuid.NewString()
	_, err := db.ExecContext(ctx, `INSERT INTO app.migration_proposals
		(id, database_id, migration_id, revision, action, source_execution_id, snapshot, snapshot_hash, created_by)
		VALUES ($1, $2, $3, 1, $4, $5, '{}'::jsonb, sha256('fixture'::bytea), $6)`,
		id, f.dbID, f.draftID, action, source, f.actorID)
	if err != nil {
		t.Fatalf("insert matching proposal: %v", err)
	}
	return id
}

func TestEnqueueTx_PersistAndGetJob(t *testing.T) {
	platformDB, ownerDB := testDBs(t)
	f := createFixture(t, platformDB, ownerDB)
	svc := New(platformDB)

	ctx := context.Background()
	tx, err := platformDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}

	requestID := uuid.NewString()
	req := executioncontract.EnqueueRequest{
		DatabaseID:          f.dbID,
		TargetID:            f.targetID,
		RequestID:           requestID,
		ProposalID:          f.proposalID,
		MigrationID:         f.draftID,
		Revision:            1,
		ActorID:             f.actorID,
		Action:              executioncontract.Apply,
		Direction:           executioncontract.Up,
		Snapshot:            f.snapshot,
		SnapshotHash:        f.hash,
		ApprovalFingerprint: "fingerprint_1",
		Traceparent:         "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
		Tracestate:          "congo=t61rcWkgMzE",
	}

	res, err := svc.EnqueueTx(ctx, tx, req)
	if err != nil {
		tx.Rollback()
		t.Fatalf("EnqueueTx: %v", err)
	}

	if res.Replayed {
		t.Errorf("expected Replayed == false, got true")
	}
	if res.JobID == "" {
		t.Errorf("expected non-empty JobID")
	}
	if res.ExecutionID == "" {
		t.Errorf("expected non-empty ExecutionID")
	}
	if res.Status != executioncontract.Queued {
		t.Errorf("status = %v, want %v", res.Status, executioncontract.Queued)
	}
	if res.DatabaseID != f.dbID {
		t.Errorf("databaseID = %v, want %v", res.DatabaseID, f.dbID)
	}
	if res.ProposalID != f.proposalID {
		t.Errorf("proposalID = %v, want %v", res.ProposalID, f.proposalID)
	}
	wantURL := "/api/databases/" + f.dbID + "/jobs/" + res.JobID
	if res.StatusURL != wantURL {
		t.Errorf("statusURL = %v, want %v", res.StatusURL, wantURL)
	}

	if err := tx.Commit(); err != nil {
		t.Fatalf("commit tx: %v", err)
	}

	jobView, err := svc.GetJob(ctx, f.dbID, res.JobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}

	if jobView.JobID != res.JobID {
		t.Errorf("jobView.JobID = %s, want %s", jobView.JobID, res.JobID)
	}
	if jobView.ExecutionID != res.ExecutionID {
		t.Errorf("jobView.ExecutionID = %s, want %s", jobView.ExecutionID, res.ExecutionID)
	}
	if jobView.MigrationID != f.draftID {
		t.Errorf("jobView.MigrationID = %s, want %s", jobView.MigrationID, f.draftID)
	}
	if jobView.Revision != 1 {
		t.Errorf("jobView.Revision = %d, want 1", jobView.Revision)
	}
	if jobView.Action != executioncontract.Apply {
		t.Errorf("jobView.Action = %v, want %v", jobView.Action, executioncontract.Apply)
	}
	if jobView.Mode != executioncontract.Standard {
		t.Errorf("jobView.Mode = %v, want %v", jobView.Mode, executioncontract.Standard)
	}
	if jobView.ExecutionStatus != executioncontract.Queued {
		t.Errorf("jobView.ExecutionStatus = %v, want %v", jobView.ExecutionStatus, executioncontract.Queued)
	}
	if jobView.AttemptNo != 1 {
		t.Errorf("jobView.AttemptNo = %d, want 1", jobView.AttemptNo)
	}
	if jobView.BlocksDatabase {
		t.Errorf("jobView.BlocksDatabase = true, want false")
	}
	if jobView.Progress != nil {
		t.Errorf("jobView.Progress = %+v, want nil for standard queued job", jobView.Progress)
	}

	// Verify directly in DB table app.jobs
	var storedStatus, storedTitle, storedTraceparent, storedTargetID string
	err = platformDB.QueryRowContext(ctx, "SELECT status, title, traceparent, target_id::text FROM app.jobs WHERE id = $1", res.JobID).Scan(
		&storedStatus, &storedTitle, &storedTraceparent, &storedTargetID,
	)
	if err != nil {
		t.Fatalf("select app.jobs: %v", err)
	}
	if storedStatus != "queued" {
		t.Errorf("db status = %s, want queued", storedStatus)
	}
	if storedTitle != f.snapshot.Title {
		t.Errorf("db title = %s, want %s", storedTitle, f.snapshot.Title)
	}
	if storedTraceparent != req.Traceparent {
		t.Errorf("db traceparent = %s, want %s", storedTraceparent, req.Traceparent)
	}
	if storedTargetID != f.targetID {
		t.Errorf("db target_id = %s, want %s", storedTargetID, f.targetID)
	}
}

func TestEnqueueTx_ConcurrentRequest(t *testing.T) {
	platformDB, ownerDB := testDBs(t)
	svc := New(platformDB)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for _, conflict := range []bool{false, true} {
		name := "replay"
		if conflict {
			name = "conflict"
		}
		t.Run(name, func(t *testing.T) {
			f := createFixture(t, platformDB, ownerDB)
			req := executioncontract.EnqueueRequest{
				DatabaseID:          f.dbID,
				TargetID:            f.targetID,
				ProposalID:          f.proposalID,
				MigrationID:         f.draftID,
				Revision:            1,
				ActorID:             f.actorID,
				Action:              executioncontract.Apply,
				Direction:           executioncontract.Up,
				Snapshot:            f.snapshot,
				SnapshotHash:        f.hash,
				ApprovalFingerprint: "fingerprint_replay",
			}
			first := req
			first.RequestID = uuid.NewString()
			second := first
			if conflict {
				second.ApprovalFingerprint = "changed"
			}

			tx1, err := platformDB.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx1.Rollback()
			want, err := svc.EnqueueTx(ctx, tx1, first)
			if err != nil {
				t.Fatalf("first enqueue: %v", err)
			}
			tx2, err := platformDB.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx2.Rollback()
			var pid int
			if err := tx2.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
				t.Fatal(err)
			}

			type result struct {
				job executioncontract.EnqueueResult
				err error
			}
			done := make(chan result, 1)
			go func() {
				job, err := svc.EnqueueTx(ctx, tx2, second)
				done <- result{job, err}
			}()
			tick := time.NewTicker(10 * time.Millisecond)
			defer tick.Stop()
			for {
				var blocked bool
				if err := platformDB.QueryRowContext(ctx, "SELECT cardinality(pg_blocking_pids($1)) > 0", pid).Scan(&blocked); err != nil {
					t.Fatalf("observe second enqueue waiting: %v", err)
				}
				if blocked {
					break
				}
				select {
				case got := <-done:
					t.Fatalf("second enqueue finished before first committed: %+v", got)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				case <-tick.C:
				}
			}
			if err := tx1.Commit(); err != nil {
				t.Fatalf("commit winner: %v", err)
			}
			got := <-done
			if conflict {
				if !errors.Is(got.err, executioncontract.ErrRequestConflict) {
					t.Fatalf("second enqueue: got %v, want REQUEST_CONFLICT", got.err)
				}
			} else if got.err != nil || !got.job.Replayed || got.job.JobAccepted != want.JobAccepted {
				t.Fatalf("second enqueue: got %+v, want replay of %+v", got, want.JobAccepted)
			}
			if err := tx2.Commit(); err != nil {
				t.Fatalf("second enqueue aborted the caller transaction: %v", err)
			}
		})
	}
}

func TestEnqueueTx_RequestConflict(t *testing.T) {
	platformDB, ownerDB := testDBs(t)
	f := createFixture(t, platformDB, ownerDB)
	svc := New(platformDB)
	ctx := context.Background()

	requestID := uuid.NewString()
	baseReq := executioncontract.EnqueueRequest{
		DatabaseID:          f.dbID,
		TargetID:            f.targetID,
		RequestID:           requestID,
		ProposalID:          f.proposalID,
		MigrationID:         f.draftID,
		Revision:            1,
		ActorID:             f.actorID,
		Action:              executioncontract.Apply,
		Direction:           executioncontract.Up,
		Snapshot:            f.snapshot,
		SnapshotHash:        f.hash,
		ApprovalFingerprint: "fingerprint_conflict",
	}

	tx, err := platformDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	_, err = svc.EnqueueTx(ctx, tx, baseReq)
	if err != nil {
		tx.Rollback()
		t.Fatalf("initial EnqueueTx: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit tx: %v", err)
	}

	assertConflict := func(t *testing.T, name string, modify func(r *executioncontract.EnqueueRequest)) {
		t.Helper()
		req := baseReq
		modify(&req)

		tx, err := platformDB.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("[%s] begin tx: %v", name, err)
		}
		defer tx.Rollback()

		_, err = svc.EnqueueTx(ctx, tx, req)
		if err == nil {
			t.Fatalf("[%s] expected error, got nil", name)
		}
		if !errors.Is(err, executioncontract.ErrRequestConflict) {
			var ce executioncontract.ContractError
			if !errors.As(err, &ce) || ce.Code != "REQUEST_CONFLICT" {
				t.Fatalf("[%s] expected REQUEST_CONFLICT, got %v", name, err)
			}
		}
	}

	t.Run("different revision", func(t *testing.T) {
		assertConflict(t, "revision", func(r *executioncontract.EnqueueRequest) {
			r.Revision = 2
		})
	})
	t.Run("different actor", func(t *testing.T) {
		assertConflict(t, "actor", func(r *executioncontract.EnqueueRequest) {
			r.ActorID = uuid.NewString()
		})
	})
	t.Run("different proposal", func(t *testing.T) {
		assertConflict(t, "proposal", func(r *executioncontract.EnqueueRequest) {
			r.ProposalID = uuid.NewString()
		})
	})
	t.Run("different action", func(t *testing.T) {
		assertConflict(t, "action", func(r *executioncontract.EnqueueRequest) {
			r.Action = executioncontract.Rollback
		})
	})
	t.Run("different hash", func(t *testing.T) {
		assertConflict(t, "hash", func(r *executioncontract.EnqueueRequest) {
			r.SnapshotHash = "altered_hash_000000000000000000"
		})
	})
	t.Run("different fingerprint", func(t *testing.T) {
		assertConflict(t, "fingerprint", func(r *executioncontract.EnqueueRequest) {
			r.ApprovalFingerprint = "altered_fingerprint"
		})
	})
	t.Run("different target", func(t *testing.T) {
		assertConflict(t, "target", func(r *executioncontract.EnqueueRequest) {
			r.TargetID = uuid.NewString()
		})
	})
}

func TestEnqueueTx_RollbackCancelsInsert(t *testing.T) {
	platformDB, ownerDB := testDBs(t)
	f := createFixture(t, platformDB, ownerDB)
	svc := New(platformDB)
	ctx := context.Background()

	requestID := uuid.NewString()
	req := executioncontract.EnqueueRequest{
		DatabaseID:          f.dbID,
		TargetID:            f.targetID,
		RequestID:           requestID,
		ProposalID:          f.proposalID,
		MigrationID:         f.draftID,
		Revision:            1,
		ActorID:             f.actorID,
		Action:              executioncontract.Apply,
		Direction:           executioncontract.Up,
		Snapshot:            f.snapshot,
		SnapshotHash:        f.hash,
		ApprovalFingerprint: "fp_rb",
	}

	tx, err := platformDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	res, err := svc.EnqueueTx(ctx, tx, req)
	if err != nil {
		tx.Rollback()
		t.Fatalf("EnqueueTx: %v", err)
	}

	// Caller aborts / rolls back transaction
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback tx: %v", err)
	}

	// Verify job does not exist via GetJob
	_, err = svc.GetJob(ctx, f.dbID, res.JobID)
	if err == nil {
		t.Fatalf("GetJob after rollback: expected error, got nil")
	}
	var ce executioncontract.ContractError
	if !errors.As(err, &ce) || ce.Code != "NOT_FOUND" {
		t.Errorf("expected NOT_FOUND, got %v", err)
	}

	// Verify directly in DB
	var count int
	err = platformDB.QueryRowContext(ctx, "SELECT count(*) FROM app.jobs WHERE id = $1", res.JobID).Scan(&count)
	if err != nil {
		t.Fatalf("query count: %v", err)
	}
	if count != 0 {
		t.Errorf("job row count = %d, want 0", count)
	}
}

func TestGetJob_ScopedToDatabase(t *testing.T) {
	platformDB, ownerDB := testDBs(t)
	f1 := createFixture(t, platformDB, ownerDB)
	f2 := createFixture(t, platformDB, ownerDB)
	svc := New(platformDB)
	ctx := context.Background()

	tx, err := platformDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	req := executioncontract.EnqueueRequest{
		DatabaseID:          f1.dbID,
		TargetID:            f1.targetID,
		RequestID:           uuid.NewString(),
		ProposalID:          f1.proposalID,
		MigrationID:         f1.draftID,
		Revision:            1,
		ActorID:             f1.actorID,
		Action:              executioncontract.Apply,
		Direction:           executioncontract.Up,
		Snapshot:            f1.snapshot,
		SnapshotHash:        f1.hash,
		ApprovalFingerprint: "fp_scoped",
	}
	res, err := svc.EnqueueTx(ctx, tx, req)
	if err != nil {
		tx.Rollback()
		t.Fatalf("EnqueueTx: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit tx: %v", err)
	}

	// Query with db1 -> found
	view, err := svc.GetJob(ctx, f1.dbID, res.JobID)
	if err != nil {
		t.Fatalf("GetJob on owner database: %v", err)
	}
	if view.JobID != res.JobID {
		t.Errorf("got job ID %s, want %s", view.JobID, res.JobID)
	}

	// Query with db2 -> NOT_FOUND (isolation across databases)
	_, err = svc.GetJob(ctx, f2.dbID, res.JobID)
	if err == nil {
		t.Fatalf("GetJob across database scope: expected error, got nil")
	}
	var ce executioncontract.ContractError
	if !errors.As(err, &ce) || ce.Code != "NOT_FOUND" {
		t.Errorf("expected NOT_FOUND for foreign database, got %v", err)
	}
}

func TestEnqueueTx_ResumeRequiresSourceExecutionID(t *testing.T) {
	platformDB, ownerDB := testDBs(t)
	f := createFixture(t, platformDB, ownerDB)
	svc := New(platformDB)
	ctx := context.Background()

	tx, err := platformDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback()

	reqNoSource := executioncontract.EnqueueRequest{
		DatabaseID:   f.dbID,
		TargetID:     f.targetID,
		RequestID:    uuid.NewString(),
		ProposalID:   f.proposalID,
		MigrationID:  f.draftID,
		Revision:     1,
		ActorID:      f.actorID,
		Action:       executioncontract.Resume,
		Direction:    executioncontract.Up,
		Snapshot:     f.snapshot,
		SnapshotHash: f.hash,
	}

	_, err = svc.EnqueueTx(ctx, tx, reqNoSource)
	if err == nil {
		t.Fatalf("expected error for Resume without SourceExecutionID, got nil")
	}

	sourceExec := uuid.NewString()
	createFixtureSource(t, ctx, tx, f, sourceExec)
	reqWithSource := reqNoSource
	reqWithSource.SourceExecutionID = &sourceExec
	reqWithSource.ProposalID = createFixtureProposal(t, ctx, tx, f, executioncontract.Resume, &sourceExec)

	res, err := svc.EnqueueTx(ctx, tx, reqWithSource)
	if err != nil {
		t.Fatalf("Resume with SourceExecutionID: %v", err)
	}
	if res.ExecutionID != sourceExec {
		t.Errorf("ExecutionID = %s, want sourceExec %s", res.ExecutionID, sourceExec)
	}
}

func TestEnqueueTx_RejectsMismatchedBinding(t *testing.T) {
	platformDB, ownerDB := testDBs(t)
	f := createFixture(t, platformDB, ownerDB)
	svc := New(platformDB)
	ctx := context.Background()

	tx, err := platformDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback()

	mismatchedTargetID := uuid.NewString()
	req := executioncontract.EnqueueRequest{
		DatabaseID:          f.dbID,
		TargetID:            mismatchedTargetID,
		RequestID:           uuid.NewString(),
		ProposalID:          f.proposalID,
		MigrationID:         f.draftID,
		Revision:            1,
		ActorID:             f.actorID,
		Action:              executioncontract.Apply,
		Direction:           executioncontract.Up,
		Snapshot:            f.snapshot,
		SnapshotHash:        f.hash,
		ApprovalFingerprint: "fp_mismatch",
	}

	_, err = svc.EnqueueTx(ctx, tx, req)
	if err == nil {
		t.Fatalf("expected error for mismatched target binding, got nil")
	}
	var ce executioncontract.ContractError
	if !errors.As(err, &ce) || ce.Code != "DATABASE_BUSY" {
		t.Fatalf("expected DATABASE_BUSY, got %v", err)
	}
}

func TestIncompleteJobsPortsFailClosed(t *testing.T) {
	svc := New(nil)
	ctx := context.Background()
	if _, err := svc.ListJobs(ctx, "", executioncontract.Page{}); err == nil {
		t.Fatal("unimplemented ListJobs reported success")
	}
	if _, err := svc.ListAttempts(ctx, "", executioncontract.Page{}); err == nil {
		t.Fatal("unimplemented ListAttempts reported success")
	}
	if err := svc.FinalizeJobTx(ctx, nil, executioncontract.JobUpdate{}); err == nil {
		t.Fatal("unfenced FinalizeJobTx reported success")
	}
}
