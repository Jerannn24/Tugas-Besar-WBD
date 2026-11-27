package worker

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Jerannn24/Tugas-Besar-WBD.git/executioncontract"
	"github.com/Jerannn24/Tugas-Besar-WBD.git/jobs"
	"github.com/Jerannn24/Tugas-Besar-WBD.git/migration"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"
)

type workerTestEnv struct {
	platformDB         *sql.DB
	ownerDB            *sql.DB
	targetExecDB       *sql.DB
	targetPrepDB       *sql.DB
	targetCleanupDB    *sql.DB
	physicalTargetID   string
	targetExecDB_B     *sql.DB
	targetPrepDB_B     *sql.DB
	targetCleanupDB_B  *sql.DB
	physicalTargetID_B string
}

func setupWorkerTestEnv(t *testing.T) *workerTestEnv {
	t.Helper()

	platformDSN := os.Getenv("MIGRATION_TEST_PLATFORM_URL")
	if platformDSN == "" {
		platformDSN = os.Getenv("HISTORY_TEST_DATABASE_URL")
	}
	if platformDSN == "" {
		if os.Getenv("TEST_INTEGRATION_REQUIRED") == "1" {
			t.Fatal("missing MIGRATION_TEST_PLATFORM_URL")
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
	}

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

	execDSNB := os.Getenv("MIGRATION_TEST_TARGET_B_EXECUTOR_URL")
	prepDSNB := os.Getenv("MIGRATION_TEST_TARGET_B_PREPARER_URL")
	cleanupDSNB := os.Getenv("MIGRATION_TEST_TARGET_B_CLEANUP_URL")

	var targetExecDB_B, targetPrepDB_B, targetCleanupDB_B *sql.DB
	var physicalTargetID_B string
	if execDSNB != "" && prepDSNB != "" {
		targetExecDB_B, err = sql.Open("pgx", execDSNB)
		if err == nil {
			t.Cleanup(func() { targetExecDB_B.Close() })
			targetPrepDB_B, err = sql.Open("pgx", prepDSNB)
			if err == nil {
				t.Cleanup(func() { targetPrepDB_B.Close() })
				if cleanupDSNB != "" {
					targetCleanupDB_B, _ = sql.Open("pgx", cleanupDSNB)
					if targetCleanupDB_B != nil {
						t.Cleanup(func() { targetCleanupDB_B.Close() })
					}
				}
				_ = targetPrepDB_B.QueryRowContext(ctx, "SELECT physical_target_id::text FROM wbd_auth.target_marker WHERE singleton").Scan(&physicalTargetID_B)
			}
		}
	}
	requireIdleQueue(t, platformDB)
	return &workerTestEnv{
		platformDB:         platformDB,
		ownerDB:            ownerDB,
		targetExecDB:       targetExecDB,
		targetPrepDB:       targetPrepDB,
		targetCleanupDB:    targetCleanupDB,
		physicalTargetID:   physicalTargetID,
		targetExecDB_B:     targetExecDB_B,
		targetPrepDB_B:     targetPrepDB_B,
		targetCleanupDB_B:  targetCleanupDB_B,
		physicalTargetID_B: physicalTargetID_B,
	}
}

func (e *workerTestEnv) makeAcquire(t *testing.T) func(ctx context.Context, databaseID string) (*migration.TargetLease, error) {
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

func (e *workerTestEnv) makeDualAcquire(t *testing.T, dbID_A, dbID_B string) func(ctx context.Context, databaseID string) (*migration.TargetLease, error) {
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

type jobFixture struct {
	dbID        string
	targetID    string
	actorID     string
	draftID     string
	proposalID  string
	jobID       string
	executionID string
	tableName   string
	snapshot    executioncontract.Draft
}

// requireIdleQueue keeps Step from claiming or reconciling jobs that another test left behind.
func requireIdleQueue(t *testing.T, db *sql.DB) {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), `
		SELECT id::text, title, status FROM app.jobs
		WHERE status IN ('queued', 'running', 'retry_wait', 'unknown') OR blocks_database`)
	if err != nil {
		t.Fatalf("check job queue: %v", err)
	}
	defer rows.Close()
	var foreign []string
	for rows.Next() {
		var id, title, status string
		if err := rows.Scan(&id, &title, &status); err != nil {
			t.Fatalf("scan job queue: %v", err)
		}
		foreign = append(foreign, id+" ("+title+", "+status+")")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read job queue: %v", err)
	}
	if len(foreign) > 0 {
		t.Fatalf("job queue must be idle before a worker test, found: %s", strings.Join(foreign, "; "))
	}
}

// planFor returns the snapshot of the fixture that owns req.JobID and refuses foreign jobs without touching them.
func planFor(req executioncontract.RevalidationRequest, version string, fixes ...jobFixture) (executioncontract.ExecutionPlan, error) {
	for _, f := range fixes {
		if f.jobID == req.JobID {
			return executioncontract.ExecutionPlan{Request: req, Snapshot: f.snapshot, Version: version}, nil
		}
	}
	return executioncontract.ExecutionPlan{}, executioncontract.ContractError{Code: "TEST_FOREIGN_JOB", Message: "job " + req.JobID + " does not belong to this test", Retryable: true}
}

func enqueueStandardJob(t *testing.T, env *workerTestEnv, targetID, dbName, sqlUp, sqlDown string) jobFixture {
	t.Helper()
	ctx := context.Background()

	dbID := uuid.NewString()
	actorID := uuid.NewString()
	draftID := uuid.NewString()
	proposalID := uuid.NewString()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:10]
	tableName := "worker_tbl_" + suffix

	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		// Evidence lives on the job's own target; leftover tickets would collide with later versions.
		targetCleanupDB := env.targetCleanupDB
		if targetID == env.physicalTargetID_B && env.physicalTargetID_B != "" {
			targetCleanupDB = env.targetCleanupDB_B
		}
		if targetCleanupDB != nil {
			for _, query := range []string{
				"DELETE FROM wbd_auth.execution_receipts WHERE database_id = $1",
				"DELETE FROM wbd_auth.execution_tickets WHERE database_id = $1",
			} {
				_, _ = targetCleanupDB.ExecContext(cleanupCtx, query, dbID)
			}
		}
		if env.targetExecDB != nil {
			_, _ = env.targetExecDB.ExecContext(cleanupCtx, "DROP TABLE IF EXISTS managed."+tableName)
		}
		cleanPlatformDB := env.ownerDB
		if cleanPlatformDB == nil {
			cleanPlatformDB = env.platformDB
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
			_, _ = cleanPlatformDB.ExecContext(cleanupCtx, query, dbID)
		}
		_, _ = cleanPlatformDB.ExecContext(cleanupCtx, "DELETE FROM app.users WHERE id = $1", actorID)
	})

	username := "u_" + suffix
	_, err := env.platformDB.ExecContext(ctx, "INSERT INTO app.users (id, username, password_hash) VALUES ($1, $2, 'hash')", actorID, username)
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

	var downVal *string
	if sqlDown != "" {
		downVal = &sqlDown
	}

	title := "Worker Migration " + suffix
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

	activeHashBytes := sha256.Sum256([]byte(sqlUp))
	snapshotHash := "hash_" + suffix

	_, err = env.platformDB.ExecContext(ctx, `
		INSERT INTO app.migration_proposals (
			id, database_id, migration_id, revision, action, status,
			snapshot, snapshot_hash, created_by
		) VALUES ($1, $2, $3, 1, 'apply', 'draft', '{}'::jsonb, $4, $5)
	`, proposalID, dbID, draftID, activeHashBytes[:], actorID)
	if err != nil {
		t.Fatalf("insert proposal: %v", err)
	}

	svc := jobs.New(env.platformDB)
	enqReq := executioncontract.EnqueueRequest{
		DatabaseID:          dbID,
		TargetID:            targetID,
		RequestID:           uuid.NewString(),
		ProposalID:          proposalID,
		MigrationID:         draftID,
		Revision:            1,
		ActorID:             actorID,
		Action:              executioncontract.Apply,
		Direction:           executioncontract.Up,
		Snapshot:            snapshot,
		SnapshotHash:        snapshotHash,
		ApprovalFingerprint: "fingerprint_" + suffix,
	}

	ptx, err := env.platformDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin enq tx: %v", err)
	}
	res, err := svc.EnqueueTx(ctx, ptx, enqReq)
	if err != nil {
		ptx.Rollback()
		t.Fatalf("enqueue tx: %v", err)
	}
	if err := ptx.Commit(); err != nil {
		t.Fatalf("commit enq tx: %v", err)
	}

	return jobFixture{
		dbID:        dbID,
		targetID:    targetID,
		actorID:     actorID,
		draftID:     draftID,
		proposalID:  proposalID,
		jobID:       res.JobID,
		executionID: res.ExecutionID,
		tableName:   tableName,
		snapshot:    snapshot,
	}
}

