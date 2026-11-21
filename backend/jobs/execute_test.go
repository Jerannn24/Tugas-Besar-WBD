package jobs

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Jerannn24/Tugas-Besar-WBD.git/executioncontract"
	"github.com/Jerannn24/Tugas-Besar-WBD.git/migration"
	"github.com/google/uuid"
)

type p3TestEnv struct {
	platformDB       *sql.DB
	ownerDB          *sql.DB
	targetExecDB     *sql.DB
	targetPrepDB     *sql.DB
	targetCleanupDB  *sql.DB
	physicalTargetID string
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

	return &p3TestEnv{
		platformDB:       platformDB,
		ownerDB:          ownerDB,
		targetExecDB:     targetExecDB,
		targetPrepDB:     targetPrepDB,
		targetCleanupDB:  targetCleanupDB,
		physicalTargetID: physicalTargetID,
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
	t.Helper()
	ctx := context.Background()

	dbID := uuid.NewString()
	targetID := env.physicalTargetID
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

		if env.targetCleanupDB != nil {
			for _, query := range []string{
				"DELETE FROM wbd_auth.execution_receipts WHERE database_id = $1",
				"DELETE FROM wbd_auth.execution_tickets WHERE database_id = $1",
			} {
				if _, err := env.targetCleanupDB.ExecContext(cleanupCtx, query, dbID); err != nil {
					t.Errorf("cleanup target %s: %v", query, err)
				}
			}
		}
		if _, err := env.targetExecDB.ExecContext(cleanupCtx, "DROP TABLE IF EXISTS managed."+tableName); err != nil {
			t.Errorf("cleanup target table: %v", err)
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
			VALUES ($1, $2, 'postgres-test', 5432, 'target_a', 'user', '\x01', 'key1', 'disable', $3, $4)
		`, dbID, connName, targetKey, targetID)
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
		WorkerID:      "worker-p3-test",
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
