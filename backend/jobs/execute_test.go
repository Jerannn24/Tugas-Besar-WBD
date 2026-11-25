package jobs

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Jerannn24/Tugas-Besar-WBD.git/executioncontract"
	"github.com/Jerannn24/Tugas-Besar-WBD.git/migration"
	"github.com/Jerannn24/Tugas-Besar-WBD.git/modules/authz"
	"github.com/Jerannn24/Tugas-Besar-WBD.git/modules/workflow"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type p3TestEnv struct {
	platformDB        *sql.DB
	ownerDB           *sql.DB
	targetExecDB      *sql.DB
	targetPrepDB      *sql.DB
	targetCleanupDB   *sql.DB
	physicalTargetID  string
	targetExecDB_B    *sql.DB
	targetPrepDB_B    *sql.DB
	targetCleanupDB_B *sql.DB
	physicalTargetID_B string
}

func setupP3TestEnv(t *testing.T) *p3TestEnv {
	return setupP3TestEnvWithTracking(t, true)
}

func setupP3TestEnvWithTracking(t *testing.T, enabled bool) *p3TestEnv {
	t.Helper()
	platformDB, ownerDB := testDBs(t)

	execDSN := os.Getenv("MIGRATION_TEST_TARGET_EXECUTOR_URL")
	prepDSN := os.Getenv("MIGRATION_TEST_TARGET_PREPARER_URL")
	cleanupDSN := os.Getenv("MIGRATION_TEST_TARGET_CLEANUP_URL")

	if execDSN == "" || prepDSN == "" {
		if os.Getenv("TEST_INTEGRATION_REQUIRED") == "1" {
			t.Fatal("missing target database configuration")
		}
		t.Skip("target database configuration not set")
	}

	targetExecDB, err := sql.Open("pgx", execDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { targetExecDB.Close() })

	targetPrepDB, err := sql.Open("pgx", prepDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { targetPrepDB.Close() })

	var targetCleanupDB *sql.DB
	if cleanupDSN != "" {
		targetCleanupDB, err = sql.Open("pgx", cleanupDSN)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { targetCleanupDB.Close() })
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var physicalTargetID string
	err = targetPrepDB.QueryRowContext(ctx, "SELECT physical_target_id::text FROM wbd_auth.target_marker WHERE singleton").Scan(&physicalTargetID)
	if err != nil {
		t.Fatalf("query physical target id: %v", err)
	}

	var tracking string
	if err := targetPrepDB.QueryRowContext(ctx, "SHOW track_commit_timestamp").Scan(&tracking); err != nil {
		t.Fatalf("read target commit timestamp setting: %v", err)
	}
	if (tracking == "on") != enabled {
		t.Skipf("requires track_commit_timestamp=%t, got %s", enabled, tracking)
	}

	execDSNB := os.Getenv("MIGRATION_TEST_TARGET_B_EXECUTOR_URL")
	prepDSNB := os.Getenv("MIGRATION_TEST_TARGET_B_PREPARER_URL")
	cleanupDSNB := os.Getenv("MIGRATION_TEST_TARGET_B_CLEANUP_URL")

	var targetExecDB_B, targetPrepDB_B, targetCleanupDB_B *sql.DB
	var physicalTargetID_B string
	if execDSNB != "" && prepDSNB != "" {
		targetExecDB_B, err = sql.Open("pgx", execDSNB)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { targetExecDB_B.Close() })

		targetPrepDB_B, err = sql.Open("pgx", prepDSNB)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { targetPrepDB_B.Close() })

		if cleanupDSNB != "" {
			targetCleanupDB_B, err = sql.Open("pgx", cleanupDSNB)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { targetCleanupDB_B.Close() })
		}

		err = targetPrepDB_B.QueryRowContext(ctx, "SELECT physical_target_id::text FROM wbd_auth.target_marker WHERE singleton").Scan(&physicalTargetID_B)
		if err != nil {
			t.Fatalf("query physical target B id: %v", err)
		}

		var trackingB string
		if err := targetPrepDB_B.QueryRowContext(ctx, "SHOW track_commit_timestamp").Scan(&trackingB); err != nil {
			t.Fatalf("read target B commit timestamp setting: %v", err)
		}
		if (trackingB == "on") != enabled {
			t.Skipf("requires target B track_commit_timestamp=%t, got %s", enabled, trackingB)
		}
	}

	return &p3TestEnv{
		platformDB:        platformDB,
		ownerDB:           ownerDB,
		targetExecDB:      targetExecDB,
		targetPrepDB:      targetPrepDB,
		targetCleanupDB:   targetCleanupDB,
		physicalTargetID:  physicalTargetID,
		targetExecDB_B:    targetExecDB_B,
		targetPrepDB_B:    targetPrepDB_B,
		targetCleanupDB_B: targetCleanupDB_B,
		physicalTargetID_B: physicalTargetID_B,
	}
}

func (e *p3TestEnv) makeAcquire(t *testing.T) func(ctx context.Context, databaseID string) (*migration.TargetLease, error) {
	t.Helper()
	return func(ctx context.Context, databaseID string) (*migration.TargetLease, error) {
		conn, err := e.targetExecDB.Conn(ctx)
		if err != nil {
			return nil, err
		}
		return &migration.TargetLease{
			Executor: conn,
			Preparer: e.targetPrepDB,
			Release: func() {
				_ = conn.Close()
			},
		}, nil
	}
}

func (e *p3TestEnv) makeDualAcquire(t *testing.T, dbID_A, dbID_B string) func(ctx context.Context, databaseID string) (*migration.TargetLease, error) {
	t.Helper()
	return func(ctx context.Context, databaseID string) (*migration.TargetLease, error) {
		switch databaseID {
		case dbID_A:
			conn, err := e.targetExecDB.Conn(ctx)
			if err != nil {
				return nil, err
			}
			return &migration.TargetLease{
				Executor: conn,
				Preparer: e.targetPrepDB,
				Release: func() {
					_ = conn.Close()
				},
			}, nil
		case dbID_B:
			if e.targetExecDB_B == nil || e.targetPrepDB_B == nil {
				return nil, errors.New("target B not configured")
			}
			conn, err := e.targetExecDB_B.Conn(ctx)
			if err != nil {
				return nil, err
			}
			return &migration.TargetLease{
				Executor: conn,
				Preparer: e.targetPrepDB_B,
				Release: func() {
					_ = conn.Close()
				},
			}, nil
		default:
			return nil, errors.New("unknown database ID")
		}
	}
}

type p3JobFixture struct {
	dbID       string
	targetID   string
	actorID    string
	draftID    string
	proposalID string
	tableName  string
	claim      *Claim
	snapshot   executioncontract.Draft
}

func createP3JobFixture(t *testing.T, env *p3TestEnv, action executioncontract.Action, direction executioncontract.Direction, sqlUp, sqlDown string) p3JobFixture {
	return createP3JobFixtureOnConnection(t, env, nil, action, direction, sqlUp, sqlDown)
}

func createP3JobFixtureOnConnection(t *testing.T, env *p3TestEnv, shared *p3JobFixture, action executioncontract.Action, direction executioncontract.Direction, sqlUp, sqlDown string) p3JobFixture {
	return createP3JobFixtureFull(t, env, env.physicalTargetID, env.targetExecDB, env.targetCleanupDB, "target_a", "worker-p3-test", shared, action, direction, sqlUp, sqlDown)
}