func enqueueBatchJob(t *testing.T, env *workerTestEnv, targetID, dbName string, batchSize int) jobFixture {
	t.Helper()
	ctx := context.Background()

	dbID := uuid.NewString()
	actorID := uuid.NewString()
	draftID := uuid.NewString()
	proposalID := uuid.NewString()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:10]

	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if env.targetCleanupDB != nil {
			for _, query := range []string{
				"DELETE FROM wbd_auth.execution_receipts WHERE database_id = $1",
				"DELETE FROM wbd_auth.execution_tickets WHERE database_id = $1",
				"DELETE FROM wbd_auth.batch_checkpoints WHERE database_id = $1",
			} {
				_, _ = env.targetCleanupDB.ExecContext(cleanupCtx, query, dbID)
			}
		}
		cleanPlatformDB := env.ownerDB
		if cleanPlatformDB == nil {
			cleanPlatformDB = env.platformDB
		}
		for _, query := range []string{
			"DELETE FROM app.job_batch_commit_proofs WHERE job_id IN (SELECT id FROM app.jobs WHERE database_id = $1)",
			"DELETE FROM app.job_batch_step_authorizations WHERE job_id IN (SELECT id FROM app.jobs WHERE database_id = $1)",
			"DELETE FROM app.job_commit_proofs WHERE job_id IN (SELECT id FROM app.jobs WHERE database_id = $1)",
			"DELETE FROM app.job_dispatch_authorizations WHERE job_id IN (SELECT id FROM app.jobs WHERE database_id = $1)",
			"DELETE FROM app.job_statement_evidence WHERE attempt_id IN (SELECT id FROM app.job_attempts WHERE database_id = $1)",
			"DELETE FROM app.job_attempts WHERE database_id = $1",
			"DELETE FROM app.jobs WHERE database_id = $1",
			"DELETE FROM app.migration_proposals WHERE database_id = $1",
			"DELETE FROM app.migration_drafts WHERE database_id = $1",
			"DELETE FROM app.database_connections WHERE id = $1",
		} {
			_, _ = cleanPlatformDB.ExecContext(cleanupCtx, query, dbID)
		}
		_, _ = cleanPlatformDB.ExecContext(cleanupCtx, "DELETE FROM app.users WHERE id = $1", actorID)
	})

	username := "batch_u_" + suffix
	_, err := env.platformDB.ExecContext(ctx, "INSERT INTO app.users (id, username, password_hash) VALUES ($1, $2, 'hash')", actorID, username)
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

	sqlUp := "UPDATE managed.batch_items SET processed = TRUE WHERE id = ANY($1::bigint[]);"
	sqlDown := "UPDATE managed.batch_items SET processed = FALSE WHERE id = ANY($1::bigint[]);"
	title := "Worker Batch Migration " + suffix

	_, err = env.platformDB.ExecContext(ctx, `
		INSERT INTO app.migration_drafts (id, database_id, revision, title, sql, sql_down, created_by)
		VALUES ($1, $2, 1, $3, $4, $5, $6)
	`, draftID, dbID, title, sqlUp, sqlDown, actorID)
	if err != nil {
		t.Fatalf("insert draft: %v", err)
	}

	snapshot := executioncontract.Draft{
		MigrationID: draftID,
		DatabaseID:  dbID,
		Revision:    1,
		Title:       title,
		SQLUp:       sqlUp,
		SQLDown:     &sqlDown,
		Mode:        executioncontract.Batch,
		Batch: &executioncontract.BatchConfig{
			BatchSize:    batchSize,
			Schema:       "managed",
			Table:        "batch_items",
			CursorColumn: "id",
		},
	}

	activeHashBytes := sha256.Sum256([]byte(sqlUp))
	snapshotHash := "hash_" + suffix

	_, err = env.platformDB.ExecContext(ctx, `
		INSERT INTO app.migration_proposals (
			id, database_id, migration_id, revision, action, status,
			snapshot, snapshot_hash, created_by
		) VALUES ($1, $2, $3, 1, 'apply', 'draft', '{}'::jsonb, $4, $5)
	`, proposalID, dbID, draftID, activeHashBytes[:], actorID)
	if err != nil {
		t.Fatalf("insert proposal: %v", err)
	}

	svc := jobs.New(env.platformDB)
	enqReq := executioncontract.EnqueueRequest{
		DatabaseID:          dbID,
		TargetID:            targetID,
		RequestID:           uuid.NewString(),
		ProposalID:          proposalID,
		MigrationID:         draftID,
		Revision:            1,
		ActorID:             actorID,
		Action:              executioncontract.Apply,
		Direction:           executioncontract.Up,
		Snapshot:            snapshot,
		SnapshotHash:        snapshotHash,
		ApprovalFingerprint: "fingerprint_" + suffix,
	}

	ptx, err := env.platformDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin enq tx: %v", err)
	}
	res, err := svc.EnqueueTx(ctx, ptx, enqReq)
	if err != nil {
		ptx.Rollback()
		t.Fatalf("enqueue tx: %v", err)
	}
	if err := ptx.Commit(); err != nil {
		t.Fatalf("commit enq tx: %v", err)
	}

	return jobFixture{
		dbID:        dbID,
		targetID:    targetID,
		actorID:     actorID,
		draftID:     draftID,
		proposalID:  proposalID,
		jobID:       res.JobID,
		executionID: res.ExecutionID,
		snapshot:    snapshot,
	}
}

func TestWorker_New_Validation(t *testing.T) {
	fakeGate := executioncontract.FakeGate{}
	acquire := func(ctx context.Context, databaseID string) (*migration.TargetLease, error) {
		return nil, nil
	}

	// 1. Missing jobs service
	_, err := New(nil, jobs.ExecutionDeps{Gate: fakeGate, Acquire: acquire}, Config{})
	if err == nil || err.Error() != "jobs service is required" {
		t.Fatalf("expected jobs service is required, got %v", err)
	}

	// 2. Missing Gate
	_, err = New(&jobs.Service{}, jobs.ExecutionDeps{Acquire: acquire}, Config{})
	if err == nil || err.Error() != "workflow gate is required" {
		t.Fatalf("expected workflow gate is required, got %v", err)
	}

	// 3. Missing Acquire
	_, err = New(&jobs.Service{}, jobs.ExecutionDeps{Gate: fakeGate}, Config{})
	if err == nil || err.Error() != "target acquire function is required" {
		t.Fatalf("expected target acquire function is required, got %v", err)
	}

	// 4. Defaults properly populated
	w, err := New(&jobs.Service{}, jobs.ExecutionDeps{Gate: fakeGate, Acquire: acquire}, Config{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(w.cfg.WorkerID, "worker-") {
		t.Errorf("expected WorkerID to have worker- prefix, got %s", w.cfg.WorkerID)
	}
	if w.cfg.LeaseDuration != 30*time.Second {
		t.Errorf("expected LeaseDuration=30s, got %v", w.cfg.LeaseDuration)
	}
	if w.cfg.RenewInterval != 10*time.Second {
		t.Errorf("expected RenewInterval=10s, got %v", w.cfg.RenewInterval)
	}
	if w.cfg.PollInterval != 200*time.Millisecond {
		t.Errorf("expected PollInterval=200ms, got %v", w.cfg.PollInterval)
	}
	if w.cfg.RecoveryInterval != 5*time.Second {
		t.Errorf("expected RecoveryInterval=5s, got %v", w.cfg.RecoveryInterval)
	}
	if w.cfg.DrainTimeout != 10*time.Second {
		t.Errorf("expected DrainTimeout=10s, got %v", w.cfg.DrainTimeout)
	}
	if w.cfg.MaxAttempts != jobs.DefaultMaxAttempts {
		t.Errorf("expected MaxAttempts=%d, got %d", jobs.DefaultMaxAttempts, w.cfg.MaxAttempts)
	}
	if w.cfg.RetryDelay != jobs.DefaultRetryDelay {
		t.Errorf("expected RetryDelay=%v, got %v", jobs.DefaultRetryDelay, w.cfg.RetryDelay)
	}
}

func TestWorker_StandardJob_Lifecycle(t *testing.T) {
	env := setupWorkerTestEnv(t)
	svc := jobs.New(env.platformDB)
	ctx := context.Background()

	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	table := "worker_std_" + suffix
	sqlUp := fmt.Sprintf("CREATE TABLE managed.%s (id bigint, name text);", table)
	t.Cleanup(func() {
		_, _ = env.targetExecDB.ExecContext(context.Background(), "DROP TABLE IF EXISTS managed."+table)
	})

	fix := enqueueStandardJob(t, env, env.physicalTargetID, "target_a", sqlUp, "")

	gate := executioncontract.FakeGate{
		RevalidateFunc: func(_ context.Context, req executioncontract.RevalidationRequest) (executioncontract.ExecutionPlan, error) {
			return planFor(req, "1", fix)
		},
		RecordCommitFunc: func(_ context.Context, _ *sql.Tx, _ executioncontract.CommitEvidence) error {
			return nil
		},
	}

	w, err := New(svc, jobs.ExecutionDeps{
		Gate:    gate,
		Acquire: env.makeAcquire(t),
		Timeout: 10 * time.Second,
	}, Config{
		PollInterval:  50 * time.Millisecond,
		LeaseDuration: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("create worker: %v", err)
	}

	didWork, err := w.Step(ctx)
	if err != nil {
		t.Fatalf("worker step error: %v", err)
	}
	if !didWork {
		t.Fatal("expected worker step to claim and execute job, got didWork=false")
	}

	// Verify job status
	job, err := svc.GetJob(ctx, fix.dbID, fix.jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.Status != executioncontract.Succeeded {
		t.Fatalf("expected job status succeeded, got %s error=%+v", job.Status, job.Error)
	}
	if job.BlocksDatabase {
		t.Fatal("expected blocks_database to be false after completion")
	}

	// Verify table created on target
	var tableExists bool
	err = env.targetExecDB.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = 'managed' AND table_name = $1)
	`, table).Scan(&tableExists)
	if err != nil || !tableExists {
		t.Fatalf("target table %s does not exist", table)
	}

	// Verify target receipt
	var receiptCount int
	err = env.targetPrepDB.QueryRowContext(ctx, `
		SELECT count(*) FROM wbd_auth.execution_receipts WHERE execution_id = $1::uuid
	`, fix.executionID).Scan(&receiptCount)
	if err != nil || receiptCount != 1 {
		t.Fatalf("expected 1 execution receipt, got count=%d, err=%v", receiptCount, err)
	}
}

type transactionalBatchGate struct{ executioncontract.FakeGate }

func (g transactionalBatchGate) RevalidateTx(ctx context.Context, _ *sql.Tx, req executioncontract.RevalidationRequest) (executioncontract.ExecutionPlan, error) {
	return g.Revalidate(ctx, req)
}

func TestWorker_BatchJob_Lifecycle(t *testing.T) {
	env := setupWorkerTestEnv(t)
	svc := jobs.New(env.platformDB)
	ctx := context.Background()

	// Verify or provision batch items table
	var batchItemsExist bool
	err := env.targetExecDB.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = 'managed' AND table_name = 'batch_items')
	`).Scan(&batchItemsExist)
	if err != nil {
		t.Fatalf("check managed.batch_items: %v", err)
	}
	if !batchItemsExist {
		_, err := env.targetExecDB.ExecContext(ctx, "CREATE TABLE managed.batch_items (id bigint PRIMARY KEY, processed boolean NOT NULL DEFAULT false)")
		if err != nil {
			t.Fatalf("create managed.batch_items fixture: %v", err)
		}
		t.Cleanup(func() {
			cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cleanCancel()
			_, _ = env.targetExecDB.ExecContext(cleanCtx, "DROP TABLE IF EXISTS managed.batch_items")
		})
		if _, err := env.targetExecDB.ExecContext(ctx, "GRANT SELECT (id, processed) ON managed.batch_items TO wbd_target_a_receipt_owner"); err != nil {
			t.Fatalf("grant fixture access: %v", err)
		}
	}

	// Seed test items
	_, _ = env.targetExecDB.ExecContext(ctx, "DELETE FROM managed.batch_items WHERE id IN (90001, 90002)")
	_, err = env.targetExecDB.ExecContext(ctx, "INSERT INTO managed.batch_items (id, processed) VALUES (90001, false), (90002, false)")
	if err != nil {
		t.Fatalf("seed batch items: %v", err)
	}
	t.Cleanup(func() {
		_, _ = env.targetExecDB.ExecContext(context.Background(), "DELETE FROM managed.batch_items WHERE id IN (90001, 90002)")
	})

	fix := enqueueBatchJob(t, env, env.physicalTargetID, "target_a", 2)

	gate := executioncontract.FakeGate{
		RevalidateFunc: func(_ context.Context, req executioncontract.RevalidationRequest) (executioncontract.ExecutionPlan, error) {
			return planFor(req, "1", fix)
		},
		RecordCommitFunc: func(_ context.Context, _ *sql.Tx, _ executioncontract.CommitEvidence) error {
			return nil
		},
	}

	w, err := New(svc, jobs.ExecutionDeps{
		Gate:    transactionalBatchGate{gate},
		Acquire: env.makeAcquire(t),
		Timeout: 10 * time.Second,
	}, Config{
		PollInterval:  50 * time.Millisecond,
		LeaseDuration: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("create worker: %v", err)
	}

	didWork, err := w.Step(ctx)
	if err != nil {
		t.Fatalf("worker step error: %v", err)
	}
	if !didWork {
		t.Fatal("expected worker step to claim and execute batch job")
	}

	// Verify batch job succeeded
	job, err := svc.GetJob(ctx, fix.dbID, fix.jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.Status != executioncontract.Succeeded {
		t.Fatalf("expected batch job status succeeded, got %s", job.Status)
	}

	// Verify items processed
	var p1, p2 bool
	err = env.targetExecDB.QueryRowContext(ctx, `
		SELECT (SELECT processed FROM managed.batch_items WHERE id = 90001),
		       (SELECT processed FROM managed.batch_items WHERE id = 90002)
	`).Scan(&p1, &p2)
	if err != nil || !p1 || !p2 {
		t.Fatalf("expected items processed=true, got p1=%v, p2=%v, err=%v", p1, p2, err)
	}
}

func TestWorker_LeaseRenewal(t *testing.T) {
	env := setupWorkerTestEnv(t)
	svc := jobs.New(env.platformDB)
	ctx := context.Background()

	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	table := "worker_renew_" + suffix
	sqlUp := fmt.Sprintf("CREATE TABLE managed.%s (id bigint);", table)
	t.Cleanup(func() {
		_, _ = env.targetExecDB.ExecContext(context.Background(), "DROP TABLE IF EXISTS managed."+table)
	})

	fix := enqueueStandardJob(t, env, env.physicalTargetID, "target_a", sqlUp, "")

	var renewMu sync.Mutex
	var renewEvents []string

	gate := executioncontract.FakeGate{
		RevalidateFunc: func(_ context.Context, req executioncontract.RevalidationRequest) (executioncontract.ExecutionPlan, error) {
			// Delay slightly so that renewal tick fires during execution
			time.Sleep(120 * time.Millisecond)
			return planFor(req, "1", fix)
		},
		RecordCommitFunc: func(_ context.Context, _ *sql.Tx, _ executioncontract.CommitEvidence) error {
			return nil
		},
	}

	w, err := New(svc, jobs.ExecutionDeps{
		Gate:    gate,
		Acquire: env.makeAcquire(t),
		Timeout: 10 * time.Second,
	}, Config{
		LeaseDuration: 500 * time.Millisecond,
		RenewInterval: 50 * time.Millisecond,
		Logger: func(event string, fields map[string]any) {
			renewMu.Lock()
			defer renewMu.Unlock()
			if event == "lease_renewed" {
				renewEvents = append(renewEvents, event)
			}
		},
	})
	if err != nil {
		t.Fatalf("create worker: %v", err)
	}

	didWork, err := w.Step(ctx)
	if err != nil {
		t.Fatalf("step error: %v", err)
	}
	if !didWork {
		t.Fatal("expected work performed")
	}

	renewMu.Lock()
	count := len(renewEvents)
	renewMu.Unlock()

	if count == 0 {
		t.Fatal("expected at least one lease_renewed event during execution")
	}

	job, err := svc.GetJob(ctx, fix.dbID, fix.jobID)
	if err != nil || job.Status != executioncontract.Succeeded {
		t.Fatalf("expected job succeeded, got job=%+v, err=%v", job, err)
	}
}

func TestWorker_StopAuthorizingOnStaleRenewal(t *testing.T) {
	env := setupWorkerTestEnv(t)
	svc := jobs.New(env.platformDB)
	ctx := context.Background()

	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	table := "worker_stale_" + suffix
	sqlUp := fmt.Sprintf("CREATE TABLE managed.%s (id bigint);", table)
	t.Cleanup(func() {
		_, _ = env.targetExecDB.ExecContext(context.Background(), "DROP TABLE IF EXISTS managed."+table)
	})

	fix := enqueueStandardJob(t, env, env.physicalTargetID, "target_a", sqlUp, "")

	var failedRenewalLogged bool
	var logMu sync.Mutex

	gate := executioncontract.FakeGate{
		RevalidateFunc: func(_ context.Context, req executioncontract.RevalidationRequest) (executioncontract.ExecutionPlan, error) {
			// Tamper with lease token in database so renewal fails
			_, tErr := env.platformDB.ExecContext(context.Background(), `
				UPDATE app.jobs SET lease_token = $1 WHERE id = $2
			`, uuid.NewString(), fix.jobID)
			if tErr != nil {
				return executioncontract.ExecutionPlan{}, tErr
			}
			// Wait for renewal tick to fire and fail
			time.Sleep(120 * time.Millisecond)
			return planFor(req, "1", fix)
		},
	}

	w, err := New(svc, jobs.ExecutionDeps{
		Gate:    gate,
		Acquire: env.makeAcquire(t),
		Timeout: 10 * time.Second,
	}, Config{
		LeaseDuration: 400 * time.Millisecond,
		RenewInterval: 40 * time.Millisecond,
		Logger: func(event string, fields map[string]any) {
			if event == "lease_renewal_failed" {
				logMu.Lock()
				failedRenewalLogged = true
				logMu.Unlock()
			}
		},
	})
	if err != nil {
		t.Fatalf("create worker: %v", err)
	}

	// Execution must fail or abort because renewal failed
	_, execErr := w.Step(ctx)
	if execErr == nil {
		t.Fatal("expected execution error on failed renewal, got nil")
	}

	logMu.Lock()
	logged := failedRenewalLogged
	logMu.Unlock()

	if !logged {
		t.Fatal("expected lease_renewal_failed event to be logged")
	}

	// Target table must NOT have been created
	var tableExists bool
	_ = env.targetExecDB.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = 'managed' AND table_name = $1)
	`, table).Scan(&tableExists)
	if tableExists {
		t.Fatal("target table must not exist when renewal failed and authorization stopped")
	}
}

func TestWorker_Shutdown_BoundedDrain(t *testing.T) {
	env := setupWorkerTestEnv(t)
	svc := jobs.New(env.platformDB)
	ctx := context.Background()

	t.Run("CleanDrain", func(t *testing.T) {
		suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
		table := "worker_drain_clean_" + suffix
		sqlUp := fmt.Sprintf("CREATE TABLE managed.%s (id bigint);", table)
		t.Cleanup(func() {
			_, _ = env.targetExecDB.ExecContext(context.Background(), "DROP TABLE IF EXISTS managed."+table)
		})

		fix := enqueueStandardJob(t, env, env.physicalTargetID, "target_a", sqlUp, "")

		stopCalled := make(chan struct{})
		gate := executioncontract.FakeGate{
			RevalidateFunc: func(_ context.Context, req executioncontract.RevalidationRequest) (executioncontract.ExecutionPlan, error) {
				<-stopCalled
				return planFor(req, "1", fix)
			},
			RecordCommitFunc: func(_ context.Context, _ *sql.Tx, _ executioncontract.CommitEvidence) error {
				return nil
			},
		}

		var drainedCleanly bool
		var mu sync.Mutex

		var w *Worker
		w, _ = New(svc, jobs.ExecutionDeps{
			Gate:    gate,
			Acquire: env.makeAcquire(t),
			Timeout: 10 * time.Second,
		}, Config{
			DrainTimeout: 5 * time.Second,
			PollInterval: 50 * time.Millisecond,
			Logger: func(event string, fields map[string]any) {
				mu.Lock()
				defer mu.Unlock()
				if event == "job_executing" {
					go w.Stop()
				}
				if event == "shutdown_drain_active_job" {
					select {
					case <-stopCalled:
					default:
						close(stopCalled)
					}
				}
				if event == "shutdown_drained_cleanly" {
					drainedCleanly = true
				}
			},
		})

		runCtx, runCancel := context.WithTimeout(ctx, 10*time.Second)
		defer runCancel()

		err := w.Run(runCtx)
		if err != nil {
			t.Fatalf("worker run error: %v", err)
		}

		mu.Lock()
		clean := drainedCleanly
		mu.Unlock()

		if !clean {
			t.Fatal("expected worker to drain cleanly within DrainTimeout")
		}

		// Job completed
		job, err := svc.GetJob(ctx, fix.dbID, fix.jobID)
		if err != nil || job.Status != executioncontract.Succeeded {
			t.Fatalf("expected job succeeded, got job=%+v, err=%v", job, err)
		}
	})

	t.Run("DrainTimeoutCancelsStalledJob", func(t *testing.T) {
		suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
		table := "worker_drain_to_" + suffix
		sqlUp := fmt.Sprintf("CREATE TABLE managed.%s (id bigint);", table)
		t.Cleanup(func() {
			_, _ = env.targetExecDB.ExecContext(context.Background(), "DROP TABLE IF EXISTS managed."+table)
		})

		fix := enqueueStandardJob(t, env, env.physicalTargetID, "target_a", sqlUp, "")
		_ = fix.jobID

		hangCh := make(chan struct{})
		t.Cleanup(func() { close(hangCh) })

		gate := executioncontract.FakeGate{
			RevalidateFunc: func(rCtx context.Context, req executioncontract.RevalidationRequest) (executioncontract.ExecutionPlan, error) {
				select {
				case <-rCtx.Done():
					return executioncontract.ExecutionPlan{}, rCtx.Err()
				case <-hangCh:
					return executioncontract.ExecutionPlan{}, errors.New("closed")
				}
			},
		}

		var timeoutCancelled bool
		var mu sync.Mutex

		var w *Worker
		w, _ = New(svc, jobs.ExecutionDeps{
			Gate:    gate,
			Acquire: env.makeAcquire(t),
			Timeout: 10 * time.Second,
		}, Config{
			DrainTimeout: 100 * time.Millisecond,
			PollInterval: 50 * time.Millisecond,
			Logger: func(event string, fields map[string]any) {
				mu.Lock()
				defer mu.Unlock()
				if event == "job_executing" {
					go w.Stop()
				}
				if event == "shutdown_drain_timeout_canceling" {
					timeoutCancelled = true
				}
			},
		})

		runCtx, runCancel := context.WithTimeout(ctx, 10*time.Second)
		defer runCancel()

		err := w.Run(runCtx)
		if err != nil {
			t.Fatalf("worker run: %v", err)
		}

		mu.Lock()
		canceled := timeoutCancelled
		mu.Unlock()

		if !canceled {
			t.Fatal("expected shutdown_drain_timeout_canceling to be logged")
		}

		// Table must not exist
		var exists bool
		_ = env.targetExecDB.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema='managed' AND table_name=$1)", table).Scan(&exists)
		if exists {
			t.Fatal("target table must not exist after drain timeout cancellation")
		}
	})
}

func TestWorker_Run_ParentCancellationDrainsJob(t *testing.T) {
	env := setupWorkerTestEnv(t)
	svc := jobs.New(env.platformDB)
	table := "worker_parent_drain_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	t.Cleanup(func() {
		_, _ = env.targetExecDB.ExecContext(context.Background(), "DROP TABLE IF EXISTS managed."+table)
	})
	fix := enqueueStandardJob(t, env, env.physicalTargetID, "target_a", "CREATE TABLE managed."+table+" (id bigint);", "")

	entered := make(chan context.Context, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	draining := make(chan struct{})
	gate := executioncontract.FakeGate{
		RevalidateFunc: func(ctx context.Context, req executioncontract.RevalidationRequest) (executioncontract.ExecutionPlan, error) {
			entered <- ctx
			select {
			case <-ctx.Done():
				return executioncontract.ExecutionPlan{}, ctx.Err()
			case <-release:
				return planFor(req, "1", fix)
			}
		},
		RecordCommitFunc: func(context.Context, *sql.Tx, executioncontract.CommitEvidence) error { return nil },
	}
	w, err := New(svc, jobs.ExecutionDeps{Gate: gate, Acquire: env.makeAcquire(t), Timeout: 10 * time.Second}, Config{
		DrainTimeout: 5 * time.Second,
		Logger: func(event string, _ map[string]any) {
			if event == "shutdown_drain_active_job" {
				close(draining)
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- w.Run(ctx) }()
	var execCtx context.Context
	select {
	case execCtx = <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("job did not reach gate")
	}
	cancel()
	select {
	case <-draining:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not start draining")
	}
	if err := execCtx.Err(); err != nil {
		t.Fatalf("execution canceled before drain: %v", err)
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after drain")
	}
	job, err := svc.GetJob(context.Background(), fix.dbID, fix.jobID)
	if err != nil || job.Status != executioncontract.Succeeded {
		t.Fatalf("expected succeeded job after drain, got job=%+v err=%v", job, err)
	}
	if didWork, err := w.Step(context.Background()); didWork || err != nil {
		t.Fatalf("stopped worker admitted another cycle: didWork=%v err=%v", didWork, err)
	}
}

func TestWorker_Run_UncooperativeGateBounded(t *testing.T) {
	env := setupWorkerTestEnv(t)
	svc := jobs.New(env.platformDB)
	table := "worker_parent_block_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	_ = enqueueStandardJob(t, env, env.physicalTargetID, "target_a", "CREATE TABLE managed."+table+" (id bigint);", "")
	entered := make(chan context.Context, 1)
	release := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	finished := make(chan struct{})
	t.Cleanup(func() {
		unblock()
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Error("blocked execution did not exit after cleanup release")
		}
	})
	gate := executioncontract.FakeGate{
		RevalidateFunc: func(ctx context.Context, _ executioncontract.RevalidationRequest) (executioncontract.ExecutionPlan, error) {
			entered <- ctx
			<-release
			return executioncontract.ExecutionPlan{}, ctx.Err()
		},
	}
	w, err := New(svc, jobs.ExecutionDeps{Gate: gate, Acquire: env.makeAcquire(t), Timeout: 10 * time.Second}, Config{
		DrainTimeout: 100 * time.Millisecond,
		Logger: func(event string, _ map[string]any) {
			if event == "job_execution_failed" {
				close(finished)
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- w.Run(ctx) }()
	var execCtx context.Context
	select {
	case execCtx = <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("job did not reach gate")
	}
	cancel()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run waited for an uncooperative gate")
	}
	if !errors.Is(execCtx.Err(), context.Canceled) {
		t.Fatalf("execution was not canceled at drain deadline: %v", execCtx.Err())
	}
	select {
	case <-finished:
		t.Fatal("execution finished while gate was still blocked")
	default:
	}
	select {
	case <-w.Done():
		t.Fatal("worker reported completion while its gate was still blocked")
	default:
	}
	unblock()
	select {
	case <-w.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not finish after the gate was released")
	}
	if didWork, err := w.Step(context.Background()); didWork || err != nil {
		t.Fatalf("stopped worker admitted another cycle: didWork=%v err=%v", didWork, err)
	}
}

func TestWorker_ReceiptOnlyRecovery(t *testing.T) {
	env := setupWorkerTestEnv(t)
	svc := jobs.New(env.platformDB)
	ctx := context.Background()

	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	table := "worker_recov_" + suffix
	sqlUp := fmt.Sprintf("CREATE TABLE managed.%s (id bigint);", table)
	t.Cleanup(func() {
		_, _ = env.targetExecDB.ExecContext(context.Background(), "DROP TABLE IF EXISTS managed."+table)
	})

	fix := enqueueStandardJob(t, env, env.physicalTargetID, "target_a", sqlUp, "")

	gate := executioncontract.FakeGate{
		RevalidateFunc: func(_ context.Context, req executioncontract.RevalidationRequest) (executioncontract.ExecutionPlan, error) {
			return planFor(req, "1", fix)
		},
		RecordCommitFunc: func(_ context.Context, _ *sql.Tx, _ executioncontract.CommitEvidence) error {
			return nil
		},
	}

	deps := jobs.ExecutionDeps{
		Gate:    gate,
		Acquire: env.makeAcquire(t),
		Timeout: 10 * time.Second,
	}

	w, err := New(svc, deps, Config{
		WorkerID: "recov-worker",
	})
	if err != nil {
		t.Fatalf("new worker: %v", err)
	}

	// Trigger fault injection: fail when platform job is updated to succeeded.
	// Target transaction will commit and record valid receipt, but platform finalization fails, leaving status='unknown'.
	suffixTrig := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	fn := "w_fail_" + suffixTrig
	tr := "w_trig_" + suffixTrig
	_, err = env.ownerDB.ExecContext(ctx, fmt.Sprintf(`
		CREATE FUNCTION app.%s() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.id = '%s'::uuid AND NEW.status = 'succeeded' THEN
				RAISE EXCEPTION 'injected platform finalize failure';
			END IF;
			RETURN NEW;
		END $$
	`, fn, fix.jobID))
	if err != nil {
		t.Fatalf("create trigger fn: %v", err)
	}
	t.Cleanup(func() {
		_, _ = env.ownerDB.ExecContext(context.Background(), fmt.Sprintf("DROP FUNCTION IF EXISTS app.%s() CASCADE", fn))
	})
	_, err = env.ownerDB.ExecContext(ctx, fmt.Sprintf(`
		CREATE TRIGGER %s BEFORE UPDATE ON app.jobs FOR EACH ROW EXECUTE FUNCTION app.%s()
	`, tr, fn))
	if err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	// Step worker once to execute on target and fail platform finalization
	didWork, execErr := w.Step(ctx)
	if execErr != nil {
		t.Fatalf("step error: %v", execErr)
	}
	if !didWork {
		t.Fatal("expected worker to claim and execute job")
	}

	// Drop trigger: proves rejection is based on live lease, not trigger exception
	_, _ = env.ownerDB.ExecContext(ctx, fmt.Sprintf("DROP TRIGGER IF EXISTS %s ON app.jobs", tr))

	// Unexpired lease: ReconcileUnresolved must NOT steal live unknown job; status stays unchanged
	n, err := w.ReconcileUnresolved(ctx)
	if err != nil {
		t.Fatalf("reconcile unexpired: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 reconciled jobs while lease unexpired, got %d", n)
	}

	jobBefore, err := svc.GetJob(ctx, fix.dbID, fix.jobID)
	if err != nil || jobBefore.Status != executioncontract.Unknown || !jobBefore.BlocksDatabase {
		t.Fatalf("expected live unknown job unchanged (status=unknown, blocks_database=true), got %+v, err=%v", jobBefore, err)
	}
	// Expiry and renewal race: if lease was renewed before atomic finalization,
	// atomic fence under target lock must reject recovery finalization.
	var claimForRace jobs.Claim
	err = env.platformDB.QueryRowContext(ctx, `
		SELECT j.id::text, j.execution_id::text, j.database_id::text, j.target_id::text,
		       j.proposal_id::text, j.migration_id::text, j.revision, j.actor_id::text,
		       j.action, j.direction, j.mode, j.snapshot_hash, COALESCE(j.approval_fingerprint, ''),
		       a.id::text, a.attempt_no
		FROM app.jobs j
		JOIN app.job_attempts a ON a.job_id = j.id AND a.attempt_no = j.attempt_no
		WHERE j.id = $1`, fix.jobID).Scan(
		&claimForRace.JobID, &claimForRace.ExecutionID, &claimForRace.DatabaseID, &claimForRace.TargetID,
		&claimForRace.ProposalID, &claimForRace.MigrationID, &claimForRace.Revision, &claimForRace.ActorID,
		&claimForRace.Action, &claimForRace.Direction, &claimForRace.Mode, &claimForRace.SnapshotHash,
		&claimForRace.ApprovalFingerprint, &claimForRace.AttemptID, &claimForRace.AttemptNo)
	if err != nil {
		t.Fatalf("query claim for race: %v", err)
	}
	_, raceErr := svc.ReconcileStandardJob(ctx, &claimForRace, deps)
	if raceErr == nil {
		t.Fatal("expected atomic finalization fence to reject recovery while lease is renewed/live")
	}

	jobDuringRace, err := svc.GetJob(ctx, fix.dbID, fix.jobID)
	if err != nil || jobDuringRace.Status != executioncontract.Unknown || !jobDuringRace.BlocksDatabase {
		t.Fatalf("expected job to remain unknown and blocked during renewal race, got %+v, err=%v", jobDuringRace, err)
	}

	// Expire lease
	_, err = env.platformDB.ExecContext(ctx, "UPDATE app.jobs SET lease_expires_at = clock_timestamp() - interval '1 second' WHERE id = $1", fix.jobID)
	if err != nil {
		t.Fatalf("expire lease: %v", err)
	}

	// Now ReconcileUnresolved should discover the expired candidate and finalize receipt without replay!
	n, err = w.ReconcileUnresolved(ctx)
	if err != nil {
		t.Fatalf("reconcile expired error: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 reconciled job, got %d", n)
	}

	// Verify job is now succeeded and unblocked
	job, err := svc.GetJob(ctx, fix.dbID, fix.jobID)
	if err != nil || job.Status != executioncontract.Succeeded || job.BlocksDatabase {
		t.Fatalf("expected job succeeded and unblocked, got %+v, err=%v", job, err)
	}
}

func TestWorker_Concurrency_DifferentTargets(t *testing.T) {
	env := setupWorkerTestEnv(t)
	if env.targetExecDB_B == nil || env.physicalTargetID_B == "" {
		t.Skip("target B not configured, skipping different-target concurrency test")
	}

	svc := jobs.New(env.platformDB)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	tableA := "tbl_a_" + suffix
	tableB := "tbl_b_" + suffix
	sqlA := fmt.Sprintf("CREATE TABLE managed.%s (id bigint);", tableA)
	sqlB := fmt.Sprintf("CREATE TABLE managed.%s (id bigint);", tableB)

	t.Cleanup(func() {
		_, _ = env.targetExecDB.ExecContext(context.Background(), "DROP TABLE IF EXISTS managed."+tableA)
		if env.targetExecDB_B != nil {
			_, _ = env.targetExecDB_B.ExecContext(context.Background(), "DROP TABLE IF EXISTS managed."+tableB)
		}
	})

	fixA := enqueueStandardJob(t, env, env.physicalTargetID, "target_a", sqlA, "")
	fixB := enqueueStandardJob(t, env, env.physicalTargetID_B, "target_b", sqlB, "")

	sharedGate := executioncontract.FakeGate{
		RevalidateFunc: func(_ context.Context, req executioncontract.RevalidationRequest) (executioncontract.ExecutionPlan, error) {
			return planFor(req, "1", fixA, fixB)
		},
		RecordCommitFunc: func(_ context.Context, _ *sql.Tx, _ executioncontract.CommitEvidence) error { return nil },
	}

	dualAcquire := env.makeDualAcquire(t, fixA.dbID, fixB.dbID)

	wA, err := New(svc, jobs.ExecutionDeps{Gate: sharedGate, Acquire: dualAcquire, Timeout: 10 * time.Second}, Config{
		WorkerID:     "worker-target-a",
		PollInterval: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("create wA: %v", err)
	}

	wB, err := New(svc, jobs.ExecutionDeps{Gate: sharedGate, Acquire: dualAcquire, Timeout: 10 * time.Second}, Config{
		WorkerID:     "worker-target-b",
		PollInterval: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("create wB: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		for ctx.Err() == nil {
			job, _ := svc.GetJob(ctx, fixA.dbID, fixA.jobID)
			if job.Status == executioncontract.Succeeded {
				return
			}
			_, _ = wA.Step(ctx)
			time.Sleep(20 * time.Millisecond)
		}
	}()

	go func() {
		defer wg.Done()
		for ctx.Err() == nil {
			job, _ := svc.GetJob(ctx, fixB.dbID, fixB.jobID)
			if job.Status == executioncontract.Succeeded {
				return
			}
			_, _ = wB.Step(ctx)
			time.Sleep(20 * time.Millisecond)
		}
	}()

	wg.Wait()

	jobA, err := svc.GetJob(ctx, fixA.dbID, fixA.jobID)
	if err != nil || jobA.Status != executioncontract.Succeeded {
		t.Fatalf("expected job A succeeded, got job=%+v, err=%v", jobA, err)
	}

	jobB, err := svc.GetJob(ctx, fixB.dbID, fixB.jobID)
	if err != nil || jobB.Status != executioncontract.Succeeded {
		t.Fatalf("expected job B succeeded, got job=%+v, err=%v", jobB, err)
	}
}

func TestWorker_Shutdown_StubbornDependency(t *testing.T) {
	env := setupWorkerTestEnv(t)
	svc := jobs.New(env.platformDB)
	ctx := context.Background()

	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	table := "worker_drain_stub_" + suffix
	sqlUp := fmt.Sprintf("CREATE TABLE managed.%s (id bigint);", table)
	t.Cleanup(func() {
		_, _ = env.targetExecDB.ExecContext(context.Background(), "DROP TABLE IF EXISTS managed."+table)
	})

	fix := enqueueStandardJob(t, env, env.physicalTargetID, "target_a", sqlUp, "")

	unblockDependency := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-unblockDependency:
		default:
			close(unblockDependency)
		}
	})

	gate := executioncontract.FakeGate{
		RevalidateFunc: func(rCtx context.Context, req executioncontract.RevalidationRequest) (executioncontract.ExecutionPlan, error) {
			select {
			case <-rCtx.Done():
				time.Sleep(150 * time.Millisecond)
				return executioncontract.ExecutionPlan{}, rCtx.Err()
			case <-unblockDependency:
				return planFor(req, "1", fix)
			}
		},
	}

	var abandonedLogged bool
	var mu sync.Mutex

	var w *Worker
	w, err := New(svc, jobs.ExecutionDeps{
		Gate:    gate,
		Acquire: env.makeAcquire(t),
		Timeout: 10 * time.Second,
	}, Config{
		DrainTimeout: 100 * time.Millisecond,
		PollInterval: 50 * time.Millisecond,
		Logger: func(event string, fields map[string]any) {
			mu.Lock()
			defer mu.Unlock()
			if event == "job_executing" {
				go w.Stop()
			}
			if event == "shutdown_drain_abandoned" {
				abandonedLogged = true
			}
		},
	})
	if err != nil {
		t.Fatalf("create worker: %v", err)
	}

	runCtx, runCancel := context.WithTimeout(ctx, 3*time.Second)
	defer runCancel()

	start := time.Now()
	err = w.Run(runCtx)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("worker run: %v", err)
	}

	if elapsed > 1500*time.Millisecond {
		t.Fatalf("shutdown took %v, exceeded drain bound for stubborn dependency", elapsed)
	}

	mu.Lock()
	abandoned := abandonedLogged
	mu.Unlock()
	if !abandoned {
		t.Fatal("expected shutdown_drain_abandoned to be logged for stubborn dependency")
	}
}

func TestWorker_Lifecycle_StopAtClaimToRegistrationBoundary(t *testing.T) {
	env := setupWorkerTestEnv(t)
	svc := jobs.New(env.platformDB)
	ctx := context.Background()

	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	table := "worker_stop_bnd_" + suffix
	sqlUp := fmt.Sprintf("CREATE TABLE managed.%s (id bigint);", table)
	t.Cleanup(func() {
		_, _ = env.targetExecDB.ExecContext(context.Background(), "DROP TABLE IF EXISTS managed."+table)
	})

	_ = enqueueStandardJob(t, env, env.physicalTargetID, "target_a", sqlUp, "")

	var w *Worker
	w, err := New(svc, jobs.ExecutionDeps{
		Gate: executioncontract.FakeGate{
			RevalidateFunc: func(_ context.Context, _ executioncontract.RevalidationRequest) (executioncontract.ExecutionPlan, error) {
				return executioncontract.ExecutionPlan{}, nil
			},
		},
		Acquire: env.makeAcquire(t),
		Timeout: 10 * time.Second,
	}, Config{
		DrainTimeout: 200 * time.Millisecond,
		PollInterval: 50 * time.Millisecond,
		Logger: func(event string, fields map[string]any) {
			if event == "job_claimed" {
				w.Stop()
			}
		},
	})
	if err != nil {
		t.Fatalf("create worker: %v", err)
	}

	runCtx, runCancel := context.WithTimeout(ctx, 3*time.Second)
	defer runCancel()

	start := time.Now()
	_ = w.Run(runCtx)
	elapsed := time.Since(start)

	if elapsed > 1500*time.Millisecond {
		t.Fatalf("shutdown took %v, exceeded bounded drain", elapsed)
	}

	var exists bool
	_ = env.targetExecDB.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema='managed' AND table_name=$1)", table).Scan(&exists)
	if exists {
		t.Fatal("target table must not exist after stopping at claim-to-registration boundary")
	}
}

func TestWorker_Lifecycle_StopDuringRecovery(t *testing.T) {
	env := setupWorkerTestEnv(t)
	svc := jobs.New(env.platformDB)
	ctx := context.Background()

	recoveryStarted := make(chan struct{})
	var stopCanceledRecovery bool
	var mu sync.Mutex

	customAcquire := func(ctx context.Context, databaseID string) (*migration.TargetLease, error) {
		select {
		case <-recoveryStarted:
		default:
			close(recoveryStarted)
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}

	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	table := "worker_stop_recov_" + suffix
	fix := enqueueStandardJob(t, env, env.physicalTargetID, "target_a", fmt.Sprintf("CREATE TABLE managed.%s (id bigint);", table), "")
	t.Cleanup(func() {
		_, _ = env.targetExecDB.ExecContext(context.Background(), "DROP TABLE IF EXISTS managed."+table)
	})

	suffixTrig := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	fn := "w_stoprec_" + suffixTrig
	tr := "w_trig_stoprec_" + suffixTrig
	_, err := env.ownerDB.ExecContext(ctx, fmt.Sprintf(`
		CREATE FUNCTION app.%s() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.id = '%s'::uuid AND NEW.status = 'succeeded' THEN
				RAISE EXCEPTION 'injected platform finalize failure';
			END IF;
			RETURN NEW;
		END $$
	`, fn, fix.jobID))
	if err != nil {
		t.Fatalf("create trigger fn: %v", err)
	}
	t.Cleanup(func() {
		_, _ = env.ownerDB.ExecContext(context.Background(), fmt.Sprintf("DROP FUNCTION IF EXISTS app.%s() CASCADE", fn))
	})
	_, err = env.ownerDB.ExecContext(ctx, fmt.Sprintf(`
		CREATE TRIGGER %s BEFORE UPDATE ON app.jobs FOR EACH ROW EXECUTE FUNCTION app.%s()
	`, tr, fn))
	if err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	t.Cleanup(func() {
		_, _ = env.ownerDB.ExecContext(context.Background(), fmt.Sprintf("DROP TRIGGER IF EXISTS %s ON app.jobs", tr))
	})

	gate := executioncontract.FakeGate{
		RevalidateFunc: func(_ context.Context, req executioncontract.RevalidationRequest) (executioncontract.ExecutionPlan, error) {
			return planFor(req, "1", fix)
		},
		RecordCommitFunc: func(_ context.Context, _ *sql.Tx, _ executioncontract.CommitEvidence) error {
			return nil
		},
	}

	wInit, _ := New(svc, jobs.ExecutionDeps{
		Gate:    gate,
		Acquire: env.makeAcquire(t),
		Timeout: 10 * time.Second,
	}, Config{})
	_, _ = wInit.Step(ctx)

	_, _ = env.ownerDB.ExecContext(ctx, fmt.Sprintf("DROP TRIGGER IF EXISTS %s ON app.jobs", tr))
	_, err = env.platformDB.ExecContext(ctx, "UPDATE app.jobs SET lease_expires_at = clock_timestamp() - interval '1 second' WHERE id = $1", fix.jobID)
	if err != nil {
		t.Fatalf("expire lease: %v", err)
	}

	var w *Worker
	w, err = New(svc, jobs.ExecutionDeps{
		Gate:    gate,
		Acquire: customAcquire,
		Timeout: 10 * time.Second,
	}, Config{
		DrainTimeout:     300 * time.Millisecond,
		PollInterval:     50 * time.Millisecond,
		RecoveryInterval: 50 * time.Millisecond,
		Logger: func(event string, fields map[string]any) {
			mu.Lock()
			defer mu.Unlock()
			if event == "shutdown_cancel_active_recovery" {
				stopCanceledRecovery = true
			}
		},
	})
	if err != nil {
		t.Fatalf("create worker: %v", err)
	}

	go func() {
		<-recoveryStarted
		w.Stop()
	}()

	runCtx, runCancel := context.WithTimeout(ctx, 3*time.Second)
	defer runCancel()

	start := time.Now()
	err = w.Run(runCtx)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("worker run error: %v", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("shutdown took %v, exceeded drain timeout", elapsed)
	}

	mu.Lock()
	canceledRec := stopCanceledRecovery
	mu.Unlock()

	if !canceledRec {
		t.Fatal("expected shutdown_cancel_active_recovery to be logged")
	}
}

func TestWorker_Step_HonorsCallerContext(t *testing.T) {
	env := setupWorkerTestEnv(t)
	svc := jobs.New(env.platformDB)

	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	table := "worker_caller_ctx_" + suffix
	sqlUp := fmt.Sprintf("CREATE TABLE managed.%s (id bigint);", table)
	t.Cleanup(func() {
		_, _ = env.targetExecDB.ExecContext(context.Background(), "DROP TABLE IF EXISTS managed."+table)
	})

	_ = enqueueStandardJob(t, env, env.physicalTargetID, "target_a", sqlUp, "")

	gateBlock := make(chan struct{})
	gate := executioncontract.FakeGate{
		RevalidateFunc: func(rCtx context.Context, req executioncontract.RevalidationRequest) (executioncontract.ExecutionPlan, error) {
			select {
			case <-rCtx.Done():
				return executioncontract.ExecutionPlan{}, rCtx.Err()
			case <-gateBlock:
				return executioncontract.ExecutionPlan{}, nil
			}
		},
	}

	w, err := New(svc, jobs.ExecutionDeps{
		Gate:    gate,
		Acquire: env.makeAcquire(t),
		Timeout: 10 * time.Second,
	}, Config{
		PollInterval: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("create worker: %v", err)
	}

	stepCtx, cancelStep := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancelStep()
	}()

	didWork, execErr := w.Step(stepCtx)
	if !didWork {
		t.Fatal("expected didWork to be true")
	}
	if execErr == nil || !errors.Is(execErr, context.Canceled) {
		t.Fatalf("expected context.Canceled error honoring caller context, got %v", execErr)
	}
}

func TestWorker_Recovery_CandidateRotation_AvoidStarvation(t *testing.T) {
	env := setupWorkerTestEnv(t)
	if env.targetExecDB_B == nil || env.physicalTargetID_B == "" {
		t.Skip("target B not configured, skipping rotation test")
	}

	svc := jobs.New(env.platformDB)
	ctx := context.Background()

	var verTargetA, verTargetB int64
	_ = env.platformDB.QueryRowContext(ctx, "SELECT COALESCE(next_version, 1) FROM app.migration_target_state WHERE id = $1", env.physicalTargetID).Scan(&verTargetA)
	_ = env.platformDB.QueryRowContext(ctx, "SELECT COALESCE(next_version, 1) FROM app.migration_target_state WHERE id = $1", env.physicalTargetID_B).Scan(&verTargetB)
	if verTargetA < 1 {
		verTargetA = 1
	}
	if verTargetB < 1 {
		verTargetB = 1
	}
	suffix1 := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	table1 := "worker_starv1_" + suffix1
	fixA := enqueueStandardJob(t, env, env.physicalTargetID, "target_a", fmt.Sprintf("CREATE TABLE managed.%s (id bigint);", table1), "")

	suffix2 := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	table2 := "worker_starv2_" + suffix2
	fixB := enqueueStandardJob(t, env, env.physicalTargetID_B, "target_b", fmt.Sprintf("CREATE TABLE managed.%s (id bigint);", table2), "")

	t.Cleanup(func() {
		_, _ = env.targetExecDB.ExecContext(context.Background(), "DROP TABLE IF EXISTS managed."+table1)
		if env.targetExecDB_B != nil {
			_, _ = env.targetExecDB_B.ExecContext(context.Background(), "DROP TABLE IF EXISTS managed."+table2)
		}
	})

	sharedGate := executioncontract.FakeGate{
		RevalidateFunc: func(_ context.Context, req executioncontract.RevalidationRequest) (executioncontract.ExecutionPlan, error) {
			ver := fmt.Sprintf("%d", verTargetA)
			if req.JobID == fixB.jobID {
				ver = fmt.Sprintf("%d", verTargetB)
			}
			return planFor(req, ver, fixA, fixB)
		},
		RecordCommitFunc: func(_ context.Context, _ *sql.Tx, _ executioncontract.CommitEvidence) error {
			return nil
		},
	}

	// Trigger fails platform finalization for both jobs so target transactions commit and receipts are written
	suffixTrig := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	fn := "w_fail_starv_" + suffixTrig
	tr := "w_trig_starv_" + suffixTrig
	_, err := env.ownerDB.ExecContext(ctx, fmt.Sprintf(`
		CREATE FUNCTION app.%s() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.status = 'succeeded' THEN
				RAISE EXCEPTION 'injected platform finalize failure';
			END IF;
			RETURN NEW;
		END $$
	`, fn))
	if err != nil {
		t.Fatalf("create trigger fn: %v", err)
	}
	t.Cleanup(func() {
		_, _ = env.ownerDB.ExecContext(context.Background(), fmt.Sprintf("DROP FUNCTION IF EXISTS app.%s() CASCADE", fn))
	})

	_, err = env.ownerDB.ExecContext(ctx, fmt.Sprintf(`
		CREATE TRIGGER %s BEFORE UPDATE ON app.jobs FOR EACH ROW EXECUTE FUNCTION app.%s()
	`, tr, fn))
	if err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	t.Cleanup(func() {
		_, _ = env.ownerDB.ExecContext(context.Background(), fmt.Sprintf("DROP TRIGGER IF EXISTS %s ON app.jobs", tr))
	})

	dualAcquire := env.makeDualAcquire(t, fixA.dbID, fixB.dbID)
	wSetup, err := New(svc, jobs.ExecutionDeps{
		Gate:    sharedGate,
		Acquire: dualAcquire,
		Timeout: 10 * time.Second,
	}, Config{
		WorkerID: "worker-setup-starv",
	})
	if err != nil {
		t.Fatalf("new setup worker: %v", err)
	}

	// Step fixA to commit on target A and enter status='unknown'
	didWork1, err1 := wSetup.Step(ctx)
	if err1 != nil || !didWork1 {
		t.Fatalf("setup step 1: didWork=%v, err=%v", didWork1, err1)
	}

	// Step fixB to commit on target B and enter status='unknown'
	didWork2, err2 := wSetup.Step(ctx)
	if err2 != nil || !didWork2 {
		t.Fatalf("setup step 2: didWork=%v, err=%v", didWork2, err2)
	}

	// Drop trigger so recovery can finalize
	_, _ = env.ownerDB.ExecContext(ctx, fmt.Sprintf("DROP TRIGGER IF EXISTS %s ON app.jobs", tr))

	firstJob, secondJob := fixA, fixB
	firstCleanupDB := env.targetCleanupDB
	if firstJob.jobID > secondJob.jobID {
		firstJob, secondJob = fixB, fixA
		firstCleanupDB = env.targetCleanupDB_B
	}
	if firstCleanupDB == nil {
		firstCleanupDB = env.targetExecDB
	}

	// Make first candidate irreconcilable by deleting target receipt and commit proof
	_, err = firstCleanupDB.ExecContext(ctx, "DELETE FROM wbd_auth.execution_receipts WHERE execution_id = $1", firstJob.executionID)
	if err != nil {
		t.Fatalf("delete target receipt: %v", err)
	}
	cleanDB := env.ownerDB
	if cleanDB == nil {
		cleanDB = env.platformDB
	}
	_, err = cleanDB.ExecContext(ctx, "DELETE FROM app.job_commit_proofs WHERE job_id = $1", firstJob.jobID)
	if err != nil {
		t.Fatalf("delete commit proof: %v", err)
	}
	// Expire leases on both jobs
	_, err = env.platformDB.ExecContext(ctx, "UPDATE app.jobs SET lease_expires_at = clock_timestamp() - interval '5 seconds' WHERE id IN ($1, $2)", firstJob.jobID, secondJob.jobID)
	if err != nil {
		t.Fatalf("expire leases: %v", err)
	}

	w, err := New(svc, jobs.ExecutionDeps{
		Gate:    sharedGate,
		Acquire: dualAcquire,
		Timeout: 10 * time.Second,
	}, Config{
		RecoveryLimit: 1,
	})
	if err != nil {
		t.Fatalf("new worker: %v", err)
	}

	// Cycle 1: inspects candidate 1, not reconcilable, not counted as reconciled
	n1, err := w.ReconcileUnresolved(ctx)
	if err != nil {
		t.Fatalf("reconcile cycle 1: %v", err)
	}
	if n1 != 0 {
		t.Fatalf("expected 0 reconciled in cycle 1, got %d", n1)
	}

	// Cycle 2: rotates to candidate 2, reconciles candidate 2!
	n2, err := w.ReconcileUnresolved(ctx)
	if err != nil {
		t.Fatalf("reconcile cycle 2: %v", err)
	}
	if n2 != 1 {
		t.Fatalf("expected 1 reconciled in cycle 2 after rotation, got %d", n2)
	}

	job2, err := svc.GetJob(ctx, secondJob.dbID, secondJob.jobID)
	if err != nil || job2.Status != executioncontract.Succeeded {
		t.Fatalf("expected second candidate succeeded after rotation, got status=%s, err=%v", job2.Status, err)
	}
}

func TestWorker_StructuredLogs_DiscardSensitiveStrings(t *testing.T) {
	env := setupWorkerTestEnv(t)
	svc := jobs.New(env.platformDB)
	ctx := context.Background()

	sensitiveSecret := "SUPER_SECRET_LEAK_TOKEN_XYZ123"

	var logMu sync.Mutex
	var logs []map[string]any

	logger := func(event string, fields map[string]any) {
		logMu.Lock()
		defer logMu.Unlock()
		entry := make(map[string]any)
		for k, v := range fields {
			entry[k] = v
		}
		entry["_event"] = event
		logs = append(logs, entry)
	}

	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	table := "worker_sens_" + suffix
	sqlUp := fmt.Sprintf("CREATE TABLE managed.%s (id bigint);", table)
	t.Cleanup(func() {
		_, _ = env.targetExecDB.ExecContext(context.Background(), "DROP TABLE IF EXISTS managed."+table)
	})

	_ = enqueueStandardJob(t, env, env.physicalTargetID, "target_a", sqlUp, "")

	gate := executioncontract.FakeGate{
		RevalidateFunc: func(_ context.Context, _ executioncontract.RevalidationRequest) (executioncontract.ExecutionPlan, error) {
			return executioncontract.ExecutionPlan{}, &pgconn.PgError{
				Code:    "42501",
				Message: "permission denied with secret: " + sensitiveSecret,
				Detail:  "connection string postgres://user:" + sensitiveSecret + "@localhost:5432/db",
			}
		},
	}

	w, err := New(svc, jobs.ExecutionDeps{
		Gate:    gate,
		Acquire: env.makeAcquire(t),
		Timeout: 10 * time.Second,
	}, Config{
		Logger:       logger,
		PollInterval: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("new worker: %v", err)
	}

	_, _ = w.Step(ctx)

	logMu.Lock()
	defer logMu.Unlock()

	for _, entry := range logs {
		for k, v := range entry {
			s := fmt.Sprintf("%v", v)
			if strings.Contains(s, sensitiveSecret) {
				t.Fatalf("sensitive string leaked in log field %q: %s", k, s)
			}
		}
	}
}

func TestWorker_TracePropagation(t *testing.T) {
	env := setupWorkerTestEnv(t)
	svc := jobs.New(env.platformDB)
	ctx := context.Background()

	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	table := "worker_trace_" + suffix
	sqlUp := fmt.Sprintf("CREATE TABLE managed.%s (id bigint);", table)
	t.Cleanup(func() {
		_, _ = env.targetExecDB.ExecContext(context.Background(), "DROP TABLE IF EXISTS managed."+table)
	})

	fix := enqueueStandardJob(t, env, env.physicalTargetID, "target_a", sqlUp, "")

	wantTraceparent := "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	wantTracestate := "rojo=1,congo=2"

	_, err := env.platformDB.ExecContext(ctx, "UPDATE app.jobs SET traceparent = $1, tracestate = $2 WHERE id = $3",
		wantTraceparent, wantTracestate, fix.jobID)
	if err != nil {
		t.Fatalf("update job trace: %v", err)
	}

	var traceCaptured bool
	var capturedTP, capturedTS string
	var mu sync.Mutex

	gate := executioncontract.FakeGate{
		RevalidateFunc: func(rCtx context.Context, req executioncontract.RevalidationRequest) (executioncontract.ExecutionPlan, error) {
			mu.Lock()
			tc, ok := TraceFromContext(rCtx)
			if ok {
				traceCaptured = true
				capturedTP = tc.Traceparent
				capturedTS = tc.Tracestate
			}
			mu.Unlock()
			return planFor(req, "1", fix)
		},
		RecordCommitFunc: func(_ context.Context, _ *sql.Tx, _ executioncontract.CommitEvidence) error {
			return nil
		},
	}

	var logTP, logTS string
	logger := func(event string, fields map[string]any) {
		mu.Lock()
		defer mu.Unlock()
		if event == "job_claimed" || event == "job_executing" {
			if tp, ok := fields["traceparent"].(string); ok {
				logTP = tp
			}
			if ts, ok := fields["tracestate"].(string); ok {
				logTS = ts
			}
		}
	}

	w, err := New(svc, jobs.ExecutionDeps{
		Gate:    gate,
		Acquire: env.makeAcquire(t),
		Timeout: 10 * time.Second,
	}, Config{
		Logger:       logger,
		PollInterval: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("new worker: %v", err)
	}

	didWork, err := w.Step(ctx)
	if err != nil {
		t.Fatalf("step error: %v", err)
	}
	if !didWork {
		t.Fatal("expected didWork to be true")
	}

	mu.Lock()
	tcFound := traceCaptured
	cTP := capturedTP
	cTS := capturedTS
	lTP := logTP
	lTS := logTS
	mu.Unlock()

	if !tcFound || cTP != wantTraceparent || cTS != wantTracestate {
		t.Fatalf("trace context not propagated: found=%v, tp=%s, ts=%s", tcFound, cTP, cTS)
	}
	if lTP != wantTraceparent || lTS != wantTracestate {
		t.Fatalf("trace fields not logged: tp=%s, ts=%s", lTP, lTS)
	}
}

func TestWorker_TerminalOutcomeEventsAndTraceHelpers(t *testing.T) {
	cases := []struct {
		status       executioncontract.JobStatus
		wantFinished string
		wantStep     string
	}{
		{executioncontract.Succeeded, "job_finished_success", "job_execution_completed"},
		{executioncontract.Failed, "job_finished_failed", "job_execution_failed"},
		{executioncontract.Partial, "job_finished_partial", "job_execution_partial"},
		{executioncontract.PartiallyReverted, "job_finished_partially_reverted", "job_execution_partially_reverted"},
		{executioncontract.Unknown, "job_finished_unknown", "job_execution_unknown"},
	}
	for _, tc := range cases {
		gotFin, gotStep := terminalOutcomeEvents(tc.status)
		if gotFin != tc.wantFinished || gotStep != tc.wantStep {
			t.Fatalf("status=%s: got (%s, %s), want (%s, %s)", tc.status, gotFin, gotStep, tc.wantFinished, tc.wantStep)
		}
	}

	validClaim := &jobs.Claim{
		Traceparent: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
		Tracestate:  "rojo=1",
	}
	validCtx, validTrace, validPersisted := prepareJobTrace(context.Background(), validClaim)
	if !validPersisted || validTrace.TraceID != "4bf92f3577b34da6a3ce929d0e0e4736" || validTrace.SpanID == "" {
		t.Fatalf("expected valid persisted trace, got persisted=%v trace=%+v", validPersisted, validTrace)
	}
	if tc, ok := TraceFromContext(validCtx); !ok || tc.Traceparent != validClaim.Traceparent || tc.Tracestate != validClaim.Tracestate {
		t.Fatalf("expected persisted trace in context, got ok=%v tc=%+v", ok, tc)
	}
	validFields := map[string]any{}
	addJobTraceFields(validFields, validClaim, validTrace, validPersisted)
	if validFields["trace_id"] != "4bf92f3577b34da6a3ce929d0e0e4736" || validFields["span_id"] != validTrace.SpanID || validFields["traceparent"] != validClaim.Traceparent || validFields["tracestate"] != validClaim.Tracestate {
		t.Fatalf("unexpected valid trace fields: %+v", validFields)
	}

	for _, badClaim := range []*jobs.Claim{
		{Traceparent: "", Tracestate: ""},
		{Traceparent: "invalid-traceparent", Tracestate: "rojo=1"},
	} {
		badCtx, localTrace, persisted := prepareJobTrace(context.Background(), badClaim)
		if persisted {
			t.Fatalf("expected persisted=false for %+v", badClaim)
		}
		if _, ok := TraceFromContext(badCtx); ok {
			t.Fatalf("expected TraceFromContext=false for %+v", badClaim)
		}
		if localTrace.TraceID == "" || localTrace.SpanID == "" {
			t.Fatalf("expected local fallback trace IDs for %+v, got %+v", badClaim, localTrace)
		}
		f := map[string]any{}
		addJobTraceFields(f, badClaim, localTrace, persisted)
		if _, hasTP := f["traceparent"]; hasTP {
			t.Fatalf("unexpected traceparent logged for %+v: %+v", badClaim, f)
		}
		if _, hasTS := f["tracestate"]; hasTS {
			t.Fatalf("unexpected tracestate logged for %+v: %+v", badClaim, f)
		}
		if f["trace_id"] != localTrace.TraceID || f["span_id"] != localTrace.SpanID {
			t.Fatalf("expected local trace_id/span_id, got %+v", f)
		}
	}

	wrappedPg := fmt.Errorf("wrapped: %w", &pgconn.PgError{Code: "42P07", Message: "secret_msg"})
	if got := classifyError(wrappedPg); got != "SQLSTATE:42P07" {
		t.Fatalf("expected SQLSTATE:42P07, got %s", got)
	}
}

func TestWorker_NonSuccessNilErrorOutcomesAndTraceCorrelation(t *testing.T) {
	env := setupWorkerTestEnv(t)
	svc := jobs.New(env.platformDB)

	type capturedLog struct {
		event  string
		fields map[string]any
	}
	captureLogger := func(mu *sync.Mutex, logs *[]capturedLog) func(string, map[string]any) {
		return func(event string, fields map[string]any) {
			mu.Lock()
			defer mu.Unlock()
			cp := make(map[string]any, len(fields))
			for k, v := range fields {
				cp[k] = v
			}
			*logs = append(*logs, capturedLog{event: event, fields: cp})
		}
	}

	t.Run("failed_sql_nil_error_logs_honest_terminal_events_and_trace_ids", func(t *testing.T) {
		ctx := context.Background()
		suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
		table := "worker_fail_sql_" + suffix
		if _, err := env.targetExecDB.ExecContext(ctx, "CREATE TABLE managed."+table+" (id bigint);"); err != nil {
			t.Fatalf("precreate table: %v", err)
		}
		t.Cleanup(func() {
			_, _ = env.targetExecDB.ExecContext(context.Background(), "DROP TABLE IF EXISTS managed."+table)
		})

		sqlUp := fmt.Sprintf("CREATE TABLE managed.%s (id bigint);", table)
		fix := enqueueStandardJob(t, env, env.physicalTargetID, "target_a", sqlUp, "")
		wantTraceID := "4bf92f3577b34da6a3ce929d0e0e4736"
		wantTP := "00-" + wantTraceID + "-00f067aa0ba902b7-01"
		wantTS := "rojo=1"
		if _, err := env.platformDB.ExecContext(ctx, "UPDATE app.jobs SET traceparent = $1, tracestate = $2 WHERE id = $3", wantTP, wantTS, fix.jobID); err != nil {
			t.Fatalf("set trace context: %v", err)
		}

		var mu sync.Mutex
		var logs []capturedLog
		gate := executioncontract.FakeGate{
			RevalidateFunc: func(_ context.Context, req executioncontract.RevalidationRequest) (executioncontract.ExecutionPlan, error) {
				return planFor(req, "1", fix)
			},
		}
		w, err := New(svc, jobs.ExecutionDeps{Gate: gate, Acquire: env.makeAcquire(t), Timeout: 10 * time.Second}, Config{
			Logger:       captureLogger(&mu, &logs),
			PollInterval: 50 * time.Millisecond,
		})
		if err != nil {
			t.Fatalf("new worker: %v", err)
		}

		didWork, err := w.Step(ctx)
		if err != nil || !didWork {
			t.Fatalf("expected didWork=true err=nil, got didWork=%v err=%v", didWork, err)
		}
		jobView, err := svc.GetJob(ctx, fix.dbID, fix.jobID)
		if err != nil || jobView.Status != executioncontract.Failed {
			t.Fatalf("expected failed job status, got %+v err=%v", jobView, err)
		}

		mu.Lock()
		defer mu.Unlock()
		seen := map[string]bool{}
		var spanID string
		for _, entry := range logs {
			seen[entry.event] = true
			if entry.event == "job_finished_success" || entry.event == "job_execution_completed" {
				t.Fatalf("unexpected success event %s for failed job: %+v", entry.event, entry.fields)
			}
			if entry.event == "job_claimed" || entry.event == "job_executing" || entry.event == "job_finished_failed" || entry.event == "job_execution_failed" {
				if entry.fields["trace_id"] != wantTraceID {
					t.Fatalf("%s trace_id=%v, want %s", entry.event, entry.fields["trace_id"], wantTraceID)
				}
				sid, _ := entry.fields["span_id"].(string)
				if sid == "" {
					t.Fatalf("%s missing span_id: %+v", entry.event, entry.fields)
				}
				if spanID == "" {
					spanID = sid
				} else if sid != spanID {
					t.Fatalf("%s span_id=%s, want consistent %s", entry.event, sid, spanID)
				}
				if entry.fields["traceparent"] != wantTP || entry.fields["tracestate"] != wantTS {
					t.Fatalf("%s trace headers mismatch: %+v", entry.event, entry.fields)
				}
			}
			if entry.event == "job_finished_failed" || entry.event == "job_execution_failed" {
				if entry.fields["status"] != "failed" || entry.fields["error_code"] != "SQLSTATE:42P07" {
					t.Fatalf("%s expected status=failed error_code=SQLSTATE:42P07, got %+v", entry.event, entry.fields)
				}
			}
		}
		for _, want := range []string{"job_claimed", "job_executing", "job_finished_failed", "job_execution_failed"} {
			if !seen[want] {
				t.Fatalf("missing expected event %s; saw %+v", want, seen)
			}
		}
	})

	t.Run("partial_batch_nil_error_logs_partial_events_with_trace_ids", func(t *testing.T) {
		ctx := context.Background()
		var batchItemsExist bool
		if err := env.targetExecDB.QueryRowContext(ctx, `
			SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = 'managed' AND table_name = 'batch_items')
		`).Scan(&batchItemsExist); err != nil {
			t.Fatalf("check managed.batch_items: %v", err)
		}
		if !batchItemsExist {
			if _, err := env.targetExecDB.ExecContext(ctx, "CREATE TABLE managed.batch_items (id bigint PRIMARY KEY, processed boolean NOT NULL DEFAULT false)"); err != nil {
				t.Fatalf("create managed.batch_items: %v", err)
			}
			t.Cleanup(func() {
				_, _ = env.targetExecDB.ExecContext(context.Background(), "DROP TABLE IF EXISTS managed.batch_items")
			})
			if _, err := env.targetExecDB.ExecContext(ctx, "GRANT SELECT (id, processed) ON managed.batch_items TO wbd_target_a_receipt_owner"); err != nil {
				t.Fatalf("grant fixture access: %v", err)
			}
		}
		_, _ = env.targetExecDB.ExecContext(ctx, "DELETE FROM managed.batch_items WHERE id IN (91001, 91002)")
		if _, err := env.targetExecDB.ExecContext(ctx, "INSERT INTO managed.batch_items (id, processed) VALUES (91001, false), (91002, false)"); err != nil {
			t.Fatalf("seed batch items: %v", err)
		}
		t.Cleanup(func() {
			_, _ = env.targetExecDB.ExecContext(context.Background(), "DELETE FROM managed.batch_items WHERE id IN (91001, 91002)")
		})

		fix := enqueueBatchJob(t, env, env.physicalTargetID, "target_a", 1)
		t.Cleanup(func() {
			if env.targetCleanupDB != nil {
				for _, q := range []string{
					"DELETE FROM wbd_auth.batch_row_versions WHERE execution_id = $1",
					"DELETE FROM wbd_auth.batch_tickets WHERE execution_id = $1",
					"DELETE FROM wbd_auth.batch_checkpoints WHERE execution_id = $1",
					"DELETE FROM wbd_auth.batch_scope_items WHERE execution_id = $1",
					"DELETE FROM wbd_auth.batch_scopes WHERE execution_id = $1",
				} {
					_, _ = env.targetCleanupDB.ExecContext(context.Background(), q, fix.executionID)
				}
			}
		})

		wantTraceID := "4bf92f3577b34da6a3ce929d0e0e4736"
		wantTP := "00-" + wantTraceID + "-00f067aa0ba902b7-01"
		if _, err := env.platformDB.ExecContext(ctx, "UPDATE app.jobs SET traceparent = $1 WHERE id = $2", wantTP, fix.jobID); err != nil {
			t.Fatalf("set batch traceparent: %v", err)
		}

		// Approval turns stale only after the first chunk commits, so the job ends partial.
		gate := executioncontract.FakeGate{
			RevalidateFunc: func(ctx context.Context, req executioncontract.RevalidationRequest) (executioncontract.ExecutionPlan, error) {
				var committed bool
				if err := env.platformDB.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM app.job_batch_commit_proofs WHERE job_id = $1)", req.JobID).Scan(&committed); err != nil {
					return executioncontract.ExecutionPlan{}, err
				}
				if committed {
					return executioncontract.ExecutionPlan{}, executioncontract.ContractError{Code: "REVIEW_STALE", Message: "approval stale"}
				}
				return planFor(req, "1", fix)
			},
			RecordCommitFunc: func(_ context.Context, _ *sql.Tx, _ executioncontract.CommitEvidence) error {
				return nil
			},
		}
		var mu sync.Mutex
		var logs []capturedLog
		w, err := New(svc, jobs.ExecutionDeps{Gate: transactionalBatchGate{gate}, Acquire: env.makeAcquire(t), Timeout: 10 * time.Second}, Config{
			Logger:       captureLogger(&mu, &logs),
			PollInterval: 50 * time.Millisecond,
		})
		if err != nil {
			t.Fatalf("new worker: %v", err)
		}

		didWork, err := w.Step(ctx)
		if err != nil || !didWork {
			t.Fatalf("expected didWork=true err=nil, got didWork=%v err=%v", didWork, err)
		}

		mu.Lock()
		defer mu.Unlock()
		seen := map[string]bool{}
		for _, entry := range logs {
			seen[entry.event] = true
			if entry.event == "job_finished_success" || entry.event == "job_execution_completed" {
				t.Fatalf("unexpected success event %s for partial job: %+v", entry.event, entry.fields)
			}
			if entry.event == "job_finished_partial" || entry.event == "job_execution_partial" {
				if entry.fields["status"] != "partial" || entry.fields["trace_id"] != wantTraceID || entry.fields["span_id"] == "" {
					t.Fatalf("%s unexpected fields: %+v", entry.event, entry.fields)
				}
			}
		}
		for _, want := range []string{"job_claimed", "job_executing", "job_finished_partial", "job_execution_partial"} {
			if !seen[want] {
				t.Fatalf("missing expected event %s; saw %+v", want, seen)
			}
		}
	})

	t.Run("invalid_trace_context_does_not_disrupt_and_correlates_renewal", func(t *testing.T) {
		ctx := context.Background()
		suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
		table := "worker_badtrace_" + suffix
		sqlUp := fmt.Sprintf("CREATE TABLE managed.%s (id bigint);", table)
		t.Cleanup(func() {
			_, _ = env.targetExecDB.ExecContext(context.Background(), "DROP TABLE IF EXISTS managed."+table)
		})

		fix := enqueueStandardJob(t, env, env.physicalTargetID, "target_a", sqlUp, "")
		if _, err := env.platformDB.ExecContext(ctx, "UPDATE app.jobs SET traceparent = 'invalid-traceparent', tracestate = 'secret=should_not_echo' WHERE id = $1", fix.jobID); err != nil {
			t.Fatalf("set invalid trace context: %v", err)
		}

		var sawPersistedContext bool
		gate := executioncontract.FakeGate{
			RevalidateFunc: func(rCtx context.Context, req executioncontract.RevalidationRequest) (executioncontract.ExecutionPlan, error) {
				if _, ok := TraceFromContext(rCtx); ok {
					sawPersistedContext = true
				}
				time.Sleep(120 * time.Millisecond)
				return planFor(req, "1", fix)
			},
			RecordCommitFunc: func(_ context.Context, _ *sql.Tx, _ executioncontract.CommitEvidence) error {
				return nil
			},
		}
		var mu sync.Mutex
		var logs []capturedLog
		w, err := New(svc, jobs.ExecutionDeps{Gate: gate, Acquire: env.makeAcquire(t), Timeout: 10 * time.Second}, Config{
			LeaseDuration: 500 * time.Millisecond,
			RenewInterval: 50 * time.Millisecond,
			Logger:        captureLogger(&mu, &logs),
		})
		if err != nil {
			t.Fatalf("new worker: %v", err)
		}

		didWork, err := w.Step(ctx)
		if err != nil || !didWork {
			t.Fatalf("expected didWork=true err=nil, got didWork=%v err=%v", didWork, err)
		}
		if sawPersistedContext {
			t.Fatal("expected TraceFromContext=false for invalid persisted traceparent")
		}

		mu.Lock()
		defer mu.Unlock()
		seen := map[string]bool{}
		var traceID, spanID string
		for _, entry := range logs {
			seen[entry.event] = true
			if _, hasTP := entry.fields["traceparent"]; hasTP {
				t.Fatalf("expected no traceparent emitted on invalid context for %s: %+v", entry.event, entry.fields)
			}
			if _, hasTS := entry.fields["tracestate"]; hasTS {
				t.Fatalf("expected no tracestate emitted on invalid context for %s: %+v", entry.event, entry.fields)
			}
			tid, _ := entry.fields["trace_id"].(string)
			sid, _ := entry.fields["span_id"].(string)
			if tid == "" || sid == "" {
				t.Fatalf("%s missing trace_id or span_id: %+v", entry.event, entry.fields)
			}
			if traceID == "" {
				traceID, spanID = tid, sid
			} else if tid != traceID || sid != spanID {
				t.Fatalf("%s mismatched trace/span (%s, %s), want (%s, %s)", entry.event, tid, sid, traceID, spanID)
			}
		}
		for _, want := range []string{"job_claimed", "job_executing", "lease_renewed", "job_finished_success", "job_execution_completed"} {
			if !seen[want] {
				t.Fatalf("missing expected event %s; saw %+v", want, seen)
			}
		}
	})
}