func createP3JobFixtureFull(t *testing.T, env *p3TestEnv, targetID string, targetExecDB, targetCleanupDB *sql.DB, dbName, workerID string, shared *p3JobFixture, action executioncontract.Action, direction executioncontract.Direction, sqlUp, sqlDown string) p3JobFixture {
	t.Helper()
	ctx := context.Background()

	dbID := uuid.NewString()
	actorID := uuid.NewString()
	draftID := uuid.NewString()
	proposalID := uuid.NewString()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:10]
	tableName := "items_" + suffix
	if shared != nil {
		dbID = shared.dbID
		actorID = shared.actorID
	}

	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if targetCleanupDB != nil {
			for _, query := range []string{
				"DELETE FROM wbd_auth.execution_receipts WHERE database_id = $1",
				"DELETE FROM wbd_auth.execution_tickets WHERE database_id = $1",
			} {
				if _, err := targetCleanupDB.ExecContext(cleanupCtx, query, dbID); err != nil {
					t.Errorf("cleanup target %s: %v", query, err)
				}
			}
		}
		if targetExecDB != nil {
			if _, err := targetExecDB.ExecContext(cleanupCtx, "DROP TABLE IF EXISTS managed."+tableName); err != nil {
				t.Errorf("cleanup target table: %v", err)
			}
		}
		for _, query := range []string{
			"DELETE FROM app.job_commit_proofs WHERE job_id IN (SELECT id FROM app.jobs WHERE database_id = $1)",
			"DELETE FROM app.job_dispatch_authorizations WHERE job_id IN (SELECT id FROM app.jobs WHERE database_id = $1)",
			"DELETE FROM app.job_statement_evidence WHERE attempt_id IN (SELECT id FROM app.job_attempts WHERE database_id = $1)",
			"DELETE FROM app.job_attempts WHERE database_id = $1",
			"DELETE FROM app.jobs WHERE database_id = $1",
			"DELETE FROM app.migration_proposals WHERE database_id = $1",
			"DELETE FROM app.migration_drafts WHERE database_id = $1",
			"DELETE FROM app.database_connections WHERE id = $1",
		} {
			if _, err := env.ownerDB.ExecContext(cleanupCtx, query, dbID); err != nil {
				t.Errorf("cleanup platform %s: %v", query, err)
			}
		}
		if _, err := env.ownerDB.ExecContext(cleanupCtx, "DELETE FROM app.users WHERE id = $1", actorID); err != nil {
			t.Errorf("cleanup user: %v", err)
		}
	})

	var err error
	if shared == nil {
		username := "u_" + suffix
		_, err = env.platformDB.ExecContext(ctx, "INSERT INTO app.users (id, username, password_hash) VALUES ($1, $2, 'hash')", actorID, username)
		if err != nil {
			t.Fatalf("insert user: %v", err)
		}

		_, _ = env.platformDB.ExecContext(ctx, "INSERT INTO app.migration_target_state (id, next_version) VALUES ($1, 1) ON CONFLICT (id) DO NOTHING", targetID)

		targetKey := "tkey_" + suffix
		connName := "conn_" + suffix
		_, err = env.platformDB.ExecContext(ctx, `
			INSERT INTO app.database_connections
				(id, name, host, port, database_name, username, password_ciphertext, credential_key_id, ssl_mode, target_key, execution_target_id)
			VALUES ($1, $2, 'postgres-test', 5432, $3, 'user', '\x01', 'key1', 'disable', $4, $5)
		`, dbID, connName, dbName, targetKey, targetID)
		if err != nil {
			t.Fatalf("insert connection: %v", err)
		}
	}

	var downVal *string
	if sqlDown != "" {
		downVal = &sqlDown
	}

	title := "P3 Migration " + suffix
	_, err = env.platformDB.ExecContext(ctx, `
		INSERT INTO app.migration_drafts (id, database_id, revision, title, sql, sql_down, created_by)
		VALUES ($1, $2, 1, $3, $4, $5, $6)
	`, draftID, dbID, title, sqlUp, downVal, actorID)
	if err != nil {
		t.Fatalf("insert draft: %v", err)
	}

	snapshot := executioncontract.Draft{
		MigrationID: draftID,
		DatabaseID:  dbID,
		Revision:    1,
		Title:       title,
		SQLUp:       sqlUp,
		SQLDown:     downVal,
		Mode:        executioncontract.Standard,
	}

	activeSQL := sqlUp
	if direction == executioncontract.Down && sqlDown != "" {
		activeSQL = sqlDown
	}
	activeHashBytes := sha256.Sum256([]byte(activeSQL))
	snapshotHash := "hash_" + suffix

	_, err = env.platformDB.ExecContext(ctx, `
		INSERT INTO app.migration_proposals (
			id, database_id, migration_id, revision, action, status,
			snapshot, snapshot_hash, created_by
		) VALUES ($1, $2, $3, 1, $4, 'draft', '{}'::jsonb, $5, $6)
	`, proposalID, dbID, draftID, string(action), activeHashBytes[:], actorID)
	if err != nil {
		t.Fatalf("insert proposal: %v", err)
	}

	svc := New(env.platformDB)
	enqReq := executioncontract.EnqueueRequest{
		DatabaseID:          dbID,
		TargetID:            targetID,
		RequestID:           uuid.NewString(),
		ProposalID:          proposalID,
		MigrationID:         draftID,
		Revision:            1,
		ActorID:             actorID,
		Action:              action,
		Direction:           direction,
		Snapshot:            snapshot,
		SnapshotHash:        snapshotHash,
		ApprovalFingerprint: "fingerprint_" + suffix,
	}

	ptx, err := env.platformDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin enq tx: %v", err)
	}
	_, err = svc.EnqueueTx(ctx, ptx, enqReq)
	if err != nil {
		ptx.Rollback()
		t.Fatalf("enqueue tx: %v", err)
	}
	if err := ptx.Commit(); err != nil {
		t.Fatalf("commit enq tx: %v", err)
	}

	claim, err := svc.ClaimJob(ctx, ClaimRequest{
		WorkerID:      workerID,
		LeaseDuration: 45 * time.Second,
	})
	if err != nil || claim == nil {
		t.Fatalf("claim job: claim=%+v err=%v", claim, err)
	}

	return p3JobFixture{
		dbID:       dbID,
		targetID:   targetID,
		actorID:    actorID,
		draftID:    draftID,
		proposalID: proposalID,
		tableName:  tableName,
		snapshot:   snapshot,
		claim:      claim,
	}
}

func TestExecuteStandardJob_StatementFailureRollsBack(t *testing.T) {
	env := setupP3TestEnv(t)
	svc := New(env.platformDB)
	ctx := context.Background()
	table := "rollback_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	t.Cleanup(func() {
		_, _ = env.targetExecDB.ExecContext(context.Background(), "DROP TABLE IF EXISTS managed."+table)
	})
	statement := "CREATE TABLE managed." + table + " (id bigint)"
	fix := createP3JobFixture(t, env, executioncontract.Apply, executioncontract.Up, statement+"; "+statement+";", "")
	gate := executioncontract.FakeGate{
		RevalidateFunc: func(_ context.Context, req executioncontract.RevalidationRequest) (executioncontract.ExecutionPlan, error) {
			return executioncontract.ExecutionPlan{Request: req, Snapshot: fix.snapshot, Version: "1"}, nil
		},
	}
	outcome, err := svc.ExecuteStandardJob(ctx, fix.claim, ExecutionDeps{Gate: gate, Acquire: env.makeAcquire(t), Timeout: 10 * time.Second})
	if err != nil || outcome == nil || outcome.Status != executioncontract.Failed || outcome.Error == nil {
		t.Fatalf("expected confirmed statement failure: outcome=%+v err=%v", outcome, err)
	}
	job, err := svc.GetJob(ctx, fix.dbID, fix.claim.JobID)
	if err != nil || job.Status != executioncontract.Failed || job.BlocksDatabase {
		t.Fatalf("expected unblocked failed job: job=%+v err=%v", job, err)
	}
	var failedEvidence bool
	if err := env.platformDB.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM app.job_statement_evidence WHERE attempt_id = $1 AND outcome = 'failed')
	`, fix.claim.AttemptID).Scan(&failedEvidence); err != nil || !failedEvidence {
		t.Fatalf("expected failed statement evidence: found=%v err=%v", failedEvidence, err)
	}
	var receipt bool
	if err := env.targetPrepDB.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM wbd_auth.execution_receipts WHERE execution_id = $1)
	`, fix.claim.ExecutionID).Scan(&receipt); err != nil || receipt {
		t.Fatalf("unexpected receipt after rollback: found=%v err=%v", receipt, err)
	}
	var exists bool
	if err := env.targetExecDB.QueryRowContext(ctx, `SELECT to_regclass($1) IS NOT NULL`, "managed."+table).Scan(&exists); err != nil || exists {
		t.Fatalf("target transaction did not roll back: exists=%v err=%v", exists, err)
	}
}

func p3CommitGate(t *testing.T, fix p3JobFixture, calls *int, sqlText, count string) executioncontract.FakeGate {
	t.Helper()
	return executioncontract.FakeGate{
		RevalidateFunc: func(_ context.Context, req executioncontract.RevalidationRequest) (executioncontract.ExecutionPlan, error) {
			return executioncontract.ExecutionPlan{Request: req, Snapshot: fix.snapshot, Version: "1", ActorUsername: "testactor", ValidatedAt: time.Now().UTC()}, nil
		},
		RecordCommitFunc: func(ctx context.Context, tx *sql.Tx, evidence executioncontract.CommitEvidence) error {
			(*calls)++
			if tx == nil || evidence.JobID != fix.claim.JobID || evidence.ExecutionID != fix.claim.ExecutionID ||
				evidence.Action != fix.claim.Action || evidence.Direction != fix.claim.Direction ||
				evidence.SQLSnapshot != sqlText || evidence.RowCount != count || evidence.CommittedAt.IsZero() {
				t.Fatalf("invalid commit evidence: %+v", evidence)
			}
			_, err := tx.ExecContext(ctx, `UPDATE app.job_statement_evidence SET safe_parameters = '{"recorded":true}'::jsonb WHERE attempt_id = $1 AND ordinal = 1`, fix.claim.AttemptID)
			return err
		},
	}
}

func p3AssertCommitState(t *testing.T, env *p3TestEnv, fix p3JobFixture, recorded bool, status executioncontract.JobStatus, blocked bool) {
	t.Helper()
	ctx := context.Background()
	var gotRecorded bool
	if err := env.platformDB.QueryRowContext(ctx, `SELECT COALESCE((safe_parameters->>'recorded')::boolean, false) FROM app.job_statement_evidence WHERE attempt_id = $1 AND ordinal = 1`, fix.claim.AttemptID).Scan(&gotRecorded); err != nil || gotRecorded != recorded {
		t.Fatalf("transactional commit marker: recorded=%v err=%v", gotRecorded, err)
	}
	job, err := New(env.platformDB).GetJob(ctx, fix.dbID, fix.claim.JobID)
	if err != nil || job.Status != status || job.BlocksDatabase != blocked {
		t.Fatalf("job state: job=%+v err=%v", job, err)
	}
}

func TestExecuteStandardJob_ApplyAndRollback(t *testing.T) {
	for _, direction := range []executioncontract.Direction{executioncontract.Up, executioncontract.Down} {
		t.Run(string(direction), func(t *testing.T) {
			env := setupP3TestEnv(t)
			ctx := context.Background()
			table := "p3_commit_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
			t.Cleanup(func() {
				_, _ = env.targetExecDB.ExecContext(context.Background(), "DROP TABLE IF EXISTS managed."+table)
			})
			sqlUp := "INSERT INTO managed." + table + " (id) VALUES (1), (2), (3)"
			sqlDown := "DELETE FROM managed." + table + " WHERE id < 3"
			if _, err := env.targetExecDB.ExecContext(ctx, "CREATE TABLE managed."+table+" (id bigint primary key)"); err != nil {
				t.Fatal(err)
			}
			action, sqlText, count, remaining, observedWant := executioncontract.Apply, sqlUp, "3", 3, 3
			if direction == executioncontract.Down {
				action, sqlText, count, remaining, observedWant = executioncontract.Rollback, sqlDown, "2", 1, 2
				if _, err := env.targetExecDB.ExecContext(ctx, sqlUp); err != nil {
					t.Fatal(err)
				}
			}
			fix := createP3JobFixture(t, env, action, direction, sqlUp, sqlDown)
			calls := 0
			gate := p3CommitGate(t, fix, &calls, sqlText, count)
			outcome, err := New(env.platformDB).ExecuteStandardJob(ctx, fix.claim, ExecutionDeps{Gate: gate, Acquire: env.makeAcquire(t), Timeout: 10 * time.Second})
			if err != nil || outcome == nil || outcome.Status != executioncontract.Succeeded || outcome.Error != nil || calls != 1 {
				t.Fatalf("commit: outcome=%+v calls=%d err=%v", outcome, calls, err)
			}
			p3AssertCommitState(t, env, fix, true, executioncontract.Succeeded, false)
			var rows, observed int
			if err := env.targetExecDB.QueryRowContext(ctx, "SELECT count(*) FROM managed."+table).Scan(&rows); err != nil || rows != remaining {
				t.Fatalf("target rows=%d err=%v", rows, err)
			}
			if err := env.platformDB.QueryRowContext(ctx, `SELECT row_count FROM app.job_statement_evidence WHERE attempt_id = $1 AND ordinal = 1`, fix.claim.AttemptID).Scan(&observed); err != nil || observed != observedWant {
				t.Fatalf("observed row count=%d err=%v", observed, err)
			}
		})
	}
}

func TestExecuteStandardJob_CrashAfterTargetCommit_RecoveryWithoutReplay(t *testing.T) {
	env := setupP3TestEnv(t)
	ctx := context.Background()
	table := "p3_recovery_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	t.Cleanup(func() {
		_, _ = env.targetExecDB.ExecContext(context.Background(), "DROP TABLE IF EXISTS managed."+table)
	})
	if _, err := env.targetExecDB.ExecContext(ctx, "CREATE TABLE managed."+table+" (id bigint primary key)"); err != nil {
		t.Fatal(err)
	}
	sqlUp := "INSERT INTO managed." + table + " (id) VALUES (777)"
	fix := createP3JobFixture(t, env, executioncontract.Apply, executioncontract.Up, sqlUp, "")
	calls := 0
	gate := p3CommitGate(t, fix, &calls, sqlUp, "1")
	crashErr := errors.New("simulated crash")
	deps := ExecutionDeps{Gate: gate, Acquire: env.makeAcquire(t), Timeout: 10 * time.Second, afterTargetCommit: func(context.Context) error { return crashErr }}
	outcome, err := New(env.platformDB).ExecuteStandardJob(ctx, fix.claim, deps)
	if err != nil || outcome == nil || outcome.Status != executioncontract.Unknown || !errors.Is(outcome.Error, crashErr) || calls != 0 {
		t.Fatalf("crash: outcome=%+v calls=%d err=%v", outcome, calls, err)
	}
	p3AssertCommitState(t, env, fix, false, executioncontract.Unknown, true)
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	function, trigger := "p3_fail_"+suffix, "p3_fail_"+suffix
	_, err = env.ownerDB.ExecContext(ctx, `CREATE FUNCTION app.`+function+`() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.id = '`+fix.claim.JobID+`'::uuid AND NEW.status = 'succeeded' THEN RAISE EXCEPTION 'injected platform write failure'; END IF; RETURN NEW; END $$`)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = env.ownerDB.ExecContext(context.Background(), `DROP FUNCTION IF EXISTS app.`+function+`() CASCADE`)
	})
	if _, err := env.ownerDB.ExecContext(ctx, `CREATE TRIGGER `+trigger+` BEFORE UPDATE ON app.jobs FOR EACH ROW EXECUTE FUNCTION app.`+function+`() `); err != nil {
		t.Fatal(err)
	}
	gate.RevalidateFunc = func(context.Context, executioncontract.RevalidationRequest) (executioncontract.ExecutionPlan, error) {
		t.Fatal("receipt recovery must not request new approval")
		return executioncontract.ExecutionPlan{}, nil
	}
	deps = ExecutionDeps{Gate: gate, Acquire: env.makeAcquire(t), Timeout: 10 * time.Second}
	_, _ = env.platformDB.ExecContext(ctx, "UPDATE app.jobs SET lease_expires_at = clock_timestamp() - interval '1 second' WHERE id = $1", fix.claim.JobID)
	outcome, err = New(env.platformDB).ReconcileStandardJobByID(ctx, fix.claim.JobID, deps)
	if err == nil && (outcome == nil || outcome.Error == nil) {
		t.Fatalf("injected platform failure not surfaced: outcome=%+v err=%v", outcome, err)
	}
	if calls != 1 {
		t.Fatalf("expected transactional fake to run before failure, got %d", calls)
	}
	p3AssertCommitState(t, env, fix, false, executioncontract.Unknown, true)
	if _, err := env.ownerDB.ExecContext(ctx, `DROP TRIGGER `+trigger+` ON app.jobs`); err != nil {
		t.Fatal(err)
	}
	outcome, err = New(env.platformDB).ReconcileStandardJobByID(ctx, fix.claim.JobID, deps)
	if err != nil || outcome == nil || outcome.Status != executioncontract.Succeeded || outcome.Error != nil || calls != 2 {
		t.Fatalf("recovery: outcome=%+v calls=%d err=%v", outcome, calls, err)
	}
	p3AssertCommitState(t, env, fix, true, executioncontract.Succeeded, false)
	outcome, err = New(env.platformDB).ReconcileStandardJob(ctx, fix.claim, deps)
	if err != nil || outcome == nil || outcome.Status != executioncontract.Succeeded || outcome.Error != nil || calls != 2 {
		t.Fatalf("idempotent reconciliation: outcome=%+v calls=%d err=%v", outcome, calls, err)
	}
	var rows, receipts, attempts int
	if err := env.targetExecDB.QueryRowContext(ctx, "SELECT count(*) FROM managed."+table).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("target rows=%d err=%v", rows, err)
	}
	if err := env.targetPrepDB.QueryRowContext(ctx, `SELECT count(*) FROM wbd_auth.execution_receipts WHERE execution_id = $1`, fix.claim.ExecutionID).Scan(&receipts); err != nil || receipts != 1 {
		t.Fatalf("target receipts=%d err=%v", receipts, err)
	}
	if err := env.platformDB.QueryRowContext(ctx, `SELECT count(*) FROM app.job_attempts WHERE job_id = $1`, fix.claim.JobID).Scan(&attempts); err != nil || attempts != 1 {
		t.Fatalf("attempts=%d err=%v", attempts, err)
	}
}

func TestReconcileStandardJob_MissingReceiptDoesNotDispatch(t *testing.T) {
	env := setupP3TestEnv(t)
	svc := New(env.platformDB)
	ctx := context.Background()
	tableName := "items_p3_missing_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	sqlUp := "UPDATE managed." + tableName + " SET value = 2 WHERE id = 1"
	fix := createP3JobFixture(t, env, executioncontract.Apply, executioncontract.Up, sqlUp, "")
	_, err := env.targetExecDB.ExecContext(ctx, "CREATE TABLE managed."+tableName+" (id bigint primary key, value bigint); INSERT INTO managed."+tableName+" VALUES (1, 1)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = env.targetExecDB.ExecContext(context.Background(), "DROP TABLE IF EXISTS managed."+tableName)
	})
	if _, err := env.platformDB.ExecContext(ctx, `UPDATE app.job_attempts SET sql_entered = true WHERE id = $1`, fix.claim.AttemptID); err != nil {
		t.Fatalf("mark attempt unresolved: %v", err)
	}
	var receiptExists bool
	if err := env.targetPrepDB.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM wbd_auth.execution_receipts WHERE execution_id = $1)", fix.claim.ExecutionID).Scan(&receiptExists); err != nil || receiptExists {
		t.Fatalf("expected no receipt: exists=%v err=%v", receiptExists, err)
	}

	revalidations, commits := 0, 0
	gate := executioncontract.FakeGate{
		RevalidateFunc: func(ctx context.Context, req executioncontract.RevalidationRequest) (executioncontract.ExecutionPlan, error) {
			revalidations++
			return executioncontract.ExecutionPlan{Request: req, Snapshot: fix.snapshot, Version: "1"}, nil
		},
		RecordCommitFunc: func(ctx context.Context, tx *sql.Tx, evidence executioncontract.CommitEvidence) error {
			commits++
			return nil
		},
	}
	deps := ExecutionDeps{Gate: gate, Acquire: env.makeAcquire(t), Timeout: 10 * time.Second}
	staleClaim := *fix.claim
	staleClaim.LeaseToken = uuid.NewString()
	for _, claim := range []*Claim{fix.claim, &staleClaim} {
		outcome, err := svc.ReconcileStandardJob(ctx, claim, deps)
		if err != nil || outcome == nil || outcome.Status != executioncontract.Unknown || outcome.Error == nil || outcome.Reconciled {
			t.Fatalf("expected unconfirmed outcome, got outcome=%+v err=%v", outcome, err)
		}
	}
	if revalidations != 0 || commits != 0 {
		t.Fatalf("receipt absence must not call gate: revalidations=%d commits=%d", revalidations, commits)
	}
	var value int
	if err := env.targetExecDB.QueryRowContext(ctx, "SELECT value FROM managed."+tableName+" WHERE id = 1").Scan(&value); err != nil || value != 1 {
		t.Fatalf("target data changed: value=%d err=%v", value, err)
	}
	var jobStatus, executionStatus, attemptStatus string
	var sqlEntered bool
	err = env.platformDB.QueryRowContext(ctx, `
		SELECT j.status, j.execution_status, a.status, a.sql_entered
		FROM app.jobs j JOIN app.job_attempts a ON a.job_id = j.id
		WHERE j.id = $1 AND a.id = $2
	`, fix.claim.JobID, fix.claim.AttemptID).Scan(&jobStatus, &executionStatus, &attemptStatus, &sqlEntered)
	if err != nil || jobStatus != "running" || executionStatus != "running" || attemptStatus != "running" || !sqlEntered {
		t.Fatalf("unresolved state changed: job=%s execution=%s attempt=%s entered=%v err=%v", jobStatus, executionStatus, attemptStatus, sqlEntered, err)
	}
}

// 4. Decisive test for rejection before SQL on stale/denied gate
func TestExecuteStandardJob_RejectionBeforeSQL_StaleGate(t *testing.T) {
	env := setupP3TestEnv(t)
	svc := New(env.platformDB)
	ctx := context.Background()

	tableName := "items_p3_stale_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	sqlUp := "CREATE TABLE managed." + tableName + " (id bigint primary key);"

	fix := createP3JobFixture(t, env, executioncontract.Apply, executioncontract.Up, sqlUp, "")

	staleErr := executioncontract.ContractError{Code: "REVIEW_STALE", Message: "leader changed approval expired"}
	fakeGate := executioncontract.FakeGate{
		RevalidateFunc: func(ctx context.Context, req executioncontract.RevalidationRequest) (executioncontract.ExecutionPlan, error) {
			return executioncontract.ExecutionPlan{}, staleErr
		},
	}

	deps := ExecutionDeps{
		Gate:    fakeGate,
		Acquire: env.makeAcquire(t),
		Timeout: 10 * time.Second,
	}

	_, err := svc.ExecuteStandardJob(ctx, fix.claim, deps)
	if err == nil || !strings.Contains(err.Error(), "ReturnToReviewTx") {
		t.Fatalf("expected missing atomic stale-review contract, got %v", err)
	}

	// Verify ZERO SQL was run: table must not exist
	var exists bool
	err = env.targetExecDB.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM pg_tables WHERE schemaname = 'managed' AND tablename = $1)
	`, tableName).Scan(&exists)
	if err != nil || exists {
		t.Fatalf("table must not exist on stale gate rejection: exists=%v", exists)
	}

	// Verify sql_entered is false in attempt
	var sqlEntered bool
	var attStatus string
	err = env.platformDB.QueryRowContext(ctx, "SELECT sql_entered, status FROM app.job_attempts WHERE id = $1", fix.claim.AttemptID).Scan(&sqlEntered, &attStatus)
	if err != nil || sqlEntered || attStatus != "running" {
		t.Fatalf("attempt should remain running without SQL: entered=%v status=%s", sqlEntered, attStatus)
	}

	// Verify statement evidence has 0 rows
	var evCount int
	err = env.platformDB.QueryRowContext(ctx, "SELECT count(*) FROM app.job_statement_evidence WHERE attempt_id = $1", fix.claim.AttemptID).Scan(&evCount)
	if err != nil || evCount != 0 {
		t.Fatalf("statement evidence must have 0 rows on stale rejection, got %d", evCount)
	}

	jobView, err := svc.GetJob(ctx, fix.dbID, fix.claim.JobID)
	if err != nil || jobView.Status != executioncontract.Running {
		t.Fatalf("expected job to remain running, got %s", jobView.Status)
	}
}

type afterWorkflowValidation struct {
	*workflow.Gate
	after func(executioncontract.ExecutionPlan)
}

func (g afterWorkflowValidation) Revalidate(ctx context.Context, in executioncontract.RevalidationRequest) (executioncontract.ExecutionPlan, error) {
	plan, err := g.Gate.Revalidate(ctx, in)
	if err == nil {
		g.after(plan)
	}
	return plan, err
}

func TestExecuteStandardJob_RealReviewChangesBeforeAuthorization(t *testing.T) {
	env := setupP3TestEnv(t)
	ctx := context.Background()
	svc := New(env.platformDB)
	table := "stale_review_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	groupID := uuid.New()
	t.Cleanup(func() {
		if _, err := env.ownerDB.ExecContext(ctx, `DELETE FROM app.groups WHERE id=$1`, groupID); err != nil {
			t.Error(err)
		}
	})
	fix := createP3JobFixture(t, env, executioncontract.Apply, executioncontract.Up,
		"CREATE TABLE managed."+table+" (id bigint);", "DROP TABLE managed."+table+";")
	if err := svc.failJob(ctx, fix.claim, "FIXTURE", "Replace test-only proposal"); err != nil {
		t.Fatal(err)
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := env.platformDB.ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`UPDATE app.migration_proposals SET status='superseded' WHERE id=$1`, fix.proposalID)
	exec(`INSERT INTO app.groups(id,name,leader_user_id) VALUES ($1,$2,$3)`, groupID, "Group "+table, fix.actorID)
	t.Cleanup(func() {
		for _, query := range []string{
			`DELETE FROM app.group_database_grants WHERE group_id=$1`,
			`DELETE FROM app.group_memberships WHERE group_id=$1`,
		} {
			if _, err := env.ownerDB.ExecContext(ctx, query, groupID); err != nil {
				t.Error(err)
			}
		}
	})
	exec(`INSERT INTO app.group_memberships(group_id,user_id) VALUES ($1,$2)`, groupID, fix.actorID)
	exec(`INSERT INTO app.group_database_grants(group_id,database_id,access) VALUES ($1,$2,'edit')`, groupID, fix.dbID)

	platformURL := os.Getenv("MIGRATION_TEST_PLATFORM_URL")
	if platformURL == "" {
		platformURL = os.Getenv("HISTORY_TEST_DATABASE_URL")
	}
	pool, err := pgxpool.New(ctx, platformURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	reviews, err := workflow.NewService(pool, authz.NewService(authz.NewRepository(pool)))
	if err != nil {
		t.Fatal(err)
	}
	reviews.SetJobs(svc)
	reviews.SetPlatform(env.platformDB)
	actor := uuid.MustParse(fix.actorID)
	proposal, err := reviews.Create(ctx, actor, workflow.CreateInput{DatabaseID: fix.dbID, MigrationID: fix.draftID, Action: "apply"})
	if err != nil {
		t.Fatal(err)
	}
	proposalID := uuid.MustParse(proposal.ProposalID)
	if _, err := reviews.Submit(ctx, actor, proposalID, proposal.Revision); err != nil {
		t.Fatal(err)
	}
	if approved, err := reviews.Approve(ctx, actor, proposalID, groupID, 1); err != nil || approved.Status != "approved" {
		t.Fatalf("approve: status=%q err=%v", approved.Status, err)
	}
	input := workflow.EnqueueInput{RequestID: uuid.NewString(), Revision: proposal.Revision}
	job, err := reviews.Enqueue(ctx, actor, proposalID, input)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := env.ownerDB.ExecContext(ctx, `DELETE FROM app.job_execution_versions WHERE execution_id=$1`, job.ExecutionID); err != nil {
			t.Error(err)
		}
	})
	claim, err := svc.ClaimJob(ctx, ClaimRequest{WorkerID: "stale-review-worker", LeaseDuration: 45 * time.Second})
	if err != nil || claim == nil || claim.JobID != job.JobID {
		t.Fatalf("claim approved job: claim=%+v err=%v", claim, err)
	}
	gate := workflow.NewGate(pool)
	deps := ExecutionDeps{Acquire: env.makeAcquire(t), Timeout: 10 * time.Second, Gate: afterWorkflowValidation{
		Gate: gate, after: func(plan executioncontract.ExecutionPlan) {
			again, err := gate.Revalidate(ctx, claim.RevalidationRequest())
			if err != nil || plan.Version == "" || plan.Version != again.Version {
				t.Fatalf("revalidation should reuse a reserved target version: %q, %q, err=%v", plan.Version, again.Version, err)
			}
			exec(`UPDATE app.group_database_grants SET access='view' WHERE group_id=$1 AND database_id=$2`, groupID, fix.dbID)
		},
	}}
	outcome, err := svc.ExecuteStandardJob(ctx, claim, deps)
	if err != nil || outcome == nil || outcome.Status != executioncontract.Failed {
		t.Fatalf("execute stale job: outcome=%+v err=%v", outcome, err)
	}
	var code, attemptStatus, approvalStatus string
	var entered, authorized bool
	err = env.platformDB.QueryRowContext(ctx, `
		SELECT j.error_code, a.status, a.sql_entered, p.status,
		       EXISTS (SELECT 1 FROM app.job_dispatch_authorizations d WHERE d.job_id=j.id)
		FROM app.jobs j JOIN app.job_attempts a ON a.job_id=j.id
		JOIN app.migration_proposals p ON p.id=j.proposal_id
		WHERE j.id=$1 AND a.id=$2`, claim.JobID, claim.AttemptID).
		Scan(&code, &attemptStatus, &entered, &approvalStatus, &authorized)
	if err != nil || code != "REVIEW_STALE" || attemptStatus != "failed" || entered || authorized || approvalStatus != "pending_review" {
		t.Fatalf("stale state: code=%s attempt=%s entered=%v authorized=%v review=%s err=%v", code, attemptStatus, entered, authorized, approvalStatus, err)
	}
	exec(`UPDATE app.group_database_grants SET access='edit' WHERE group_id=$1 AND database_id=$2`, groupID, fix.dbID)
	replay, err := reviews.Enqueue(ctx, actor, proposalID, input)
	if err != nil || !replay.Replayed || replay.JobID != job.JobID || replay.Status != executioncontract.Failed {
		t.Fatalf("stale replay: result=%+v err=%v", replay, err)
	}
	different := input
	different.Revision++
	if _, err := reviews.Enqueue(ctx, actor, proposalID, different); !errors.Is(err, executioncontract.ErrRequestConflict) {
		t.Fatalf("changed revision replay: %v", err)
	}
	different = input
	different.RequestID = uuid.NewString()
	if _, err := reviews.Enqueue(ctx, actor, proposalID, different); !errors.Is(err, workflow.ErrConflict) {
		t.Fatalf("pending review admitted a new request: %v", err)
	}
	if err := env.platformDB.QueryRowContext(ctx, `SELECT status FROM app.migration_proposals WHERE id=$1`, proposalID).Scan(&approvalStatus); err != nil || approvalStatus != "pending_review" {
		t.Fatalf("replay changed review state: status=%s err=%v", approvalStatus, err)
	}
	exec(`UPDATE app.group_database_grants SET access='view' WHERE group_id=$1 AND database_id=$2`, groupID, fix.dbID)
	var exists bool
	if err := env.targetExecDB.QueryRowContext(ctx, "SELECT to_regclass($1) IS NOT NULL", "managed."+table).Scan(&exists); err != nil || exists {
		t.Fatalf("stale SQL must not run: table=%v err=%v", exists, err)
	}
	if approved, err := reviews.Approve(ctx, actor, proposalID, groupID, 1); err != nil || approved.Status != "approved" {
		t.Fatalf("approve fresh round: status=%q err=%v", approved.Status, err)
	}
	exec(`UPDATE app.group_database_grants SET access='edit' WHERE group_id=$1 AND database_id=$2`, groupID, fix.dbID)
	if _, err := reviews.Enqueue(ctx, actor, proposalID, input); !errors.Is(err, executioncontract.ErrRequestConflict) {
		t.Fatalf("reapproved changed fingerprint replay: %v", err)
	}
}

// 5. Decisive test for rejection before SQL on preflight forbidden schema
func TestExecuteStandardJob_RejectionBeforeSQL_PreflightForbidden(t *testing.T) {
	env := setupP3TestEnv(t)
	svc := New(env.platformDB)
	ctx := context.Background()

	// Forbidden: touching public schema
	sqlUp := "CREATE TABLE public.forbidden_p3 (id bigint primary key);"
	fix := createP3JobFixture(t, env, executioncontract.Apply, executioncontract.Up, sqlUp, "")

	fakeGate := executioncontract.FakeGate{
		RevalidateFunc: func(ctx context.Context, req executioncontract.RevalidationRequest) (executioncontract.ExecutionPlan, error) {
			return executioncontract.ExecutionPlan{
				Request:       req,
				Snapshot:      fix.snapshot,
				Version:       "1",
				ActorUsername: "testactor",
				ValidatedAt:   time.Now().UTC(),
			}, nil
		},
	}

	deps := ExecutionDeps{
		Gate:    fakeGate,
		Acquire: env.makeAcquire(t),
		Timeout: 10 * time.Second,
	}

	outcome, err := svc.ExecuteStandardJob(ctx, fix.claim, deps)
	if err != nil {
		t.Fatalf("expected outcome returned without top-level error, got %v", err)
	}
	if outcome.Status != executioncontract.Failed {
		t.Fatalf("expected failed outcome, got %s", outcome.Status)
	}

	// Verify table does not exist
	var exists bool
	_ = env.targetExecDB.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM pg_tables WHERE schemaname = 'public' AND tablename = 'forbidden_p3')").Scan(&exists)
	if exists {
		t.Fatal("forbidden table was created!")
	}

	// Verify sql_entered is false
	var sqlEntered bool
	_ = env.platformDB.QueryRowContext(ctx, "SELECT sql_entered FROM app.job_attempts WHERE id = $1", fix.claim.AttemptID).Scan(&sqlEntered)
	if sqlEntered {
		t.Fatal("sql_entered should be false on preflight rejection")
	}
}

// 6. Decisive test for token fencing & lease mismatch before dispatch
func TestExecuteStandardJob_TokenFencing_LeaseMismatchBeforeDispatch(t *testing.T) {
	env := setupP3TestEnv(t)
	svc := New(env.platformDB)
	ctx := context.Background()

	tableName := "items_p3_fencing_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	sqlUp := "CREATE TABLE managed." + tableName + " (id bigint primary key);"

	fix := createP3JobFixture(t, env, executioncontract.Apply, executioncontract.Up, sqlUp, "")

	fakeGate := executioncontract.FakeGate{
		RevalidateFunc: func(ctx context.Context, req executioncontract.RevalidationRequest) (executioncontract.ExecutionPlan, error) {
			return executioncontract.ExecutionPlan{
				Request:       req,
				Snapshot:      fix.snapshot,
				Version:       "1",
				ActorUsername: "testactor",
				ValidatedAt:   time.Now().UTC(),
			}, nil
		},
	}

	// Worker 1 has claim with lease_token A.
	// Now simulate another worker taking over or mutating the lease token before Worker 1 dispatches:
	newToken := uuid.NewString()
	_, err := env.platformDB.ExecContext(ctx, `
		UPDATE app.jobs
		SET lease_token = $1
		WHERE id = $2
	`, newToken, fix.claim.JobID)
	if err != nil {
		t.Fatalf("mutate lease token: %v", err)
	}
	if err := svc.failJob(ctx, fix.claim, "STALE", "stale worker"); err == nil {
		t.Fatal("stale worker unexpectedly failed job")
	}
	if err := svc.markJobUnknown(ctx, fix.claim, errors.New("stale worker")); err == nil {
		t.Fatal("stale worker unexpectedly marked job unknown")
	}
	var status string
	if err := env.platformDB.QueryRowContext(ctx, `SELECT status FROM app.jobs WHERE id = $1`, fix.claim.JobID).Scan(&status); err != nil || status != "running" {
		t.Fatalf("stale worker changed job state: status=%s err=%v", status, err)
	}

	deps := ExecutionDeps{
		Gate:    fakeGate,
		Acquire: env.makeAcquire(t),
		Timeout: 10 * time.Second,
	}

	// Worker 1 attempts dispatch with stale token:
	_, err = svc.ExecuteStandardJob(ctx, fix.claim, deps)
	if err == nil {
		t.Fatal("expected lease mismatch error, got nil")
	}
	var contractErr executioncontract.ContractError
	if !errors.As(err, &contractErr) || contractErr.Code != "LEASE_MISMATCH" {
		t.Fatalf("expected LEASE_MISMATCH contract error, got %v", err)
	}

	// Verify ZERO SQL was run
	var exists bool
	_ = env.targetExecDB.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM pg_tables WHERE schemaname = 'managed' AND tablename = $1)", tableName).Scan(&exists)
	if exists {
		t.Fatal("table was created despite lease mismatch!")
	}

	// Verify sql_entered remains false
	var sqlEntered bool
	_ = env.platformDB.QueryRowContext(ctx, "SELECT sql_entered FROM app.job_attempts WHERE id = $1", fix.claim.AttemptID).Scan(&sqlEntered)
	if sqlEntered {
		t.Fatal("sql_entered must remain false on lease mismatch")
	}
}

// 7. Decisive test for target advisory lock concurrency protection
func TestExecuteStandardJob_TargetAdvisoryLockProtection(t *testing.T) {
	env := setupP3TestEnv(t)
	svc := New(env.platformDB)
	ctx := context.Background()

	tableName1 := "items_p3_lock1_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	sqlUp1 := "CREATE TABLE managed." + tableName1 + " (id bigint primary key);"
	fix1 := createP3JobFixture(t, env, executioncontract.Apply, executioncontract.Up, sqlUp1, "")

	tableName2 := "items_p3_lock2_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	sqlUp2 := "CREATE TABLE managed." + tableName2 + " (id bigint primary key);"
	enqReq2 := executioncontract.EnqueueRequest{
		DatabaseID:          fix1.dbID,
		TargetID:            fix1.targetID,
		RequestID:           uuid.NewString(),
		ProposalID:          fix1.proposalID,
		MigrationID:         fix1.draftID,
		Revision:            1,
		ActorID:             fix1.actorID,
		Action:              executioncontract.Apply,
		Direction:           executioncontract.Up,
		Snapshot:            executioncontract.Draft{MigrationID: fix1.draftID, DatabaseID: fix1.dbID, Revision: 1, SQLUp: sqlUp2, Mode: executioncontract.Standard},
		SnapshotHash:        "hash2_" + tableName2,
		ApprovalFingerprint: "fingerprint2",
	}
	ptx2, err := env.platformDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.EnqueueTx(ctx, ptx2, enqReq2)
	if err != nil {
		ptx2.Rollback()
		t.Fatal(err)
	}
	if err := ptx2.Commit(); err != nil {
		t.Fatal(err)
	}
	claim2, err := svc.ClaimJob(ctx, ClaimRequest{WorkerID: "worker-lock-2", LeaseDuration: 45 * time.Second})
	if err != nil || claim2 == nil {
		t.Fatalf("claim job 2: %v", err)
	}
	fakeGate := func(snapshot executioncontract.Draft) executioncontract.FakeGate {
		return executioncontract.FakeGate{
			RevalidateFunc: func(ctx context.Context, req executioncontract.RevalidationRequest) (executioncontract.ExecutionPlan, error) {
				return executioncontract.ExecutionPlan{
					Request:       req,
					Snapshot:      snapshot,
					Version:       "1",
					ActorUsername: "testactor",
					ValidatedAt:   time.Now().UTC(),
				}, nil
			},
			RecordCommitFunc: func(ctx context.Context, tx *sql.Tx, in executioncontract.CommitEvidence) error {
				return nil
			},
		}
	}

	holdLock := make(chan struct{})
	worker1Started := make(chan struct{})

	deps1 := ExecutionDeps{
		Gate:    fakeGate(fix1.snapshot),
		Acquire: env.makeAcquire(t),
		Timeout: 10 * time.Second,
		beforeDispatch: func(ctx context.Context) error {
			close(worker1Started)
			<-holdLock
			return nil
		},
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = svc.ExecuteStandardJob(ctx, fix1.claim, deps1)
	}()

	<-worker1Started

	// Worker 2 attempts execution on the SAME target with a short timeout
	shortCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()

	deps2 := ExecutionDeps{
		Gate:    fakeGate(enqReq2.Snapshot),
		Acquire: env.makeAcquire(t),
		Timeout: 500 * time.Millisecond,
	}

	_, err = svc.ExecuteStandardJob(shortCtx, claim2, deps2)
	if err == nil {
		t.Fatal("expected Worker 2 to fail or timeout acquiring target advisory lock")
	}

	// Release Worker 1
	close(holdLock)
	wg.Wait()
}
// 7b. Decisive integration test for distinct physical target parallel execution
func TestExecuteStandardJob_DistinctPhysicalTargets_ParallelExecution(t *testing.T) {
	env := setupP3TestEnv(t)
	if env.targetExecDB_B == nil || env.physicalTargetID_B == "" {
		if os.Getenv("TEST_INTEGRATION_REQUIRED") == "1" {
			t.Fatal("missing target B database configuration")
		}
		t.Skip("target B database configuration not set")
	}
	if env.physicalTargetID == env.physicalTargetID_B {
		t.Fatal("target A and target B must be distinct physical targets")
	}

	svc := New(env.platformDB)
	ctx := context.Background()

	tableNameA := "items_p3_par_a_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	sqlUpA := "CREATE TABLE managed." + tableNameA + " (id bigint primary key);"
	fixA := createP3JobFixtureFull(t, env, env.physicalTargetID, env.targetExecDB, env.targetCleanupDB, "target_a", "worker-par-a", nil, executioncontract.Apply, executioncontract.Up, sqlUpA, "")

	tableNameB := "items_p3_par_b_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	sqlUpB := "CREATE TABLE managed." + tableNameB + " (id bigint primary key);"
	fixB := createP3JobFixtureFull(t, env, env.physicalTargetID_B, env.targetExecDB_B, env.targetCleanupDB_B, "target_b", "worker-par-b", nil, executioncontract.Apply, executioncontract.Up, sqlUpB, "")
	// A target's ticket versions outlive tests that don't remove their audit rows.
	version := strconv.FormatInt(time.Now().UnixNano(), 10)

	fakeGate := func(snapshot executioncontract.Draft) executioncontract.FakeGate {
		return executioncontract.FakeGate{
			RevalidateFunc: func(ctx context.Context, req executioncontract.RevalidationRequest) (executioncontract.ExecutionPlan, error) {
				return executioncontract.ExecutionPlan{
					Request:       req,
					Snapshot:      snapshot,
					Version:       version,
					ActorUsername: "testactor",
					ValidatedAt:   time.Now().UTC(),
				}, nil
			},
			RecordCommitFunc: func(ctx context.Context, tx *sql.Tx, in executioncontract.CommitEvidence) error {
				return nil
			},
		}
	}

	workerAHoldingLock := make(chan struct{})
	releaseWorkerA := make(chan struct{})

	acquire := env.makeDualAcquire(t, fixA.dbID, fixB.dbID)

	depsA := ExecutionDeps{
		Gate:    fakeGate(fixA.snapshot),
		Acquire: acquire,
		Timeout: 10 * time.Second,
		beforeDispatch: func(ctx context.Context) error {
			close(workerAHoldingLock)
			<-releaseWorkerA
			return nil
		},
	}

	var wg sync.WaitGroup
	wg.Add(1)
	var outcomeA *ExecutionOutcome
	var errA error
	go func() {
		defer wg.Done()
		outcomeA, errA = svc.ExecuteStandardJob(ctx, fixA.claim, depsA)
	}()

	// Wait until Worker A is holding Target A's advisory lock
	<-workerAHoldingLock

	// While Worker A is paused and holding Target A's lock, execute Worker B on Target B to completion
	depsB := ExecutionDeps{
		Gate:    fakeGate(fixB.snapshot),
		Acquire: acquire,
		Timeout: 10 * time.Second,
	}

	outcomeB, errB := svc.ExecuteStandardJob(ctx, fixB.claim, depsB)
	if errB != nil {
		close(releaseWorkerA)
		wg.Wait()
		t.Fatalf("worker B failed to execute independently: %v", errB)
	}
	if outcomeB == nil || outcomeB.Status != executioncontract.Succeeded {
		close(releaseWorkerA)
		wg.Wait()
		t.Fatalf("expected worker B succeeded, got: %+v", outcomeB)
	}

	// Verify Target B commit and platform finalization completed BEFORE releasing A
	var jobBStatus string
	var jobBBlocked bool
	err := env.platformDB.QueryRowContext(ctx, "SELECT status, blocks_database FROM app.jobs WHERE id = $1", fixB.claim.JobID).Scan(&jobBStatus, &jobBBlocked)
	if err != nil || jobBStatus != "succeeded" || jobBBlocked {
		close(releaseWorkerA)
		wg.Wait()
		t.Fatalf("job B platform state mismatch: status=%s blocked=%t err=%v", jobBStatus, jobBBlocked, err)
	}

	var proofBExists bool
	err = env.platformDB.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM app.job_commit_proofs WHERE job_id = $1)", fixB.claim.JobID).Scan(&proofBExists)
	if err != nil || !proofBExists {
		close(releaseWorkerA)
		wg.Wait()
		t.Fatalf("job B commit proof missing: %v", err)
	}

	var tableBExists bool
	err = env.targetExecDB_B.QueryRowContext(ctx, "SELECT to_regclass($1) IS NOT NULL", "managed."+tableNameB).Scan(&tableBExists)
	if err != nil || !tableBExists {
		close(releaseWorkerA)
		wg.Wait()
		t.Fatalf("target B table was not committed: %v", err)
	}

	// Now release Worker A
	close(releaseWorkerA)
	wg.Wait()

	if errA != nil {
		t.Fatalf("worker A failed after release: %v", errA)
	}
	if outcomeA == nil || outcomeA.Status != executioncontract.Succeeded {
		t.Fatalf("expected worker A succeeded, got: %+v", outcomeA)
	}

	var jobAStatus string
	var jobABlocked bool
	err = env.platformDB.QueryRowContext(ctx, "SELECT status, blocks_database FROM app.jobs WHERE id = $1", fixA.claim.JobID).Scan(&jobAStatus, &jobABlocked)
	if err != nil || jobAStatus != "succeeded" || jobABlocked {
		t.Fatalf("job A platform state mismatch: status=%s blocked=%t err=%v", jobAStatus, jobABlocked, err)
	}

	var tableAExists bool
	err = env.targetExecDB.QueryRowContext(ctx, "SELECT to_regclass($1) IS NOT NULL", "managed."+tableNameA).Scan(&tableAExists)
	if err != nil || !tableAExists {
		t.Fatalf("target A table was not committed: %v", err)
	}
}

// 8. Decisive test for ambiguous/conflicting receipt protection (never replay SQL, block database)
func TestExecuteStandardJob_AmbiguousConflictingReceipt_BlocksDatabase(t *testing.T) {
	env := setupP3TestEnv(t)
	if env.targetCleanupDB == nil {
		t.Skip("target cleanup database configuration not set")
	}
	svc := New(env.platformDB)
	ctx := context.Background()

	tableName := "items_p3_ambiguous_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	sqlUp := "CREATE TABLE managed." + tableName + " (id bigint primary key);"
	fix := createP3JobFixture(t, env, executioncontract.Apply, executioncontract.Up, sqlUp, "")

	// Insert an alien/conflicting receipt on target with a DIFFERENT actor_id
	otherActorID := uuid.NewString()
	_, err := env.targetCleanupDB.ExecContext(ctx, `
		INSERT INTO wbd_auth.execution_tickets
			(execution_id, physical_target_id, database_id, target_xid, version, sql_sha256, actor_id, capability_sha256)
		VALUES ($1::uuid, $2::uuid, $3::uuid, '9999'::xid8, 99, '\x0102030405060708091011121314151617181920212223242526272829303132', $4::uuid, '\x0000000000000000000000000000000000000000000000000000000000000000')
	`, fix.claim.ExecutionID, fix.targetID, fix.dbID, otherActorID)
	if err != nil {
		t.Fatalf("insert ticket: %v", err)
	}

	_, err = env.targetCleanupDB.ExecContext(ctx, `
		INSERT INTO wbd_auth.execution_receipts
			(execution_id, physical_target_id, database_id, target_xid, version, sql_sha256, actor_id)
		VALUES ($1::uuid, $2::uuid, $3::uuid, '9999'::xid8, 99, '\x0102030405060708091011121314151617181920212223242526272829303132', $4::uuid)
	`, fix.claim.ExecutionID, fix.targetID, fix.dbID, otherActorID)
	if err != nil {
		t.Fatalf("insert conflicting receipt: %v", err)
	}
	fakeGate := executioncontract.FakeGate{
		RevalidateFunc: func(ctx context.Context, req executioncontract.RevalidationRequest) (executioncontract.ExecutionPlan, error) {
			return executioncontract.ExecutionPlan{
				Request:       req,
				Snapshot:      fix.snapshot,
				Version:       "1",
				ActorUsername: "testactor",
				ValidatedAt:   time.Now().UTC(),
			}, nil
		},
	}

	deps := ExecutionDeps{
		Gate:    fakeGate,
		Acquire: env.makeAcquire(t),
		Timeout: 10 * time.Second,
	}

	outcome, err := svc.ExecuteStandardJob(ctx, fix.claim, deps)
	if err != nil {
		t.Fatalf("expected outcome returned without top-level panic/error, got %v", err)
	}
	if outcome.Status != executioncontract.Unknown {
		t.Fatalf("expected unknown outcome on conflicting receipt, got %s", outcome.Status)
	}

	// Verify ZERO SQL was run: table must not exist
	var exists bool
	_ = env.targetExecDB.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM pg_tables WHERE schemaname = 'managed' AND tablename = $1)", tableName).Scan(&exists)
	if exists {
		t.Fatal("table must not be created on ambiguous receipt conflict!")
	}

	// Verify job is marked unknown with blocks_database = true
	jobView, err := svc.GetJob(ctx, fix.dbID, fix.claim.JobID)
	if err != nil {
		t.Fatalf("get job failed: %v", err)
	}
	if jobView.Status != executioncontract.Unknown || !jobView.BlocksDatabase {
		t.Fatalf("expected job to be unknown with blocks_database=true, got status=%s blocks=%v", jobView.Status, jobView.BlocksDatabase)
	}
}

func TestExecuteStandardJob_BlockedPhysicalTarget(t *testing.T) {
	env := setupP3TestEnv(t)
	svc := New(env.platformDB)
	ctx := context.Background()
	firstSQL := "CREATE TABLE managed.blocker_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:8] + " (id bigint)"
	blocker := createP3JobFixture(t, env, executioncontract.Apply, executioncontract.Up, firstSQL, "")
	table := "blocked_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	sqlText := "CREATE TABLE managed." + table + " (id bigint)"
	t.Cleanup(func() {
		_, _ = env.targetExecDB.ExecContext(context.Background(), "DROP TABLE IF EXISTS managed."+table)
	})
	candidate := createP3JobFixtureOnConnection(t, env, &blocker, executioncontract.Apply, executioncontract.Up, sqlText, "")
	if err := svc.markJobUnknown(ctx, blocker.claim, errors.New("unconfirmed")); err != nil {
		t.Fatalf("mark blocker unknown: %v", err)
	}
	// A fresh service must discover the durable block rather than relying on in-memory state.
	svc = New(env.platformDB)
	blockedJob, err := svc.GetJob(ctx, blocker.dbID, blocker.claim.JobID)
	if err != nil || blockedJob.Status != executioncontract.Unknown || !blockedJob.BlocksDatabase {
		t.Fatalf("expected durable unresolved blocker: job=%+v err=%v", blockedJob, err)
	}
	gate := executioncontract.FakeGate{
		RevalidateFunc: func(_ context.Context, req executioncontract.RevalidationRequest) (executioncontract.ExecutionPlan, error) {
			return executioncontract.ExecutionPlan{Request: req, Snapshot: candidate.snapshot, Version: "1"}, nil
		},
	}
	_, err = svc.ExecuteStandardJob(ctx, candidate.claim, ExecutionDeps{Gate: gate, Acquire: env.makeAcquire(t), Timeout: 10 * time.Second})
	var contractErr executioncontract.ContractError
	if !errors.As(err, &contractErr) || contractErr.Code != "TARGET_BLOCKED" {
		t.Fatalf("expected physical target block, got %v", err)
	}
	var exists bool
	if err := env.targetExecDB.QueryRowContext(ctx, `SELECT to_regclass($1) IS NOT NULL`, "managed."+table).Scan(&exists); err != nil || exists {
		t.Fatalf("blocked SQL dispatched: exists=%v err=%v", exists, err)
	}
	// An 008 attempt with entered SQL but no authorization proof is not a clean rollback.
	if _, err := env.platformDB.ExecContext(ctx, `UPDATE app.jobs SET status='failed', execution_status='failed', blocks_database=false WHERE id=$1`, blocker.claim.JobID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.platformDB.ExecContext(ctx, `UPDATE app.job_attempts SET status='failed', sql_entered=true WHERE id=$1`, blocker.claim.AttemptID); err != nil {
		t.Fatal(err)
	}
	_, err = svc.ExecuteStandardJob(ctx, candidate.claim, ExecutionDeps{Gate: gate, Acquire: env.makeAcquire(t), Timeout: 10 * time.Second})
	if !errors.As(err, &contractErr) || contractErr.Code != "TARGET_BLOCKED" {
		t.Fatalf("unproven legacy attempt did not block target: %v", err)
	}
	if _, err := env.platformDB.ExecContext(ctx, `UPDATE app.job_attempts SET sql_entered=false WHERE id=$1`, blocker.claim.AttemptID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.platformDB.ExecContext(ctx, `INSERT INTO app.job_statement_evidence (attempt_id, ordinal, sql_text, outcome) VALUES ($1, 1, 'SELECT 1', 'unknown')`, blocker.claim.AttemptID); err != nil {
		t.Fatal(err)
	}
	_, err = svc.ExecuteStandardJob(ctx, candidate.claim, ExecutionDeps{Gate: gate, Acquire: env.makeAcquire(t), Timeout: 10 * time.Second})
	if !errors.As(err, &contractErr) || contractErr.Code != "TARGET_BLOCKED" {
		t.Fatalf("unproven legacy statement did not block target: %v", err)
	}
}

func TestExecuteStandardJob_UnsupportedCommitTimeStopsBeforeDispatch(t *testing.T) {
	env := setupP3TestEnvWithTracking(t, false)
	ctx := context.Background()
	table := "no_dispatch_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	fix := createP3JobFixture(t, env, executioncontract.Apply, executioncontract.Up, "CREATE TABLE managed."+table+" (id bigint)", "")
	commits := 0
	gate := executioncontract.FakeGate{
		RevalidateFunc: func(_ context.Context, req executioncontract.RevalidationRequest) (executioncontract.ExecutionPlan, error) {
			return executioncontract.ExecutionPlan{Request: req, Snapshot: fix.snapshot, Version: "1"}, nil
		},
		RecordCommitFunc: func(context.Context, *sql.Tx, executioncontract.CommitEvidence) error {
			commits++
			return nil
		},
	}
	outcome, err := New(env.platformDB).ExecuteStandardJob(ctx, fix.claim, ExecutionDeps{Gate: gate, Acquire: env.makeAcquire(t), Timeout: 10 * time.Second})
	if err != nil || outcome == nil || outcome.Status != executioncontract.Failed || outcome.Error == nil {
		t.Fatalf("expected unsupported commit time: outcome=%+v err=%v", outcome, err)
	}
	var entered, authorized, exists, receipt bool
	if err := env.platformDB.QueryRowContext(ctx, `SELECT sql_entered, EXISTS (SELECT 1 FROM app.job_dispatch_authorizations WHERE job_id = $2) FROM app.job_attempts WHERE id = $1`, fix.claim.AttemptID, fix.claim.JobID).Scan(&entered, &authorized); err != nil {
		t.Fatal(err)
	}
	if err := env.targetExecDB.QueryRowContext(ctx, `SELECT to_regclass($1) IS NOT NULL`, "managed."+table).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if err := env.targetPrepDB.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM wbd_auth.execution_receipts WHERE execution_id = $1)`, fix.claim.ExecutionID).Scan(&receipt); err != nil {
		t.Fatal(err)
	}
	if entered || authorized || exists || receipt || commits != 0 {
		t.Fatalf("unexpected dispatch: entered=%v authorized=%v table=%v receipt=%v commits=%d", entered, authorized, exists, receipt, commits)
	}
}
