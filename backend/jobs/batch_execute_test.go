package jobs
import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Jerannn24/Tugas-Besar-WBD.git/executioncontract"
	"github.com/Jerannn24/Tugas-Besar-WBD.git/migration"
	"github.com/google/uuid"
)
type batchJobFixture struct {
	dbID        string
	targetID    string
	actorID     string
	draftID     string
	proposalID  string
	claim       *Claim
	snapshot    executioncontract.Draft
	executionID string
}

func setupBatchTestEnv(t *testing.T) (checkpointEnv, *sql.DB, *sql.DB) {
	t.Helper()
	if os.Getenv("BATCH_SCOPE_ISOLATED_TEST_TARGET") != "1" {
		t.Skip("BATCH_SCOPE_ISOLATED_TEST_TARGET=1 required for isolated batch execution test")
	}
	e := checkpointSetup(t)
	platformDB, ownerDB := testDBs(t)
	return e, platformDB, ownerDB
}

func createBatchFixture(t *testing.T, platformDB, ownerDB *sql.DB, targetID string, batchSize int, action executioncontract.Action, existingFixture *batchJobFixture) batchJobFixture {
	return createBatchFixtureWithActorAndDuration(t, platformDB, ownerDB, targetID, batchSize, action, existingFixture, "", 0)
}

func createBatchFixtureWithDuration(t *testing.T, platformDB, ownerDB *sql.DB, targetID string, batchSize int, action executioncontract.Action, existingFixture *batchJobFixture, leaseDuration time.Duration) batchJobFixture {
	return createBatchFixtureWithActorAndDuration(t, platformDB, ownerDB, targetID, batchSize, action, existingFixture, "", leaseDuration)
}

func createBatchFixtureWithActor(t *testing.T, platformDB, ownerDB *sql.DB, targetID string, batchSize int, action executioncontract.Action, existingFixture *batchJobFixture, customActorID string) batchJobFixture {
	return createBatchFixtureWithActorAndDuration(t, platformDB, ownerDB, targetID, batchSize, action, existingFixture, customActorID, 0)
}

func createBatchFixtureWithActorAndDuration(t *testing.T, platformDB, ownerDB *sql.DB, targetID string, batchSize int, action executioncontract.Action, existingFixture *batchJobFixture, customActorID string, leaseDuration time.Duration) batchJobFixture {
	return createBatchFixtureCore(t, platformDB, ownerDB, targetID, batchSize, action, existingFixture, customActorID, leaseDuration, nil)
}

func createBatchFixtureOnConnection(t *testing.T, platformDB, ownerDB *sql.DB, targetID string, batchSize int, shared *batchJobFixture) batchJobFixture {
	return createBatchFixtureCore(t, platformDB, ownerDB, targetID, batchSize, executioncontract.Apply, nil, "", 0, shared)
}

func createBatchFixtureCore(t *testing.T, platformDB, ownerDB *sql.DB, targetID string, batchSize int, action executioncontract.Action, existingFixture *batchJobFixture, customActorID string, leaseDuration time.Duration, shared *batchJobFixture) batchJobFixture {
	t.Helper()
	ctx := context.Background()

	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	sqlUp := "UPDATE managed.batch_items SET processed = TRUE WHERE id = ANY($1::bigint[]);"
	sqlDown := "UPDATE managed.batch_items SET processed = FALSE WHERE id = ANY($1::bigint[]);"
	title := "Batch Migration " + suffix
	var dbID, actorID, draftID string
	var sourceExecutionID *string
	if existingFixture != nil {
		dbID = existingFixture.dbID
		actorID = existingFixture.actorID
		if customActorID != "" {
			actorID = customActorID
		}
		draftID = existingFixture.draftID
		sourceExecutionID = &existingFixture.executionID
	} else if shared != nil {
		dbID = shared.dbID
		actorID = shared.actorID
		draftID = uuid.NewString()
		if _, err := platformDB.ExecContext(ctx, `
			INSERT INTO app.migration_drafts (id, database_id, revision, title, sql, sql_down, created_by)
			VALUES ($1, $2, 1, $3, $4, $5, $6)
		`, draftID, dbID, title, sqlUp, sqlDown, actorID); err != nil {
			t.Fatalf("insert draft: %v", err)
		}
	} else {
		dbID = uuid.NewString()
		actorID = uuid.NewString()
		if customActorID != "" {
			actorID = customActorID
		}
		draftID = uuid.NewString()

		t.Cleanup(func() {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			for _, q := range []string{
				"DELETE FROM app.job_batch_commit_proofs WHERE job_id IN (SELECT id FROM app.jobs WHERE database_id = $1)",
				"DELETE FROM app.job_batch_step_authorizations WHERE job_id IN (SELECT id FROM app.jobs WHERE database_id = $1)",
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
				_, _ = ownerDB.ExecContext(cleanupCtx, q, dbID)
			}
			_, _ = ownerDB.ExecContext(cleanupCtx, "DELETE FROM app.users WHERE id = $1", actorID)
		})

		username := "batch_u_" + suffix
		if _, err := platformDB.ExecContext(ctx, "INSERT INTO app.users (id, username, password_hash) VALUES ($1, $2, 'hash')", actorID, username); err != nil {
			t.Fatalf("insert user: %v", err)
		}
		if _, err := ownerDB.ExecContext(ctx, "INSERT INTO app.migration_target_state (id, next_version) VALUES ($1, 1) ON CONFLICT (id) DO NOTHING", targetID); err != nil {
			t.Fatalf("insert target state: %v", err)
		}
		connName := "batch_conn_" + suffix
		if _, err := platformDB.ExecContext(ctx, `
			INSERT INTO app.database_connections
				(id, name, host, port, database_name, username, password_ciphertext, credential_key_id, ssl_mode, target_key, execution_target_id)
			VALUES ($1, $2, 'postgres-test', 5432, 'test_target_a', 'batch_key', '\x01', 'key1', 'disable', 'tkey_' || $2, $3)
		`, dbID, connName, targetID); err != nil {
			t.Fatalf("insert database_connection: %v", err)
		}

		if _, err := platformDB.ExecContext(ctx, `
			INSERT INTO app.migration_drafts (id, database_id, revision, title, sql, sql_down, created_by)
			VALUES ($1, $2, 1, $3, $4, $5, $6)
		`, draftID, dbID, title, sqlUp, sqlDown, actorID); err != nil {
			t.Fatalf("insert draft: %v", err)
		}
	}

	proposalID := uuid.NewString()
	batchCfg := &executioncontract.BatchConfig{
		Schema:       "managed",
		Table:        "batch_items",
		CursorColumn: "id",
		BatchSize:    batchSize,
	}
	snapshot := executioncontract.Draft{
		MigrationID: draftID,
		DatabaseID:  dbID,
		Revision:    1,
		Title:       title,
		SQLUp:       sqlUp,
		SQLDown:     &sqlDown,
		Mode:        executioncontract.Batch,
		Batch:       batchCfg,
	}

	hashBytes := sha256.Sum256([]byte(sqlUp))
	snapshotBytes, _ := json.Marshal(snapshot)
	if _, err := platformDB.ExecContext(ctx, `
		INSERT INTO app.migration_proposals (
			id, database_id, migration_id, revision, action, source_execution_id, status,
			snapshot, snapshot_hash, created_by
		) VALUES ($1, $2, $3, 1, $4, $5, 'draft', $6::jsonb, $7, $8)
	`, proposalID, dbID, draftID, string(action), sourceExecutionID, string(snapshotBytes), hashBytes[:], actorID); err != nil {
		t.Fatalf("insert proposal: %v", err)
	}

	svc := New(platformDB)
	tx, err := platformDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin enqueue tx: %v", err)
	}
	defer tx.Rollback()

	reqUUID := uuid.NewString()
	enqRes, err := svc.EnqueueTx(ctx, tx, executioncontract.EnqueueRequest{
		TargetID:          targetID,
		RequestID:         reqUUID,
		ActorID:           actorID,
		DatabaseID:        dbID,
		ProposalID:        proposalID,
		MigrationID:       draftID,
		Revision:          1,
		Action:            action,
		Direction:         executioncontract.Up,
		SourceExecutionID: sourceExecutionID,
		Snapshot:          snapshot,
		SnapshotHash:      hex.EncodeToString(hashBytes[:]),
	})
	if err != nil {
		var exists bool
		_ = platformDB.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM app.migration_target_state WHERE id = $1)", targetID).Scan(&exists)
		t.Fatalf("enqueue batch job: %v (targetID=%s, target_state exists=%t)", err, targetID, exists)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit enqueue: %v", err)
	}
	leaseDur := 30 * time.Second
	if leaseDuration > 0 {
		leaseDur = leaseDuration
	}
	claim, err := svc.ClaimJob(ctx, ClaimRequest{
		WorkerID:      "worker-" + suffix,
		LeaseDuration: leaseDur,
		MaxAttempts:   3,
	})
	if err != nil || claim == nil {
		t.Fatalf("claim batch job: claim=%+v err=%v", claim, err)
	}

	return batchJobFixture{
		dbID:        dbID,
		targetID:    targetID,
		actorID:     actorID,
		draftID:     draftID,
		proposalID:  proposalID,
		claim:       claim,
		snapshot:    snapshot,
		executionID: enqRes.ExecutionID,
	}
}

func TestClaimJob_BatchForwardFromSucceededSource(t *testing.T) {
	e, platformDB, ownerDB := setupBatchTestEnv(t)
	setupBatchItemsFixture(t, e)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := e.exec.ExecContext(ctx, "INSERT INTO managed.batch_items (id, processed) VALUES (1, false)"); err != nil {
		t.Fatal(err)
	}
	svc := New(platformDB)
	acquire := makeBatchAcquire(t, e.exec, e.prep)
	source := createBatchFixture(t, platformDB, ownerDB, e.target, 1, executioncontract.Apply, nil)
	cleanupBatchScope(t, e, source.executionID)
	out, err := svc.ExecuteBatchJob(ctx, source.claim, ExecutionDeps{Gate: batchFakeGate(source.snapshot), Acquire: acquire, Timeout: 10 * time.Second})
	if err != nil || out == nil || out.Status != executioncontract.Succeeded {
		t.Fatalf("source outcome=%+v err=%v", out, err)
	}
	if _, err := e.exec.ExecContext(ctx, "UPDATE managed.batch_items SET processed = false"); err != nil {
		t.Fatalf("prepare row in reverted state: %v", err)
	}
	forward := createBatchFixture(t, platformDB, ownerDB, e.target, 1, executioncontract.Forward, &source)
	cleanupBatchScope(t, e, forward.executionID)
	if forward.claim.JobID == source.claim.JobID || forward.claim.Action != executioncontract.Forward {
		t.Fatalf("unexpected forward claim: %+v", forward.claim)
	}
	out, err = svc.ExecuteBatchJob(ctx, forward.claim, ExecutionDeps{Gate: batchFakeGate(forward.snapshot), Acquire: acquire, Timeout: 10 * time.Second})
	if err != nil || out == nil || out.Status != executioncontract.Succeeded {
		t.Fatalf("forward outcome=%+v err=%v", out, err)
	}
}

func makeBatchAcquire(t *testing.T, execDB, prepDB *sql.DB) func(ctx context.Context, databaseID string) (*migration.TargetLease, error) {
	t.Helper()
	return func(ctx context.Context, databaseID string) (*migration.TargetLease, error) {
		conn, err := execDB.Conn(ctx)
		if err != nil {
			return nil, err
		}
		return &migration.TargetLease{
			Executor: conn,
			Preparer: prepDB,
			Release: func() {
				_ = conn.Close()
			},
		}, nil
	}
}

type transactionalBatchGate struct {
	executioncontract.FakeGate
}

func (g transactionalBatchGate) RevalidateTx(ctx context.Context, _ *sql.Tx, req executioncontract.RevalidationRequest) (executioncontract.ExecutionPlan, error) {
	return g.Revalidate(ctx, req)
}

func batchFakeGate(snapshot executioncontract.Draft) transactionalBatchGate {
	return transactionalBatchGate{executioncontract.FakeGate{
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
	}}
}

type staleBatchGate struct {
	transactionalBatchGate
	stale *bool
}

func (g staleBatchGate) RevalidateTx(ctx context.Context, tx *sql.Tx, req executioncontract.RevalidationRequest) (executioncontract.ExecutionPlan, error) {
	if *g.stale {
		return executioncontract.ExecutionPlan{}, executioncontract.ContractError{Code: "REVIEW_STALE", Message: "approval expired"}
	}
	return g.transactionalBatchGate.RevalidateTx(ctx, tx, req)
}

func (g staleBatchGate) ReturnToReviewTx(context.Context, *sql.Tx, executioncontract.RevalidationRequest) error {
	return nil
}

func TestExecuteBatchJob_StaleApprovalAtDispatchFence(t *testing.T) {
	e, platformDB, ownerDB := setupBatchTestEnv(t)
	setupBatchItemsFixture(t, e)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := e.exec.ExecContext(ctx, "INSERT INTO managed.batch_items (id, processed) VALUES (1, false)"); err != nil {
		t.Fatal(err)
	}
	fix := createBatchFixture(t, platformDB, ownerDB, e.target, 2, executioncontract.Apply, nil)
	cleanupBatchScope(t, e, fix.executionID)
	stale := false
	out, err := New(platformDB).ExecuteBatchJob(ctx, fix.claim, ExecutionDeps{
		Gate: staleBatchGate{batchFakeGate(fix.snapshot), &stale},
		Acquire: makeBatchAcquire(t, e.exec, e.prep),
		Timeout: 10 * time.Second,
		afterTicketReservation: func(context.Context, int) error { stale = true; return nil },
	})
	if err != nil || out == nil || out.Status != executioncontract.Failed || !stale {
		t.Fatalf("outcome=%+v err=%v stale=%v", out, err, stale)
	}
	var processed bool
	if err := e.exec.QueryRowContext(ctx, "SELECT processed FROM managed.batch_items WHERE id=1").Scan(&processed); err != nil || processed {
		t.Fatalf("processed=%v err=%v", processed, err)
	}
	var status, code string
	var evidence int
	if err := platformDB.QueryRowContext(ctx, `SELECT status, error_code,
		(SELECT count(*) FROM app.job_statement_evidence WHERE attempt_id=$2)
		FROM app.job_attempts WHERE id=$2 AND job_id=$1`, fix.claim.JobID, fix.claim.AttemptID).Scan(&status, &code, &evidence); err != nil || status != "failed" || code != "REVIEW_STALE" || evidence != 0 {
		t.Fatalf("status=%s code=%s evidence=%d err=%v", status, code, evidence, err)
	}
}

// 1. Decisive PostgreSQL test:
// Batch 1 commits, Batch 2 fails with no checkpoint/changes,
// a new service resumes Batch 2 without replaying Batch 1 or touching later inserted rows.
func TestExecuteBatchJob_Decisive_Batch1Commits_Batch2Fails_ResumeSucceeds(t *testing.T) {
	e, platformDB, ownerDB := setupBatchTestEnv(t)
	setupBatchItemsFixture(t, e)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Initial dataset: 4 rows (1, 2, 3, 4), processed = false
	if _, err := e.exec.ExecContext(ctx, "INSERT INTO managed.batch_items (id, processed) VALUES (1, false), (2, false), (3, false), (4, false)"); err != nil {
		t.Fatalf("insert initial batch items: %v", err)
	}

	// Batch size 2 -> Batch 0: IDs [1, 2], Batch 1: IDs [3, 4]
	fix1 := createBatchFixture(t, platformDB, ownerDB, e.target, 2, executioncontract.Apply, nil)
	cleanupBatchScope(t, e, fix1.executionID)

	acquire := makeBatchAcquire(t, e.exec, e.prep)
	gate1 := batchFakeGate(fix1.snapshot)

	svc1 := New(platformDB)
	var batch1Ran, batch2Attempted bool

	deps1 := ExecutionDeps{
		Gate:    gate1,
		Acquire: acquire,
		Timeout: 10 * time.Second,
		afterTicketReservation: func(ctx context.Context, batchIndex int) error {
			if batchIndex == 0 {
				batch1Ran = true
				return nil
			}
			if batchIndex == 1 {
				batch2Attempted = true
				// Fail batch 2 AFTER ticket reservation on preparer
				return errors.New("simulated batch 2 failure after ticket reservation")
			}
			return nil
		},
	}

	outcome1, err := svc1.ExecuteBatchJob(ctx, fix1.claim, deps1)
	if err != nil {
		t.Fatalf("expected graceful partial outcome, got error: %v", err)
	}
	if outcome1 == nil || outcome1.Status != executioncontract.Partial {
		t.Fatalf("expected partial outcome on batch 2 failure, got: %+v", outcome1)
	}
	if !batch1Ran || !batch2Attempted {
		t.Fatalf("dispatch hooks: batch1Ran=%t batch2Attempted=%t", batch1Ran, batch2Attempted)
	}

	// Verify Target DB state:
	// Batch 1 committed -> rows 1 and 2 are true
	// Batch 2 failed -> rows 3 and 4 remain false
	var p1, p2, p3, p4 bool
	if err := e.exec.QueryRowContext(ctx, "SELECT (SELECT processed FROM managed.batch_items WHERE id = 1), (SELECT processed FROM managed.batch_items WHERE id = 2), (SELECT processed FROM managed.batch_items WHERE id = 3), (SELECT processed FROM managed.batch_items WHERE id = 4)").Scan(&p1, &p2, &p3, &p4); err != nil {
		t.Fatalf("query item states: %v", err)
	}
	if !p1 || !p2 || p3 || p4 {
		t.Fatalf("target rows state mismatch: 1=%t 2=%t 3=%t 4=%t", p1, p2, p3, p4)
	}

	// Exactly 1 checkpoint recorded on target (batch_index = 0)
	var chkCount int
	if err := e.prep.QueryRowContext(ctx, "SELECT count(*) FROM wbd_auth.batch_checkpoints WHERE execution_id = $1::uuid", fix1.executionID).Scan(&chkCount); err != nil {
		t.Fatalf("count checkpoints: %v", err)
	}
	if chkCount != 1 {
		t.Fatalf("expected 1 checkpoint, got %d", chkCount)
	}

	// Verify ticket for batch 1 exists on target and its target_xid is confirmed aborted
	// Verify preparer cannot SELECT from batch_tickets (least-privilege preservation)
	var unauthorizedTicketCount int
	if err := e.prep.QueryRowContext(ctx, "SELECT count(*) FROM wbd_auth.batch_tickets WHERE execution_id = $1::uuid AND batch_index = 1", fix1.executionID).Scan(&unauthorizedTicketCount); err == nil {
		t.Fatalf("expected preparer SELECT on batch_tickets to fail with permission denied")
	}

	// Verify ticket for batch 1 exists on target and its target_xid is confirmed aborted via cleanup role
	var ticketCount int
	if err := e.cleanup.QueryRowContext(ctx, "SELECT count(*) FROM wbd_auth.batch_tickets WHERE execution_id = $1::uuid AND batch_index = 1", fix1.executionID).Scan(&ticketCount); err != nil || ticketCount != 1 {
		t.Fatalf("expected ticket for batch 1 after reservation, count=%d err=%v", ticketCount, err)
	}
	var ticketXID string
	if err := e.cleanup.QueryRowContext(ctx, "SELECT target_xid::text FROM wbd_auth.batch_tickets WHERE execution_id = $1::uuid AND batch_index = 1", fix1.executionID).Scan(&ticketXID); err != nil {
		t.Fatalf("query batch 1 ticket XID: %v", err)
	}
	var ticketStatus string
	if err := e.cleanup.QueryRowContext(ctx, "SELECT pg_catalog.pg_xact_status($1::xid8)", ticketXID).Scan(&ticketStatus); err != nil || ticketStatus != "aborted" {
		t.Fatalf("expected batch 1 ticket XID to be aborted, got status=%s err=%v", ticketStatus, err)
	}
	// Verify Platform DB state: status = partial, blocks_database = true, progress records 1 batch committed
	var jobStatus string
	var blocksDB bool
	var progBytes []byte
	if err := platformDB.QueryRowContext(ctx, "SELECT status, blocks_database, progress FROM app.jobs WHERE id = $1", fix1.claim.JobID).Scan(&jobStatus, &blocksDB, &progBytes); err != nil {
		t.Fatalf("query job state: %v", err)
	}
	if jobStatus != "partial" || !blocksDB {
		t.Fatalf("job platform state: status=%s blocks_database=%t", jobStatus, blocksDB)
	}

	var prog executioncontract.Progress
	if err := json.Unmarshal(progBytes, &prog); err != nil || prog.BatchesCommitted != "1" || prog.RowsCommitted != "2" {
		t.Fatalf("job progress mismatch: %+v", prog)
	}

	// Step: External writer inserts row 5 (NOT member of frozen scope)
	if _, err := e.exec.ExecContext(ctx, "INSERT INTO managed.batch_items (id, processed) VALUES (5, false)"); err != nil {
		t.Fatalf("insert row 5: %v", err)
	}

	// Step: A NEW service resumes batch two
	svc2 := New(platformDB)

	fixResume := createBatchFixture(t, platformDB, ownerDB, e.target, 2, executioncontract.Resume, &fix1)

	var replayedBatch1 bool
	var resumedBatch2Ran bool
	deps2 := ExecutionDeps{
		Gate:    batchFakeGate(fixResume.snapshot),
		Acquire: acquire,
		Timeout: 10 * time.Second,
		beforeBatchDispatch: func(ctx context.Context, batchIndex int) error {
			if batchIndex == 0 {
				replayedBatch1 = true
				return errors.New("batch 1 replayed! should have been skipped")
			}
			if batchIndex == 1 {
				resumedBatch2Ran = true
			}
			return nil
		},
	}

	outcome2, err := svc2.ExecuteBatchJob(ctx, fixResume.claim, deps2)
	if err != nil {
		t.Fatalf("resume execution failed: %v", err)
	}
	if outcome2 == nil || outcome2.Status != executioncontract.Succeeded {
		t.Fatalf("expected resume succeeded, got: %+v", outcome2)
	}
	if replayedBatch1 {
		t.Fatal("FAILED: Batch 1 was replayed during resume!")
	}
	if !resumedBatch2Ran {
		t.Fatal("FAILED: Batch 2 was not executed during resume!")
	}

	// Verify Target DB state after resume:
	// Rows 1, 2, 3, 4 are processed = true
	// Row 5 STILL processed = false (never touched!)
	var r1, r2, r3, r4, r5 bool
	if err := e.exec.QueryRowContext(ctx, "SELECT (SELECT processed FROM managed.batch_items WHERE id = 1), (SELECT processed FROM managed.batch_items WHERE id = 2), (SELECT processed FROM managed.batch_items WHERE id = 3), (SELECT processed FROM managed.batch_items WHERE id = 4), (SELECT processed FROM managed.batch_items WHERE id = 5)").Scan(&r1, &r2, &r3, &r4, &r5); err != nil {
		t.Fatalf("query final item states: %v", err)
	}
	if !r1 || !r2 || !r3 || !r4 {
		t.Fatalf("scope rows not processed: 1=%t 2=%t 3=%t 4=%t", r1, r2, r3, r4)
	}
	if r5 {
		t.Fatal("FAILED: row 5 outside scope was touched by batch execution!")
	}

	// Exactly 2 checkpoints recorded on target
	if err := e.prep.QueryRowContext(ctx, "SELECT count(*) FROM wbd_auth.batch_checkpoints WHERE execution_id = $1::uuid", fix1.executionID).Scan(&chkCount); err != nil {
		t.Fatalf("count final checkpoints: %v", err)
	}
	if chkCount != 2 {
		t.Fatalf("expected 2 checkpoints, got %d", chkCount)
	}

	// Verify Platform DB state after resume:
	var finalStatus string
	var finalBlocks bool
	var finalProgBytes []byte
	if err := platformDB.QueryRowContext(ctx, "SELECT status, blocks_database, progress FROM app.jobs WHERE id = $1", fixResume.claim.JobID).Scan(&finalStatus, &finalBlocks, &finalProgBytes); err != nil {
		t.Fatalf("query resume job state: %v", err)
	}
	if finalStatus != "succeeded" || finalBlocks {
		t.Fatalf("resume job not succeeded or still blocked: status=%s blocks=%t", finalStatus, finalBlocks)
	}

	var finalProg executioncontract.Progress
	if err := json.Unmarshal(finalProgBytes, &finalProg); err != nil || finalProg.BatchesCommitted != "2" || finalProg.RowsCommitted != "4" {
		t.Fatalf("final progress mismatch: %+v", finalProg)
	}
}

// 2. Test wrong SQL shape and wrong batch config rejection
func TestExecuteBatchJob_WrongShape(t *testing.T) {
	e, platformDB, ownerDB := setupBatchTestEnv(t)
	setupBatchItemsFixture(t, e)

	ctx := context.Background()
	acquire := makeBatchAcquire(t, e.exec, e.prep)
	svc := New(platformDB)

	// Wrong SQL: arbitrary UPDATE
	fix := createBatchFixture(t, platformDB, ownerDB, e.target, 2, executioncontract.Apply, nil)
	cleanupBatchScope(t, e, fix.executionID)

	badSnapshot := fix.snapshot
	badSnapshot.SQLUp = "UPDATE managed.batch_items SET id = 999 WHERE id = 1"
	badGate := batchFakeGate(badSnapshot)

	outcome, err := svc.ExecuteBatchJob(ctx, fix.claim, ExecutionDeps{Gate: badGate, Acquire: acquire, Timeout: 10 * time.Second})
	if err == nil && (outcome == nil || outcome.Status != executioncontract.Failed) {
		t.Fatalf("expected failure on wrong SQL shape, got: outcome=%+v err=%v", outcome, err)
	}
}

// 3. Test external row change detection (fail closed with BATCH_UNSAFE)
func TestExecuteBatchJob_ExternalRowChanges_BatchUnsafe(t *testing.T) {
	e, platformDB, ownerDB := setupBatchTestEnv(t)
	setupBatchItemsFixture(t, e)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if _, err := e.exec.ExecContext(ctx, "INSERT INTO managed.batch_items (id, processed) VALUES (1, false), (2, false)"); err != nil {
		t.Fatalf("insert items: %v", err)
	}

	fix := createBatchFixture(t, platformDB, ownerDB, e.target, 2, executioncontract.Apply, nil)
	cleanupBatchScope(t, e, fix.executionID)

	acquire := makeBatchAcquire(t, e.exec, e.prep)
	svc := New(platformDB)

	// Before worker dispatches, external writer alters row 1 to processed = true
	deps := ExecutionDeps{
		Gate:    batchFakeGate(fix.snapshot),
		Acquire: acquire,
		Timeout: 10 * time.Second,
		beforeBatchDispatch: func(ctx context.Context, batchIndex int) error {
			_, err := e.exec.ExecContext(ctx, "UPDATE managed.batch_items SET processed = true WHERE id = 1")
			return err
		},
	}

	outcome, err := svc.ExecuteBatchJob(ctx, fix.claim, deps)
	if err == nil && (outcome == nil || outcome.Status != executioncontract.Failed) {
		t.Fatalf("expected BATCH_UNSAFE failure, got: outcome=%+v err=%v", outcome, err)
	}

	// Verify database is cleanly unblocked because target rollback was confirmed and no earlier step committed
	var blocksDB bool
	var jobStatus string
	if err := platformDB.QueryRowContext(ctx, "SELECT status, blocks_database FROM app.jobs WHERE id = $1", fix.claim.JobID).Scan(&jobStatus, &blocksDB); err != nil {
		t.Fatalf("query job: %v", err)
	}
	if blocksDB || jobStatus != "failed" {
		t.Fatalf("expected unblocked clean failed job, got status=%s blocked=%t", jobStatus, blocksDB)
	}
}

// 4. Test lease mismatch before batch dispatch
func TestExecuteBatchJob_LeaseMismatchBeforeDispatch(t *testing.T) {
	e, platformDB, ownerDB := setupBatchTestEnv(t)
	setupBatchItemsFixture(t, e)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if _, err := e.exec.ExecContext(ctx, "INSERT INTO managed.batch_items (id, processed) VALUES (1, false), (2, false)"); err != nil {
		t.Fatalf("insert items: %v", err)
	}

	fix := createBatchFixture(t, platformDB, ownerDB, e.target, 2, executioncontract.Apply, nil)
	cleanupBatchScope(t, e, fix.executionID)

	acquire := makeBatchAcquire(t, e.exec, e.prep)
	svc := New(platformDB)

	// Tamper lease before dispatch: change lease_token
	_, err := platformDB.ExecContext(ctx, "UPDATE app.jobs SET lease_token = gen_random_uuid() WHERE id = $1", fix.claim.JobID)
	if err != nil {
		t.Fatalf("tamper lease: %v", err)
	}

	_, err = svc.ExecuteBatchJob(ctx, fix.claim, ExecutionDeps{
		Gate:    batchFakeGate(fix.snapshot),
		Acquire: acquire,
		Timeout: 10 * time.Second,
	})
	if err == nil {
		t.Fatal("expected lease mismatch error, got nil")
	}
	var cErr executioncontract.ContractError
	if !errors.As(err, &cErr) || cErr.Code != "LEASE_MISMATCH" {
		t.Fatalf("expected LEASE_MISMATCH, got: %v", err)
	}

	// Verify 0 rows touched on target
	var processedCount int
	if err := e.exec.QueryRowContext(ctx, "SELECT count(*) FROM managed.batch_items WHERE processed = true").Scan(&processedCount); err != nil {
		t.Fatalf("query processed count: %v", err)
	}
	if processedCount != 0 {
		t.Fatalf("expected 0 rows processed, got %d", processedCount)
	}
}

// 5. Test crash after target commit before platform finalization, followed by resume reconciliation without replay
func TestExecuteBatchJob_CrashAfterTargetCommit_ReconcilesWithoutReplay(t *testing.T) {
	e, platformDB, ownerDB := setupBatchTestEnv(t)
	setupBatchItemsFixture(t, e)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := e.exec.ExecContext(ctx, "INSERT INTO managed.batch_items (id, processed) VALUES (1, false), (2, false), (3, false), (4, false)"); err != nil {
		t.Fatalf("insert items: %v", err)
	}

	fix1 := createBatchFixture(t, platformDB, ownerDB, e.target, 2, executioncontract.Apply, nil)
	cleanupBatchScope(t, e, fix1.executionID)

	acquire := makeBatchAcquire(t, e.exec, e.prep)
	svc1 := New(platformDB)

	// Simulate crash right after target commit of batch 0 (before platform finalization commits)
	deps1 := ExecutionDeps{
		Gate:    batchFakeGate(fix1.snapshot),
		Acquire: acquire,
		Timeout: 10 * time.Second,
		afterBatchCommit: func(ctx context.Context, batchIndex int) error {
			if batchIndex == 0 {
				return errors.New("simulated crash after target commit")
			}
			return nil
		},
	}

	outcome1, err := svc1.ExecuteBatchJob(ctx, fix1.claim, deps1)
	if err == nil && (outcome1 == nil || outcome1.Status != executioncontract.Unknown) {
		t.Fatalf("expected unknown outcome on crash after target commit, got: outcome=%+v err=%v", outcome1, err)
	}

	// Verify target DB: Batch 0 committed -> rows 1 and 2 are true
	var p1, p2 bool
	if err := e.exec.QueryRowContext(ctx, "SELECT (SELECT processed FROM managed.batch_items WHERE id = 1), (SELECT processed FROM managed.batch_items WHERE id = 2)").Scan(&p1, &p2); err != nil || !p1 || !p2 {
		t.Fatalf("target rows 1 and 2 should be committed: p1=%t p2=%t err=%v", p1, p2, err)
	}

	// Target has 1 checkpoint
	var chkCount int
	if err := e.prep.QueryRowContext(ctx, "SELECT count(*) FROM wbd_auth.batch_checkpoints WHERE execution_id = $1::uuid", fix1.executionID).Scan(&chkCount); err != nil || chkCount != 1 {
		t.Fatalf("expected 1 checkpoint on target, got %d", chkCount)
	}

	// Now a new service resumes the job
	svc2 := New(platformDB)
	fixResume := createBatchFixture(t, platformDB, ownerDB, e.target, 2, executioncontract.Resume, &fix1)

	var replayedBatch0 bool
	var batch1Ran bool
	gateResume := batchFakeGate(fixResume.snapshot)

	deps2 := ExecutionDeps{
		Gate:    gateResume,
		Acquire: acquire,
		Timeout: 10 * time.Second,
		beforeBatchDispatch: func(ctx context.Context, batchIndex int) error {
			if batchIndex == 0 {
				replayedBatch0 = true
				return errors.New("batch 0 was replayed! should have been reconciled without replay")
			}
			if batchIndex == 1 {
				batch1Ran = true
			}
			return nil
		},
	}

	outcome2, err := svc2.ExecuteBatchJob(ctx, fixResume.claim, deps2)
	if err != nil || outcome2 == nil || outcome2.Status != executioncontract.Succeeded {
		t.Fatalf("expected resume to succeed via reconciliation, got outcome=%+v err=%v", outcome2, err)
	}
	if replayedBatch0 {
		t.Fatal("FAILED: Batch 0 SQL was replayed instead of reconciled from proof!")
	}
	if !batch1Ran {
		t.Fatal("FAILED: Batch 1 was not executed!")
	}

	// Verify all 4 rows processed
	var allProcessed bool
	if err := e.exec.QueryRowContext(ctx, "SELECT bool_and(processed) FROM managed.batch_items WHERE id IN (1, 2, 3, 4)").Scan(&allProcessed); err != nil || !allProcessed {
		t.Fatalf("expected all rows processed: %t err=%v", allProcessed, err)
	}

	// Verify both checkpoints present
	if err := e.prep.QueryRowContext(ctx, "SELECT count(*) FROM wbd_auth.batch_checkpoints WHERE execution_id = $1::uuid", fix1.executionID).Scan(&chkCount); err != nil || chkCount != 2 {
		t.Fatalf("expected 2 checkpoints on target, got %d", chkCount)
	}
}

// 6. Test no dispatch after lease is lost at the actual dispatch boundary
func TestExecuteBatchJob_LeaseLostAtActualBoundary_NoDispatch(t *testing.T) {
	e, platformDB, ownerDB := setupBatchTestEnv(t)
	setupBatchItemsFixture(t, e)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if _, err := e.exec.ExecContext(ctx, "INSERT INTO managed.batch_items (id, processed) VALUES (1, false), (2, false)"); err != nil {
		t.Fatalf("insert items: %v", err)
	}

	fix := createBatchFixture(t, platformDB, ownerDB, e.target, 2, executioncontract.Apply, nil)
	cleanupBatchScope(t, e, fix.executionID)

	acquire := makeBatchAcquire(t, e.exec, e.prep)
	svc := New(platformDB)

	// Invalidate lease right before actual dispatch
	deps := ExecutionDeps{
		Gate:    batchFakeGate(fix.snapshot),
		Acquire: acquire,
		Timeout: 10 * time.Second,
		beforeBatchDispatch: func(ctx context.Context, batchIndex int) error {
			_, err := platformDB.ExecContext(ctx, "UPDATE app.jobs SET lease_expires_at = clock_timestamp() - interval '5 seconds' WHERE id = $1", fix.claim.JobID)
			return err
		},
	}

	_, err := svc.ExecuteBatchJob(ctx, fix.claim, deps)
	if err == nil {
		t.Fatal("expected lease error at dispatch boundary, got nil")
	}
	var cErr executioncontract.ContractError
	if !errors.As(err, &cErr) || cErr.Code != "LEASE_MISMATCH" {
		t.Fatalf("expected LEASE_MISMATCH at boundary, got: %v", err)
	}

	// Verify 0 rows touched on target and 0 tickets/checkpoints
	var processedCount int
	if err := e.exec.QueryRowContext(ctx, "SELECT count(*) FROM managed.batch_items WHERE processed = true").Scan(&processedCount); err != nil {
		t.Fatalf("query processed count: %v", err)
	}
	if processedCount != 0 {
		t.Fatalf("expected 0 rows processed after boundary lease loss, got %d", processedCount)
	}
	var ticketCount int
	if err := e.cleanup.QueryRowContext(ctx, "SELECT count(*) FROM wbd_auth.batch_tickets WHERE execution_id = $1::uuid", fix.executionID).Scan(&ticketCount); err != nil {
		t.Fatalf("query ticket count: %v", err)
	}
	if ticketCount != 0 {
		t.Fatalf("expected 0 tickets after boundary lease loss, got %d", ticketCount)
	}
}

// 7. Test no replay on uncertain XID
func TestExecuteBatchJob_UncertainXID_NoReplay(t *testing.T) {
	e, platformDB, ownerDB := setupBatchTestEnv(t)
	setupBatchItemsFixture(t, e)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if _, err := e.exec.ExecContext(ctx, "INSERT INTO managed.batch_items (id, processed) VALUES (1, false), (2, false)"); err != nil {
		t.Fatalf("insert items: %v", err)
	}

	fix := createBatchFixture(t, platformDB, ownerDB, e.target, 2, executioncontract.Apply, nil)
	cleanupBatchScope(t, e, fix.executionID)

	// Pre-insert a corrupt checkpoint with an invalid/nonexistent target_xid
	// Note: We use a non-existent xid8 that PostgreSQL will not report as committed
	_, err := e.cleanup.ExecContext(ctx, `
		INSERT INTO wbd_auth.batch_checkpoints
		    (execution_id, physical_target_id, target_xid, batch_index, direction, rows_affected)
		VALUES ($1::uuid, $2::uuid, '999999999999'::xid8, 0, 'up', 2)
	`, fix.executionID, e.target)
	if err != nil {
		t.Fatalf("insert corrupt checkpoint: %v", err)
	}

	acquire := makeBatchAcquire(t, e.exec, e.prep)
	svc := New(platformDB)

	outcome, err := svc.ExecuteBatchJob(ctx, fix.claim, ExecutionDeps{
		Gate:    batchFakeGate(fix.snapshot),
		Acquire: acquire,
		Timeout: 10 * time.Second,
	})
	if err == nil && (outcome == nil || outcome.Status != executioncontract.Unknown) {
		t.Fatalf("expected unknown outcome on uncertain XID checkpoint, got: outcome=%+v err=%v", outcome, err)
	}

	// Verify database remains blocked and no row updates happened
	var blocksDB bool
	if err := platformDB.QueryRowContext(ctx, "SELECT blocks_database FROM app.jobs WHERE id = $1", fix.claim.JobID).Scan(&blocksDB); err != nil || !blocksDB {
		t.Fatalf("expected target to remain blocked on uncertain XID: blocked=%t err=%v", blocksDB, err)
	}

	var processedCount int
	if err := e.exec.QueryRowContext(ctx, "SELECT count(*) FROM managed.batch_items WHERE processed = true").Scan(&processedCount); err != nil || processedCount != 0 {
		t.Fatalf("expected 0 rows processed on uncertain XID, got %d", processedCount)
	}
}

// 8. Test resume by a different actor after a partial commit interrupted after target commit before platform finalization
func TestExecuteBatchJob_ResumeByDifferentActor_InterruptedCommit_ReconcilesOriginalEvidence(t *testing.T) {
	e, platformDB, ownerDB := setupBatchTestEnv(t)
	setupBatchItemsFixture(t, e)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := e.exec.ExecContext(ctx, "INSERT INTO managed.batch_items (id, processed) VALUES (1, false), (2, false), (3, false), (4, false)"); err != nil {
		t.Fatalf("insert items: %v", err)
	}

	// Actor 1 creates Apply job (batch size 2 -> 2 batches)
	fix1 := createBatchFixture(t, platformDB, ownerDB, e.target, 2, executioncontract.Apply, nil)
	cleanupBatchScope(t, e, fix1.executionID)

	acquire := makeBatchAcquire(t, e.exec, e.prep)
	svc1 := New(platformDB)

	var committedEvidences []executioncontract.CommitEvidence
	var commitMu sync.Mutex
	recordCommit := func(ctx context.Context, tx *sql.Tx, in executioncontract.CommitEvidence) error {
		commitMu.Lock()
		defer commitMu.Unlock()
		committedEvidences = append(committedEvidences, in)
		return nil
	}

	gate1 := executioncontract.FakeGate{
		RevalidateFunc: func(ctx context.Context, req executioncontract.RevalidationRequest) (executioncontract.ExecutionPlan, error) {
			return executioncontract.ExecutionPlan{
				Request:       req,
				Snapshot:      fix1.snapshot,
				Version:       "1",
				ActorUsername: "actor1",
				ValidatedAt:   time.Now().UTC(),
			}, nil
		},
		RecordCommitFunc: recordCommit,
	}

	// Interruption after target commit of batch 0, before platform finalization
	deps1 := ExecutionDeps{
		Gate:    transactionalBatchGate{gate1},
		Acquire: acquire,
		Timeout: 10 * time.Second,
		afterBatchCommit: func(ctx context.Context, batchIndex int) error {
			if batchIndex == 0 {
				return errors.New("simulated crash after target commit of batch 0")
			}
			return nil
		},
	}

	outcome1, err := svc1.ExecuteBatchJob(ctx, fix1.claim, deps1)
	if err == nil && (outcome1 == nil || outcome1.Status != executioncontract.Unknown) {
		t.Fatalf("expected unknown outcome on crash after target commit, got: outcome=%+v err=%v", outcome1, err)
	}

	// Verify target DB: Batch 0 committed -> rows 1 and 2 processed
	var p1, p2 bool
	if err := e.exec.QueryRowContext(ctx, "SELECT (SELECT processed FROM managed.batch_items WHERE id = 1), (SELECT processed FROM managed.batch_items WHERE id = 2)").Scan(&p1, &p2); err != nil || !p1 || !p2 {
		t.Fatalf("target rows 1 and 2 should be committed: p1=%t p2=%t err=%v", p1, p2, err)
	}

	// Platform finalization did NOT commit for Job 1 before crash: committedEvidences should be empty
	commitMu.Lock()
	if len(committedEvidences) != 0 {
		commitMu.Unlock()
		t.Fatalf("expected 0 committed evidences before platform finalization, got %d", len(committedEvidences))
	}
	commitMu.Unlock()

	// Create Actor 2 (a different actor!)
	actor2ID := uuid.NewString()
	actor2Username := "batch_u_actor2_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	if _, err := platformDB.ExecContext(ctx, "INSERT INTO app.users (id, username, password_hash) VALUES ($1, $2, 'hash')", actor2ID, actor2Username); err != nil {
		t.Fatalf("insert actor2: %v", err)
	}
	t.Cleanup(func() {
		_, _ = ownerDB.ExecContext(context.Background(), "DELETE FROM app.users WHERE id = $1", actor2ID)
	})

	// Actor 2 resumes the job
	fixResume := createBatchFixtureWithActor(t, platformDB, ownerDB, e.target, 2, executioncontract.Resume, &fix1, actor2ID)
	if fixResume.actorID != actor2ID {
		t.Fatalf("expected resume actor to be actor2 (%s), got %s", actor2ID, fixResume.actorID)
	}
	if fixResume.actorID == fix1.actorID {
		t.Fatal("expected different actors for initial job and resume job")
	}

	svc2 := New(platformDB)
	var replayedBatch0 bool
	var batch1Ran bool

	gate2 := executioncontract.FakeGate{
		RevalidateFunc: func(ctx context.Context, req executioncontract.RevalidationRequest) (executioncontract.ExecutionPlan, error) {
			return executioncontract.ExecutionPlan{
				Request:       req,
				Snapshot:      fixResume.snapshot,
				Version:       "1",
				ActorUsername: "actor2",
				ValidatedAt:   time.Now().UTC(),
			}, nil
		},
		RecordCommitFunc: recordCommit,
	}

	deps2 := ExecutionDeps{
		Gate:    transactionalBatchGate{gate2},
		Acquire: acquire,
		Timeout: 10 * time.Second,
		beforeBatchDispatch: func(ctx context.Context, batchIndex int) error {
			if batchIndex == 0 {
				replayedBatch0 = true
				return errors.New("batch 0 was replayed! should have been reconciled")
			}
			if batchIndex == 1 {
				batch1Ran = true
			}
			return nil
		},
	}

	outcome2, err := svc2.ExecuteBatchJob(ctx, fixResume.claim, deps2)
	if err != nil || outcome2 == nil || outcome2.Status != executioncontract.Succeeded {
		t.Fatalf("expected resume to succeed, got outcome=%+v err=%v", outcome2, err)
	}

	// 1. Assert no replay of Batch 0
	if replayedBatch0 {
		t.Fatal("FAILED: Batch 0 SQL was replayed instead of reconciled from proof!")
	}
	if !batch1Ran {
		t.Fatal("FAILED: Batch 1 was not executed!")
	}

	// 2. Assert no duplicate history writes: exactly 2 commits recorded (Batch 0 and Batch 1)
	commitMu.Lock()
	defer commitMu.Unlock()
	if len(committedEvidences) != 2 {
		t.Fatalf("expected exactly 2 committed evidences (no duplicate history writes), got %d", len(committedEvidences))
	}

	// 3. Assert complete original identity and attribution for Batch 0
	ev0 := committedEvidences[0]
	if ev0.JobID != fix1.claim.JobID {
		t.Errorf("batch 0 JobID = %s, want original job %s", ev0.JobID, fix1.claim.JobID)
	}
	if ev0.ExecutionID != fix1.executionID {
		t.Errorf("batch 0 ExecutionID = %s, want original execution %s", ev0.ExecutionID, fix1.executionID)
	}
	if ev0.ProposalID != fix1.proposalID {
		t.Errorf("batch 0 ProposalID = %s, want original proposal %s", ev0.ProposalID, fix1.proposalID)
	}
	if ev0.MigrationID != fix1.draftID {
		t.Errorf("batch 0 MigrationID = %s, want original draft %s", ev0.MigrationID, fix1.draftID)
	}
	if ev0.ActorID != fix1.actorID {
		t.Errorf("batch 0 ActorID = %s, want original actor %s", ev0.ActorID, fix1.actorID)
	}
	if ev0.DatabaseID != fix1.dbID {
		t.Errorf("batch 0 DatabaseID = %s, want original database %s", ev0.DatabaseID, fix1.dbID)
	}
	if ev0.TargetID != fix1.targetID {
		t.Errorf("batch 0 TargetID = %s, want original target %s", ev0.TargetID, fix1.targetID)
	}
	if ev0.Revision != fix1.claim.Revision {
		t.Errorf("batch 0 Revision = %d, want original revision %d", ev0.Revision, fix1.claim.Revision)
	}
	if ev0.Version != "1" {
		t.Errorf("batch 0 Version = %s, want 1", ev0.Version)
	}
	if ev0.Action != executioncontract.Apply {
		t.Errorf("batch 0 Action = %s, want original action Apply", ev0.Action)
	}
	if ev0.Direction != executioncontract.Up {
		t.Errorf("batch 0 Direction = %s, want original direction Up", ev0.Direction)
	}
	if ev0.SQLSnapshot != fix1.snapshot.SQLUp {
		t.Errorf("batch 0 SQLSnapshot = %s, want original sqlUp %s", ev0.SQLSnapshot, fix1.snapshot.SQLUp)
	}
	if ev0.SnapshotHash != fix1.claim.SnapshotHash {
		t.Errorf("batch 0 SnapshotHash = %s, want original hash %s", ev0.SnapshotHash, fix1.claim.SnapshotHash)
	}
	if ev0.Receipt != fix1.executionID {
		t.Errorf("batch 0 Receipt = %s, want original receipt %s", ev0.Receipt, fix1.executionID)
	}
	if ev0.RowCount != "2" {
		t.Errorf("batch 0 RowCount = %s, want 2", ev0.RowCount)
	}
	if ev0.SourceExecutionID != nil {
		t.Errorf("batch 0 SourceExecutionID = %v, want nil", ev0.SourceExecutionID)
	}
	if ev0.FinalStep {
		t.Errorf("batch 0 FinalStep = true, want false")
	}
	if ev0.Batch == nil {
		t.Fatal("batch 0 commit evidence missing BatchCommit details")
	}
	if ev0.Batch.JobID != fix1.claim.JobID {
		t.Errorf("batch 0 Batch.JobID = %s, want original job %s", ev0.Batch.JobID, fix1.claim.JobID)
	}
	if ev0.Batch.ExecutedBy.ID != fix1.actorID {
		t.Errorf("batch 0 Batch.ExecutedBy = %s, want original actor %s", ev0.Batch.ExecutedBy.ID, fix1.actorID)
	}
	if ev0.Batch.BatchIndex != "0" {
		t.Errorf("batch 0 BatchIndex = %s, want 0", ev0.Batch.BatchIndex)
	}
	if ev0.Batch.RowCount != "2" {
		t.Errorf("batch 0 Batch.RowCount = %s, want 2", ev0.Batch.RowCount)
	}
	if ev0.Batch.Receipt != fix1.executionID {
		t.Errorf("batch 0 Batch.Receipt = %s, want original receipt %s", ev0.Batch.Receipt, fix1.executionID)
	}
	if ev0.Batch.SQLSnapshot != fix1.snapshot.SQLUp {
		t.Errorf("batch 0 Batch.SQLSnapshot = %s, want original sqlUp %s", ev0.Batch.SQLSnapshot, fix1.snapshot.SQLUp)
	}
	if ev0.Batch.RangeStart == nil || *ev0.Batch.RangeStart != "1" {
		t.Errorf("batch 0 RangeStart = %v, want 1", ev0.Batch.RangeStart)
	}
	if ev0.Batch.RangeEnd == nil || *ev0.Batch.RangeEnd != "2" {
		t.Errorf("batch 0 RangeEnd = %v, want 2", ev0.Batch.RangeEnd)
	}

	// Batch 1 attribution: executed by Actor 2 on Resume Job
	ev1 := committedEvidences[1]
	if ev1.JobID != fixResume.claim.JobID {
		t.Errorf("batch 1 JobID = %s, want resume job %s", ev1.JobID, fixResume.claim.JobID)
	}
	if ev1.ExecutionID != fixResume.executionID {
		t.Errorf("batch 1 ExecutionID = %s, want resume execution %s", ev1.ExecutionID, fixResume.executionID)
	}
	if ev1.ActorID != actor2ID {
		t.Errorf("batch 1 ActorID = %s, want resume actor %s", ev1.ActorID, actor2ID)
	}
	if ev1.Action != executioncontract.Resume {
		t.Errorf("batch 1 Action = %s, want resume action %s", ev1.Action, executioncontract.Resume)
	}
	if ev1.SourceExecutionID == nil || *ev1.SourceExecutionID != fix1.executionID {
		t.Errorf("batch 1 SourceExecutionID = %v, want %s", ev1.SourceExecutionID, fix1.executionID)
	}
	if !ev1.FinalStep {
		t.Errorf("batch 1 FinalStep = false, want true")
	}
	if ev1.Batch == nil {
		t.Fatal("batch 1 commit evidence missing BatchCommit details")
	}
	if ev1.Batch.ExecutedBy.ID != actor2ID {
		t.Errorf("batch 1 Batch.ExecutedBy = %s, want resume actor %s", ev1.Batch.ExecutedBy.ID, actor2ID)
	}
	if ev1.Batch.BatchIndex != "1" {
		t.Errorf("batch 1 BatchIndex = %s, want 1", ev1.Batch.BatchIndex)
	}

	// 4. Assert original attempt evidence in app.job_statement_evidence
	// Original Attempt 1 (Job 1) must have batch 0 evidence with outcome 'succeeded'
	var origOutcome string
	var origRowCount int64
	err = platformDB.QueryRowContext(ctx, `
		SELECT outcome, COALESCE(row_count, 0)
		FROM app.job_statement_evidence
		WHERE attempt_id = $1 AND ordinal = 1 AND batch_index = 0
	`, fix1.claim.AttemptID).Scan(&origOutcome, &origRowCount)
	if err != nil || origOutcome != "succeeded" || origRowCount != 2 {
		t.Fatalf("original attempt statement evidence: outcome=%s rows=%d err=%v", origOutcome, origRowCount, err)
	}

	// Resume Attempt (Job 2) must have batch 1 evidence, and NO batch 0 evidence
	var resumeOutcome string
	var resumeRowCount int64
	err = platformDB.QueryRowContext(ctx, `
		SELECT outcome, COALESCE(row_count, 0)
		FROM app.job_statement_evidence
		WHERE attempt_id = $1 AND ordinal = 2 AND batch_index = 1
	`, fixResume.claim.AttemptID).Scan(&resumeOutcome, &resumeRowCount)
	if err != nil || resumeOutcome != "succeeded" || resumeRowCount != 2 {
		t.Fatalf("resume attempt statement evidence: outcome=%s rows=%d err=%v", resumeOutcome, resumeRowCount, err)
	}

	var resumeHasBatch0 bool
	_ = platformDB.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM app.job_statement_evidence
			WHERE attempt_id = $1 AND batch_index = 0
		)
	`, fixResume.claim.AttemptID).Scan(&resumeHasBatch0)
	if resumeHasBatch0 {
		t.Fatal("resume attempt should NOT contain statement evidence for batch 0")
	}
	// Verify originating attempt status was preserved as unknown, not marked succeeded solely because batch 0 committed
	var origAttemptStatus string
	if err := platformDB.QueryRowContext(ctx, "SELECT status FROM app.job_attempts WHERE id = $1", fix1.claim.AttemptID).Scan(&origAttemptStatus); err != nil {
		t.Fatalf("query orig attempt status: %v", err)
	}
	if origAttemptStatus == "succeeded" {
		t.Fatalf("originating partial attempt should NOT be marked succeeded solely because one step committed, got %s", origAttemptStatus)
	}

	// Verify all 4 target rows processed
	var allProcessed bool
	if err := e.exec.QueryRowContext(ctx, "SELECT bool_and(processed) FROM managed.batch_items WHERE id IN (1, 2, 3, 4)").Scan(&allProcessed); err != nil || !allProcessed {
		t.Fatalf("expected all rows processed: %t err=%v", allProcessed, err)
	}
}

func TestBatchStepAuthorizations_DistinctPhysicalTargetsCanShareXIDWithoutConflation(t *testing.T) {
	_, platformDB, ownerDB := setupBatchTestEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// 1. Create two distinct physical targets in app.migration_target_state
	targetID1 := uuid.NewString()
	targetID2 := uuid.NewString()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, q := range []string{
			"DELETE FROM app.job_batch_commit_proofs WHERE physical_target_id IN ($1, $2)",
			"DELETE FROM app.job_batch_step_authorizations WHERE physical_target_id IN ($1, $2)",
			"DELETE FROM app.job_dispatch_authorizations WHERE job_id IN (SELECT id FROM app.jobs WHERE target_id IN ($1, $2))",
			"DELETE FROM app.job_statement_evidence WHERE attempt_id IN (SELECT id FROM app.job_attempts WHERE database_id IN (SELECT id FROM app.database_connections WHERE execution_target_id IN ($1, $2)))",
			"DELETE FROM app.job_attempts WHERE database_id IN (SELECT id FROM app.database_connections WHERE execution_target_id IN ($1, $2))",
			"DELETE FROM app.jobs WHERE target_id IN ($1, $2)",
			"DELETE FROM app.migration_executions WHERE execution_target_id IN ($1, $2)",
			"DELETE FROM app.migration_drafts WHERE database_id IN (SELECT id FROM app.database_connections WHERE execution_target_id IN ($1, $2))",
			"DELETE FROM app.database_connections WHERE execution_target_id IN ($1, $2)",
			"DELETE FROM app.migration_target_state WHERE id IN ($1, $2)",
		} {
			_, _ = ownerDB.ExecContext(ctx, q, targetID1, targetID2)
		}
	})

	for _, tid := range []string{targetID1, targetID2} {
		if _, err := ownerDB.ExecContext(ctx, "INSERT INTO app.migration_target_state (id, next_version) VALUES ($1, 1)", tid); err != nil {
			t.Fatalf("insert migration_target_state %s: %v", tid, err)
		}
	}

	// 2. Set up fixtures for Target 1 and Target 2
	fix1 := createBatchFixture(t, platformDB, ownerDB, targetID1, 2, executioncontract.Apply, nil)
	fix2 := createBatchFixture(t, platformDB, ownerDB, targetID2, 2, executioncontract.Apply, nil)

	sharedXID := "999888777"
	scopeExec1 := uuid.NewString()
	scopeExec2 := uuid.NewString()
	dummyHash := sha256.Sum256([]byte("dummy"))

	// 3. Authorize step for Target 1 with sharedXID
	var step1ID string
	err := platformDB.QueryRowContext(ctx, `
		INSERT INTO app.job_batch_step_authorizations
		    (job_id, attempt_id, actor_id, job_execution_id, scope_execution_id,
		     physical_target_id, direction, batch_index, ordinal, chunk_ids, plan, sql_text, sql_sha256,
		     target_xid, target_version)
		VALUES ($1, $2, $3, $4, $5, $6, 'up', 0, 1, '[1,2]'::jsonb, '{"snapshot":{}}'::jsonb,
		        'UPDATE managed.batch_items SET processed=true WHERE id = ANY($1::bigint[]);',
		        $7, $8, 1)
		RETURNING id::text
	`, fix1.claim.JobID, fix1.claim.AttemptID, fix1.actorID, fix1.executionID, scopeExec1,
		targetID1, dummyHash[:], sharedXID).Scan(&step1ID)
	if err != nil {
		t.Fatalf("insert step auth target 1: %v", err)
	}

	// 4. Authorize step for Target 2 with the SAME sharedXID: MUST succeed!
	var step2ID string
	err = platformDB.QueryRowContext(ctx, `
		INSERT INTO app.job_batch_step_authorizations
		    (job_id, attempt_id, actor_id, job_execution_id, scope_execution_id,
		     physical_target_id, direction, batch_index, ordinal, chunk_ids, plan, sql_text, sql_sha256,
		     target_xid, target_version)
		VALUES ($1, $2, $3, $4, $5, $6, 'up', 0, 1, '[1,2]'::jsonb, '{"snapshot":{}}'::jsonb,
		        'UPDATE managed.batch_items SET processed=true WHERE id = ANY($1::bigint[]);',
		        $7, $8, 1)
		RETURNING id::text
	`, fix2.claim.JobID, fix2.claim.AttemptID, fix2.actorID, fix2.executionID, scopeExec2,
		targetID2, dummyHash[:], sharedXID).Scan(&step2ID)
	if err != nil {
		t.Fatalf("distinct physical target sharing same XID should SUCCEED, got error: %v", err)
	}

	// 5. Duplicate step auth for SAME target and SAME XID must FAIL with unique violation (23505)
	_, err = platformDB.ExecContext(ctx, `
		INSERT INTO app.job_batch_step_authorizations
		    (job_id, attempt_id, actor_id, job_execution_id, scope_execution_id,
		     physical_target_id, direction, batch_index, ordinal, chunk_ids, plan, sql_text, sql_sha256,
		     target_xid, target_version)
		VALUES ($1, $2, $3, $4, $5, $6, 'up', 1, 2, '[3,4]'::jsonb, '{"snapshot":{}}'::jsonb,
		        'UPDATE managed.batch_items SET processed=true WHERE id = ANY($1::bigint[]);',
		        $7, $8, 1)
	`, fix1.claim.JobID, fix1.claim.AttemptID, fix1.actorID, fix1.executionID, scopeExec1,
		targetID1, dummyHash[:], sharedXID)
	if err == nil {
		t.Fatal("expected unique violation for duplicate (physical_target_id, target_xid), got nil")
	}
	checkpointState(t, err, "23505")

	// 6. Query lookup by physical_target_id and XID must NOT conflate step evidence between targets
	var foundStep1ID, foundTarget1 string
	err = platformDB.QueryRowContext(ctx, `
		SELECT id::text, physical_target_id::text
		FROM app.job_batch_step_authorizations
		WHERE scope_execution_id = $1::uuid AND physical_target_id = $2::uuid AND target_xid = $3
	`, scopeExec1, targetID1, sharedXID).Scan(&foundStep1ID, &foundTarget1)
	if err != nil || foundStep1ID != step1ID || foundTarget1 != targetID1 {
		t.Fatalf("target 1 lookup conflation check: id=%s (want %s) target=%s (want %s) err=%v",
			foundStep1ID, step1ID, foundTarget1, targetID1, err)
	}

	var foundStep2ID, foundTarget2 string
	err = platformDB.QueryRowContext(ctx, `
		SELECT id::text, physical_target_id::text
		FROM app.job_batch_step_authorizations
		WHERE scope_execution_id = $1::uuid AND physical_target_id = $2::uuid AND target_xid = $3
	`, scopeExec2, targetID2, sharedXID).Scan(&foundStep2ID, &foundTarget2)
	if err != nil || foundStep2ID != step2ID || foundTarget2 != targetID2 {
		t.Fatalf("target 2 lookup conflation check: id=%s (want %s) target=%s (want %s) err=%v",
			foundStep2ID, step2ID, foundTarget2, targetID2, err)
	}

	// 7. Insert commit proofs for both targets with sharedXID
	commitTime := time.Now().UTC()
	_, err = platformDB.ExecContext(ctx, `
		INSERT INTO app.job_batch_commit_proofs
		    (step_auth_id, job_id, attempt_id, scope_execution_id, physical_target_id, direction, batch_index, target_xid, row_count, committed_at)
		VALUES ($1, $2, $3, $4, $5, 'up', 0, $6, 2, $7)
	`, step1ID, fix1.claim.JobID, fix1.claim.AttemptID, scopeExec1, targetID1, sharedXID, commitTime)
	if err != nil {
		t.Fatalf("insert commit proof target 1: %v", err)
	}

	_, err = platformDB.ExecContext(ctx, `
		INSERT INTO app.job_batch_commit_proofs
		    (step_auth_id, job_id, attempt_id, scope_execution_id, physical_target_id, direction, batch_index, target_xid, row_count, committed_at)
		VALUES ($1, $2, $3, $4, $5, 'up', 0, $6, 2, $7)
	`, step2ID, fix2.claim.JobID, fix2.claim.AttemptID, scopeExec2, targetID2, sharedXID, commitTime)
	if err != nil {
		t.Fatalf("insert commit proof target 2: %v", err)
	}

	// Verify proof lookups remain isolated and do not conflate
	var proofXID1 string
	err = platformDB.QueryRowContext(ctx, `
		SELECT target_xid
		FROM app.job_batch_commit_proofs
		WHERE scope_execution_id = $1::uuid AND physical_target_id = $2::uuid AND batch_index = 0
	`, scopeExec1, targetID1).Scan(&proofXID1)
	if err != nil || proofXID1 != sharedXID {
		t.Fatalf("query proof target 1: xid=%s err=%v", proofXID1, err)
	}

	var proofXID2 string
	err = platformDB.QueryRowContext(ctx, `
		SELECT target_xid
		FROM app.job_batch_commit_proofs
		WHERE scope_execution_id = $1::uuid AND physical_target_id = $2::uuid AND batch_index = 0
	`, scopeExec2, targetID2).Scan(&proofXID2)
	if err != nil || proofXID2 != sharedXID {
		t.Fatalf("query proof target 2: xid=%s err=%v", proofXID2, err)
	}
}

// 10. Test empty scope does not invent target mutation or receipt and does not release unrelated work
func TestExecuteBatchJob_EmptyScope_NoTargetMutationOrReceipt(t *testing.T) {
	e, platformDB, ownerDB := setupBatchTestEnv(t)
	setupBatchItemsFixture(t, e)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// Ensure 0 rows in managed.batch_items
	if _, err := e.exec.ExecContext(ctx, "DELETE FROM managed.batch_items"); err != nil {
		t.Fatalf("delete batch items: %v", err)
	}

	fix := createBatchFixture(t, platformDB, ownerDB, e.target, 2, executioncontract.Apply, nil)
	cleanupBatchScope(t, e, fix.executionID)

	acquire := makeBatchAcquire(t, e.exec, e.prep)
	gate := batchFakeGate(fix.snapshot)
	svc := New(platformDB)

	outcome, err := svc.ExecuteBatchJob(ctx, fix.claim, ExecutionDeps{
		Gate:    gate,
		Acquire: acquire,
		Timeout: 10 * time.Second,
	})
	if err != nil || outcome == nil || outcome.Status != executioncontract.Succeeded {
		t.Fatalf("expected empty scope job to succeed, got outcome=%+v err=%v", outcome, err)
	}
	if outcome.Receipt != "" {
		t.Fatalf("expected empty receipt for empty scope without target mutation, got %q", outcome.Receipt)
	}

	// Verify job is succeeded, blocks_database = false, progress indicates 0 rows/batches
	var (
		status    string
		blocksDB  bool
		progBytes []byte
	)
	err = platformDB.QueryRowContext(ctx, "SELECT status, blocks_database, progress FROM app.jobs WHERE id = $1", fix.claim.JobID).Scan(&status, &blocksDB, &progBytes)
	if err != nil || status != "succeeded" || blocksDB {
		t.Fatalf("job state mismatch: status=%s blocks_database=%t err=%v", status, blocksDB, err)
	}
	var prog executioncontract.Progress
	if err := json.Unmarshal(progBytes, &prog); err != nil || prog.BatchesCommitted != "0" || prog.RowsCommitted != "0" || prog.TotalRows == nil || *prog.TotalRows != "0" {
		t.Fatalf("progress mismatch: %+v", prog)
	}

	// Verify attempt succeeded
	var attemptStatus string
	if err := platformDB.QueryRowContext(ctx, "SELECT status FROM app.job_attempts WHERE id = $1", fix.claim.AttemptID).Scan(&attemptStatus); err != nil || attemptStatus != "succeeded" {
		t.Fatalf("attempt status mismatch: status=%s err=%v", attemptStatus, err)
	}

	// Verify no checkpoints on target
	var chkCount int
	if err := e.prep.QueryRowContext(ctx, "SELECT count(*) FROM wbd_auth.batch_checkpoints WHERE execution_id = $1::uuid", fix.executionID).Scan(&chkCount); err != nil || chkCount != 0 {
		t.Fatalf("expected 0 checkpoints on target, got count=%d err=%v", chkCount, err)
	}

	// Verify no statement evidence was invented
	var evCount int
	if err := platformDB.QueryRowContext(ctx, "SELECT count(*) FROM app.job_statement_evidence WHERE attempt_id = $1", fix.claim.AttemptID).Scan(&evCount); err != nil || evCount != 0 {
		t.Fatalf("expected 0 statement evidence for empty scope, got count=%d err=%v", evCount, err)
	}
}

// 11. Test forged or inconsistent proof is rejected defensively as TARGET_BLOCKED, not bypassed
func TestExecuteBatchJob_ForgedOrInconsistentProof_RejectedDefensively(t *testing.T) {
	e, platformDB, ownerDB := setupBatchTestEnv(t)
	setupBatchItemsFixture(t, e)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := e.exec.ExecContext(ctx, "INSERT INTO managed.batch_items (id, processed) VALUES (1, false), (2, false), (3, false), (4, false)"); err != nil {
		t.Fatalf("insert items: %v", err)
	}

	fix1 := createBatchFixture(t, platformDB, ownerDB, e.target, 2, executioncontract.Apply, nil)
	cleanupBatchScope(t, e, fix1.executionID)

	acquire := makeBatchAcquire(t, e.exec, e.prep)
	svc := New(platformDB)

	// Interrupt after target commit of batch 0
	deps1 := ExecutionDeps{
		Gate:    batchFakeGate(fix1.snapshot),
		Acquire: acquire,
		Timeout: 10 * time.Second,
		afterBatchCommit: func(ctx context.Context, batchIndex int) error {
			if batchIndex == 0 {
				return errors.New("simulated crash after target commit")
			}
			return nil
		},
	}
	_, _ = svc.ExecuteBatchJob(ctx, fix1.claim, deps1)

	// Target has batch 0 committed.
	// Find step authorization id
	var stepAuthID string
	if err := platformDB.QueryRowContext(ctx, `
		SELECT id::text FROM app.job_batch_step_authorizations
		WHERE scope_execution_id = $1::uuid AND batch_index = 0
	`, fix1.executionID).Scan(&stepAuthID); err != nil {
		t.Fatalf("query step auth: %v", err)
	}

	// Insert forged proof with mismatched row count (e.g. 99 instead of 2)
	_, err := platformDB.ExecContext(ctx, `
		INSERT INTO app.job_batch_commit_proofs
		    (step_auth_id, job_id, attempt_id, scope_execution_id, physical_target_id, direction, batch_index, target_xid, row_count, committed_at)
		VALUES ($1::uuid, $2::uuid, $3::uuid, $4::uuid, $5::uuid, 'up', 0, '100', 99, clock_timestamp())
	`, stepAuthID, fix1.claim.JobID, fix1.claim.AttemptID, fix1.executionID, fix1.targetID)
	if err != nil {
		t.Fatalf("insert forged proof: %v", err)
	}

	// Now attempt to resume
	fixResume := createBatchFixture(t, platformDB, ownerDB, e.target, 2, executioncontract.Resume, &fix1)
	outcome, err := svc.ExecuteBatchJob(ctx, fixResume.claim, ExecutionDeps{
		Gate:    batchFakeGate(fixResume.snapshot),
		Acquire: acquire,
		Timeout: 10 * time.Second,
	})

	// Must be rejected defensively as TARGET_BLOCKED, not bypassed
	if outcome == nil || outcome.Status != executioncontract.Failed {
		t.Fatalf("expected TARGET_BLOCKED failure for forged proof, got outcome=%+v err=%v", outcome, err)
	}

	// Verify database is blocked on platform
	var blocksDB bool
	if err := platformDB.QueryRowContext(ctx, "SELECT blocks_database FROM app.jobs WHERE id = $1", fixResume.claim.JobID).Scan(&blocksDB); err != nil || !blocksDB {
		t.Fatalf("expected resume job to remain blocked, got blocks_database=%t err=%v", blocksDB, err)
	}
}

// 12. Test confirmed target rollback releases ONLY this job block, leaving other blocked jobs untouched
func TestExecuteBatchJob_ConfirmedRollback_ReleasesOnlyThisJobBlock(t *testing.T) {
	e, platformDB, ownerDB := setupBatchTestEnv(t)
	setupBatchItemsFixture(t, e)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if _, err := e.exec.ExecContext(ctx, "INSERT INTO managed.batch_items (id, processed) VALUES (1, false), (2, false)"); err != nil {
		t.Fatalf("insert items: %v", err)
	}

	fix1 := createBatchFixture(t, platformDB, ownerDB, e.target, 2, executioncontract.Apply, nil)
	cleanupBatchScope(t, e, fix1.executionID)

	// Create an existing unrelated blocked job
	unrelatedTargetID := uuid.NewString()
	unrelatedFix := createBatchFixture(t, platformDB, ownerDB, unrelatedTargetID, 2, executioncontract.Apply, nil)
	cleanupBatchScope(t, e, unrelatedFix.executionID)
	if _, err := platformDB.ExecContext(ctx, "UPDATE app.jobs SET blocks_database = true, status = 'partial', execution_status = 'partial' WHERE id = $1", unrelatedFix.claim.JobID); err != nil {
		t.Fatalf("mark unrelated job blocked: %v", err)
	}
	unrelatedJobID := unrelatedFix.claim.JobID

	acquire := makeBatchAcquire(t, e.exec, e.prep)
	svc := New(platformDB)

	// Batch 0 encounters external row modification before SQL execution, triggering confirmed rollback
	deps := ExecutionDeps{
		Gate:    batchFakeGate(fix1.snapshot),
		Acquire: acquire,
		Timeout: 10 * time.Second,
		beforeBatchDispatch: func(ctx context.Context, batchIndex int) error {
			_, err := e.exec.ExecContext(ctx, "UPDATE managed.batch_items SET processed = true WHERE id = 1")
			return err
		},
	}

	outcome, err := svc.ExecuteBatchJob(ctx, fix1.claim, deps)
	if outcome == nil || outcome.Status != executioncontract.Failed {
		t.Fatalf("expected BATCH_UNSAFE failed outcome, got %+v err=%v", outcome, err)
	}

	// Verify fix1 job block is released (blocks_database = false)
	var fix1Blocked bool
	if err := platformDB.QueryRowContext(ctx, "SELECT blocks_database FROM app.jobs WHERE id = $1", fix1.claim.JobID).Scan(&fix1Blocked); err != nil || fix1Blocked {
		t.Fatalf("fix1 job should be cleanly unblocked: blocked=%t err=%v", fix1Blocked, err)
	}

	// Verify unrelated job REMAINS blocked (not released)
	var unrelatedBlocked bool
	if err := platformDB.QueryRowContext(ctx, "SELECT blocks_database FROM app.jobs WHERE id = $1", unrelatedJobID).Scan(&unrelatedBlocked); err != nil || !unrelatedBlocked {
		t.Fatalf("unrelated job must remain blocked, got blocked=%t err=%v", unrelatedBlocked, err)
	}
}

// 13. Test platform finalization failure and clean idempotent recovery without duplicate history
func TestExecuteBatchJob_PlatformTxFailureAndRecovery(t *testing.T) {
	e, platformDB, ownerDB := setupBatchTestEnv(t)
	setupBatchItemsFixture(t, e)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := e.exec.ExecContext(ctx, "INSERT INTO managed.batch_items (id, processed) VALUES (1, false), (2, false), (3, false), (4, false)"); err != nil {
		t.Fatalf("insert items: %v", err)
	}

	fix1 := createBatchFixture(t, platformDB, ownerDB, e.target, 2, executioncontract.Apply, nil)
	cleanupBatchScope(t, e, fix1.executionID)

	acquire := makeBatchAcquire(t, e.exec, e.prep)
	svc := New(platformDB)

	var recordedCommits int
	gate1 := executioncontract.FakeGate{
		RevalidateFunc: func(ctx context.Context, req executioncontract.RevalidationRequest) (executioncontract.ExecutionPlan, error) {
			return executioncontract.ExecutionPlan{
				Request:       req,
				Snapshot:      fix1.snapshot,
				Version:       "1",
				ActorUsername: "testactor",
				ValidatedAt:   time.Now().UTC(),
			}, nil
		},
		RecordCommitFunc: func(ctx context.Context, tx *sql.Tx, in executioncontract.CommitEvidence) error {
			recordedCommits++
			// Fail during platform finalization transaction of batch 0
			return errors.New("simulated platform tx failure in RecordCommitTx")
		},
	}

	outcome1, err := svc.ExecuteBatchJob(ctx, fix1.claim, ExecutionDeps{
		Gate:    transactionalBatchGate{gate1},
		Acquire: acquire,
		Timeout: 10 * time.Second,
	})
	if outcome1 == nil || outcome1.Status != executioncontract.Unknown {
		t.Fatalf("expected unknown outcome on platform commit failure, got outcome=%+v err=%v", outcome1, err)
	}

	// Target has batch 0 committed
	var p1, p2 bool
	if err := e.exec.QueryRowContext(ctx, "SELECT (SELECT processed FROM managed.batch_items WHERE id = 1), (SELECT processed FROM managed.batch_items WHERE id = 2)").Scan(&p1, &p2); err != nil || !p1 || !p2 {
		t.Fatalf("target rows 1 and 2 should be committed: p1=%t p2=%t err=%v", p1, p2, err)
	}

	// Now resume with a healthy gate
	fixResume := createBatchFixture(t, platformDB, ownerDB, e.target, 2, executioncontract.Resume, &fix1)
	var resumedCommits []executioncontract.CommitEvidence
	var resumedMu sync.Mutex
	gate2 := executioncontract.FakeGate{
		RevalidateFunc: func(ctx context.Context, req executioncontract.RevalidationRequest) (executioncontract.ExecutionPlan, error) {
			return executioncontract.ExecutionPlan{
				Request:       req,
				Snapshot:      fixResume.snapshot,
				Version:       "1",
				ActorUsername: "testactor",
				ValidatedAt:   time.Now().UTC(),
			}, nil
		},
		RecordCommitFunc: func(ctx context.Context, tx *sql.Tx, in executioncontract.CommitEvidence) error {
			resumedMu.Lock()
			defer resumedMu.Unlock()
			resumedCommits = append(resumedCommits, in)
			return nil
		},
	}
	outcome2, err := svc.ExecuteBatchJob(ctx, fixResume.claim, ExecutionDeps{
		Gate:    transactionalBatchGate{gate2},
		Acquire: acquire,
		Timeout: 10 * time.Second,
	})
	if err != nil || outcome2 == nil || outcome2.Status != executioncontract.Succeeded {
		t.Fatalf("expected successful resume after platform recovery, got outcome=%+v err=%v", outcome2, err)
	}

	resumedMu.Lock()
	defer resumedMu.Unlock()
	// Exactly 2 commits recorded: batch 0 (reconciled) and batch 1 (live executed)
	if len(resumedCommits) != 2 {
		t.Fatalf("expected exactly 2 commits recorded on recovery, got %d", len(resumedCommits))
	}

	// Verify all 4 target rows processed
	var allDone bool
	if err := e.exec.QueryRowContext(ctx, "SELECT bool_and(processed) FROM managed.batch_items WHERE id IN (1, 2, 3, 4)").Scan(&allDone); err != nil || !allDone {
		t.Fatalf("expected all rows processed: %t err=%v", allDone, err)
	}
}

// 14. Regression: after first committed batch the gate denies the next chunk,
// which must not dispatch or checkpoint and must retain committed progress and target block.
func TestExecuteBatchJob_GateDeniesNextChunkAfterFirstCommit_RetainsProgressAndBlock(t *testing.T) {
	e, platformDB, ownerDB := setupBatchTestEnv(t)
	setupBatchItemsFixture(t, e)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := e.exec.ExecContext(ctx, "INSERT INTO managed.batch_items (id, processed) VALUES (1, false), (2, false), (3, false), (4, false)"); err != nil {
		t.Fatalf("insert items: %v", err)
	}

	fix := createBatchFixture(t, platformDB, ownerDB, e.target, 2, executioncontract.Apply, nil)
	cleanupBatchScope(t, e, fix.executionID)

	acquire := makeBatchAcquire(t, e.exec, e.prep)
	svc := New(platformDB)

	var revalCount int
	var mu sync.Mutex
	gate := executioncontract.FakeGate{
		RevalidateFunc: func(ctx context.Context, req executioncontract.RevalidationRequest) (executioncontract.ExecutionPlan, error) {
			mu.Lock()
			defer mu.Unlock()
			revalCount++
			if revalCount > 3 {
				return executioncontract.ExecutionPlan{}, executioncontract.ContractError{
					Code:    "REVIEW_STALE",
					Message: "leader changed approval expired before chunk 2",
				}
			}
			return executioncontract.ExecutionPlan{
				Request:       req,
				Snapshot:      fix.snapshot,
				Version:       "1",
				ActorUsername: "testactor",
				ValidatedAt:   time.Now().UTC(),
			}, nil
		},
		RecordCommitFunc: func(ctx context.Context, tx *sql.Tx, in executioncontract.CommitEvidence) error {
			return nil
		},
	}

	outcome, err := svc.ExecuteBatchJob(ctx, fix.claim, ExecutionDeps{
		Gate:    transactionalBatchGate{gate},
		Acquire: acquire,
		Timeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("expected graceful partial outcome, got error: %v", err)
	}
	if outcome == nil || outcome.Status != executioncontract.Partial {
		t.Fatalf("expected partial outcome on gate denial of chunk 2, got: %+v", outcome)
	}

	// Target DB:
	// Chunk 1 (id 1, 2) committed -> processed = true
	// Chunk 2 (id 3, 4) not dispatched -> processed = false
	var p1, p2, p3, p4 bool
	if err := e.exec.QueryRowContext(ctx, "SELECT (SELECT processed FROM managed.batch_items WHERE id = 1), (SELECT processed FROM managed.batch_items WHERE id = 2), (SELECT processed FROM managed.batch_items WHERE id = 3), (SELECT processed FROM managed.batch_items WHERE id = 4)").Scan(&p1, &p2, &p3, &p4); err != nil {
		t.Fatalf("query item states: %v", err)
	}
	if !p1 || !p2 || p3 || p4 {
		t.Fatalf("target rows state mismatch: 1=%t 2=%t 3=%t 4=%t", p1, p2, p3, p4)
	}

	// Target checkpoints: exactly 1 checkpoint for batch_index = 0, NONE for batch_index = 1
	var chkCount int
	if err := e.cleanup.QueryRowContext(ctx, "SELECT count(*) FROM wbd_auth.batch_checkpoints WHERE execution_id = $1::uuid", fix.executionID).Scan(&chkCount); err != nil {
		t.Fatalf("query checkpoint count: %v", err)
	}
	if chkCount != 1 {
		t.Fatalf("expected exactly 1 checkpoint, got %d", chkCount)
	}

	// Platform DB: job status must be partial, blocks_database must remain true, progress must retain 1 batch committed
	var jobStatus string
	var blocksDB bool
	var progJSON []byte
	if err := platformDB.QueryRowContext(ctx, "SELECT status, blocks_database, progress FROM app.jobs WHERE id = $1", fix.claim.JobID).Scan(&jobStatus, &blocksDB, &progJSON); err != nil {
		t.Fatalf("query job: %v", err)
	}
	if jobStatus != "partial" || !blocksDB {
		t.Fatalf("expected job status 'partial' and blocks_database=true, got status=%s blocked=%t", jobStatus, blocksDB)
	}
	var prog executioncontract.Progress
	if err := json.Unmarshal(progJSON, &prog); err != nil {
		t.Fatalf("decode progress: %v", err)
	}
	if prog.BatchesCommitted != "1" || prog.RowsCommitted != "2" {
		t.Fatalf("expected progress batches=1 rows=2, got batches=%s rows=%s", prog.BatchesCommitted, prog.RowsCommitted)
	}
}

// 15. Regression: lease lost during real preflight/row-lock interval is rejected
// at final boundary immediately before SQL, with no new target changes or checkpoints.
func TestExecuteBatchJob_LeaseLostDuringPreflightRowLock_RejectedAtFinalBoundary(t *testing.T) {
	e, platformDB, ownerDB := setupBatchTestEnv(t)
	setupBatchItemsFixture(t, e)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := e.exec.ExecContext(ctx, "INSERT INTO managed.batch_items (id, processed) VALUES (1, false), (2, false), (3, false), (4, false)"); err != nil {
		t.Fatalf("insert items: %v", err)
	}

	fix := createBatchFixture(t, platformDB, ownerDB, e.target, 2, executioncontract.Apply, nil)
	cleanupBatchScope(t, e, fix.executionID)

	acquire := makeBatchAcquire(t, e.exec, e.prep)
	svc := New(platformDB)

	var hookFired bool
	deps := ExecutionDeps{
		Gate:    batchFakeGate(fix.snapshot),
		Acquire: acquire,
		Timeout: 10 * time.Second,
		afterTicketReservation: func(ctx context.Context, batchIndex int) error {
			if batchIndex == 1 {
				hookFired = true
				// Simulate lease expiration during real preflight / row-lock / ticket preparation interval
				// on chunk 2, immediately before the final atomic fence and SQL execution
				_, err := platformDB.ExecContext(ctx, "UPDATE app.jobs SET lease_expires_at = clock_timestamp() - interval '5 seconds' WHERE id = $1", fix.claim.JobID)
				return err
			}
			return nil
		},
	}

	_, err := svc.ExecuteBatchJob(ctx, fix.claim, deps)
	if err == nil {
		t.Fatal("expected lease mismatch error at final boundary, got nil")
	}
	var cErr executioncontract.ContractError
	if !errors.As(err, &cErr) || cErr.Code != "LEASE_MISMATCH" {
		t.Fatalf("expected LEASE_MISMATCH at final boundary, got: %v", err)
	}
	if !hookFired {
		t.Fatal("expected afterTicketReservation hook to fire for chunk 1")
	}

	// Target DB:
	// Chunk 1 (id 1, 2) committed -> processed = true
	// Chunk 2 (id 3, 4) rejected at final boundary -> processed = false (NO target modifications)
	var p1, p2, p3, p4 bool
	if err := e.exec.QueryRowContext(ctx, "SELECT (SELECT processed FROM managed.batch_items WHERE id = 1), (SELECT processed FROM managed.batch_items WHERE id = 2), (SELECT processed FROM managed.batch_items WHERE id = 3), (SELECT processed FROM managed.batch_items WHERE id = 4)").Scan(&p1, &p2, &p3, &p4); err != nil {
		t.Fatalf("query item states: %v", err)
	}
	if !p1 || !p2 || p3 || p4 {
		t.Fatalf("target rows state mismatch: 1=%t 2=%t 3=%t 4=%t", p1, p2, p3, p4)
	}

	// Checkpoints: exactly 1 checkpoint for chunk 1, NO checkpoint for chunk 2
	var chkCount int
	if err := e.cleanup.QueryRowContext(ctx, "SELECT count(*) FROM wbd_auth.batch_checkpoints WHERE execution_id = $1::uuid", fix.executionID).Scan(&chkCount); err != nil {
		t.Fatalf("query checkpoint count: %v", err)
	}
	if chkCount != 1 {
		t.Fatalf("expected exactly 1 checkpoint, got %d", chkCount)
	}

	// Platform DB: chunk 1 committed progress and target block preserved
	var blocksDB bool
	var progJSON []byte
	if err := platformDB.QueryRowContext(ctx, "SELECT blocks_database, progress FROM app.jobs WHERE id = $1", fix.claim.JobID).Scan(&blocksDB, &progJSON); err != nil {
		t.Fatalf("query job: %v", err)
	}
	if !blocksDB {
		t.Fatalf("expected blocks_database=true to be retained")
	}
	var prog executioncontract.Progress
	if err := json.Unmarshal(progJSON, &prog); err != nil {
		t.Fatalf("decode progress: %v", err)
	}
	if prog.BatchesCommitted != "1" || prog.RowsCommitted != "2" {
		t.Fatalf("expected progress batches=1 rows=2, got batches=%s rows=%s", prog.BatchesCommitted, prog.RowsCommitted)
	}

	// Statement evidence: NO evidence row for chunk 2 (ordinal 2)
	var evCount int
	if err := platformDB.QueryRowContext(ctx, "SELECT count(*) FROM app.job_statement_evidence WHERE attempt_id = $1::uuid AND ordinal = 2", fix.claim.AttemptID).Scan(&evCount); err != nil {
		t.Fatalf("query statement evidence: %v", err)
	}
	if evCount != 0 {
		t.Fatalf("expected 0 statement evidence rows for un-fenced chunk 2, got %d", evCount)
	}
}

func TestExecuteBatchJob_Regression_CrashBeforeDispatch_ReclaimAndRestart(t *testing.T) {
	e, platformDB, ownerDB := setupBatchTestEnv(t)
	setupBatchItemsFixture(t, e)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := e.exec.ExecContext(ctx, "INSERT INTO managed.batch_items (id, processed) VALUES (1, false), (2, false), (3, false), (4, false)"); err != nil {
		t.Fatalf("insert items: %v", err)
	}

	fix1 := createBatchFixture(t, platformDB, ownerDB, e.target, 2, executioncontract.Apply, nil)
	cleanupBatchScope(t, e, fix1.executionID)

	acquire := makeBatchAcquire(t, e.exec, e.prep)
	svc := New(platformDB)

	deps1 := ExecutionDeps{
		Gate:    batchFakeGate(fix1.snapshot),
		Acquire: acquire,
		Timeout: 10 * time.Second,
		afterBatchCommit: func(ctx context.Context, batchIndex int) error {
			if batchIndex == 0 {
				return errors.New("simulated crash after batch 0 target commit")
			}
			return nil
		},
	}
	outcome1, _ := svc.ExecuteBatchJob(ctx, fix1.claim, deps1)
	if outcome1 == nil || outcome1.Status != executioncontract.Unknown {
		t.Fatalf("expected unknown outcome on crash, got outcome=%+v", outcome1)
	}

	fixResume := createBatchFixture(t, platformDB, ownerDB, e.target, 2, executioncontract.Resume, &fix1)
	cleanupBatchScope(t, e, fixResume.executionID)

	var sqlEntered bool
	if err := platformDB.QueryRowContext(ctx, "SELECT sql_entered FROM app.job_attempts WHERE id = $1", fixResume.claim.AttemptID).Scan(&sqlEntered); err != nil || sqlEntered {
		t.Fatalf("expected sql_entered=false, got %t err=%v", sqlEntered, err)
	}

	if _, err := platformDB.ExecContext(ctx, "UPDATE app.jobs SET lease_expires_at = clock_timestamp() - interval '5 seconds' WHERE id = $1", fixResume.claim.JobID); err != nil {
		t.Fatalf("expire lease: %v", err)
	}

	reclaimed, err := svc.reclaimExpiredJobs(ctx, 3, 10*time.Millisecond)
	if err != nil || reclaimed != 1 {
		t.Fatalf("reclaim expired resume job: reclaimed=%d err=%v", reclaimed, err)
	}

	var jobStatus, errCode string
	if err := platformDB.QueryRowContext(ctx, "SELECT status, error_code FROM app.jobs WHERE id = $1", fixResume.claim.JobID).Scan(&jobStatus, &errCode); err != nil {
		t.Fatalf("query job: %v", err)
	}
	if jobStatus != "retry_wait" || errCode != "LEASE_EXPIRED" {
		t.Fatalf("expected job status retry_wait with LEASE_EXPIRED, got %s / %s", jobStatus, errCode)
	}

	if _, err := platformDB.ExecContext(ctx, "UPDATE app.jobs SET next_retry_at = clock_timestamp() - interval '1 second' WHERE id = $1", fixResume.claim.JobID); err != nil {
		t.Fatalf("fast forward retry: %v", err)
	}

	claim2, err := svc.ClaimJob(ctx, ClaimRequest{
		WorkerID:      "worker-restarted",
		LeaseDuration: 30 * time.Second,
		MaxAttempts:   3,
	})
	if err != nil || claim2 == nil {
		t.Fatalf("expected worker 2 to claim retry_wait resume job, got claim=%+v err=%v", claim2, err)
	}
	if claim2.JobID != fixResume.claim.JobID || claim2.AttemptNo != 2 {
		t.Fatalf("expected claim for job %s attempt 2, got job %s attempt %d", fixResume.claim.JobID, claim2.JobID, claim2.AttemptNo)
	}

	var replayedBatch0 bool
	deps2 := ExecutionDeps{
		Gate:    batchFakeGate(fixResume.snapshot),
		Acquire: acquire,
		Timeout: 10 * time.Second,
		beforeBatchDispatch: func(ctx context.Context, batchIndex int) error {
			if batchIndex == 0 {
				replayedBatch0 = true
				return errors.New("batch 0 was replayed! should have been reconciled")
			}
			return nil
		},
	}
	outcome2, err := svc.ExecuteBatchJob(ctx, claim2, deps2)
	if err != nil || outcome2 == nil || outcome2.Status != executioncontract.Succeeded {
		t.Fatalf("expected resume attempt 2 to succeed, got outcome=%+v err=%v", outcome2, err)
	}
	if replayedBatch0 {
		t.Fatal("batch 0 was replayed!")
	}

	var p1, p2, p3, p4 bool
	if err := e.exec.QueryRowContext(ctx, "SELECT (SELECT processed FROM managed.batch_items WHERE id = 1), (SELECT processed FROM managed.batch_items WHERE id = 2), (SELECT processed FROM managed.batch_items WHERE id = 3), (SELECT processed FROM managed.batch_items WHERE id = 4)").Scan(&p1, &p2, &p3, &p4); err != nil || !p1 || !p2 || !p3 || !p4 {
		t.Fatalf("expected all rows processed: %t %t %t %t err=%v", p1, p2, p3, p4, err)
	}
}

func TestExecuteBatchJob_Regression_CrashAfterResumedCommit_SubsequentResume(t *testing.T) {
	e, platformDB, ownerDB := setupBatchTestEnv(t)
	setupBatchItemsFixture(t, e)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := e.exec.ExecContext(ctx, "INSERT INTO managed.batch_items (id, processed) VALUES (1, false), (2, false), (3, false), (4, false), (5, false), (6, false)"); err != nil {
		t.Fatalf("insert items: %v", err)
	}

	fix1 := createBatchFixture(t, platformDB, ownerDB, e.target, 2, executioncontract.Apply, nil)
	cleanupBatchScope(t, e, fix1.executionID)

	acquire := makeBatchAcquire(t, e.exec, e.prep)
	svc := New(platformDB)

	deps1 := ExecutionDeps{
		Gate:    batchFakeGate(fix1.snapshot),
		Acquire: acquire,
		Timeout: 10 * time.Second,
		afterBatchCommit: func(ctx context.Context, batchIndex int) error {
			if batchIndex == 0 {
				return errors.New("crash after batch 0 commit")
			}
			return nil
		},
	}
	outcome1, _ := svc.ExecuteBatchJob(ctx, fix1.claim, deps1)
	if outcome1 == nil || outcome1.Status != executioncontract.Unknown {
		t.Fatalf("expected unknown outcome on Job 1, got outcome=%+v", outcome1)
	}

	fixResume1 := createBatchFixture(t, platformDB, ownerDB, e.target, 2, executioncontract.Resume, &fix1)
	cleanupBatchScope(t, e, fixResume1.executionID)

	depsResume1 := ExecutionDeps{
		Gate:    batchFakeGate(fixResume1.snapshot),
		Acquire: acquire,
		Timeout: 10 * time.Second,
		afterBatchCommit: func(ctx context.Context, batchIndex int) error {
			if batchIndex == 1 {
				return errors.New("crash after resumed batch 1 commit")
			}
			return nil
		},
	}
	outcomeResume1, _ := svc.ExecuteBatchJob(ctx, fixResume1.claim, depsResume1)
	if outcomeResume1 == nil || outcomeResume1.Status != executioncontract.Unknown {
		t.Fatalf("expected unknown outcome on Resume 1, got outcome=%+v", outcomeResume1)
	}

	if _, err := platformDB.ExecContext(ctx, "UPDATE app.jobs SET status = 'failed', execution_status = 'failed', blocks_database = true WHERE id = $1", fixResume1.claim.JobID); err != nil {
		t.Fatalf("fail resume 1: %v", err)
	}
	if _, err := platformDB.ExecContext(ctx, "UPDATE app.migration_proposals SET status = 'superseded' WHERE id = $1", fixResume1.proposalID); err != nil {
		t.Fatalf("supersede resume 1 proposal: %v", err)
	}
	fixResume2 := createBatchFixture(t, platformDB, ownerDB, e.target, 2, executioncontract.Resume, &fix1)
	cleanupBatchScope(t, e, fixResume2.executionID)

	var replayed0or1 bool
	depsResume2 := ExecutionDeps{
		Gate:    batchFakeGate(fixResume2.snapshot),
		Acquire: acquire,
		Timeout: 10 * time.Second,
		beforeBatchDispatch: func(ctx context.Context, batchIndex int) error {
			if batchIndex <= 1 {
				replayed0or1 = true
				return fmt.Errorf("batch %d was replayed! should have been reconciled", batchIndex)
			}
			return nil
		},
	}
	outcomeResume2, err := svc.ExecuteBatchJob(ctx, fixResume2.claim, depsResume2)
	if err != nil || outcomeResume2 == nil || outcomeResume2.Status != executioncontract.Succeeded {
		t.Fatalf("expected Resume 2 to succeed, got outcome=%+v err=%v", outcomeResume2, err)
	}
	if replayed0or1 {
		t.Fatal("batch 0 or batch 1 was replayed!")
	}

	var allDone bool
	if err := e.exec.QueryRowContext(ctx, "SELECT bool_and(processed) FROM managed.batch_items WHERE id IN (1, 2, 3, 4, 5, 6)").Scan(&allDone); err != nil || !allDone {
		t.Fatalf("expected all items processed: %t err=%v", allDone, err)
	}

	var origStatus, origExecStatus string
	var origBlocksDB bool
	if err := platformDB.QueryRowContext(ctx, "SELECT status, execution_status, blocks_database FROM app.jobs WHERE id = $1", fix1.claim.JobID).Scan(&origStatus, &origExecStatus, &origBlocksDB); err != nil {
		t.Fatalf("query orig job: %v", err)
	}
	if origExecStatus != "succeeded" {
		t.Fatalf("orig job execution_status must be updated to succeeded! got %s", origExecStatus)
	}
	if origStatus == "succeeded" {
		t.Fatalf("orig job own status must NOT be overwritten to succeeded! got %s", origStatus)
	}
	if origBlocksDB {
		t.Fatalf("orig job blocks_database should be released (false) after full lineage completion, got true")
	}
}

func TestExecuteBatchJob_Regression_UnrelatedBlockedJobStaysBlocked(t *testing.T) {
	e, platformDB, ownerDB := setupBatchTestEnv(t)
	setupBatchItemsFixture(t, e)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := e.exec.ExecContext(ctx, "INSERT INTO managed.batch_items (id, processed) VALUES (1, false), (2, false), (3, false), (4, false)"); err != nil {
		t.Fatalf("insert items: %v", err)
	}

	fix1 := createBatchFixture(t, platformDB, ownerDB, e.target, 2, executioncontract.Apply, nil)
	cleanupBatchScope(t, e, fix1.executionID)

	acquire := makeBatchAcquire(t, e.exec, e.prep)
	svc := New(platformDB)

	deps1 := ExecutionDeps{
		Gate:    batchFakeGate(fix1.snapshot),
		Acquire: acquire,
		Timeout: 10 * time.Second,
		afterBatchCommit: func(ctx context.Context, batchIndex int) error {
			if batchIndex == 0 {
				return errors.New("simulated crash after batch 0 target commit")
			}
			return nil
		},
	}
	_, _ = svc.ExecuteBatchJob(ctx, fix1.claim, deps1)

	unrelatedDBID := fix1.dbID

	unrelatedDraftID := uuid.NewString()
	unrelatedProposalID := uuid.NewString()
	if _, err := platformDB.ExecContext(ctx, `
		INSERT INTO app.migration_drafts (id, database_id, revision, title, sql, created_by)
		VALUES ($1, $2, 1, 'Unrelated Migration', 'UPDATE managed.batch_items SET processed = true WHERE id = 1;', $3)
	`, unrelatedDraftID, unrelatedDBID, fix1.actorID); err != nil {
		t.Fatalf("insert draft: %v", err)
	}
	unrelatedSnapshot := executioncontract.Draft{
		MigrationID: unrelatedDraftID,
		DatabaseID:  unrelatedDBID,
		Revision:    1,
		Title:       "Unrelated Migration",
		SQLUp:       "UPDATE managed.batch_items SET processed = true WHERE id = 1;",
		Mode:        executioncontract.Batch,
		Batch: &executioncontract.BatchConfig{
			Schema:       "managed",
			Table:        "batch_items",
			CursorColumn: "id",
			BatchSize:    2,
		},
	}
	hashBytes := sha256.Sum256([]byte(unrelatedSnapshot.SQLUp))
	snapJSON, _ := json.Marshal(unrelatedSnapshot)
	if _, err := platformDB.ExecContext(ctx, `
		INSERT INTO app.migration_proposals (id, database_id, migration_id, revision, action, status, snapshot, snapshot_hash, created_by)
		VALUES ($1, $2, $3, 1, 'apply', 'draft', $4::jsonb, $5, $6)
	`, unrelatedProposalID, unrelatedDBID, unrelatedDraftID, string(snapJSON), hashBytes[:], fix1.actorID); err != nil {
		t.Fatalf("insert proposal: %v", err)
	}

	enqTx, err := platformDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	enqResU, err := svc.EnqueueTx(ctx, enqTx, executioncontract.EnqueueRequest{
		TargetID:     e.target,
		RequestID:    uuid.NewString(),
		ActorID:      fix1.actorID,
		DatabaseID:   unrelatedDBID,
		ProposalID:   unrelatedProposalID,
		MigrationID:  unrelatedDraftID,
		Revision:     1,
		Action:       executioncontract.Apply,
		Direction:    executioncontract.Up,
		Snapshot:     unrelatedSnapshot,
		SnapshotHash: hex.EncodeToString(hashBytes[:]),
	})
	if err != nil {
		t.Fatalf("enqueue unrelated: %v", err)
	}
	if err := enqTx.Commit(); err != nil {
		t.Fatalf("commit tx: %v", err)
	}

	claimU, _ := svc.ClaimJob(ctx, ClaimRequest{
		WorkerID:      "worker-unrelated",
		LeaseDuration: 30 * time.Second,
		MaxAttempts:   3,
	})
	if claimU != nil {
		t.Fatalf("unrelated job should be blocked and not claimed! got claim: %+v", claimU)
	}

	fixResume := createBatchFixture(t, platformDB, ownerDB, e.target, 2, executioncontract.Resume, &fix1)
	cleanupBatchScope(t, e, fixResume.executionID)

	claimU2, _ := svc.ClaimJob(ctx, ClaimRequest{
		WorkerID:      "worker-unrelated-2",
		LeaseDuration: 30 * time.Second,
		MaxAttempts:   3,
	})
	if claimU2 != nil {
		t.Fatalf("unrelated job should stay blocked while resume is running! got: %+v", claimU2)
	}

	outcomeResume, err := svc.ExecuteBatchJob(ctx, fixResume.claim, ExecutionDeps{
		Gate:    batchFakeGate(fixResume.snapshot),
		Acquire: acquire,
		Timeout: 10 * time.Second,
	})
	if err != nil || outcomeResume == nil || outcomeResume.Status != executioncontract.Succeeded {
		t.Fatalf("expected resume to succeed, got outcome=%+v err=%v", outcomeResume, err)
	}

	claimU3, err := svc.ClaimJob(ctx, ClaimRequest{
		WorkerID:      "worker-unrelated-3",
		LeaseDuration: 30 * time.Second,
		MaxAttempts:   3,
	})
	if err != nil || claimU3 == nil {
		t.Fatalf("expected unrelated job to be claimed after unblock, got claim=%+v err=%v", claimU3, err)
	}
	if claimU3.JobID != enqResU.JobID {
		t.Fatalf("expected claimed job to be unrelated job %s, got %s", enqResU.JobID, claimU3.JobID)
	}
}

func TestExecuteBatchJob_Regression_HistoryCountsRemainOnceOnly(t *testing.T) {
	e, platformDB, ownerDB := setupBatchTestEnv(t)
	setupBatchItemsFixture(t, e)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := e.exec.ExecContext(ctx, "INSERT INTO managed.batch_items (id, processed) VALUES (1, false), (2, false), (3, false), (4, false)"); err != nil {
		t.Fatalf("insert items: %v", err)
	}

	fix1 := createBatchFixture(t, platformDB, ownerDB, e.target, 2, executioncontract.Apply, nil)
	cleanupBatchScope(t, e, fix1.executionID)

	acquire := makeBatchAcquire(t, e.exec, e.prep)
	svc := New(platformDB)

	var recordCommitMu sync.Mutex
	commitEvidenceBatches := make(map[string]int)

	recordCommitFunc := func(c context.Context, tx *sql.Tx, in executioncontract.CommitEvidence) error {
		recordCommitMu.Lock()
		defer recordCommitMu.Unlock()
		if in.Batch != nil {
			commitEvidenceBatches[in.Batch.BatchIndex]++
		}
		return nil
	}

	gate1 := executioncontract.FakeGate{
		RevalidateFunc: func(ctx context.Context, req executioncontract.RevalidationRequest) (executioncontract.ExecutionPlan, error) {
			return executioncontract.ExecutionPlan{
				Request:       req,
				Snapshot:      fix1.snapshot,
				Version:       "1",
				ActorUsername: "testactor",
				ValidatedAt:   time.Now().UTC(),
			}, nil
		},
		RecordCommitFunc: recordCommitFunc,
	}

	deps1 := ExecutionDeps{
		Gate:    transactionalBatchGate{gate1},
		Acquire: acquire,
		Timeout: 10 * time.Second,
		beforeBatchDispatch: func(ctx context.Context, batchIndex int) error {
			if batchIndex == 1 {
				return errors.New("simulated crash before dispatching batch 1")
			}
			return nil
		},
	}
	outcome1, _ := svc.ExecuteBatchJob(ctx, fix1.claim, deps1)
	if outcome1 == nil || outcome1.Status != executioncontract.Partial {
		t.Fatalf("expected partial outcome on Job 1, got outcome=%+v", outcome1)
	}

	recordCommitMu.Lock()
	if commitEvidenceBatches["0"] != 1 {
		recordCommitMu.Unlock()
		t.Fatalf("expected batch 0 to have 1 RecordCommit call, got %d", commitEvidenceBatches["0"])
	}
	recordCommitMu.Unlock()

	fixResume1 := createBatchFixture(t, platformDB, ownerDB, e.target, 2, executioncontract.Resume, &fix1)
	cleanupBatchScope(t, e, fixResume1.executionID)

	gate2 := executioncontract.FakeGate{
		RevalidateFunc: func(ctx context.Context, req executioncontract.RevalidationRequest) (executioncontract.ExecutionPlan, error) {
			return executioncontract.ExecutionPlan{
				Request:       req,
				Snapshot:      fixResume1.snapshot,
				Version:       "1",
				ActorUsername: "testactor",
				ValidatedAt:   time.Now().UTC(),
			}, nil
		},
		RecordCommitFunc: recordCommitFunc,
	}

	depsResume1 := ExecutionDeps{
		Gate:    transactionalBatchGate{gate2},
		Acquire: acquire,
		Timeout: 10 * time.Second,
	}
	outcomeResume1, err := svc.ExecuteBatchJob(ctx, fixResume1.claim, depsResume1)
	if err != nil || outcomeResume1 == nil || outcomeResume1.Status != executioncontract.Succeeded {
		t.Fatalf("expected Resume 1 to succeed, got outcome=%+v err=%v", outcomeResume1, err)
	}

	recordCommitMu.Lock()
	defer recordCommitMu.Unlock()
	if commitEvidenceBatches["0"] != 1 {
		t.Fatalf("batch 0 RecordCommit call was duplicated! count=%d", commitEvidenceBatches["0"])
	}
	if commitEvidenceBatches["1"] != 1 {
		t.Fatalf("batch 1 RecordCommit call count mismatch: expected 1, got %d", commitEvidenceBatches["1"])
	}

	var proof0Count, proof1Count int
	if err := platformDB.QueryRowContext(ctx, "SELECT count(*) FROM app.job_batch_commit_proofs WHERE scope_execution_id = $1::uuid AND batch_index = 0", fix1.executionID).Scan(&proof0Count); err != nil {
		t.Fatalf("query proof 0: %v", err)
	}
	if proof0Count != 1 {
		t.Fatalf("expected exactly 1 commit proof for batch 0, got %d", proof0Count)
	}
	if err := platformDB.QueryRowContext(ctx, "SELECT count(*) FROM app.job_batch_commit_proofs WHERE scope_execution_id = $1::uuid AND batch_index = 1", fix1.executionID).Scan(&proof1Count); err != nil {
		t.Fatalf("query proof 1: %v", err)
	}
	if proof1Count != 1 {
		t.Fatalf("expected exactly 1 commit proof for batch 1, got %d", proof1Count)
	}
}

func TestExecuteBatchJob_Regression_BoundedMultiPage_Cancel(t *testing.T) {
	e, platformDB, ownerDB := setupBatchTestEnv(t)
	setupBatchItemsFixture(t, e)

	oldLimit := chkPageLimit
	chkPageLimit = 1
	t.Cleanup(func() { chkPageLimit = oldLimit })

	ctx := context.Background()
	if _, err := e.exec.ExecContext(ctx, "INSERT INTO managed.batch_items (id, processed) VALUES (1, false), (2, false), (3, false), (4, false), (5, false), (6, false)"); err != nil {
		t.Fatalf("insert items: %v", err)
	}

	fix1 := createBatchFixture(t, platformDB, ownerDB, e.target, 2, executioncontract.Apply, nil)
	cleanupBatchScope(t, e, fix1.executionID)

	acquire := makeBatchAcquire(t, e.exec, e.prep)
	svc := New(platformDB)

	runCtx, cancel := context.WithCancel(ctx)
	deps1 := ExecutionDeps{
		Gate:    batchFakeGate(fix1.snapshot),
		Acquire: acquire,
		Timeout: 10 * time.Second,
		afterBatchCommit: func(c context.Context, batchIndex int) error {
			if batchIndex == 1 {
				cancel()
			}
			return nil
		},
	}

	outcome1, err := svc.ExecuteBatchJob(runCtx, fix1.claim, deps1)
	if err == nil && (outcome1 == nil || outcome1.Status != executioncontract.Unknown || outcome1.Error == nil) {
		t.Fatalf("expected cancellation or unknown outcome, got outcome=%+v err=%v", outcome1, err)
	}

	var p1, p2, p3, p4, p5, p6 bool
	if err := e.exec.QueryRowContext(ctx, "SELECT (SELECT processed FROM managed.batch_items WHERE id = 1), (SELECT processed FROM managed.batch_items WHERE id = 2), (SELECT processed FROM managed.batch_items WHERE id = 3), (SELECT processed FROM managed.batch_items WHERE id = 4), (SELECT processed FROM managed.batch_items WHERE id = 5), (SELECT processed FROM managed.batch_items WHERE id = 6)").Scan(&p1, &p2, &p3, &p4, &p5, &p6); err != nil {
		t.Fatalf("query items: %v", err)
	}
	if !p1 || !p2 || !p3 || !p4 || p5 || p6 {
		t.Fatalf("expected batches 0 and 1 committed after cancel: %t %t %t %t %t %t", p1, p2, p3, p4, p5, p6)
	}

	var chkCount int
	if err := e.cleanup.QueryRowContext(ctx, "SELECT count(*) FROM wbd_auth.batch_checkpoints WHERE execution_id = $1::uuid", fix1.executionID).Scan(&chkCount); err != nil || chkCount != 2 {
		t.Fatalf("expected 2 checkpoints, got %d err=%v", chkCount, err)
	}
	fixResume := createBatchFixture(t, platformDB, ownerDB, e.target, 2, executioncontract.Resume, &fix1)
	cleanupBatchScope(t, e, fixResume.executionID)

	depsResume := ExecutionDeps{
		Gate:    batchFakeGate(fixResume.snapshot),
		Acquire: acquire,
		Timeout: 10 * time.Second,
	}
	outcomeResume, err := svc.ExecuteBatchJob(ctx, fixResume.claim, depsResume)
	if err != nil || outcomeResume == nil || outcomeResume.Status != executioncontract.Succeeded {
		t.Fatalf("expected resume to succeed, got outcome=%+v err=%v", outcomeResume, err)
	}

	var allDone bool
	if err := e.exec.QueryRowContext(ctx, "SELECT bool_and(processed) FROM managed.batch_items WHERE id IN (1, 2, 3, 4, 5, 6)").Scan(&allDone); err != nil || !allDone {
		t.Fatalf("expected all items processed after resume: %t err=%v", allDone, err)
	}
}

func TestExecuteBatchJob_AtomicRollback_PostDispatchFailure(t *testing.T) {
	t.Run("FailureAfterTargetUpdateBeforeCheckpointCommit", func(t *testing.T) {
		e, platformDB, ownerDB := setupBatchTestEnv(t)
		setupBatchItemsFixture(t, e)

		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()

		if _, err := e.exec.ExecContext(ctx, "INSERT INTO managed.batch_items (id, processed) VALUES (1, false), (2, false)"); err != nil {
			t.Fatalf("insert items: %v", err)
		}

		fix := createBatchFixture(t, platformDB, ownerDB, e.target, 2, executioncontract.Apply, nil)
		cleanupBatchScope(t, e, fix.executionID)

		acquire := makeBatchAcquire(t, e.exec, e.prep)
		svc := New(platformDB)

		var updateHookRan bool
		deps := ExecutionDeps{
			Gate:    batchFakeGate(fix.snapshot),
			Acquire: acquire,
			Timeout: 10 * time.Second,
			afterBatchUpdate: func(ctx context.Context, batchIndex int) error {
				updateHookRan = true
				return errors.New("simulated failure after target update before checkpoint")
			},
		}

		outcome, err := svc.ExecuteBatchJob(ctx, fix.claim, deps)
		if outcome == nil || outcome.Status != executioncontract.Failed {
			t.Fatalf("expected Failed outcome, got %+v (err=%v)", outcome, err)
		}
		if !updateHookRan {
			t.Fatal("expected afterBatchUpdate hook to execute")
		}

		// 1. Target database: target transaction was confirmed rolled back
		var p1, p2 bool
		if err := e.exec.QueryRowContext(ctx, "SELECT (SELECT processed FROM managed.batch_items WHERE id = 1), (SELECT processed FROM managed.batch_items WHERE id = 2)").Scan(&p1, &p2); err != nil {
			t.Fatalf("query item states: %v", err)
		}
		if p1 || p2 {
			t.Fatalf("target rows must remain false after rollback: p1=%t, p2=%t", p1, p2)
		}

		// 2. Target checkpoint: no checkpoint committed on target
		var chkCount int
		if err := e.prep.QueryRowContext(ctx, "SELECT count(*) FROM wbd_auth.batch_checkpoints WHERE execution_id = $1::uuid", fix.executionID).Scan(&chkCount); err != nil {
			t.Fatalf("count checkpoints: %v", err)
		}
		if chkCount != 0 {
			t.Fatalf("expected 0 checkpoints, got %d", chkCount)
		}

		// 3. Statement evidence: atomically updated to 'failed' with ordinal=1 and batch_index=0
		var seOutcome string
		var seBatchIdx int
		if err := platformDB.QueryRowContext(ctx, "SELECT outcome, batch_index FROM app.job_statement_evidence WHERE attempt_id = $1 AND ordinal = 1", fix.claim.AttemptID).Scan(&seOutcome, &seBatchIdx); err != nil {
			t.Fatalf("query statement evidence: %v", err)
		}
		if seOutcome != "failed" || seBatchIdx != 0 {
			t.Fatalf("statement evidence mismatch: outcome=%s, batch_index=%d", seOutcome, seBatchIdx)
		}

		// 4. Job state: atomically updated to failed and clean unblocked (blocks_database=false)
		var jobStatus, jobExecStatus string
		var jobBlocked bool
		if err := platformDB.QueryRowContext(ctx, "SELECT status, execution_status, blocks_database FROM app.jobs WHERE id = $1", fix.claim.JobID).Scan(&jobStatus, &jobExecStatus, &jobBlocked); err != nil {
			t.Fatalf("query job: %v", err)
		}
		if jobStatus != "failed" || jobExecStatus != "failed" || jobBlocked {
			t.Fatalf("job state mismatch: status=%s, execStatus=%s, blocked=%t", jobStatus, jobExecStatus, jobBlocked)
		}

		// 5. Attempt state: status is failed
		var attStatus string
		if err := platformDB.QueryRowContext(ctx, "SELECT status FROM app.job_attempts WHERE id = $1", fix.claim.AttemptID).Scan(&attStatus); err != nil {
			t.Fatalf("query attempt: %v", err)
		}
		if attStatus != "failed" {
			t.Fatalf("attempt status mismatch: %s", attStatus)
		}
	})

	t.Run("FailureDuringPlatformRollbackTx_EvidenceAndJobRollbackTogether", func(t *testing.T) {
		e, platformDB, ownerDB := setupBatchTestEnv(t)
		setupBatchItemsFixture(t, e)

		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()

		if _, err := e.exec.ExecContext(ctx, "INSERT INTO managed.batch_items (id, processed) VALUES (1, false), (2, false)"); err != nil {
			t.Fatalf("insert items: %v", err)
		}

		fix := createBatchFixture(t, platformDB, ownerDB, e.target, 2, executioncontract.Apply, nil)
		cleanupBatchScope(t, e, fix.executionID)

		acquire := makeBatchAcquire(t, e.exec, e.prep)
		svc := New(platformDB)

		var updateHookRan, txHookRan bool
		deps := ExecutionDeps{
			Gate:    batchFakeGate(fix.snapshot),
			Acquire: acquire,
			Timeout: 10 * time.Second,
			afterBatchUpdate: func(ctx context.Context, batchIndex int) error {
				updateHookRan = true
				return errors.New("simulated error after target update")
			},
			afterRollbackFirstWrite: func(ctx context.Context) error {
				txHookRan = true
				return errors.New("simulated failure inside platform rollback tx after statement evidence write")
			},
		}

		outcome, err := svc.ExecuteBatchJob(ctx, fix.claim, deps)
		if err == nil {
			t.Fatalf("expected error from aborted platform tx, got outcome=%+v", outcome)
		}
		if !updateHookRan || !txHookRan {
			t.Fatalf("hooks did not run: updateHookRan=%t, txHookRan=%t", updateHookRan, txHookRan)
		}

		// 1. Target database: target transaction was confirmed rolled back
		var p1, p2 bool
		if err := e.exec.QueryRowContext(ctx, "SELECT (SELECT processed FROM managed.batch_items WHERE id = 1), (SELECT processed FROM managed.batch_items WHERE id = 2)").Scan(&p1, &p2); err != nil {
			t.Fatalf("query item states: %v", err)
		}
		if p1 || p2 {
			t.Fatalf("target rows must remain false after rollback: p1=%t, p2=%t", p1, p2)
		}

		// 2. Statement evidence: rolled back together with platform tx (reverted to 'unknown', not 'failed')
		var seOutcome string
		if err := platformDB.QueryRowContext(ctx, "SELECT outcome FROM app.job_statement_evidence WHERE attempt_id = $1 AND ordinal = 1", fix.claim.AttemptID).Scan(&seOutcome); err != nil {
			t.Fatalf("query statement evidence: %v", err)
		}
		if seOutcome != "unknown" {
			t.Fatalf("statement evidence should have rolled back to unknown, got: %s", seOutcome)
		}

		// 3. Job state: target block remains preserved (blocks_database=true), status remains 'running'
		var jobStatus, jobExecStatus string
		var jobBlocked bool
		if err := platformDB.QueryRowContext(ctx, "SELECT status, execution_status, blocks_database FROM app.jobs WHERE id = $1", fix.claim.JobID).Scan(&jobStatus, &jobExecStatus, &jobBlocked); err != nil {
			t.Fatalf("query job: %v", err)
		}
		if !jobBlocked {
			t.Fatalf("job block must remain true when platform rollback tx fails, got blocks_database=%t", jobBlocked)
		}
		if jobStatus != "running" || jobExecStatus != "running" {
			t.Fatalf("job status must remain running when platform tx rolled back: status=%s, execStatus=%s", jobStatus, jobExecStatus)
		}

		// 4. Attempt state: status remains 'running'
		var attStatus string
		if err := platformDB.QueryRowContext(ctx, "SELECT status FROM app.job_attempts WHERE id = $1", fix.claim.AttemptID).Scan(&attStatus); err != nil {
			t.Fatalf("query attempt: %v", err)
		}
		if attStatus != "running" {
			t.Fatalf("attempt status must remain running, got: %s", attStatus)
		}
	})
}

func recoverCrashedBatchJob(t *testing.T, ctx context.Context, platformDB *sql.DB, jobID string, snapshot executioncontract.Draft, acquire func(context.Context, string) (*migration.TargetLease, error)) *ExecutionOutcome {
	t.Helper()
	return recoverCrashedBatchJobWith(t, ctx, platformDB, jobID, ExecutionDeps{Gate: batchFakeGate(snapshot), Acquire: acquire, Timeout: 10 * time.Second})
}

func recoverCrashedBatchJobWith(t *testing.T, ctx context.Context, platformDB *sql.DB, jobID string, deps ExecutionDeps) *ExecutionOutcome {
	t.Helper()
	svc := New(platformDB)
	found, cursor := false, ""
	for !found {
		ids, err := svc.ListUnresolvedBatchJobIDs(ctx, 100, cursor)
		if err != nil {
			t.Fatalf("list unresolved batch jobs: %v", err)
		}
		if len(ids) == 0 {
			t.Fatalf("recovery did not discover crashed job %s", jobID)
		}
		for _, id := range ids {
			found = found || id == jobID
		}
		cursor = ids[len(ids)-1]
	}
	outcome, err := svc.ReconcileBatchJobByID(ctx, jobID, deps)
	if err != nil || outcome == nil {
		t.Fatalf("reconcile crashed job %s: outcome=%+v err=%v", jobID, outcome, err)
	}
	return outcome
}

func requireRecoveryKeepsBlock(t *testing.T, ctx context.Context, platformDB *sql.DB, targetID string, origin *batchJobFixture, outcome *ExecutionOutcome) {
	t.Helper()
	var cErr executioncontract.ContractError
	if outcome.Reconciled || outcome.Status == executioncontract.Succeeded || !errors.As(outcome.Error, &cErr) || cErr.Code != "TARGET_BLOCKED" {
		t.Fatalf("recovery must fail closed with TARGET_BLOCKED, got %+v", outcome)
	}
	var blocked bool
	if err := platformDB.QueryRowContext(ctx, "SELECT blocks_database FROM app.jobs WHERE id = $1", origin.claim.JobID).Scan(&blocked); err != nil || !blocked {
		t.Fatalf("origin job must keep blocking the target: blocked=%t err=%v", blocked, err)
	}
	svc := New(platformDB)
	tx, err := platformDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	proposalID := uuid.NewString()
	hashBytes := sha256.Sum256([]byte(origin.snapshot.SQLUp))
	snapshotBytes, _ := json.Marshal(origin.snapshot)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO app.migration_proposals (id, database_id, migration_id, revision, action, source_execution_id, status, snapshot, snapshot_hash, created_by)
		VALUES ($1, $2, $3, 1, 'resume', $4, 'draft', $5::jsonb, $6, $7)
	`, proposalID, origin.dbID, origin.draftID, origin.executionID, string(snapshotBytes), hashBytes[:], origin.actorID); err != nil {
		t.Fatalf("insert resume proposal: %v", err)
	}
	if _, err := svc.EnqueueTx(ctx, tx, executioncontract.EnqueueRequest{
		TargetID: targetID, RequestID: uuid.NewString(), ActorID: origin.actorID, DatabaseID: origin.dbID,
		ProposalID: proposalID, MigrationID: origin.draftID, Revision: 1, Action: executioncontract.Resume,
		Direction: executioncontract.Up, SourceExecutionID: &origin.executionID, Snapshot: origin.snapshot,
		SnapshotHash: hex.EncodeToString(hashBytes[:]),
	}); err != nil {
		t.Fatalf("enqueue resume: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if claim, err := svc.ClaimJob(ctx, ClaimRequest{WorkerID: "worker-blocked-resume", LeaseDuration: 30 * time.Second}); err != nil || claim != nil {
		t.Fatalf("resume must not be claimable while the origin is unresolved: claim=%+v err=%v", claim, err)
	}
}

func TestExecuteBatchJob_HardCrashRecovery(t *testing.T) {
	t.Run("HardCrashAfterAuthorizationBeforeCheckpoint_ProvedAbortedTx_ResumeSucceedsWithoutReplay", func(t *testing.T) {
		e, platformDB, ownerDB := setupBatchTestEnv(t)
		setupBatchItemsFixture(t, e)

		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()

		if _, err := e.exec.ExecContext(ctx, "INSERT INTO managed.batch_items (id, processed) VALUES (1, false), (2, false), (3, false), (4, false)"); err != nil {
			t.Fatalf("insert items: %v", err)
		}

		// 1. Worker 1 claims with 1s lease
		fix1 := createBatchFixtureWithDuration(t, platformDB, ownerDB, e.target, 2, executioncontract.Apply, nil, 1*time.Second)
		cleanupBatchScope(t, e, fix1.executionID)

		acquire := makeBatchAcquire(t, e.exec, e.prep)
		svc := New(platformDB)

		var crashHookRan bool
		deps1 := ExecutionDeps{
			Gate:    batchFakeGate(fix1.snapshot),
			Acquire: acquire,
			Timeout: 10 * time.Second,
			crashAfterBatchAuthorize: func(ctx context.Context, batchIndex int) error {
				crashHookRan = true
				return errors.New("simulated hard crash after authorization before target commit")
			},
		}

		outcome1, err := svc.ExecuteBatchJob(ctx, fix1.claim, deps1)
		if err == nil {
			t.Fatalf("expected crash error, got outcome=%+v", outcome1)
		}
		if !crashHookRan {
			t.Fatal("expected crashAfterBatchAuthorize hook to execute")
		}

		// 2. Durable state verification after crash:
		// Target transaction was proved aborted
		var authXID string
		if err := platformDB.QueryRowContext(ctx, "SELECT target_xid FROM app.job_batch_step_authorizations WHERE job_id = $1 AND batch_index = 0", fix1.claim.JobID).Scan(&authXID); err != nil {
			t.Fatalf("query authorized step target_xid: %v", err)
		}

		var xactStatus string
		if err := e.prep.QueryRowContext(ctx, "SELECT pg_catalog.pg_xact_status($1::xid8)", authXID).Scan(&xactStatus); err != nil {
			t.Fatalf("query target xact status: %v", err)
		}
		if xactStatus != "aborted" {
			t.Fatalf("expected target transaction to be proved aborted, got: %s", xactStatus)
		}

		// Target checkpoints: 0 recorded
		var chkCount int
		if err := e.prep.QueryRowContext(ctx, "SELECT count(*) FROM wbd_auth.batch_checkpoints WHERE execution_id = $1::uuid", fix1.executionID).Scan(&chkCount); err != nil {
			t.Fatalf("count checkpoints: %v", err)
		}
		if chkCount != 0 {
			t.Fatalf("expected 0 checkpoints after crash, got %d", chkCount)
		}

		// Platform job and attempt state before lease expiry: still running with blocks_database=true
		var jobStatus string
		var jobBlocked bool
		if err := platformDB.QueryRowContext(ctx, "SELECT status, blocks_database FROM app.jobs WHERE id = $1", fix1.claim.JobID).Scan(&jobStatus, &jobBlocked); err != nil {
			t.Fatalf("query job: %v", err)
		}
		if jobStatus != "running" || !jobBlocked {
			t.Fatalf("expected job running and blocked after crash, got status=%s blocked=%t", jobStatus, jobBlocked)
		}

		var attStatus string
		var attSQL bool
		if err := platformDB.QueryRowContext(ctx, "SELECT status, sql_entered FROM app.job_attempts WHERE id = $1", fix1.claim.AttemptID).Scan(&attStatus, &attSQL); err != nil {
			t.Fatalf("query attempt: %v", err)
		}
		if attStatus != "running" || !attSQL {
			t.Fatalf("expected attempt running and sql_entered=true after crash, got status=%s sql=%t", attStatus, attSQL)
		}

		// 3. Natural clock lease expiry (no manual rewriting of job/attempt)
		time.Sleep(3000 * time.Millisecond)
		recoverCrashedBatchJob(t, ctx, platformDB, fix1.claim.JobID, fix1.snapshot, acquire)
		// 4. Enqueue and claim Resume job
		fixResume := createBatchFixture(t, platformDB, ownerDB, e.target, 2, executioncontract.Resume, &fix1)

		var b0Ran, b1Ran bool
		depsResume := ExecutionDeps{
			Gate:    batchFakeGate(fixResume.snapshot),
			Acquire: acquire,
			Timeout: 10 * time.Second,
			beforeBatchDispatch: func(ctx context.Context, batchIndex int) error {
				if batchIndex == 0 {
					b0Ran = true
				}
				if batchIndex == 1 {
					b1Ran = true
				}
				return nil
			},
		}

		outcomeResume, err := svc.ExecuteBatchJob(ctx, fixResume.claim, depsResume)
		if err != nil || outcomeResume == nil || outcomeResume.Status != executioncontract.Succeeded {
			t.Fatalf("expected resume to succeed, got outcome=%+v err=%v", outcomeResume, err)
		}
		if !b0Ran {
			t.Fatal("batch 0 was not executed during resume (must execute freshly since prior was aborted)")
		}
		if !b1Ran {
			t.Fatal("batch 1 was not executed during resume")
		}

		// 5. Post-recovery verification:
		// Target items: all 4 processed
		var allDone bool
		if err := e.exec.QueryRowContext(ctx, "SELECT bool_and(processed) FROM managed.batch_items WHERE id IN (1, 2, 3, 4)").Scan(&allDone); err != nil || !allDone {
			t.Fatalf("expected all items processed after resume: %t err=%v", allDone, err)
		}

		// Target checkpoints: exactly 2 checkpoints recorded
		if err := e.prep.QueryRowContext(ctx, "SELECT count(*) FROM wbd_auth.batch_checkpoints WHERE execution_id = $1::uuid", fix1.executionID).Scan(&chkCount); err != nil {
			t.Fatalf("count checkpoints after resume: %v", err)
		}
		if chkCount != 2 {
			t.Fatalf("expected 2 checkpoints after resume, got %d", chkCount)
		}

		// Originating job 1 was closed as failed (0 committed batches) and unblocked
		var origStatus string
		var origBlocked bool
		if err := platformDB.QueryRowContext(ctx, "SELECT status, blocks_database FROM app.jobs WHERE id = $1", fix1.claim.JobID).Scan(&origStatus, &origBlocked); err != nil {
			t.Fatalf("query orig job: %v", err)
		}
		if origStatus != "failed" || origBlocked {
			t.Fatalf("expected orig job to be closed as failed and unblocked, got status=%s blocked=%t", origStatus, origBlocked)
		}

		// Originating attempt was closed as failed
		var origAttStatus string
		if err := platformDB.QueryRowContext(ctx, "SELECT status FROM app.job_attempts WHERE id = $1", fix1.claim.AttemptID).Scan(&origAttStatus); err != nil {
			t.Fatalf("query orig attempt: %v", err)
		}
		if origAttStatus != "failed" {
			t.Fatalf("expected orig attempt to be failed, got: %s", origAttStatus)
		}

		// Originating statement evidence for batch 0 was marked failed
		var seOutcome string
		if err := platformDB.QueryRowContext(ctx, "SELECT outcome FROM app.job_statement_evidence WHERE attempt_id = $1 AND ordinal = 1", fix1.claim.AttemptID).Scan(&seOutcome); err != nil {
			t.Fatalf("query orig statement evidence: %v", err)
		}
		if seOutcome != "failed" {
			t.Fatalf("expected orig statement evidence to be failed, got: %s", seOutcome)
		}

		// Resume job succeeded and unblocked
		var resStatus string
		var resBlocked bool
		if err := platformDB.QueryRowContext(ctx, "SELECT status, blocks_database FROM app.jobs WHERE id = $1", fixResume.claim.JobID).Scan(&resStatus, &resBlocked); err != nil {
			t.Fatalf("query resume job: %v", err)
		}
		if resStatus != "succeeded" || resBlocked {
			t.Fatalf("expected resume job succeeded and unblocked, got status=%s blocked=%t", resStatus, resBlocked)
		}
	})

	t.Run("HardCrashWithExpiredCommittedCheckpoint_TruthfulPartialProgress_NoSQLReplay", func(t *testing.T) {
		e, platformDB, ownerDB := setupBatchTestEnv(t)
		setupBatchItemsFixture(t, e)

		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()

		if _, err := e.exec.ExecContext(ctx, "INSERT INTO managed.batch_items (id, processed) VALUES (1, false), (2, false), (3, false), (4, false)"); err != nil {
			t.Fatalf("insert items: %v", err)
		}

		fix1 := createBatchFixtureWithDuration(t, platformDB, ownerDB, e.target, 2, executioncontract.Apply, nil, 2*time.Second)
		cleanupBatchScope(t, e, fix1.executionID)

		acquire := makeBatchAcquire(t, e.exec, e.prep)
		svc := New(platformDB)

		var committedB0Crash bool
		deps1 := ExecutionDeps{
			Gate:    batchFakeGate(fix1.snapshot),
			Acquire: acquire,
			Timeout: 10 * time.Second,
			crashAfterBatchCommit: func(ctx context.Context, batchIndex int) error {
				if batchIndex == 0 {
					committedB0Crash = true
					return errors.New("simulated hard crash after batch 0 committed before platform finalization")
				}
				return nil
			},
		}

		outcome1, err := svc.ExecuteBatchJob(ctx, fix1.claim, deps1)
		if err == nil {
			t.Fatalf("expected crash error, got outcome=%+v", outcome1)
		}
		if !committedB0Crash {
			t.Fatal("expected crashAfterBatchCommit hook to run")
		}

		// Target has checkpoint 0 committed
		var chkCount int
		if err := e.prep.QueryRowContext(ctx, "SELECT count(*) FROM wbd_auth.batch_checkpoints WHERE execution_id = $1::uuid AND batch_index = 0", fix1.executionID).Scan(&chkCount); err != nil || chkCount != 1 {
			t.Fatalf("expected 1 checkpoint on target, got count=%d err=%v", chkCount, err)
		}

		// Rows 1 and 2 are processed
		var r1, r2, r3, r4 bool
		if err := e.exec.QueryRowContext(ctx, "SELECT (SELECT processed FROM managed.batch_items WHERE id = 1), (SELECT processed FROM managed.batch_items WHERE id = 2), (SELECT processed FROM managed.batch_items WHERE id = 3), (SELECT processed FROM managed.batch_items WHERE id = 4)").Scan(&r1, &r2, &r3, &r4); err != nil {
			t.Fatalf("query item states: %v", err)
		}
		if !r1 || !r2 || r3 || r4 {
			t.Fatalf("item states unexpected after batch 0 commit: r1=%t r2=%t r3=%t r4=%t", r1, r2, r3, r4)
		}

		// Wait for lease expiry
		if _, err := platformDB.ExecContext(ctx, "UPDATE app.jobs SET lease_expires_at = clock_timestamp() - interval '5 seconds' WHERE id = $1", fix1.claim.JobID); err != nil {
			t.Fatalf("expire job: %v", err)
		}

		recoverCrashedBatchJob(t, ctx, platformDB, fix1.claim.JobID, fix1.snapshot, acquire)

		// Resume execution
		fixResume := createBatchFixture(t, platformDB, ownerDB, e.target, 2, executioncontract.Resume, &fix1)

		var replayedBatch0 bool
		var ranBatch1 bool
		depsResume := ExecutionDeps{
			Gate:    batchFakeGate(fixResume.snapshot),
			Acquire: acquire,
			Timeout: 10 * time.Second,
			beforeBatchDispatch: func(ctx context.Context, batchIndex int) error {
				if batchIndex == 0 {
					replayedBatch0 = true
					return errors.New("batch 0 was replayed! SQL replay violation")
				}
				if batchIndex == 1 {
					ranBatch1 = true
				}
				return nil
			},
		}

		outcomeResume, err := svc.ExecuteBatchJob(ctx, fixResume.claim, depsResume)
		if err != nil || outcomeResume == nil || outcomeResume.Status != executioncontract.Succeeded {
			t.Fatalf("expected resume to succeed, got outcome=%+v err=%v", outcomeResume, err)
		}
		if replayedBatch0 {
			t.Fatal("SQL replay violation: committed batch 0 was replayed during resume")
		}
		if !ranBatch1 {
			t.Fatal("batch 1 was not executed during resume")
		}

		// Originating job 1 had truthful partial progress: closed as 'partial'
		var origStatus string
		if err := platformDB.QueryRowContext(ctx, "SELECT status FROM app.jobs WHERE id = $1", fix1.claim.JobID).Scan(&origStatus); err != nil {
			t.Fatalf("query orig job: %v", err)
		}
		if origStatus != "partial" {
			t.Fatalf("expected orig job to preserve truthful partial status, got: %s", origStatus)
		}

		// Originating attempt had truthful partial progress: closed as 'partial'
		var origAttStatus string
		if err := platformDB.QueryRowContext(ctx, "SELECT status FROM app.job_attempts WHERE id = $1", fix1.claim.AttemptID).Scan(&origAttStatus); err != nil {
			t.Fatalf("query orig attempt: %v", err)
		}
		if origAttStatus != "partial" {
			t.Fatalf("expected orig attempt to be partial, got: %s", origAttStatus)
		}

		// Target checkpoints: 2
		if err := e.prep.QueryRowContext(ctx, "SELECT count(*) FROM wbd_auth.batch_checkpoints WHERE execution_id = $1::uuid", fix1.executionID).Scan(&chkCount); err != nil || chkCount != 2 {
			t.Fatalf("expected 2 checkpoints, got %d err=%v", chkCount, err)
		}

		// All 4 items processed
		var allDone bool
		if err := e.exec.QueryRowContext(ctx, "SELECT bool_and(processed) FROM managed.batch_items WHERE id IN (1, 2, 3, 4)").Scan(&allDone); err != nil || !allDone {
			t.Fatalf("expected all items processed: %t err=%v", allDone, err)
		}
	})

	t.Run("UnresolvedXID_CommittedWithoutCheckpoint_StaysBlockedWithNoRedispatch", func(t *testing.T) {
		e, platformDB, ownerDB := setupBatchTestEnv(t)
		setupBatchItemsFixture(t, e)

		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()

		if _, err := e.exec.ExecContext(ctx, "INSERT INTO managed.batch_items (id, processed) VALUES (1, false), (2, false)"); err != nil {
			t.Fatalf("insert items: %v", err)
		}

		fix1 := createBatchFixtureWithDuration(t, platformDB, ownerDB, e.target, 2, executioncontract.Apply, nil, 100*time.Millisecond)
		cleanupBatchScope(t, e, fix1.executionID)
		svc := New(platformDB)
		ident := BatchScopeIdentity{
			ExecutionID: fix1.executionID,
			TargetID:    fix1.targetID,
			DatabaseID:  fix1.dbID,
			ActorID:     fix1.actorID,
		}
		if _, err := svc.FreezeBatchScope(ctx, e.prep, ident); err != nil {
			t.Fatalf("freeze batch scope: %v", err)
		}
		// Authorize batch 0
		var xid string
		tx, err := e.exec.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("begin target tx: %v", err)
		}
		if err := tx.QueryRowContext(ctx, "SELECT pg_catalog.pg_current_xact_id()::text").Scan(&xid); err != nil {
			t.Fatalf("query target xid: %v", err)
		}
		// Commit target tx WITHOUT recording checkpoint
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit target tx: %v", err)
		}
		plan, err := batchFakeGate(fix1.snapshot).Revalidate(ctx, fix1.claim.RevalidationRequest())
		if err != nil {
			t.Fatalf("revalidate: %v", err)
		}
		sqlText, _ := selectSQL(plan, fix1.claim.Action, fix1.claim.Direction)
		chunkIDs := []int64{1, 2}
		if err := svc.authorizeBatchDispatch(ctx, fix1.claim, batchFakeGate(fix1.snapshot), fix1.executionID, plan, sqlText, xid, 1, 1, 0, chunkIDs); err != nil {
			t.Fatalf("authorize dispatch: %v", err)
		}

		// Wait for lease to expire
		time.Sleep(150 * time.Millisecond)

		outcome := recoverCrashedBatchJob(t, ctx, platformDB, fix1.claim.JobID, fix1.snapshot, makeBatchAcquire(t, e.exec, e.prep))
		requireRecoveryKeepsBlock(t, ctx, platformDB, e.target, &fix1, outcome)
	})
}

func TestExecuteBatchJob_Regression_SharedSourceUnrelatedBlockerPreserved(t *testing.T) {
	e, platformDB, ownerDB := setupBatchTestEnv(t)
	setupBatchItemsFixture(t, e)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := e.exec.ExecContext(ctx, "INSERT INTO managed.batch_items (id, processed) VALUES (1, false), (2, false), (3, false), (4, false)"); err != nil {
		t.Fatalf("insert items: %v", err)
	}

	fix1 := createBatchFixture(t, platformDB, ownerDB, e.target, 2, executioncontract.Apply, nil)
	cleanupBatchScope(t, e, fix1.executionID)

	acquire := makeBatchAcquire(t, e.exec, e.prep)
	svc := New(platformDB)

	// Crash after batch 0 so fix1 is partial and blocks_database = true
	deps1 := ExecutionDeps{
		Gate:    batchFakeGate(fix1.snapshot),
		Acquire: acquire,
		Timeout: 10 * time.Second,
		afterBatchCommit: func(ctx context.Context, batchIndex int) error {
			if batchIndex == 0 {
				return errors.New("simulated crash after batch 0")
			}
			return nil
		},
	}
	_, _ = svc.ExecuteBatchJob(ctx, fix1.claim, deps1)

	// Verify fix1 is blocked
	var origBlocked bool
	if err := platformDB.QueryRowContext(ctx, "SELECT blocks_database FROM app.jobs WHERE id = $1", fix1.claim.JobID).Scan(&origBlocked); err != nil || !origBlocked {
		t.Fatalf("expected fix1 to be blocked: %t", origBlocked)
	}

	// Claim resume job before inserting unrelated blocker
	fixResume := createBatchFixture(t, platformDB, ownerDB, e.target, 2, executioncontract.Resume, &fix1)
	cleanupBatchScope(t, e, fixResume.executionID)

	otherTargetID := uuid.NewString()
	if _, err := ownerDB.ExecContext(ctx, "INSERT INTO app.migration_target_state (id, next_version) VALUES ($1, 1)", otherTargetID); err != nil {
		t.Fatalf("insert other target state: %v", err)
	}
	t.Cleanup(func() {
		_, _ = ownerDB.ExecContext(context.Background(), "DELETE FROM app.migration_target_state WHERE id = $1", otherTargetID)
	})

	// Insert an unrelated job that references the same source_execution_id, but on otherTargetID with action = 'rollback' and blocks_database = true
	unrelatedJobID := uuid.NewString()
	unrelatedExecID := uuid.NewString()
	unrelatedReqID := uuid.NewString()
	if _, err := platformDB.ExecContext(ctx, `
		INSERT INTO app.jobs (
			id, request_id, database_id, target_id, proposal_id, migration_id, revision,
			execution_id, actor_id, action, direction, mode, status, execution_status,
			attempt_no, snapshot, snapshot_hash, blocks_database, source_execution_id
		) VALUES (
			$1, $2, $3, $4, $5, $6, 1,
			$7, $8, 'rollback', 'down', 'standard', 'failed', 'failed',
			1, '{}'::jsonb, repeat('0', 64), true, $9
		)
	`, unrelatedJobID, unrelatedReqID, fix1.dbID, otherTargetID, fix1.proposalID, fix1.draftID,
		unrelatedExecID, fix1.actorID, fix1.executionID); err != nil {
		t.Fatalf("insert shared-source unrelated blocked job: %v", err)
	}
	t.Cleanup(func() {
		_, _ = ownerDB.ExecContext(context.Background(), "DELETE FROM app.jobs WHERE id = $1", unrelatedJobID)
	})
	depsResume := ExecutionDeps{
		Gate:    batchFakeGate(fixResume.snapshot),
		Acquire: acquire,
		Timeout: 10 * time.Second,
	}
	outcomeResume, err := svc.ExecuteBatchJob(ctx, fixResume.claim, depsResume)
	if err != nil || outcomeResume == nil || outcomeResume.Status != executioncontract.Succeeded {
		t.Fatalf("expected resume to succeed, got outcome=%+v err=%v", outcomeResume, err)
	}

	// 1. Source job fix1 should have execution_status = 'succeeded', blocks_database = false, and status retained ('partial')
	var fix1Status, fix1ExecStatus string
	var fix1Blocked bool
	if err := platformDB.QueryRowContext(ctx, "SELECT status, execution_status, blocks_database FROM app.jobs WHERE id = $1", fix1.claim.JobID).Scan(&fix1Status, &fix1ExecStatus, &fix1Blocked); err != nil {
		t.Fatalf("query fix1: %v", err)
	}
	if fix1Blocked || fix1ExecStatus != "succeeded" || fix1Status == "succeeded" {
		t.Fatalf("fix1 state unexpected: blocked=%t execStatus=%s status=%s", fix1Blocked, fix1ExecStatus, fix1Status)
	}

	// 2. Unrelated job referencing the same source execution ID MUST REMAIN BLOCKED (blocks_database = true)
	var unrelatedBlocked bool
	if err := platformDB.QueryRowContext(ctx, "SELECT blocks_database FROM app.jobs WHERE id = $1", unrelatedJobID).Scan(&unrelatedBlocked); err != nil {
		t.Fatalf("query unrelated job: %v", err)
	}
	if !unrelatedBlocked {
		t.Fatal("shared-source unrelated job blocks_database MUST NOT be cleared by resume completion!")
	}
}

func TestExecuteBatchJob_Regression_MismatchedOriginatingEvidence_RetainsBlockNoReplay(t *testing.T) {
	e, platformDB, ownerDB := setupBatchTestEnv(t)
	setupBatchItemsFixture(t, e)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := e.exec.ExecContext(ctx, "INSERT INTO managed.batch_items (id, processed) VALUES (1, false), (2, false), (3, false), (4, false)"); err != nil {
		t.Fatalf("insert items: %v", err)
	}

	fix1 := createBatchFixture(t, platformDB, ownerDB, e.target, 2, executioncontract.Apply, nil)
	cleanupBatchScope(t, e, fix1.executionID)

	acquire := makeBatchAcquire(t, e.exec, e.prep)
	svc := New(platformDB)

	// Commit batch 0 target checkpoint
	deps1 := ExecutionDeps{
		Gate:    batchFakeGate(fix1.snapshot),
		Acquire: acquire,
		Timeout: 10 * time.Second,
		afterBatchCommit: func(ctx context.Context, batchIndex int) error {
			if batchIndex == 0 {
				return errors.New("simulated crash after batch 0 commit")
			}
			return nil
		},
	}
	_, _ = svc.ExecuteBatchJob(ctx, fix1.claim, deps1)

	// Tamper with originating evidence in app.job_batch_step_authorizations: corrupt SQL hash
	if _, err := ownerDB.ExecContext(ctx, `
		UPDATE app.job_batch_step_authorizations
		SET sql_sha256 = decode(repeat('00', 32), 'hex')
		WHERE scope_execution_id = $1::uuid AND batch_index = 0
	`, fix1.executionID); err != nil {
		t.Fatalf("corrupt step auth: %v", err)
	}

	fixResume := createBatchFixture(t, platformDB, ownerDB, e.target, 2, executioncontract.Resume, &fix1)
	cleanupBatchScope(t, e, fixResume.executionID)

	var replayed0 bool
	depsResume := ExecutionDeps{
		Gate:    batchFakeGate(fixResume.snapshot),
		Acquire: acquire,
		Timeout: 10 * time.Second,
		beforeBatchDispatch: func(ctx context.Context, batchIndex int) error {
			if batchIndex == 0 {
				replayed0 = true
			}
			return nil
		},
	}

	outcome, err := svc.ExecuteBatchJob(ctx, fixResume.claim, depsResume)
	if outcome == nil || outcome.Status != executioncontract.Failed {
		t.Fatalf("expected Failed blocked outcome due to evidence mismatch, got outcome=%+v err=%v", outcome, err)
	}
	var cErr executioncontract.ContractError
	if !errors.As(outcome.Error, &cErr) || cErr.Code != "TARGET_BLOCKED" {
		t.Fatalf("expected TARGET_BLOCKED error code, got: %v", outcome.Error)
	}
	if replayed0 {
		t.Fatal("batch 0 was replayed on mismatched evidence!")
	}

	// Physical block must be retained
	var resumeBlocked bool
	if err := platformDB.QueryRowContext(ctx, "SELECT blocks_database FROM app.jobs WHERE id = $1", fixResume.claim.JobID).Scan(&resumeBlocked); err != nil || !resumeBlocked {
		t.Fatalf("resume job must retain blocks_database=true, got %t", resumeBlocked)
	}
}

func TestExecuteBatchJob_Regression_OutOfScopeCheckpoints_FailClosed(t *testing.T) {
	t.Run("ZeroRowScope_ExtraCheckpoint_FailsClosed", func(t *testing.T) {
		e, platformDB, ownerDB := setupBatchTestEnv(t)
		setupBatchItemsFixture(t, e)

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// Target managed.batch_items has 0 rows -> totalBatches = 0
		fix := createBatchFixture(t, platformDB, ownerDB, e.target, 2, executioncontract.Apply, nil)
		cleanupBatchScope(t, e, fix.executionID)

		capBytes := make([]byte, 32)
		capBytes[0] = 0xAA
		hash := sha256.Sum256(capBytes)
		tx, err := e.exec.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		var curXID string
		if err := tx.QueryRowContext(ctx, "SELECT pg_catalog.pg_current_xact_id()::text").Scan(&curXID); err != nil {
			t.Fatal(err)
		}
		_, err = e.cleanup.ExecContext(ctx, `INSERT INTO wbd_auth.batch_tickets (execution_id, physical_target_id, target_xid, batch_index, direction, capability_sha256)
			VALUES ($1, $2, $3::xid8, 0, 'up', $4)`, fix.executionID, e.target, curXID, hash[:])
		if err != nil {
			t.Fatalf("insert ticket: %v", err)
		}
		_, err = tx.ExecContext(ctx, `SELECT wbd_auth.record_checkpoint($1::uuid, 0, 'up', 0, $2::bytea)`, fix.executionID, capBytes)
		if err != nil {
			t.Fatalf("record checkpoint: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, _ = e.cleanup.ExecContext(context.Background(), "DELETE FROM wbd_auth.batch_checkpoints WHERE execution_id = $1", fix.executionID)
			_, _ = e.cleanup.ExecContext(context.Background(), "DELETE FROM wbd_auth.batch_tickets WHERE execution_id = $1", fix.executionID)
		})

		acquire := makeBatchAcquire(t, e.exec, e.prep)
		svc := New(platformDB)
		deps := ExecutionDeps{
			Gate:    batchFakeGate(fix.snapshot),
			Acquire: acquire,
			Timeout: 10 * time.Second,
		}

		outcome, err := svc.ExecuteBatchJob(ctx, fix.claim, deps)
		if outcome == nil || outcome.Status != executioncontract.Failed {
			t.Fatalf("expected Failed blocked outcome for zero-row scope with extra checkpoint, got outcome=%+v err=%v", outcome, err)
		}
		var cErr executioncontract.ContractError
		if !errors.As(outcome.Error, &cErr) || cErr.Code != "TARGET_BLOCKED" {
			t.Fatalf("expected TARGET_BLOCKED, got: %v", outcome.Error)
		}

		var blocked bool
		if err := platformDB.QueryRowContext(ctx, "SELECT blocks_database FROM app.jobs WHERE id = $1", fix.claim.JobID).Scan(&blocked); err != nil || !blocked {
			t.Fatalf("job must retain blocks_database=true: %t", blocked)
		}
	})

	t.Run("ExtraCheckpointOutsideBounds_FailsClosed", func(t *testing.T) {
		e, platformDB, ownerDB := setupBatchTestEnv(t)
		setupBatchItemsFixture(t, e)

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		if _, err := e.exec.ExecContext(ctx, "INSERT INTO managed.batch_items (id, processed) VALUES (1, false), (2, false), (3, false), (4, false)"); err != nil {
			t.Fatalf("insert items: %v", err)
		}

		// Total rows 4, batchSize 2 -> totalBatches = 2. Valid indices [0, 1].
		fix := createBatchFixture(t, platformDB, ownerDB, e.target, 2, executioncontract.Apply, nil)
		cleanupBatchScope(t, e, fix.executionID)

		capBytes := make([]byte, 32)
		capBytes[0] = 0xBB
		hash := sha256.Sum256(capBytes)
		tx, err := e.exec.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		var curXID string
		if err := tx.QueryRowContext(ctx, "SELECT pg_catalog.pg_current_xact_id()::text").Scan(&curXID); err != nil {
			t.Fatal(err)
		}
		_, err = e.cleanup.ExecContext(ctx, `INSERT INTO wbd_auth.batch_tickets (execution_id, physical_target_id, target_xid, batch_index, direction, capability_sha256)
			VALUES ($1, $2, $3::xid8, 2, 'up', $4)`, fix.executionID, e.target, curXID, hash[:])
		if err != nil {
			t.Fatalf("insert ticket: %v", err)
		}
		_, err = tx.ExecContext(ctx, `SELECT wbd_auth.record_checkpoint($1::uuid, 2, 'up', 2, $2::bytea)`, fix.executionID, capBytes)
		if err != nil {
			t.Fatalf("record checkpoint: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, _ = e.cleanup.ExecContext(context.Background(), "DELETE FROM wbd_auth.batch_checkpoints WHERE execution_id = $1", fix.executionID)
			_, _ = e.cleanup.ExecContext(context.Background(), "DELETE FROM wbd_auth.batch_tickets WHERE execution_id = $1", fix.executionID)
		})

		acquire := makeBatchAcquire(t, e.exec, e.prep)
		svc := New(platformDB)
		deps := ExecutionDeps{
			Gate:    batchFakeGate(fix.snapshot),
			Acquire: acquire,
			Timeout: 10 * time.Second,
		}

		outcome, err := svc.ExecuteBatchJob(ctx, fix.claim, deps)
		if outcome == nil || outcome.Status != executioncontract.Failed {
			t.Fatalf("expected Failed blocked outcome for out-of-bounds checkpoint, got outcome=%+v err=%v", outcome, err)
		}
		var cErr executioncontract.ContractError
		if !errors.As(outcome.Error, &cErr) || cErr.Code != "TARGET_BLOCKED" {
			t.Fatalf("expected TARGET_BLOCKED, got: %v", outcome.Error)
		}

		var blocked bool
		if err := platformDB.QueryRowContext(ctx, "SELECT blocks_database FROM app.jobs WHERE id = $1", fix.claim.JobID).Scan(&blocked); err != nil || !blocked {
			t.Fatalf("job must retain blocks_database=true: %t", blocked)
		}
	})
}

func TestExecuteBatchJob_Regression_RecoveryScan_DeterministicCancellation(t *testing.T) {
	e, platformDB, ownerDB := setupBatchTestEnv(t)
	setupBatchItemsFixture(t, e)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := e.exec.ExecContext(ctx, "INSERT INTO managed.batch_items (id, processed) VALUES (1, false), (2, false), (3, false), (4, false)"); err != nil {
		t.Fatalf("insert items: %v", err)
	}

	fix1 := createBatchFixture(t, platformDB, ownerDB, e.target, 2, executioncontract.Apply, nil)
	cleanupBatchScope(t, e, fix1.executionID)

	acquire := makeBatchAcquire(t, e.exec, e.prep)
	svc := New(platformDB)

	// Commit batch 0
	deps1 := ExecutionDeps{
		Gate:    batchFakeGate(fix1.snapshot),
		Acquire: acquire,
		Timeout: 10 * time.Second,
		afterBatchCommit: func(ctx context.Context, batchIndex int) error {
			if batchIndex == 0 {
				return errors.New("simulated crash after batch 0")
			}
			return nil
		},
	}
	_, _ = svc.ExecuteBatchJob(ctx, fix1.claim, deps1)

	// Create resume job
	fixResume := createBatchFixture(t, platformDB, ownerDB, e.target, 2, executioncontract.Resume, &fix1)
	cleanupBatchScope(t, e, fixResume.executionID)

	// Pre-cancel context before executing recovery scan
	cancelCtx, cancelFn := context.WithCancel(ctx)
	cancelFn() // cancel immediately

	depsResume := ExecutionDeps{
		Gate:    batchFakeGate(fixResume.snapshot),
		Acquire: acquire,
		Timeout: 10 * time.Second,
	}

	outcome, err := svc.ExecuteBatchJob(cancelCtx, fixResume.claim, depsResume)
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled error during recovery scan, got outcome=%+v err=%v", outcome, err)
	}

	// Verify no state advancement: batch 1 was not committed
	var p3, p4 bool
	if err := e.exec.QueryRowContext(ctx, "SELECT (SELECT processed FROM managed.batch_items WHERE id = 3), (SELECT processed FROM managed.batch_items WHERE id = 4)").Scan(&p3, &p4); err != nil {
		t.Fatalf("query items: %v", err)
	}
	if p3 || p4 {
		t.Fatalf("batch 1 items must remain false after canceled recovery: p3=%t, p4=%t", p3, p4)
	}
}

func TestExecuteBatchJob_Regression_ValidFinalCommitPendingProof_ExtraCheckpoint_FailsClosed(t *testing.T) {
	e, platformDB, ownerDB := setupBatchTestEnv(t)
	setupBatchItemsFixture(t, e)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := e.exec.ExecContext(ctx, "INSERT INTO managed.batch_items (id, processed) VALUES (1, false), (2, false), (3, false), (4, false)"); err != nil {
		t.Fatalf("insert items: %v", err)
	}

	// Total rows 4, batchSize 2 -> totalBatches = 2.
	fix1 := createBatchFixture(t, platformDB, ownerDB, e.target, 2, executioncontract.Apply, nil)
	cleanupBatchScope(t, e, fix1.executionID)

	acquire := makeBatchAcquire(t, e.exec, e.prep)
	svc := New(platformDB)

	// Step 1: Run batch 0 normally, but crash batch 1 after target commit (before platform proof).
	deps1 := ExecutionDeps{
		Gate:    batchFakeGate(fix1.snapshot),
		Acquire: acquire,
		Timeout: 10 * time.Second,
		crashAfterBatchCommit: func(ctx context.Context, batchIndex int) error {
			if batchIndex == 1 {
				return errors.New("simulated crash after final batch target commit")
			}
			return nil
		},
	}
	outcome1, err1 := svc.ExecuteBatchJob(ctx, fix1.claim, deps1)
	if err1 == nil && (outcome1 != nil && outcome1.Status == executioncontract.Succeeded) {
		t.Fatalf("expected crash after batch 1 commit, got outcome=%+v err=%v", outcome1, err1)
	}

	// Verify batch 1 has checkpoint on target but NO proof on platform DB
	var b1ProofCount int
	if err := platformDB.QueryRowContext(ctx, `
		SELECT count(*) FROM app.job_batch_commit_proofs
		WHERE scope_execution_id = $1::uuid AND batch_index = 1
	`, fix1.executionID).Scan(&b1ProofCount); err != nil || b1ProofCount != 0 {
		t.Fatalf("expected 0 proofs for batch 1 before recovery, got count=%d, err=%v", b1ProofCount, err)
	}

	// Step 2: Inject an extra checkpoint (batch_index = 2) on the target
	capBytes := make([]byte, 32)
	capBytes[0] = 0xEE
	hash := sha256.Sum256(capBytes)
	tx, err := e.exec.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var curXID string
	if err := tx.QueryRowContext(ctx, "SELECT pg_catalog.pg_current_xact_id()::text").Scan(&curXID); err != nil {
		t.Fatal(err)
	}
	_, err = e.cleanup.ExecContext(ctx, `INSERT INTO wbd_auth.batch_tickets (execution_id, physical_target_id, target_xid, batch_index, direction, capability_sha256)
		VALUES ($1, $2, $3::xid8, 2, 'up', $4)`, fix1.executionID, e.target, curXID, hash[:])
	if err != nil {
		t.Fatalf("insert ticket: %v", err)
	}
	_, err = tx.ExecContext(ctx, `SELECT wbd_auth.record_checkpoint($1::uuid, 2, 'up', 2, $2::bytea)`, fix1.executionID, capBytes)
	if err != nil {
		t.Fatalf("record checkpoint: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = e.cleanup.ExecContext(context.Background(), "DELETE FROM wbd_auth.batch_checkpoints WHERE execution_id = $1", fix1.executionID)
		_, _ = e.cleanup.ExecContext(context.Background(), "DELETE FROM wbd_auth.batch_tickets WHERE execution_id = $1", fix1.executionID)
	})

	// Expire fix1 lease so resume can proceed
	if _, err := platformDB.ExecContext(ctx, `
		UPDATE app.jobs
		SET status = 'running', execution_status = 'running', lease_expires_at = clock_timestamp() - interval '10 seconds'
		WHERE id = $1
	`, fix1.claim.JobID); err != nil {
		t.Fatalf("expire job lease: %v", err)
	}
	if _, err := platformDB.ExecContext(ctx, `
		UPDATE app.job_attempts
		SET status = 'running'
		WHERE id = $1
	`, fix1.claim.AttemptID); err != nil {
		t.Fatalf("set attempt running: %v", err)
	}

	var finalStepRecordCommitCalled bool
	recoveryDeps := ExecutionDeps{
		Gate: transactionalBatchGate{executioncontract.FakeGate{
			RevalidateFunc: func(ctx context.Context, req executioncontract.RevalidationRequest) (executioncontract.ExecutionPlan, error) {
				return executioncontract.ExecutionPlan{
					Request:       req,
					Snapshot:      fix1.snapshot,
					Version:       "1",
					ActorUsername: "testactor",
					ValidatedAt:   time.Now().UTC(),
				}, nil
			},
			RecordCommitFunc: func(ctx context.Context, tx *sql.Tx, in executioncontract.CommitEvidence) error {
				if in.FinalStep {
					finalStepRecordCommitCalled = true
				}
				return nil
			},
		}},
		Acquire: acquire,
		Timeout: 10 * time.Second,
	}
	outcome := recoverCrashedBatchJobWith(t, ctx, platformDB, fix1.claim.JobID, recoveryDeps)

	requireRecoveryKeepsBlock(t, ctx, platformDB, e.target, &fix1, outcome)
	if finalStepRecordCommitCalled {
		t.Fatalf("must produce no final RecordCommitTx call")
	}
	var b1FinalProofCount int
	if err := platformDB.QueryRowContext(ctx, `
		SELECT count(*) FROM app.job_batch_commit_proofs
		WHERE scope_execution_id = $1::uuid AND batch_index = 1
	`, fix1.executionID).Scan(&b1FinalProofCount); err != nil || b1FinalProofCount != 0 {
		t.Fatalf("batch 1 proof must not be inserted on blocked reconciliation, got count=%d", b1FinalProofCount)
	}
}

func TestExecuteBatchJob_Regression_RecoveredFinalCheckpoint_PartiallyCommittedExpiredOrigin(t *testing.T) {
	e, platformDB, ownerDB := setupBatchTestEnv(t)
	setupBatchItemsFixture(t, e)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := e.exec.ExecContext(ctx, "INSERT INTO managed.batch_items (id, processed) VALUES (1, false), (2, false), (3, false), (4, false)"); err != nil {
		t.Fatalf("insert items: %v", err)
	}

	// 2 batches of size 2
	fix1 := createBatchFixture(t, platformDB, ownerDB, e.target, 2, executioncontract.Apply, nil)
	cleanupBatchScope(t, e, fix1.executionID)

	acquire := makeBatchAcquire(t, e.exec, e.prep)
	svc := New(platformDB)

	// Step 1: Job 1 runs and commits batch 0, then stops (simulating partial execution).
	deps1 := ExecutionDeps{
		Gate:    batchFakeGate(fix1.snapshot),
		Acquire: acquire,
		Timeout: 10 * time.Second,
		afterBatchCommit: func(ctx context.Context, batchIndex int) error {
			if batchIndex == 0 {
				return errors.New("simulated pause after batch 0")
			}
			return nil
		},
	}
	_, _ = svc.ExecuteBatchJob(ctx, fix1.claim, deps1)

	// Step 2: Job 1 expires with partially committed state (1 of 2 batches committed).
	if _, err := platformDB.ExecContext(ctx, `
		UPDATE app.jobs
		SET status = 'running', execution_status = 'running', lease_expires_at = clock_timestamp() - interval '10 seconds'
		WHERE id = $1
	`, fix1.claim.JobID); err != nil {
		t.Fatalf("expire job 1 lease: %v", err)
	}
	if _, err := platformDB.ExecContext(ctx, `
		UPDATE app.job_attempts
		SET status = 'running'
		WHERE id = $1
	`, fix1.claim.AttemptID); err != nil {
		t.Fatalf("set attempt 1 running: %v", err)
	}

	recoverCrashedBatchJob(t, ctx, platformDB, fix1.claim.JobID, fix1.snapshot, acquire)

	// Step 3: Resume job 1 executes batch 1, commits on target, but crashes before platform proof.
	fixResume1 := createBatchFixture(t, platformDB, ownerDB, e.target, 2, executioncontract.Resume, &fix1)
	cleanupBatchScope(t, e, fixResume1.executionID)

	depsResume1 := ExecutionDeps{
		Gate:    batchFakeGate(fixResume1.snapshot),
		Acquire: acquire,
		Timeout: 10 * time.Second,
		crashAfterBatchCommit: func(ctx context.Context, batchIndex int) error {
			if batchIndex == 1 {
				return errors.New("simulated crash after final batch commit on target")
			}
			return nil
		},
	}
	_, _ = svc.ExecuteBatchJob(ctx, fixResume1.claim, depsResume1)

	// Expire resume job 1 so a subsequent recovery can claim/reconcile
	if _, err := platformDB.ExecContext(ctx, `
		UPDATE app.jobs
		SET status = 'running', execution_status = 'running', lease_expires_at = clock_timestamp() - interval '10 seconds'
		WHERE id = $1
	`, fixResume1.claim.JobID); err != nil {
		t.Fatalf("expire resume job 1 lease: %v", err)
	}
	if _, err := platformDB.ExecContext(ctx, `
		UPDATE app.job_attempts
		SET status = 'running'
		WHERE id = $1
	`, fixResume1.claim.AttemptID); err != nil {
		t.Fatalf("set attempt running: %v", err)
	}

	if _, err := platformDB.ExecContext(ctx, "UPDATE app.migration_proposals SET status = 'superseded' WHERE id = $1", fixResume1.proposalID); err != nil {
		t.Fatalf("supersede resume 1 proposal: %v", err)
	}

	// Recovery and resume share one counter: the final step must be published exactly once overall.
	var recordCommitFinalCount int
	countingDeps := func(snapshot executioncontract.Draft) ExecutionDeps {
		return ExecutionDeps{
			Gate: transactionalBatchGate{executioncontract.FakeGate{
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
					if in.FinalStep {
						recordCommitFinalCount++
					}
					return nil
				},
			}},
			Acquire: acquire,
			Timeout: 10 * time.Second,
		}
	}
	recoverCrashedBatchJobWith(t, ctx, platformDB, fixResume1.claim.JobID, countingDeps(fixResume1.snapshot))

	// Step 4: Resume job 2 recovers the final checkpoint.
	fixResume2 := createBatchFixture(t, platformDB, ownerDB, e.target, 2, executioncontract.Resume, &fix1)
	cleanupBatchScope(t, e, fixResume2.executionID)

	depsResume2 := countingDeps(fixResume2.snapshot)

	outcome, err := svc.ExecuteBatchJob(ctx, fixResume2.claim, depsResume2)
	if err != nil || outcome == nil || outcome.Status != executioncontract.Succeeded {
		t.Fatalf("expected Succeeded outcome for recovered final checkpoint, got outcome=%+v err=%v", outcome, err)
	}

	// Decisive checks:
	// 1. Source aggregate succeeded and unblocked
	var srcExecStatus string
	var srcBlocked bool
	if err := platformDB.QueryRowContext(ctx, `
		SELECT execution_status, blocks_database
		FROM app.jobs WHERE id = $1
	`, fix1.claim.JobID).Scan(&srcExecStatus, &srcBlocked); err != nil {
		t.Fatalf("query source job: %v", err)
	}
	if srcExecStatus != "succeeded" {
		t.Fatalf("expected source job execution_status=succeeded, got %s", srcExecStatus)
	}
	if srcBlocked {
		t.Fatalf("expected source job blocks_database=false, got true")
	}

	// 2. Original job outcome is historical (partial, because it only committed batch 0)
	var origJobStatus string
	if err := platformDB.QueryRowContext(ctx, "SELECT status FROM app.jobs WHERE id = $1", fix1.claim.JobID).Scan(&origJobStatus); err != nil {
		t.Fatalf("query orig job status: %v", err)
	}
	if origJobStatus != "partial" {
		t.Fatalf("expected original job status=partial, got %s", origJobStatus)
	}

	// 3. Original attempt outcome is historical (partial)
	var origAttStatus string
	if err := platformDB.QueryRowContext(ctx, "SELECT status FROM app.job_attempts WHERE id = $1", fix1.claim.AttemptID).Scan(&origAttStatus); err != nil {
		t.Fatalf("query orig attempt status: %v", err)
	}
	if origAttStatus != "partial" {
		t.Fatalf("expected original attempt status=partial, got %s", origAttStatus)
	}

	// 4. Without replay: checkpoints on target remain exactly 2 (batch 0 and batch 1)
	var chkCount int
	if err := e.prep.QueryRowContext(ctx, `
		SELECT count(*) FROM wbd_auth.batch_checkpoints WHERE execution_id = $1::uuid
	`, fix1.executionID).Scan(&chkCount); err != nil || chkCount != 2 {
		t.Fatalf("expected exactly 2 checkpoints on target without replay, got %d (err=%v)", chkCount, err)
	}

	// 5. All items are processed
	var pCount int
	if err := e.exec.QueryRowContext(ctx, "SELECT count(*) FROM managed.batch_items WHERE processed = true").Scan(&pCount); err != nil || pCount != 4 {
		t.Fatalf("expected 4 processed items, got %d (err=%v)", pCount, err)
	}

	// 6. Final RecordCommitTx was called exactly once
	if recordCommitFinalCount != 1 {
		t.Fatalf("expected RecordCommitTx with FinalStep to be called once, got %d", recordCommitFinalCount)
	}
}

func TestExecuteBatchJob_Regression_OtherwiseMatchingProof_WrongOrdinalOrParams_RetainsBlock(t *testing.T) {
	t.Run("WrongStatementOrdinal", func(t *testing.T) {
		e, platformDB, ownerDB := setupBatchTestEnv(t)
		setupBatchItemsFixture(t, e)

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		if _, err := e.exec.ExecContext(ctx, "INSERT INTO managed.batch_items (id, processed) VALUES (1, false), (2, false), (3, false), (4, false)"); err != nil {
			t.Fatalf("insert items: %v", err)
		}

		fix1 := createBatchFixture(t, platformDB, ownerDB, e.target, 2, executioncontract.Apply, nil)
		cleanupBatchScope(t, e, fix1.executionID)

		acquire := makeBatchAcquire(t, e.exec, e.prep)
		svc := New(platformDB)

		// Commit batch 0 target checkpoint
		deps1 := ExecutionDeps{
			Gate:    batchFakeGate(fix1.snapshot),
			Acquire: acquire,
			Timeout: 10 * time.Second,
			afterBatchCommit: func(ctx context.Context, batchIndex int) error {
				if batchIndex == 0 {
					return errors.New("simulated crash after batch 0 commit")
				}
				return nil
			},
		}
		_, _ = svc.ExecuteBatchJob(ctx, fix1.claim, deps1)

		// Tamper with originating statement evidence: change ordinal from 1 to 2
		if _, err := ownerDB.ExecContext(ctx, `
			UPDATE app.job_statement_evidence
			SET ordinal = 2
			WHERE attempt_id = $1::uuid AND batch_index = 0
		`, fix1.claim.AttemptID); err != nil {
			t.Fatalf("tamper statement evidence ordinal: %v", err)
		}

		fixResume := createBatchFixture(t, platformDB, ownerDB, e.target, 2, executioncontract.Resume, &fix1)
		cleanupBatchScope(t, e, fixResume.executionID)

		depsResume := ExecutionDeps{
			Gate:    batchFakeGate(fixResume.snapshot),
			Acquire: acquire,
			Timeout: 10 * time.Second,
		}

		outcome, err := svc.ExecuteBatchJob(ctx, fixResume.claim, depsResume)
		if outcome == nil || outcome.Status != executioncontract.Failed {
			t.Fatalf("expected Failed outcome due to wrong statement ordinal, got outcome=%+v err=%v", outcome, err)
		}
		var cErr executioncontract.ContractError
		if !errors.As(outcome.Error, &cErr) || cErr.Code != "TARGET_BLOCKED" {
			t.Fatalf("expected TARGET_BLOCKED error code, got %v", outcome.Error)
		}

		var blocked bool
		if err := platformDB.QueryRowContext(ctx, "SELECT blocks_database FROM app.jobs WHERE id = $1", fixResume.claim.JobID).Scan(&blocked); err != nil || !blocked {
			t.Fatalf("resume job must retain blocks_database=true, got %t (err=%v)", blocked, err)
		}
	})

	t.Run("WrongStatementParameters", func(t *testing.T) {
		e, platformDB, ownerDB := setupBatchTestEnv(t)
		setupBatchItemsFixture(t, e)

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		if _, err := e.exec.ExecContext(ctx, "INSERT INTO managed.batch_items (id, processed) VALUES (1, false), (2, false), (3, false), (4, false)"); err != nil {
			t.Fatalf("insert items: %v", err)
		}

		fix1 := createBatchFixture(t, platformDB, ownerDB, e.target, 2, executioncontract.Apply, nil)
		cleanupBatchScope(t, e, fix1.executionID)

		acquire := makeBatchAcquire(t, e.exec, e.prep)
		svc := New(platformDB)

		// Commit batch 0 target checkpoint
		deps1 := ExecutionDeps{
			Gate:    batchFakeGate(fix1.snapshot),
			Acquire: acquire,
			Timeout: 10 * time.Second,
			afterBatchCommit: func(ctx context.Context, batchIndex int) error {
				if batchIndex == 0 {
					return errors.New("simulated crash after batch 0 commit")
				}
				return nil
			},
		}
		_, _ = svc.ExecuteBatchJob(ctx, fix1.claim, deps1)

		// Tamper with originating statement evidence: change parameters to mismatched IDs
		if _, err := ownerDB.ExecContext(ctx, `
			UPDATE app.job_statement_evidence
			SET safe_parameters = '{"ids": [999, 998]}'::jsonb
			WHERE attempt_id = $1::uuid AND batch_index = 0
		`, fix1.claim.AttemptID); err != nil {
			t.Fatalf("tamper statement evidence params: %v", err)
		}

		fixResume := createBatchFixture(t, platformDB, ownerDB, e.target, 2, executioncontract.Resume, &fix1)
		cleanupBatchScope(t, e, fixResume.executionID)

		depsResume := ExecutionDeps{
			Gate:    batchFakeGate(fixResume.snapshot),
			Acquire: acquire,
			Timeout: 10 * time.Second,
		}

		outcome, err := svc.ExecuteBatchJob(ctx, fixResume.claim, depsResume)
		if outcome == nil || outcome.Status != executioncontract.Failed {
			t.Fatalf("expected Failed outcome due to wrong parameters, got outcome=%+v err=%v", outcome, err)
		}
		var cErr executioncontract.ContractError
		if !errors.As(outcome.Error, &cErr) || cErr.Code != "TARGET_BLOCKED" {
			t.Fatalf("expected TARGET_BLOCKED error code, got %v", outcome.Error)
		}

		var blocked bool
		if err := platformDB.QueryRowContext(ctx, "SELECT blocks_database FROM app.jobs WHERE id = $1", fixResume.claim.JobID).Scan(&blocked); err != nil || !blocked {
			t.Fatalf("resume job must retain blocks_database=true, got %t (err=%v)", blocked, err)
		}
	})
}

func TestExecuteBatchJob_Regression_UnprovenSecondAuthSameIndex_RetainsBlock(t *testing.T) {
	e, platformDB, ownerDB := setupBatchTestEnv(t)
	setupBatchItemsFixture(t, e)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := e.exec.ExecContext(ctx, "INSERT INTO managed.batch_items (id, processed) VALUES (1, false), (2, false), (3, false), (4, false)"); err != nil {
		t.Fatalf("insert items: %v", err)
	}

	fix1 := createBatchFixture(t, platformDB, ownerDB, e.target, 2, executioncontract.Apply, nil)
	cleanupBatchScope(t, e, fix1.executionID)

	acquire := makeBatchAcquire(t, e.exec, e.prep)
	svc := New(platformDB)

	// Commit batch 0 target checkpoint
	deps1 := ExecutionDeps{
		Gate:    batchFakeGate(fix1.snapshot),
		Acquire: acquire,
		Timeout: 10 * time.Second,
		afterBatchCommit: func(ctx context.Context, batchIndex int) error {
			if batchIndex == 0 {
				return errors.New("simulated crash after batch 0 commit")
			}
			return nil
		},
	}
	_, _ = svc.ExecuteBatchJob(ctx, fix1.claim, deps1)

	// Open an in-progress transaction on target to obtain an uncommitted live XID.
	txTarget, err := e.exec.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin target tx: %v", err)
	}
	defer txTarget.Rollback()

	var inProgressXID string
	if err := txTarget.QueryRowContext(ctx, "SELECT pg_current_xact_id()::text").Scan(&inProgressXID); err != nil {
		t.Fatalf("query in progress xid: %v", err)
	}

	// Create a second attempt record under fix1.claim.JobID
	var secondAttemptID string
	if err := ownerDB.QueryRowContext(ctx, `
		INSERT INTO app.job_attempts (job_id, execution_id, database_id, migration_id, revision, actor_id, attempt_no, status, started_at)
		VALUES ($1, $2, $3, $4, $5, $6, 2, 'running', clock_timestamp())
		RETURNING id::text
	`, fix1.claim.JobID, fix1.claim.ExecutionID, fix1.claim.DatabaseID, fix1.claim.MigrationID, fix1.claim.Revision, fix1.claim.ActorID).Scan(&secondAttemptID); err != nil {
		t.Fatalf("insert second attempt: %v", err)
	}

	// Insert a second authorization at the same batch index (0) with the in-progress XID
	planJSON, _ := json.Marshal(executioncontract.ExecutionPlan{
		Snapshot: fix1.snapshot,
		Version:  "1",
	})
	hash := sha256.Sum256([]byte("UPDATE managed.batch_items SET processed = true WHERE id = ANY($1::bigint[]);"))
	if _, err := ownerDB.ExecContext(ctx, `
		INSERT INTO app.job_batch_step_authorizations
		    (job_id, attempt_id, actor_id, job_execution_id, scope_execution_id,
		     physical_target_id, direction, batch_index, ordinal, chunk_ids, plan, sql_text, sql_sha256,
		     target_xid, target_version)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 0, 1, '[1, 2]'::jsonb, $8::jsonb, $9, $10, $11, 1)
	`, fix1.claim.JobID, secondAttemptID, fix1.claim.ActorID, fix1.claim.ExecutionID, fix1.executionID,
		fix1.claim.TargetID, string(fix1.claim.Direction), string(planJSON),
		"UPDATE managed.batch_items SET processed = true WHERE id = ANY($1::bigint[]);", hash[:],
		inProgressXID); err != nil {
		t.Fatalf("insert unproven second authorization at index 0: %v", err)
	}

	// Also insert statement evidence for this second authorization
	if _, err := ownerDB.ExecContext(ctx, `
		INSERT INTO app.job_statement_evidence
		    (attempt_id, ordinal, batch_index, sql_text, safe_parameters, outcome, executed_at)
		VALUES ($1::uuid, 1, 0, $2, '{"ids": [1, 2]}'::jsonb, 'unknown', clock_timestamp())
	`, secondAttemptID, "UPDATE managed.batch_items SET processed = true WHERE id = ANY($1::bigint[]);"); err != nil {
		t.Fatalf("insert statement evidence for second auth: %v", err)
	}

	// Expire the lease on fix1 so the resume job can be claimed
	if _, err := ownerDB.ExecContext(ctx, `
		UPDATE app.jobs
		SET lease_expires_at = clock_timestamp() - interval '10 seconds'
		WHERE id = $1
	`, fix1.claim.JobID); err != nil {
		t.Fatalf("expire job lease: %v", err)
	}

	fixResume := createBatchFixture(t, platformDB, ownerDB, e.target, 2, executioncontract.Resume, &fix1)
	cleanupBatchScope(t, e, fixResume.executionID)

	depsResume := ExecutionDeps{
		Gate:    batchFakeGate(fixResume.snapshot),
		Acquire: acquire,
		Timeout: 10 * time.Second,
	}

	outcome, err := svc.ExecuteBatchJob(ctx, fixResume.claim, depsResume)
	if outcome == nil || outcome.Status != executioncontract.Failed {
		t.Fatalf("expected Failed outcome due to unproven second auth with in-progress XID, got outcome=%+v err=%v", outcome, err)
	}
	var cErr executioncontract.ContractError
	if !errors.As(outcome.Error, &cErr) || cErr.Code != "TARGET_BLOCKED" {
		t.Fatalf("expected TARGET_BLOCKED error code, got %v", outcome.Error)
	}

	// Must retain blocker
	var resumeBlocked bool
	if err := platformDB.QueryRowContext(ctx, "SELECT blocks_database FROM app.jobs WHERE id = $1", fixResume.claim.JobID).Scan(&resumeBlocked); err != nil || !resumeBlocked {
		t.Fatalf("resume job must retain blocks_database=true, got %t (err=%v)", resumeBlocked, err)
	}
}

func TestExecuteBatchJob_Regression_RecoveryScan_MultiPageCancellation(t *testing.T) {
	e, platformDB, ownerDB := setupBatchTestEnv(t)
	setupBatchItemsFixture(t, e)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := e.exec.ExecContext(ctx, "INSERT INTO managed.batch_items (id, processed) VALUES (1, false), (2, false), (3, false), (4, false)"); err != nil {
		t.Fatalf("insert items: %v", err)
	}

	fix1 := createBatchFixture(t, platformDB, ownerDB, e.target, 2, executioncontract.Apply, nil)
	cleanupBatchScope(t, e, fix1.executionID)

	acquire := makeBatchAcquire(t, e.exec, e.prep)
	svc := New(platformDB)

	// Commit batch 0 target checkpoint
	deps1 := ExecutionDeps{
		Gate:    batchFakeGate(fix1.snapshot),
		Acquire: acquire,
		Timeout: 10 * time.Second,
		afterBatchCommit: func(ctx context.Context, batchIndex int) error {
			if batchIndex == 0 {
				return errors.New("simulated crash after batch 0 commit")
			}
			return nil
		},
	}
	_, _ = svc.ExecuteBatchJob(ctx, fix1.claim, deps1)

	// Set small page limit = 1 to force multi-page recovery scan across checkpoints
	oldLimit := chkPageLimit
	oldRecLimit := recoveryPageLimit
	chkPageLimit = 1
	recoveryPageLimit = 1
	t.Cleanup(func() {
		chkPageLimit = oldLimit
		recoveryPageLimit = oldRecLimit
	})

	fixResume := createBatchFixture(t, platformDB, ownerDB, e.target, 2, executioncontract.Resume, &fix1)
	cleanupBatchScope(t, e, fixResume.executionID)

	runCtx, runCancel := context.WithCancel(ctx)
	depsResume := ExecutionDeps{
		Gate:    batchFakeGate(fixResume.snapshot),
		Acquire: acquire,
		Timeout: 10 * time.Second,
		beforeRecoveryScanStep: func(c context.Context, batchIndex int) error {
			// Cancel when scanning batch 0
			runCancel()
			return nil
		},
	}

	outcome, err := svc.ExecuteBatchJob(runCtx, fixResume.claim, depsResume)
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled error during multi-page recovery scan, got outcome=%+v err=%v", outcome, err)
	}

	// Verify no progression: batch 1 items remain unprocessed
	var p3, p4 bool
	if err := e.exec.QueryRowContext(ctx, "SELECT (SELECT processed FROM managed.batch_items WHERE id = 3), (SELECT processed FROM managed.batch_items WHERE id = 4)").Scan(&p3, &p4); err != nil {
		t.Fatalf("query items: %v", err)
	}
	if p3 || p4 {
		t.Fatalf("batch 1 items must remain false after canceled recovery: p3=%t, p4=%t", p3, p4)
	}
}

func TestExecuteBatchJob_Regression_RecoveryClosure_MultiPageBoundedAndCancellation(t *testing.T) {
	e, platformDB, ownerDB := setupBatchTestEnv(t)
	setupBatchItemsFixture(t, e)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if _, err := e.exec.ExecContext(ctx, "INSERT INTO managed.batch_items (id, processed) VALUES (1, false), (2, false), (3, false), (4, false)"); err != nil {
		t.Fatalf("insert items: %v", err)
	}

	// 1. Worker 1 claims with short lease (100ms), batchSize = 2
	fix1 := createBatchFixtureWithDuration(t, platformDB, ownerDB, e.target, 2, executioncontract.Apply, nil, 100*time.Millisecond)
	cleanupBatchScope(t, e, fix1.executionID)

	acquire := makeBatchAcquire(t, e.exec, e.prep)

	ident := BatchScopeIdentity{
		ExecutionID: fix1.executionID,
		TargetID:    fix1.targetID,
		DatabaseID:  fix1.dbID,
		ActorID:     fix1.actorID,
	}
	svcScope := New(platformDB)
	if _, err := svcScope.FreezeBatchScope(ctx, e.prep, ident); err != nil {
		t.Fatalf("freeze batch scope: %v", err)
	}

	// Set small page limit = 1 to enforce bounded single-item pages
	oldRecLimit := recoveryPageLimit
	recoveryPageLimit = 1
	t.Cleanup(func() {
		recoveryPageLimit = oldRecLimit
	})

	// Generate 2 positively aborted target transactions on target PostgreSQL
	txA, err := e.exec.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin target txA: %v", err)
	}
	var xidA string
	if err := txA.QueryRowContext(ctx, "SELECT pg_current_xact_id()::text").Scan(&xidA); err != nil {
		t.Fatalf("query xidA: %v", err)
	}
	if err := txA.Rollback(); err != nil {
		t.Fatalf("rollback txA: %v", err)
	}

	txB, err := e.exec.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin target txB: %v", err)
	}
	var xidB string
	if err := txB.QueryRowContext(ctx, "SELECT pg_current_xact_id()::text").Scan(&xidB); err != nil {
		t.Fatalf("query xidB: %v", err)
	}
	if err := txB.Rollback(); err != nil {
		t.Fatalf("rollback txB: %v", err)
	}

	// Verify both xids are aborted on PostgreSQL
	var statA, statB string
	if err := e.prep.QueryRowContext(ctx, "SELECT pg_catalog.pg_xact_status($1::xid8)", xidA).Scan(&statA); err != nil || statA != "aborted" {
		t.Fatalf("expected xidA aborted, got status=%s err=%v", statA, err)
	}
	if err := e.prep.QueryRowContext(ctx, "SELECT pg_catalog.pg_xact_status($1::xid8)", xidB).Scan(&statB); err != nil || statB != "aborted" {
		t.Fatalf("expected xidB aborted, got status=%s err=%v", statB, err)
	}

	// Set job to blocked state so discovery finds it for recovery
	if _, err := platformDB.ExecContext(ctx, "UPDATE app.jobs SET blocks_database = true WHERE id = $1", fix1.claim.JobID); err != nil {
		t.Fatalf("set blocks_database: %v", err)
	}

	// Insert 2 unproven authorizations referencing xidA and xidB under attempt 1
	planJSON, _ := json.Marshal(executioncontract.ExecutionPlan{
		Snapshot: fix1.snapshot,
		Version:  "1",
	})
	hash := sha256.Sum256([]byte("UPDATE managed.batch_items SET processed = true WHERE id = ANY($1::bigint[]);"))

	// Authorization 1 (attempt 1, ordinal 1)
	if _, err := ownerDB.ExecContext(ctx, `
		INSERT INTO app.job_batch_step_authorizations
		    (job_id, attempt_id, actor_id, job_execution_id, scope_execution_id,
		     physical_target_id, direction, batch_index, ordinal, chunk_ids, plan, sql_text, sql_sha256,
		     target_xid, target_version)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 0, 1, '[1, 2]'::jsonb, $8::jsonb, $9, $10, $11, 1)
	`, fix1.claim.JobID, fix1.claim.AttemptID, fix1.claim.ActorID, fix1.claim.ExecutionID, fix1.executionID,
		fix1.claim.TargetID, string(fix1.claim.Direction), string(planJSON),
		"UPDATE managed.batch_items SET processed = true WHERE id = ANY($1::bigint[]);", hash[:],
		xidA); err != nil {
		t.Fatalf("insert first auth: %v", err)
	}
	if _, err := ownerDB.ExecContext(ctx, `
		INSERT INTO app.job_statement_evidence
		    (attempt_id, ordinal, batch_index, sql_text, safe_parameters, outcome, executed_at)
		VALUES ($1::uuid, 1, 0, $2, '{"ids": [1, 2]}'::jsonb, 'unknown', clock_timestamp())
	`, fix1.claim.AttemptID, "UPDATE managed.batch_items SET processed = true WHERE id = ANY($1::bigint[]);"); err != nil {
		t.Fatalf("insert first statement evidence: %v", err)
	}

	// Authorization 2 (attempt 1, ordinal 2, batch_index 1)
	if _, err := ownerDB.ExecContext(ctx, `
		INSERT INTO app.job_batch_step_authorizations
		    (job_id, attempt_id, actor_id, job_execution_id, scope_execution_id,
		     physical_target_id, direction, batch_index, ordinal, chunk_ids, plan, sql_text, sql_sha256,
		     target_xid, target_version)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 1, 2, '[3, 4]'::jsonb, $8::jsonb, $9, $10, $11, 1)
	`, fix1.claim.JobID, fix1.claim.AttemptID, fix1.claim.ActorID, fix1.claim.ExecutionID, fix1.executionID,
		fix1.claim.TargetID, string(fix1.claim.Direction), string(planJSON),
		"UPDATE managed.batch_items SET processed = true WHERE id = ANY($1::bigint[]);", hash[:],
		xidB); err != nil {
		t.Fatalf("insert second auth: %v", err)
	}
	if _, err := ownerDB.ExecContext(ctx, `
		INSERT INTO app.job_statement_evidence
		    (attempt_id, ordinal, batch_index, sql_text, safe_parameters, outcome, executed_at)
		VALUES ($1::uuid, 2, 1, $2, '{"ids": [3, 4]}'::jsonb, 'unknown', clock_timestamp())
	`, fix1.claim.AttemptID, "UPDATE managed.batch_items SET processed = true WHERE id = ANY($1::bigint[]);"); err != nil {
		t.Fatalf("insert second statement evidence: %v", err)
	}
	if _, err := platformDB.ExecContext(ctx, "UPDATE app.jobs SET lease_expires_at = clock_timestamp() - interval '5 seconds' WHERE id = $1", fix1.claim.JobID); err != nil {
		t.Fatalf("expire job: %v", err)
	}
	// Simulate restart recovery
	restartSvc := New(platformDB)

	// First pass: verify bounded pages and test cancellation during the second page
	var (
		pagesSeen []int
		maxPageSz int
	)
	runCtx, runCancel := context.WithCancel(ctx)
	depsCancel := ExecutionDeps{
		Gate:    batchFakeGate(fix1.snapshot),
		Acquire: acquire,
		Timeout: 10 * time.Second,
		beforeRecoveryClosurePage: func(c context.Context, kind string, pageIndex int, pageSize int) error {
			if pageSize > maxPageSz {
				maxPageSz = pageSize
			}
			if pageSize > 1 {
				t.Fatalf("expected bounded page size <= 1, got pageSize=%d", pageSize)
			}
			if kind == "authorizations" {
				pagesSeen = append(pagesSeen, pageIndex)
				if pageIndex == 1 {
					// Cancel on the second page of authorization closure
					runCancel()
				}
			}
			return nil
		},
	}

	outcome, err := restartSvc.ReconcileBatchJobByID(runCtx, fix1.claim.JobID, depsCancel)
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled error during multi-page closure, got outcome=%+v err=%v", outcome, err)
	}
	if len(pagesSeen) < 2 {
		t.Fatalf("expected multiple pages visited before cancel, got pagesSeen=%v", pagesSeen)
	}
	if maxPageSz != 1 {
		t.Fatalf("expected at most 1 item processed per page, got maxPageSz=%d", maxPageSz)
	}

	// Verify coherent production state after cancellation:
	// Page 0 was processed in memory during the tx, but because tx rolled back,
	// statement evidence for both attempt 1 and attempt 2 MUST remain 'unknown'!
	var ev1Outcome, ev2Outcome string
	if err := platformDB.QueryRowContext(ctx, "SELECT outcome FROM app.job_statement_evidence WHERE attempt_id = $1 AND ordinal = 1", fix1.claim.AttemptID).Scan(&ev1Outcome); err != nil {
		t.Fatalf("query evidence 1: %v", err)
	}
	if err := platformDB.QueryRowContext(ctx, "SELECT outcome FROM app.job_statement_evidence WHERE attempt_id = $1 AND ordinal = 2", fix1.claim.AttemptID).Scan(&ev2Outcome); err != nil {
		t.Fatalf("query evidence 2: %v", err)
	}
	if ev1Outcome != "unknown" || ev2Outcome != "unknown" {
		t.Fatalf("evidence outcomes must remain 'unknown' after rollback: ev1=%s ev2=%s", ev1Outcome, ev2Outcome)
	}

	// Job status must remain running and blocked
	var jobStatus string
	var jobBlocked bool
	if err := platformDB.QueryRowContext(ctx, "SELECT status, blocks_database FROM app.jobs WHERE id = $1", fix1.claim.JobID).Scan(&jobStatus, &jobBlocked); err != nil {
		t.Fatalf("query job after cancel: %v", err)
	}
	if jobStatus != "running" || !jobBlocked {
		t.Fatalf("job must remain running and blocked after cancel: status=%s blocked=%t", jobStatus, jobBlocked)
	}

	// Second pass: full recovery without cancellation succeeds across all bounded pages
	var finalPages []int
	depsClean := ExecutionDeps{
		Gate:    batchFakeGate(fix1.snapshot),
		Acquire: acquire,
		Timeout: 10 * time.Second,
		beforeRecoveryClosurePage: func(c context.Context, kind string, pageIndex int, pageSize int) error {
			if pageSize > 1 {
				t.Fatalf("expected bounded page size <= 1, got pageSize=%d", pageSize)
			}
			if kind == "authorizations" {
				finalPages = append(finalPages, pageIndex)
			}
			return nil
		},
	}

	cleanOutcome, err := restartSvc.ReconcileBatchJobByID(ctx, fix1.claim.JobID, depsClean)
	if err != nil || cleanOutcome == nil {
		t.Fatalf("expected successful reconcile, got outcome=%+v err=%v", cleanOutcome, err)
	}
	if cleanOutcome.Status != executioncontract.Failed || !cleanOutcome.Reconciled {
		t.Fatalf("expected Failed reconciled outcome, got: %+v", cleanOutcome)
	}
	if len(finalPages) < 2 {
		t.Fatalf("expected multiple authorization pages in clean run, got finalPages=%v", finalPages)
	}

	// Verify all statement evidences are now 'failed'
	var failedEvidenceCount int
	if err := platformDB.QueryRowContext(ctx, "SELECT count(*) FROM app.job_statement_evidence WHERE attempt_id = $1 AND outcome = 'failed'", fix1.claim.AttemptID).Scan(&failedEvidenceCount); err != nil {
		t.Fatalf("query failed evidence count: %v", err)
	}
	if failedEvidenceCount != 2 {
		t.Fatalf("expected 2 failed statement evidences, got %d", failedEvidenceCount)
	}

	// Verify job and attempts are cleanly closed
	var finalJobStatus string
	var finalJobBlocked bool
	if err := platformDB.QueryRowContext(ctx, "SELECT status, blocks_database FROM app.jobs WHERE id = $1", fix1.claim.JobID).Scan(&finalJobStatus, &finalJobBlocked); err != nil {
		t.Fatalf("query final job: %v", err)
	}
	if finalJobStatus != "failed" || finalJobBlocked {
		t.Fatalf("job must be closed clean (status=failed, blocks=false), got status=%s blocked=%t", finalJobStatus, finalJobBlocked)
	}
}

func TestExecuteBatchJob_RestartRecovery_AutonomousDiscoveryAndReceiptRecovery(t *testing.T) {
	t.Run("ProvedAbortWithoutCheckpoint_DiscoveredAndClosedClean", func(t *testing.T) {
		e, platformDB, ownerDB := setupBatchTestEnv(t)
		setupBatchItemsFixture(t, e)

		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()

		if _, err := e.exec.ExecContext(ctx, "INSERT INTO managed.batch_items (id, processed) VALUES (1, false), (2, false), (3, false), (4, false)"); err != nil {
			t.Fatalf("insert items: %v", err)
		}

		// 1. Worker 1 claims with short lease (100ms), batchSize = 2
		fix1 := createBatchFixtureWithDuration(t, platformDB, ownerDB, e.target, 2, executioncontract.Apply, nil, 600*time.Millisecond)
		cleanupBatchScope(t, e, fix1.executionID)

		acquire := makeBatchAcquire(t, e.exec, e.prep)
		svc := New(platformDB)

		var crashHookRan bool
		deps1 := ExecutionDeps{
			Gate:    batchFakeGate(fix1.snapshot),
			Acquire: acquire,
			Timeout: 10 * time.Second,
			crashAfterBatchAuthorize: func(ctx context.Context, batchIndex int) error {
				crashHookRan = true
				return errors.New("simulated hard crash after authorization before target commit")
			},
		}

		outcome1, err := svc.ExecuteBatchJob(ctx, fix1.claim, deps1)
		if err == nil {
			t.Fatalf("expected crash error, got outcome=%+v", outcome1)
		}
		if !crashHookRan {
			t.Fatal("expected crashAfterBatchAuthorize hook to execute")
		}

		// 2. Natural clock lease expiry (no synthetic/manual state modification)
		time.Sleep(650 * time.Millisecond)

		// 3. Process restart simulation: create fresh Service instance
		restartSvc := New(platformDB)

		// 4. Autonomous discovery via bounded seam
		unresolvedIDs, err := restartSvc.ListUnresolvedBatchJobIDs(ctx, 10)
		if err != nil {
			t.Fatalf("ListUnresolvedBatchJobIDs: %v", err)
		}
		var found bool
		for _, id := range unresolvedIDs {
			if id == fix1.claim.JobID {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("expected job %s in unresolved list: %v", fix1.claim.JobID, unresolvedIDs)
		}

		// 5. Gate revalidation and SQL dispatch hooks trap: must NOT be called
		recoveryGate := batchFakeGate(fix1.snapshot)
		recoveryGate.RevalidateFunc = func(context.Context, executioncontract.RevalidationRequest) (executioncontract.ExecutionPlan, error) {
			t.Fatal("gate revalidate must not be called during receipt recovery")
			return executioncontract.ExecutionPlan{}, nil
		}
		recoveryDeps := ExecutionDeps{
			Gate:    recoveryGate,
			Acquire: acquire,
			Timeout: 10 * time.Second,
			beforeBatchDispatch: func(context.Context, int) error {
				t.Fatal("SQL dispatch hook beforeBatchDispatch must not be called during recovery")
				return nil
			},
			crashAfterBatchAuthorize: func(context.Context, int) error {
				t.Fatal("SQL hook crashAfterBatchAuthorize must not be called during recovery")
				return nil
			},
		}

		// 6. Direct recovery entrypoint by durable job ID without incoming resume job
		recoveryOutcome, err := restartSvc.ReconcileBatchJobByID(ctx, fix1.claim.JobID, recoveryDeps)
		if err != nil || recoveryOutcome == nil {
			t.Fatalf("ReconcileBatchJobByID: outcome=%+v err=%v", recoveryOutcome, err)
		}
		if recoveryOutcome.Status != executioncontract.Failed || !recoveryOutcome.Reconciled {
			t.Fatalf("expected proved abort to close as failed reconciled, got: %+v", recoveryOutcome)
		}

		// 7. Verify clean close: job failed and unblocked
		var jobStatus string
		var jobBlocked bool
		if err := platformDB.QueryRowContext(ctx, "SELECT status, blocks_database FROM app.jobs WHERE id = $1", fix1.claim.JobID).Scan(&jobStatus, &jobBlocked); err != nil {
			t.Fatalf("query job: %v", err)
		}
		if jobStatus != "failed" || jobBlocked {
			t.Fatalf("expected job status='failed' and blocks_database=false, got status=%s blocked=%t", jobStatus, jobBlocked)
		}

		var attStatus string
		if err := platformDB.QueryRowContext(ctx, "SELECT status FROM app.job_attempts WHERE id = $1", fix1.claim.AttemptID).Scan(&attStatus); err != nil {
			t.Fatalf("query attempt: %v", err)
		}
		if attStatus != "failed" {
			t.Fatalf("expected attempt status='failed', got: %s", attStatus)
		}

		var seOutcome string
		if err := platformDB.QueryRowContext(ctx, "SELECT outcome FROM app.job_statement_evidence WHERE attempt_id = $1 AND ordinal = 1", fix1.claim.AttemptID).Scan(&seOutcome); err != nil {
			t.Fatalf("query statement evidence: %v", err)
		}
		if seOutcome != "failed" {
			t.Fatalf("expected statement evidence outcome='failed', got: %s", seOutcome)
		}

		// 8. Repeated recovery is idempotent
		repeatedOutcome, err := restartSvc.ReconcileBatchJobByID(ctx, fix1.claim.JobID, recoveryDeps)
		if err != nil || repeatedOutcome == nil {
			t.Fatalf("repeated recovery: outcome=%+v err=%v", repeatedOutcome, err)
		}
		if repeatedOutcome.Status != executioncontract.Failed || !repeatedOutcome.Reconciled {
			t.Fatalf("repeated recovery must be idempotent failed: %+v", repeatedOutcome)
		}

		// 9. Job is no longer in unresolved list
		afterIDs, err := restartSvc.ListUnresolvedBatchJobIDs(ctx, 10)
		if err != nil {
			t.Fatalf("ListUnresolvedBatchJobIDs after recovery: %v", err)
		}
		for _, id := range afterIDs {
			if id == fix1.claim.JobID {
				t.Fatalf("job %s should no longer be unresolved after clean closure", fix1.claim.JobID)
			}
		}
	})

	t.Run("CommittedCheckpoint_PartialExecution_DiscoveredAndRetainsBlock", func(t *testing.T) {
		e, platformDB, ownerDB := setupBatchTestEnv(t)
		setupBatchItemsFixture(t, e)

		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()

		if _, err := e.exec.ExecContext(ctx, "INSERT INTO managed.batch_items (id, processed) VALUES (1, false), (2, false), (3, false), (4, false)"); err != nil {
			t.Fatalf("insert items: %v", err)
		}

		// 1. Worker claims with short lease (100ms), batchSize = 2 (2 batches total)
		fix := createBatchFixtureWithDuration(t, platformDB, ownerDB, e.target, 2, executioncontract.Apply, nil, 100*time.Millisecond)
		cleanupBatchScope(t, e, fix.executionID)

		acquire := makeBatchAcquire(t, e.exec, e.prep)
		svc := New(platformDB)

		// Worker executes batch 0, crashes upon authorising batch 1
		deps := ExecutionDeps{
			Gate:    batchFakeGate(fix.snapshot),
			Acquire: acquire,
			Timeout: 10 * time.Second,
			crashAfterBatchAuthorize: func(ctx context.Context, batchIndex int) error {
				if batchIndex == 1 {
					return errors.New("simulated hard crash during batch 1 authorization")
				}
				return nil
			},
		}

		outcome, err := svc.ExecuteBatchJob(ctx, fix.claim, deps)
		if err == nil {
			t.Fatalf("expected crash during batch 1, got outcome=%+v", outcome)
		}

		// 2. Natural lease expiry
		if _, err := platformDB.ExecContext(ctx, "UPDATE app.jobs SET lease_expires_at = clock_timestamp() - interval '5 seconds' WHERE id = $1", fix.claim.JobID); err != nil {
			t.Fatalf("expire job: %v", err)
		}
		// 3. Process restart simulation
		restartSvc := New(platformDB)

		// 4. Discovery via bounded seam
		ids, err := restartSvc.ListUnresolvedBatchJobIDs(ctx, 10)
		if err != nil {
			t.Fatalf("ListUnresolvedBatchJobIDs: %v", err)
		}
		var found bool
		for _, id := range ids {
			if id == fix.claim.JobID {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("expected partial job %s in unresolved list: %v", fix.claim.JobID, ids)
		}

		// 5. Gate revalidation and SQL dispatch hooks trap
		recoveryGate := batchFakeGate(fix.snapshot)
		recoveryGate.RevalidateFunc = func(context.Context, executioncontract.RevalidationRequest) (executioncontract.ExecutionPlan, error) {
			t.Fatal("gate revalidate must not be called during receipt recovery")
			return executioncontract.ExecutionPlan{}, nil
		}
		recoveryDeps := ExecutionDeps{
			Gate:    recoveryGate,
			Acquire: acquire,
			Timeout: 10 * time.Second,
			beforeBatchDispatch: func(context.Context, int) error {
				t.Fatal("SQL dispatch hook must not be called during recovery")
				return nil
			},
		}

		// 6. Direct recovery entrypoint: partial evidence remains partial and blocked
		recOutcome, err := restartSvc.ReconcileBatchJobByID(ctx, fix.claim.JobID, recoveryDeps)
		if err != nil || recOutcome == nil {
			t.Fatalf("ReconcileBatchJobByID: outcome=%+v err=%v", recOutcome, err)
		}
		if recOutcome.Status != executioncontract.Partial || !recOutcome.Reconciled {
			t.Fatalf("expected partial status and reconciled=true, got: %+v", recOutcome)
		}

		// 7. Verify partial and blocked state
		var jobStatus string
		var jobBlocked bool
		if err := platformDB.QueryRowContext(ctx, "SELECT status, blocks_database FROM app.jobs WHERE id = $1", fix.claim.JobID).Scan(&jobStatus, &jobBlocked); err != nil {
			t.Fatalf("query job: %v", err)
		}
		if jobStatus != "partial" || !jobBlocked {
			t.Fatalf("expected job status='partial' and blocks_database=true, got status=%s blocked=%t", jobStatus, jobBlocked)
		}

		var attStatus string
		if err := platformDB.QueryRowContext(ctx, "SELECT status FROM app.job_attempts WHERE id = $1", fix.claim.AttemptID).Scan(&attStatus); err != nil {
			t.Fatalf("query attempt: %v", err)
		}
		if attStatus != "partial" {
			t.Fatalf("expected attempt status='partial', got: %s", attStatus)
		}

		// 8. Repeated recovery is idempotent with proper block
		repeatedOutcome, err := restartSvc.ReconcileBatchJobByID(ctx, fix.claim.JobID, recoveryDeps)
		if err != nil || repeatedOutcome == nil {
			t.Fatalf("repeated recovery: outcome=%+v err=%v", repeatedOutcome, err)
		}
		if repeatedOutcome.Status != executioncontract.Partial || !repeatedOutcome.Reconciled {
			t.Fatalf("repeated recovery must be idempotent partial: %+v", repeatedOutcome)
		}

		var jobStatusAfter string
		var jobBlockedAfter bool
		if err := platformDB.QueryRowContext(ctx, "SELECT status, blocks_database FROM app.jobs WHERE id = $1", fix.claim.JobID).Scan(&jobStatusAfter, &jobBlockedAfter); err != nil {
			t.Fatalf("query job after repeated: %v", err)
		}
		if jobStatusAfter != "partial" || !jobBlockedAfter {
			t.Fatalf("job must remain partial and blocked after repeated recovery: status=%s blocked=%t", jobStatusAfter, jobBlockedAfter)
		}
	})

	t.Run("CommittedCheckpoint_FullyComplete_DiscoveredAndPublishesSuccess", func(t *testing.T) {
		e, platformDB, ownerDB := setupBatchTestEnv(t)
		setupBatchItemsFixture(t, e)

		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()

		if _, err := e.exec.ExecContext(ctx, "INSERT INTO managed.batch_items (id, processed) VALUES (1, false), (2, false), (3, false), (4, false)"); err != nil {
			t.Fatalf("insert items: %v", err)
		}

		// 1. Worker claims with 1s lease, batchSize = 2 (2 batches total)
		fix := createBatchFixtureWithDuration(t, platformDB, ownerDB, e.target, 2, executioncontract.Apply, nil, 1*time.Second)
		cleanupBatchScope(t, e, fix.executionID)

		acquire := makeBatchAcquire(t, e.exec, e.prep)
		svc := New(platformDB)

		// Crash right after final target commit before platform transaction
		deps := ExecutionDeps{
			Gate:    batchFakeGate(fix.snapshot),
			Acquire: acquire,
			Timeout: 10 * time.Second,
			crashAfterBatchCommit: func(ctx context.Context, batchIndex int) error {
				if batchIndex == 1 {
					return errors.New("simulated crash after final target commit")
				}
				return nil
			},
		}

		outcome, err := svc.ExecuteBatchJob(ctx, fix.claim, deps)
		if err == nil {
			t.Fatalf("expected crash after final target commit, got outcome=%+v", outcome)
		}

		// 2. Natural lease expiry
		time.Sleep(1100 * time.Millisecond)
		// 3. Process restart simulation
		restartSvc := New(platformDB)

		// 4. Discovery via bounded seam
		ids, err := restartSvc.ListUnresolvedBatchJobIDs(ctx, 10)
		if err != nil {
			t.Fatalf("ListUnresolvedBatchJobIDs: %v", err)
		}
		var found bool
		for _, id := range ids {
			if id == fix.claim.JobID {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("expected job %s in unresolved list: %v", fix.claim.JobID, ids)
		}

		// 5. Gate revalidation and SQL dispatch hooks trap
		var gateRecordCalled bool
		recoveryGate := batchFakeGate(fix.snapshot)
		recoveryGate.RevalidateFunc = func(context.Context, executioncontract.RevalidationRequest) (executioncontract.ExecutionPlan, error) {
			t.Fatal("gate revalidate must not be called during receipt recovery")
			return executioncontract.ExecutionPlan{}, nil
		}
		recoveryGate.RecordCommitFunc = func(ctx context.Context, tx *sql.Tx, ev executioncontract.CommitEvidence) error {
			gateRecordCalled = true
			return nil
		}
		recoveryDeps := ExecutionDeps{
			Gate:    recoveryGate,
			Acquire: acquire,
			Timeout: 10 * time.Second,
			beforeBatchDispatch: func(context.Context, int) error {
				t.Fatal("SQL dispatch hook must not be called during recovery")
				return nil
			},
		}

		// 6. Direct recovery entrypoint: fully proven complete evidence publishes final history and aggregate state
		recOutcome, err := restartSvc.ReconcileBatchJobByID(ctx, fix.claim.JobID, recoveryDeps)
		if err != nil || recOutcome == nil {
			t.Fatalf("ReconcileBatchJobByID: outcome=%+v err=%v", recOutcome, err)
		}
		if recOutcome.Status != executioncontract.Succeeded || !recOutcome.Reconciled {
			t.Fatalf("expected succeeded and reconciled=true, got: %+v", recOutcome)
		}
		if !gateRecordCalled {
			t.Fatal("expected gate RecordCommitTx to be called for final commit evidence")
		}

		// 7. Verify succeeded and unblocked state
		var jobStatus string
		var jobBlocked bool
		if err := platformDB.QueryRowContext(ctx, "SELECT status, blocks_database FROM app.jobs WHERE id = $1", fix.claim.JobID).Scan(&jobStatus, &jobBlocked); err != nil {
			t.Fatalf("query job: %v", err)
		}
		if jobStatus != "succeeded" || jobBlocked {
			t.Fatalf("expected job status='succeeded' and blocks_database=false, got status=%s blocked=%t", jobStatus, jobBlocked)
		}

		var attStatus string
		if err := platformDB.QueryRowContext(ctx, "SELECT status FROM app.job_attempts WHERE id = $1", fix.claim.AttemptID).Scan(&attStatus); err != nil {
			t.Fatalf("query attempt: %v", err)
		}
		if attStatus != "succeeded" {
			t.Fatalf("expected attempt status='succeeded', got: %s", attStatus)
		}

		var proofCount int
		if err := platformDB.QueryRowContext(ctx, "SELECT count(*) FROM app.job_batch_commit_proofs WHERE job_id = $1", fix.claim.JobID).Scan(&proofCount); err != nil || proofCount != 2 {
			t.Fatalf("expected 2 batch commit proofs, got %d err=%v", proofCount, err)
		}

		// 8. Repeated recovery is idempotent
		repeatedOutcome, err := restartSvc.ReconcileBatchJobByID(ctx, fix.claim.JobID, recoveryDeps)
		if err != nil || repeatedOutcome == nil {
			t.Fatalf("repeated recovery: outcome=%+v err=%v", repeatedOutcome, err)
		}
		if repeatedOutcome.Status != executioncontract.Succeeded || !repeatedOutcome.Reconciled {
			t.Fatalf("repeated recovery must be idempotent succeeded: %+v", repeatedOutcome)
		}
	})

	t.Run("RefusalOfUnknownOutcome_And_ActiveLeaseFencing", func(t *testing.T) {
		e, platformDB, ownerDB := setupBatchTestEnv(t)
		setupBatchItemsFixture(t, e)

		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()

		if _, err := e.exec.ExecContext(ctx, "INSERT INTO managed.batch_items (id, processed) VALUES (1, false), (2, false), (3, false), (4, false)"); err != nil {
			t.Fatalf("insert items: %v", err)
		}

		// 1. Worker claims with long active lease (30 seconds)
		fix := createBatchFixtureWithDuration(t, platformDB, ownerDB, e.target, 2, executioncontract.Apply, nil, 30*time.Second)
		cleanupBatchScope(t, e, fix.executionID)

		acquire := makeBatchAcquire(t, e.exec, e.prep)
		svc := New(platformDB)

		// Crash before commit
		deps := ExecutionDeps{
			Gate:    batchFakeGate(fix.snapshot),
			Acquire: acquire,
			Timeout: 10 * time.Second,
			crashAfterBatchAuthorize: func(ctx context.Context, batchIndex int) error {
				return errors.New("simulated crash with active lease")
			},
		}

		outcome, err := svc.ExecuteBatchJob(ctx, fix.claim, deps)
		if err == nil {
			t.Fatalf("expected crash, got outcome=%+v", outcome)
		}

		// 2. Lease has NOT expired yet (active lease fencing)
		// Discovery must NOT return this job
		ids, err := svc.ListUnresolvedBatchJobIDs(ctx, 10)
		if err != nil {
			t.Fatalf("ListUnresolvedBatchJobIDs: %v", err)
		}
		for _, id := range ids {
			if id == fix.claim.JobID {
				t.Fatalf("unexpired job %s must not be discovered as unresolved", fix.claim.JobID)
			}
		}

		// 3. Direct recovery attempt must be refused due to active lease
		recoveryDeps := ExecutionDeps{
			Gate:    batchFakeGate(fix.snapshot),
			Acquire: acquire,
			Timeout: 10 * time.Second,
		}
		recOutcome, err := svc.ReconcileBatchJobByID(ctx, fix.claim.JobID, recoveryDeps)
		if err == nil {
			t.Fatalf("expected recovery to be refused for unexpired job, got outcome=%+v", recOutcome)
		}
		var cErr executioncontract.ContractError
		if !errors.As(err, &cErr) || cErr.Code != "TARGET_BLOCKED" {
			t.Fatalf("expected TARGET_BLOCKED contract error, got: %v", err)
		}

		// Job must remain running and blocked
		var jobStatus string
		var jobBlocked bool
		if err := platformDB.QueryRowContext(ctx, "SELECT status, blocks_database FROM app.jobs WHERE id = $1", fix.claim.JobID).Scan(&jobStatus, &jobBlocked); err != nil {
			t.Fatalf("query job: %v", err)
		}
		if jobStatus != "running" || !jobBlocked {
			t.Fatalf("unexpired job must remain running and blocked, got status=%s blocked=%t", jobStatus, jobBlocked)
		}
	})

	t.Run("RefusalOfUnknownOutcome_UncertainTargetOutcomeStaysBlocked", func(t *testing.T) {
		e, platformDB, ownerDB := setupBatchTestEnv(t)
		setupBatchItemsFixture(t, e)

		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()

		if _, err := e.exec.ExecContext(ctx, "INSERT INTO managed.batch_items (id, processed) VALUES (1, false), (2, false)"); err != nil {
			t.Fatalf("insert items: %v", err)
		}

		// 1. Worker claims with short lease (100ms), batchSize = 2
		fix := createBatchFixtureWithDuration(t, platformDB, ownerDB, e.target, 2, executioncontract.Apply, nil, 400*time.Millisecond)
		cleanupBatchScope(t, e, fix.executionID)

		acquire := makeBatchAcquire(t, e.exec, e.prep)
		svc := New(platformDB)

		var crashHookRan bool
		deps := ExecutionDeps{
			Gate:    batchFakeGate(fix.snapshot),
			Acquire: acquire,
			Timeout: 10 * time.Second,
			crashAfterBatchAuthorize: func(ctx context.Context, batchIndex int) error {
				crashHookRan = true
				return errors.New("simulated crash after authorization")
			},
		}

		outcome, err := svc.ExecuteBatchJob(ctx, fix.claim, deps)
		if err == nil {
			t.Fatalf("expected crash after authorization, got outcome=%+v", outcome)
		}
		if !crashHookRan {
			t.Fatalf("expected crashAfterBatchAuthorize hook to execute, got err=%v", err)
		}

		// Insert an unconfirmed checkpoint with non-existent xid8 for the authorized step
		// This simulates target checkpoint outcome that cannot be confirmed
		_, err = e.cleanup.ExecContext(ctx, `
			INSERT INTO wbd_auth.batch_checkpoints
			    (execution_id, physical_target_id, target_xid, batch_index, direction, rows_affected)
			VALUES ($1::uuid, $2::uuid, '999999999999'::xid8, 0, 'up', 2)
		`, fix.executionID, e.target)
		if err != nil {
			t.Fatalf("insert uncertain checkpoint: %v", err)
		}
		// 2. Natural lease expiry (process restart with no synthetic state modification)
		time.Sleep(450 * time.Millisecond)

		// 3. Process restart simulation: fresh Service instance
		restartSvc := New(platformDB)

		// 4. Discovery via bounded seam
		ids, err := restartSvc.ListUnresolvedBatchJobIDs(ctx, 10)
		if err != nil {
			t.Fatalf("ListUnresolvedBatchJobIDs: %v", err)
		}
		var found bool
		for _, id := range ids {
			if id == fix.claim.JobID {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("expected expired job %s in unresolved list: %v", fix.claim.JobID, ids)
		}

		// 5. Gate revalidation and SQL dispatch hooks trap: MUST NOT be called
		recoveryGate := batchFakeGate(fix.snapshot)
		recoveryGate.RevalidateFunc = func(context.Context, executioncontract.RevalidationRequest) (executioncontract.ExecutionPlan, error) {
			t.Fatal("gate revalidate must not be called during recovery of uncertain outcome")
			return executioncontract.ExecutionPlan{}, nil
		}
		recoveryDeps := ExecutionDeps{
			Gate:    recoveryGate,
			Acquire: acquire,
			Timeout: 10 * time.Second,
			beforeBatchDispatch: func(context.Context, int) error {
				t.Fatal("SQL dispatch hook must not be called during recovery of uncertain outcome")
				return nil
			},
		}

		// 6. Direct recovery attempt: outcome is unknown/unconfirmed, recovery refused
		recOutcome, err := restartSvc.ReconcileBatchJobByID(ctx, fix.claim.JobID, recoveryDeps)
		if err == nil && (recOutcome == nil || recOutcome.Status != executioncontract.Unknown) {
			t.Fatalf("expected unknown outcome refusal, got outcome=%+v err=%v", recOutcome, err)
		}

		// 7. Target and job remain blocked; no fabricated success or clean close
		var jobStatus string
		var jobBlocked bool
		if err := platformDB.QueryRowContext(ctx, "SELECT status, blocks_database FROM app.jobs WHERE id = $1", fix.claim.JobID).Scan(&jobStatus, &jobBlocked); err != nil {
			t.Fatalf("query job: %v", err)
		}
		if !jobBlocked {
			t.Fatalf("job must remain blocked when target outcome is unknown, got status=%s blocked=%t", jobStatus, jobBlocked)
		}

		// 8. Repeated recovery is idempotent with proper block
		repeatedOutcome, err := restartSvc.ReconcileBatchJobByID(ctx, fix.claim.JobID, recoveryDeps)
		if err == nil && (repeatedOutcome == nil || repeatedOutcome.Status != executioncontract.Unknown) {
			t.Fatalf("repeated recovery must remain refused with unknown outcome, got: %+v err=%v", repeatedOutcome, err)
		}
		var jobBlockedAfter bool
		if err := platformDB.QueryRowContext(ctx, "SELECT blocks_database FROM app.jobs WHERE id = $1", fix.claim.JobID).Scan(&jobBlockedAfter); err != nil || !jobBlockedAfter {
			t.Fatalf("job must remain blocked after repeated recovery: %t", jobBlockedAfter)
		}
	})

	t.Run("UnknownStatus_ProvedAbort_RecoveryAndIdempotentRetry", func(t *testing.T) {
		e, platformDB, ownerDB := setupBatchTestEnv(t)
		setupBatchItemsFixture(t, e)

		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()

		if _, err := e.exec.ExecContext(ctx, "INSERT INTO managed.batch_items (id, processed) VALUES (1, false), (2, false)"); err != nil {
			t.Fatalf("insert items: %v", err)
		}

		fix := createBatchFixtureWithDuration(t, platformDB, ownerDB, e.target, 2, executioncontract.Apply, nil, 100*time.Millisecond)
		cleanupBatchScope(t, e, fix.executionID)

		acquire := makeBatchAcquire(t, e.exec, e.prep)
		svc := New(platformDB)

		deps := ExecutionDeps{
			Gate:    batchFakeGate(fix.snapshot),
			Acquire: acquire,
			Timeout: 10 * time.Second,
			crashAfterBatchAuthorize: func(ctx context.Context, batchIndex int) error {
				return errors.New("simulated hard crash before commit")
			},
		}

		outcome, err := svc.ExecuteBatchJob(ctx, fix.claim, deps)
		if err == nil {
			t.Fatalf("expected crash before commit, got: %+v", outcome)
		}

		time.Sleep(150 * time.Millisecond)

		if _, err := platformDB.ExecContext(ctx, "UPDATE app.jobs SET status = 'unknown', execution_status = 'unknown' WHERE id = $1", fix.claim.JobID); err != nil {
			t.Fatalf("set job unknown: %v", err)
		}
		if _, err := platformDB.ExecContext(ctx, "UPDATE app.job_attempts SET status = 'unknown' WHERE id = $1", fix.claim.AttemptID); err != nil {
			t.Fatalf("set attempt unknown: %v", err)
		}

		restartSvc := New(platformDB)
		ids, err := restartSvc.ListUnresolvedBatchJobIDs(ctx, 10)
		if err != nil {
			t.Fatalf("ListUnresolvedBatchJobIDs: %v", err)
		}
		var found bool
		for _, id := range ids {
			if id == fix.claim.JobID {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("expected unknown job %s in unresolved list: %v", fix.claim.JobID, ids)
		}

		recoveryGate := batchFakeGate(fix.snapshot)
		recoveryGate.RevalidateFunc = func(context.Context, executioncontract.RevalidationRequest) (executioncontract.ExecutionPlan, error) {
			t.Fatal("gate revalidate must not be called during receipt recovery")
			return executioncontract.ExecutionPlan{}, nil
		}
		recoveryDeps := ExecutionDeps{
			Gate:    recoveryGate,
			Acquire: acquire,
			Timeout: 10 * time.Second,
		}

		recOutcome, err := restartSvc.ReconcileBatchJobByID(ctx, fix.claim.JobID, recoveryDeps)
		if err != nil || recOutcome == nil {
			t.Fatalf("ReconcileBatchJobByID: outcome=%+v err=%v", recOutcome, err)
		}
		if recOutcome.Status != executioncontract.Failed || !recOutcome.Reconciled {
			t.Fatalf("expected proved abort to close as failed reconciled, got: %+v", recOutcome)
		}

		var jobStatus, execStatus string
		var jobBlocked bool
		if err := platformDB.QueryRowContext(ctx, "SELECT status, execution_status, blocks_database FROM app.jobs WHERE id = $1", fix.claim.JobID).Scan(&jobStatus, &execStatus, &jobBlocked); err != nil {
			t.Fatalf("query job: %v", err)
		}
		if jobStatus != "failed" || execStatus != "failed" || jobBlocked {
			t.Fatalf("expected failed clean unblocked job, got status=%s execStatus=%s blocked=%t", jobStatus, execStatus, jobBlocked)
		}

		var attStatus string
		if err := platformDB.QueryRowContext(ctx, "SELECT status FROM app.job_attempts WHERE id = $1", fix.claim.AttemptID).Scan(&attStatus); err != nil {
			t.Fatalf("query attempt: %v", err)
		}
		if attStatus != "failed" {
			t.Fatalf("expected attempt status='failed', got: %s", attStatus)
		}

		repOutcome, err := restartSvc.ReconcileBatchJobByID(ctx, fix.claim.JobID, recoveryDeps)
		if err != nil || repOutcome == nil {
			t.Fatalf("repeated recovery: outcome=%+v err=%v", repOutcome, err)
		}
		if repOutcome.Status != executioncontract.Failed || !repOutcome.Reconciled {
			t.Fatalf("expected repeated recovery to be failed reconciled, got: %+v", repOutcome)
		}
	})

	t.Run("UnknownStatus_ProvedPartial_RecoveryAndIdempotentRetry", func(t *testing.T) {
		e, platformDB, ownerDB := setupBatchTestEnv(t)
		setupBatchItemsFixture(t, e)

		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()

		if _, err := e.exec.ExecContext(ctx, "INSERT INTO managed.batch_items (id, processed) VALUES (1, false), (2, false), (3, false), (4, false)"); err != nil {
			t.Fatalf("insert items: %v", err)
		}

		fix := createBatchFixtureWithDuration(t, platformDB, ownerDB, e.target, 2, executioncontract.Apply, nil, 100*time.Millisecond)
		cleanupBatchScope(t, e, fix.executionID)

		acquire := makeBatchAcquire(t, e.exec, e.prep)
		svc := New(platformDB)

		deps := ExecutionDeps{
			Gate:    batchFakeGate(fix.snapshot),
			Acquire: acquire,
			Timeout: 10 * time.Second,
			crashAfterBatchAuthorize: func(ctx context.Context, batchIndex int) error {
				if batchIndex == 1 {
					return errors.New("simulated hard crash during batch 1 authorization")
				}
				return nil
			},
		}

		outcome, err := svc.ExecuteBatchJob(ctx, fix.claim, deps)
		if err == nil {
			t.Fatalf("expected crash during batch 1, got outcome=%+v", outcome)
		}

		time.Sleep(150 * time.Millisecond)

		if _, err := platformDB.ExecContext(ctx, "UPDATE app.jobs SET status = 'unknown', execution_status = 'unknown' WHERE id = $1", fix.claim.JobID); err != nil {
			t.Fatalf("set job unknown: %v", err)
		}
		if _, err := platformDB.ExecContext(ctx, "UPDATE app.job_attempts SET status = 'unknown' WHERE id = $1", fix.claim.AttemptID); err != nil {
			t.Fatalf("set attempt unknown: %v", err)
		}

		restartSvc := New(platformDB)
		ids, err := restartSvc.ListUnresolvedBatchJobIDs(ctx, 10)
		if err != nil {
			t.Fatalf("ListUnresolvedBatchJobIDs: %v", err)
		}
		var found bool
		for _, id := range ids {
			if id == fix.claim.JobID {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("expected unknown job %s in unresolved list: %v", fix.claim.JobID, ids)
		}

		recoveryGate := batchFakeGate(fix.snapshot)
		recoveryGate.RevalidateFunc = func(context.Context, executioncontract.RevalidationRequest) (executioncontract.ExecutionPlan, error) {
			t.Fatal("gate revalidate must not be called during receipt recovery")
			return executioncontract.ExecutionPlan{}, nil
		}
		recoveryDeps := ExecutionDeps{
			Gate:    recoveryGate,
			Acquire: acquire,
			Timeout: 10 * time.Second,
		}

		recOutcome, err := restartSvc.ReconcileBatchJobByID(ctx, fix.claim.JobID, recoveryDeps)
		if err != nil || recOutcome == nil {
			t.Fatalf("ReconcileBatchJobByID: outcome=%+v err=%v", recOutcome, err)
		}
		if recOutcome.Status != executioncontract.Partial || !recOutcome.Reconciled {
			t.Fatalf("expected partial status and reconciled=true, got: %+v", recOutcome)
		}

		var jobStatus, execStatus string
		var jobBlocked bool
		if err := platformDB.QueryRowContext(ctx, "SELECT status, execution_status, blocks_database FROM app.jobs WHERE id = $1", fix.claim.JobID).Scan(&jobStatus, &execStatus, &jobBlocked); err != nil {
			t.Fatalf("query job: %v", err)
		}
		if jobStatus != "partial" || execStatus != "partial" || !jobBlocked {
			t.Fatalf("expected job status='partial' and blocks_database=true, got status=%s execStatus=%s blocked=%t", jobStatus, execStatus, jobBlocked)
		}

		var attStatus string
		if err := platformDB.QueryRowContext(ctx, "SELECT status FROM app.job_attempts WHERE id = $1", fix.claim.AttemptID).Scan(&attStatus); err != nil {
			t.Fatalf("query attempt: %v", err)
		}
		if attStatus != "partial" {
			t.Fatalf("expected attempt status='partial', got: %s", attStatus)
		}

		repOutcome, err := restartSvc.ReconcileBatchJobByID(ctx, fix.claim.JobID, recoveryDeps)
		if err != nil || repOutcome == nil {
			t.Fatalf("repeated recovery: outcome=%+v err=%v", repOutcome, err)
		}
		if repOutcome.Status != executioncontract.Partial || !repOutcome.Reconciled {
			t.Fatalf("repeated recovery must be idempotent partial: %+v", repOutcome)
		}
	})

	t.Run("UnknownStatus_ProvedComplete_RecoveryAndIdempotentRetry", func(t *testing.T) {
		e, platformDB, ownerDB := setupBatchTestEnv(t)
		setupBatchItemsFixture(t, e)

		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()

		if _, err := e.exec.ExecContext(ctx, "INSERT INTO managed.batch_items (id, processed) VALUES (1, false), (2, false), (3, false), (4, false)"); err != nil {
			t.Fatalf("insert items: %v", err)
		}

		fix := createBatchFixtureWithDuration(t, platformDB, ownerDB, e.target, 2, executioncontract.Apply, nil, 1*time.Second)
		cleanupBatchScope(t, e, fix.executionID)
		acquire := makeBatchAcquire(t, e.exec, e.prep)
		svc := New(platformDB)

		deps := ExecutionDeps{
			Gate:    batchFakeGate(fix.snapshot),
			Acquire: acquire,
			Timeout: 10 * time.Second,
			crashAfterBatchCommit: func(ctx context.Context, batchIndex int) error {
				if batchIndex == 1 {
					return errors.New("simulated crash after final target commit")
				}
				return nil
			},
		}

		outcome, err := svc.ExecuteBatchJob(ctx, fix.claim, deps)
		if err == nil {
			t.Fatalf("expected crash after final target commit, got outcome=%+v", outcome)
		}

		time.Sleep(1500 * time.Millisecond)
		if _, err := platformDB.ExecContext(ctx, "UPDATE app.jobs SET status = 'unknown', execution_status = 'unknown', lease_expires_at = clock_timestamp() - interval '5 seconds' WHERE id = $1", fix.claim.JobID); err != nil {
			t.Fatalf("set job unknown: %v", err)
		}
		if _, err := platformDB.ExecContext(ctx, "UPDATE app.job_attempts SET status = 'unknown' WHERE id = $1", fix.claim.AttemptID); err != nil {
			t.Fatalf("set attempt unknown: %v", err)
		}

		restartSvc := New(platformDB)
		ids, err := restartSvc.ListUnresolvedBatchJobIDs(ctx, 10)
		if err != nil {
			t.Fatalf("ListUnresolvedBatchJobIDs: %v", err)
		}
		var found bool
		for _, id := range ids {
			if id == fix.claim.JobID {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("expected unknown job %s in unresolved list: %v", fix.claim.JobID, ids)
		}

		recoveryGate := batchFakeGate(fix.snapshot)
		recoveryGate.RevalidateFunc = func(context.Context, executioncontract.RevalidationRequest) (executioncontract.ExecutionPlan, error) {
			t.Fatal("gate revalidate must not be called during receipt recovery")
			return executioncontract.ExecutionPlan{}, nil
		}
		recoveryDeps := ExecutionDeps{
			Gate:    recoveryGate,
			Acquire: acquire,
			Timeout: 10 * time.Second,
		}

		recOutcome, err := restartSvc.ReconcileBatchJobByID(ctx, fix.claim.JobID, recoveryDeps)
		if err != nil || recOutcome == nil {
			t.Fatalf("ReconcileBatchJobByID: outcome=%+v err=%v", recOutcome, err)
		}
		if recOutcome.Status != executioncontract.Succeeded || !recOutcome.Reconciled || recOutcome.Receipt != fix.claim.ExecutionID {
			t.Fatalf("expected succeeded reconciled outcome with matching receipt, got: %+v", recOutcome)
		}

		var jobStatus, execStatus string
		var jobBlocked bool
		if err := platformDB.QueryRowContext(ctx, "SELECT status, execution_status, blocks_database FROM app.jobs WHERE id = $1", fix.claim.JobID).Scan(&jobStatus, &execStatus, &jobBlocked); err != nil {
			t.Fatalf("query job: %v", err)
		}
		if jobStatus != "succeeded" || execStatus != "succeeded" || jobBlocked {
			t.Fatalf("expected succeeded unblocked job, got status=%s execStatus=%s blocked=%t", jobStatus, execStatus, jobBlocked)
		}

		var attStatus string
		if err := platformDB.QueryRowContext(ctx, "SELECT status FROM app.job_attempts WHERE id = $1", fix.claim.AttemptID).Scan(&attStatus); err != nil {
			t.Fatalf("query attempt: %v", err)
		}
		if attStatus != "succeeded" {
			t.Fatalf("expected attempt status='succeeded', got: %s", attStatus)
		}

		repOutcome, err := restartSvc.ReconcileBatchJobByID(ctx, fix.claim.JobID, recoveryDeps)
		if err != nil || repOutcome == nil {
			t.Fatalf("repeated recovery: outcome=%+v err=%v", repOutcome, err)
		}
		if repOutcome.Status != executioncontract.Succeeded || !repOutcome.Reconciled || repOutcome.Receipt != fix.claim.ExecutionID {
			t.Fatalf("repeated recovery must be idempotent succeeded: %+v", repOutcome)
		}
	})

	t.Run("FinalResumeCrash_PriorCheckpoints_PreviouslyPartialResume_AndUnrelatedBlockedJob", func(t *testing.T) {
		e, platformDB, ownerDB := setupBatchTestEnv(t)
		setupBatchItemsFixture(t, e)

		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()

		if _, err := e.exec.ExecContext(ctx, "INSERT INTO managed.batch_items (id, processed) VALUES (1, false), (2, false), (3, false)"); err != nil {
			t.Fatalf("insert items: %v", err)
		}

		acquire := makeBatchAcquire(t, e.exec, e.prep)
		svc := New(platformDB)

		// 1. Initial Job 1 (Apply): batchSize = 1, commits batch 0, crashes authorising batch 1
		fix1 := createBatchFixture(t, platformDB, ownerDB, e.target, 1, executioncontract.Apply, nil)
		cleanupBatchScope(t, e, fix1.executionID)
		expireLease := func(jobID string) {
			t.Helper()
			if _, err := platformDB.ExecContext(ctx, "UPDATE app.jobs SET lease_expires_at = clock_timestamp() - interval '5 seconds' WHERE id = $1", jobID); err != nil {
				t.Fatalf("expire crashed job lease: %v", err)
			}
		}
		crash := errors.New("simulated batch execution crash")

		deps1 := ExecutionDeps{
			Gate:    batchFakeGate(fix1.snapshot),
			Acquire: acquire,
			Timeout: 10 * time.Second,
			crashAfterBatchAuthorize: func(ctx context.Context, batchIndex int) error {
				if batchIndex == 1 {
					return crash
				}
				return nil
			},
		}
		outcome1, err := svc.ExecuteBatchJob(ctx, fix1.claim, deps1)
		if !errors.Is(err, crash) {
			t.Fatalf("expected injected crash on job 1, got outcome=%+v err=%v", outcome1, err)
		}
		expireLease(fix1.claim.JobID)

		recOutcome1, err := svc.ReconcileBatchJobByID(ctx, fix1.claim.JobID, ExecutionDeps{
			Gate:    batchFakeGate(fix1.snapshot),
			Acquire: acquire,
			Timeout: 10 * time.Second,
		})
		if err != nil || recOutcome1.Status != executioncontract.Partial {
			t.Fatalf("reconcile job 1: outcome=%+v err=%v", recOutcome1, err)
		}

		// 2. Job 2 (Resume 1): resumes scope, commits batch 1, crashes authorising batch 2
		fix2 := createBatchFixture(t, platformDB, ownerDB, e.target, 1, executioncontract.Resume, &fix1)
		deps2 := ExecutionDeps{
			Gate:    batchFakeGate(fix2.snapshot),
			Acquire: acquire,
			Timeout: 10 * time.Second,
			crashAfterBatchAuthorize: func(ctx context.Context, batchIndex int) error {
				if batchIndex == 2 {
					return crash
				}
				return nil
			},
		}
		outcome2, err := svc.ExecuteBatchJob(ctx, fix2.claim, deps2)
		if !errors.Is(err, crash) {
			t.Fatalf("expected injected crash on job 2, got outcome=%+v err=%v", outcome2, err)
		}
		expireLease(fix2.claim.JobID)

		recOutcome2, err := svc.ReconcileBatchJobByID(ctx, fix2.claim.JobID, ExecutionDeps{
			Gate:    batchFakeGate(fix2.snapshot),
			Acquire: acquire,
			Timeout: 10 * time.Second,
		})
		if err != nil || recOutcome2.Status != executioncontract.Partial {
			t.Fatalf("reconcile job 2: outcome=%+v err=%v", recOutcome2, err)
		}
		if _, err := platformDB.ExecContext(ctx, "UPDATE app.migration_proposals SET status = 'superseded' WHERE id = $1", fix2.proposalID); err != nil {
			t.Fatalf("supersede resume 1 proposal: %v", err)
		}

		// 3. Job 3 (Final Resume): resumes scope, commits batch 2 (the final batch!), crashes post-commit
		fix3 := createBatchFixture(t, platformDB, ownerDB, e.target, 1, executioncontract.Resume, &fix1)
		deps3 := ExecutionDeps{
			Gate:    batchFakeGate(fix3.snapshot),
			Acquire: acquire,
			Timeout: 10 * time.Second,
			crashAfterBatchCommit: func(ctx context.Context, batchIndex int) error {
				if batchIndex == 2 {
					return crash
				}
				return nil
			},
		}
		outcome3, err := svc.ExecuteBatchJob(ctx, fix3.claim, deps3)
		if !errors.Is(err, crash) {
			t.Fatalf("expected injected crash on final resume, got outcome=%+v err=%v", outcome3, err)
		}
		expireLease(fix3.claim.JobID)

		// 4. Create an unrelated blocked job (different migration/proposal)
		unrelatedJobID := uuid.NewString()
		unrelatedExecID := uuid.NewString()
		unrelatedDraftID := uuid.NewString()
		unrelatedProposalID := uuid.NewString()
		unrelatedHash := sha256.Sum256([]byte("unrelated_migration"))
		unrelatedHashHex := hex.EncodeToString(unrelatedHash[:])
		if _, err := platformDB.ExecContext(ctx, `
			INSERT INTO app.migration_drafts (id, database_id, revision, title, sql, created_by)
			VALUES ($1, $2, 1, 'Unrelated Migration', 'UPDATE managed.batch_items SET processed = true WHERE id = 1;', $3)
		`, unrelatedDraftID, fix1.dbID, fix1.actorID); err != nil {
			t.Fatalf("insert draft: %v", err)
		}
		if _, err := platformDB.ExecContext(ctx, `
			INSERT INTO app.migration_proposals (id, database_id, migration_id, revision, action, status, snapshot, snapshot_hash, created_by)
			VALUES ($1, $2, $3, 1, 'apply', 'draft', '{}'::jsonb, $4, $5)
		`, unrelatedProposalID, fix1.dbID, unrelatedDraftID, unrelatedHash[:], fix1.actorID); err != nil {
			t.Fatalf("insert proposal: %v", err)
		}
		if _, err := platformDB.ExecContext(ctx, `
			UPDATE app.migration_proposals SET status = 'pending_review' WHERE id = $1
		`, unrelatedProposalID); err != nil {
			t.Fatalf("pending_review proposal: %v", err)
		}
		if _, err := platformDB.ExecContext(ctx, `
			UPDATE app.migration_proposals SET status = 'approved' WHERE id = $1
		`, unrelatedProposalID); err != nil {
			t.Fatalf("approve proposal: %v", err)
		}
		if _, err := platformDB.ExecContext(ctx, `
			UPDATE app.migration_proposals SET status = 'queued' WHERE id = $1
		`, unrelatedProposalID); err != nil {
			t.Fatalf("queue proposal: %v", err)
		}
		if _, err := platformDB.ExecContext(ctx, `
			INSERT INTO app.jobs (id, execution_id, database_id, target_id, request_id, proposal_id, migration_id, revision, actor_id, action, direction, mode, status, execution_status, blocks_database, snapshot_hash, snapshot, attempt_no)
			VALUES ($1, $2, $3, $4, $5, $6, $7, 1, $8, 'apply', 'up', 'batch', 'partial', 'partial', true, $9, '{"batch":{"batch_size":1,"schema":"managed","table":"batch_items","cursor_column":"id"}}'::jsonb, 1)
		`, unrelatedJobID, unrelatedExecID, fix1.dbID, e.target, uuid.NewString(), unrelatedProposalID, unrelatedDraftID, fix1.actorID, unrelatedHashHex); err != nil {
			t.Fatalf("insert unrelated job: %v", err)
		}
		if _, err := platformDB.ExecContext(ctx, `
			INSERT INTO app.job_attempts (id, job_id, execution_id, database_id, migration_id, revision, actor_id, attempt_no, status, sql_entered)
			VALUES ($1, $2, $3, $4, $5, 1, $6, 1, 'partial', true)
		`, uuid.NewString(), unrelatedJobID, unrelatedExecID, fix1.dbID, unrelatedDraftID, fix1.actorID); err != nil {
			t.Fatalf("insert unrelated attempt: %v", err)
		}
		// 5. Recover scope via final resume Job 3
		restartSvc := New(platformDB)
		recoveryDeps := ExecutionDeps{
			Gate:    batchFakeGate(fix3.snapshot),
			Acquire: acquire,
			Timeout: 10 * time.Second,
		}

		recOutcome3, err := restartSvc.ReconcileBatchJobByID(ctx, fix3.claim.JobID, recoveryDeps)
		if err != nil || recOutcome3 == nil {
			t.Fatalf("ReconcileBatchJobByID final resume: outcome=%+v err=%v", recOutcome3, err)
		}
		if recOutcome3.Status != executioncontract.Succeeded || !recOutcome3.Reconciled || recOutcome3.Receipt != fix3.claim.ExecutionID {
			t.Fatalf("expected final resume to succeed reconciled, got: %+v", recOutcome3)
		}

		var j3Status, j3ExecStatus string
		var j3Blocked bool
		if err := platformDB.QueryRowContext(ctx, "SELECT status, execution_status, blocks_database FROM app.jobs WHERE id = $1", fix3.claim.JobID).Scan(&j3Status, &j3ExecStatus, &j3Blocked); err != nil {
			t.Fatalf("query job 3: %v", err)
		}
		if j3Status != "succeeded" || j3ExecStatus != "succeeded" || j3Blocked {
			t.Fatalf("job 3 must be succeeded and unblocked: status=%s execStatus=%s blocked=%t", j3Status, j3ExecStatus, j3Blocked)
		}
		var a3Status string
		if err := platformDB.QueryRowContext(ctx, "SELECT status FROM app.job_attempts WHERE id = $1", fix3.claim.AttemptID).Scan(&a3Status); err != nil {
			t.Fatalf("query attempt 3: %v", err)
		}
		if a3Status != "succeeded" {
			t.Fatalf("attempt 3 must be succeeded, got: %s", a3Status)
		}

		var j1Status, j1ExecStatus string
		var j1Blocked bool
		if err := platformDB.QueryRowContext(ctx, "SELECT status, execution_status, blocks_database FROM app.jobs WHERE id = $1", fix1.claim.JobID).Scan(&j1Status, &j1ExecStatus, &j1Blocked); err != nil {
			t.Fatalf("query job 1: %v", err)
		}
		if j1Status != "partial" || j1ExecStatus != "succeeded" || j1Blocked {
			t.Fatalf("job 1 must preserve historical partial with aggregate succeeded and unblocked: status=%s execStatus=%s blocked=%t", j1Status, j1ExecStatus, j1Blocked)
		}
		var a1Status string
		if err := platformDB.QueryRowContext(ctx, "SELECT status FROM app.job_attempts WHERE id = $1", fix1.claim.AttemptID).Scan(&a1Status); err != nil {
			t.Fatalf("query attempt 1: %v", err)
		}
		if a1Status != "partial" {
			t.Fatalf("attempt 1 must preserve historical partial, got: %s", a1Status)
		}

		var j2Status, j2ExecStatus string
		var j2Blocked bool
		if err := platformDB.QueryRowContext(ctx, "SELECT status, execution_status, blocks_database FROM app.jobs WHERE id = $1", fix2.claim.JobID).Scan(&j2Status, &j2ExecStatus, &j2Blocked); err != nil {
			t.Fatalf("query job 2: %v", err)
		}
		if j2Status != "partial" || j2ExecStatus != "succeeded" || j2Blocked {
			t.Fatalf("job 2 must preserve historical partial with aggregate succeeded and unblocked: status=%s execStatus=%s blocked=%t", j2Status, j2ExecStatus, j2Blocked)
		}
		var a2Status string
		if err := platformDB.QueryRowContext(ctx, "SELECT status FROM app.job_attempts WHERE id = $1", fix2.claim.AttemptID).Scan(&a2Status); err != nil {
			t.Fatalf("query attempt 2: %v", err)
		}
		if a2Status != "partial" {
			t.Fatalf("attempt 2 must preserve historical partial, got: %s", a2Status)
		}

		var jUnrelatedStatus string
		var jUnrelatedBlocked bool
		if err := platformDB.QueryRowContext(ctx, "SELECT status, blocks_database FROM app.jobs WHERE id = $1", unrelatedJobID).Scan(&jUnrelatedStatus, &jUnrelatedBlocked); err != nil {
			t.Fatalf("query unrelated job: %v", err)
		}
		if !jUnrelatedBlocked {
			t.Fatalf("unrelated job must remain blocked, got: blocked=%t status=%s", jUnrelatedBlocked, jUnrelatedStatus)
		}

		repeatedOutcome3, err := restartSvc.ReconcileBatchJobByID(ctx, fix3.claim.JobID, recoveryDeps)
		if err != nil || repeatedOutcome3 == nil {
			t.Fatalf("repeated recovery on final resume: outcome=%+v err=%v", repeatedOutcome3, err)
		}
		if repeatedOutcome3.Status != executioncontract.Succeeded || !repeatedOutcome3.Reconciled || repeatedOutcome3.Receipt != fix3.claim.ExecutionID {
			t.Fatalf("repeated recovery must be idempotent succeeded: %+v", repeatedOutcome3)
		}
	})
}

func TestExecuteBatchJob_Regression_LaterResumeRecoveringPreviousFinalCommitWithEarlierContributors(t *testing.T) {
	e, platformDB, ownerDB := setupBatchTestEnv(t)
	setupBatchItemsFixture(t, e)

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	if _, err := e.exec.ExecContext(ctx, "INSERT INTO managed.batch_items (id, processed) VALUES (1, false), (2, false), (3, false)"); err != nil {
		t.Fatalf("insert items: %v", err)
	}

	acquire := makeBatchAcquire(t, e.exec, e.prep)
	svc := New(platformDB)

	// 1. Initial Job 1 (Apply): batchSize = 1, commits batch 0, crashes during batch 1 authorize
	fix1 := createBatchFixtureWithDuration(t, platformDB, ownerDB, e.target, 1, executioncontract.Apply, nil, 100*time.Millisecond)
	cleanupBatchScope(t, e, fix1.executionID)

	deps1 := ExecutionDeps{
		Gate:    batchFakeGate(fix1.snapshot),
		Acquire: acquire,
		Timeout: 10 * time.Second,
		crashAfterBatchAuthorize: func(ctx context.Context, batchIndex int) error {
			if batchIndex == 1 {
				return errors.New("simulated crash during batch 1 authorize")
			}
			return nil
		},
	}
	outcome1, err := svc.ExecuteBatchJob(ctx, fix1.claim, deps1)
	if err == nil {
		t.Fatalf("expected crash on job 1, got outcome=%+v", outcome1)
	}
	time.Sleep(150 * time.Millisecond)

	recOutcome1, err := svc.ReconcileBatchJobByID(ctx, fix1.claim.JobID, ExecutionDeps{
		Gate:    batchFakeGate(fix1.snapshot),
		Acquire: acquire,
		Timeout: 10 * time.Second,
	})
	if err != nil || recOutcome1.Status != executioncontract.Partial {
		t.Fatalf("reconcile job 1: outcome=%+v err=%v", recOutcome1, err)
	}
	if _, err := platformDB.ExecContext(ctx, "UPDATE app.migration_proposals SET status = 'superseded' WHERE id = $1", fix1.proposalID); err != nil {
		t.Fatalf("supersede proposal 1: %v", err)
	}

	// 2. Job 2 (Resume 1): resumes scope, commits batch 1, crashes during batch 2 authorize
	fix2 := createBatchFixtureWithDuration(t, platformDB, ownerDB, e.target, 1, executioncontract.Resume, &fix1, 100*time.Millisecond)
	deps2 := ExecutionDeps{
		Gate:    batchFakeGate(fix2.snapshot),
		Acquire: acquire,
		Timeout: 10 * time.Second,
		crashAfterBatchAuthorize: func(ctx context.Context, batchIndex int) error {
			if batchIndex == 2 {
				return errors.New("simulated crash during batch 2 authorize")
			}
			return nil
		},
	}
	outcome2, err := svc.ExecuteBatchJob(ctx, fix2.claim, deps2)
	if err == nil {
		t.Fatalf("expected crash on job 2, got outcome=%+v", outcome2)
	}
	time.Sleep(300 * time.Millisecond)

	recOutcome2, err := svc.ReconcileBatchJobByID(ctx, fix2.claim.JobID, ExecutionDeps{
		Gate:    batchFakeGate(fix2.snapshot),
		Acquire: acquire,
		Timeout: 10 * time.Second,
	})
	if err != nil || recOutcome2.Status != executioncontract.Partial {
		t.Fatalf("reconcile job 2: outcome=%+v err=%v", recOutcome2, err)
	}
	if _, err := platformDB.ExecContext(ctx, "UPDATE app.migration_proposals SET status = 'superseded' WHERE id = $1", fix2.proposalID); err != nil {
		t.Fatalf("supersede proposal 2: %v", err)
	}

	// 3. Job 3 (Resume 2): resumes scope, commits batch 2 (the final batch!), crashes post-commit
	fix3 := createBatchFixtureWithDuration(t, platformDB, ownerDB, e.target, 1, executioncontract.Resume, &fix1, 100*time.Millisecond)
	deps3 := ExecutionDeps{
		Gate:    batchFakeGate(fix3.snapshot),
		Acquire: acquire,
		Timeout: 10 * time.Second,
		crashAfterBatchCommit: func(ctx context.Context, batchIndex int) error {
			if batchIndex == 2 {
				return errors.New("simulated post-commit crash on final commit in job 3")
			}
			return nil
		},
	}
	outcome3, err := svc.ExecuteBatchJob(ctx, fix3.claim, deps3)
	if err == nil {
		t.Fatalf("expected crash on job 3, got outcome=%+v", outcome3)
	}
	time.Sleep(150 * time.Millisecond)
	if _, err := platformDB.ExecContext(ctx, "UPDATE app.migration_proposals SET status = 'superseded' WHERE id = $1", fix3.proposalID); err != nil {
		t.Fatalf("supersede proposal 3: %v", err)
	}

	recoverCrashedBatchJob(t, ctx, platformDB, fix3.claim.JobID, fix3.snapshot, acquire)

	// 4. Job 4 (Resume 3): new worker executes normal execution recovery for the scope!
	fix4 := createBatchFixtureWithDuration(t, platformDB, ownerDB, e.target, 1, executioncontract.Resume, &fix1, 30*time.Second)
	svc4 := New(platformDB)
	deps4 := ExecutionDeps{
		Gate:    batchFakeGate(fix4.snapshot),
		Acquire: acquire,
		Timeout: 10 * time.Second,
	}
	outcome4, err := svc4.ExecuteBatchJob(ctx, fix4.claim, deps4)
	if err != nil || outcome4 == nil {
		t.Fatalf("ExecuteBatchJob job 4 (recovery): outcome=%+v err=%v", outcome4, err)
	}
	if outcome4.Status != executioncontract.Succeeded || !outcome4.Reconciled || outcome4.Receipt != fix4.claim.ExecutionID {
		t.Fatalf("expected job 4 to succeed reconciled, got: %+v", outcome4)
	}

	// Verify durable disposition of originating finalizer Job 3:
	var j3Status, j3ExecStatus string
	var j3Blocked bool
	if err := platformDB.QueryRowContext(ctx, "SELECT status, execution_status, blocks_database FROM app.jobs WHERE id = $1", fix3.claim.JobID).Scan(&j3Status, &j3ExecStatus, &j3Blocked); err != nil {
		t.Fatalf("query job 3: %v", err)
	}
	if j3Status != "succeeded" || j3ExecStatus != "succeeded" || j3Blocked {
		t.Fatalf("job 3 must be succeeded and unblocked: status=%s execStatus=%s blocked=%t", j3Status, j3ExecStatus, j3Blocked)
	}
	var a3Status string
	if err := platformDB.QueryRowContext(ctx, "SELECT status FROM app.job_attempts WHERE id = $1", fix3.claim.AttemptID).Scan(&a3Status); err != nil {
		t.Fatalf("query attempt 3: %v", err)
	}
	if a3Status != "succeeded" {
		t.Fatalf("attempt 3 must be succeeded, got: %s", a3Status)
	}
	var proofExists bool
	if err := platformDB.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM app.job_batch_commit_proofs WHERE job_id = $1 AND batch_index = 2)", fix3.claim.JobID).Scan(&proofExists); err != nil || !proofExists {
		t.Fatalf("expected commit proof for job 3 batch 2, got: %t err=%v", proofExists, err)
	}

	// Verify earlier contributors (Job 1 and Job 2):
	var j1Status, j1ExecStatus string
	var j1Blocked bool
	if err := platformDB.QueryRowContext(ctx, "SELECT status, execution_status, blocks_database FROM app.jobs WHERE id = $1", fix1.claim.JobID).Scan(&j1Status, &j1ExecStatus, &j1Blocked); err != nil {
		t.Fatalf("query job 1: %v", err)
	}
	if j1Status != "partial" || j1ExecStatus != "succeeded" || j1Blocked {
		t.Fatalf("job 1 must preserve historical partial with aggregate succeeded and unblocked: status=%s execStatus=%s blocked=%t", j1Status, j1ExecStatus, j1Blocked)
	}

	var j2Status, j2ExecStatus string
	var j2Blocked bool
	if err := platformDB.QueryRowContext(ctx, "SELECT status, execution_status, blocks_database FROM app.jobs WHERE id = $1", fix2.claim.JobID).Scan(&j2Status, &j2ExecStatus, &j2Blocked); err != nil {
		t.Fatalf("query job 2: %v", err)
	}
	if j2Status != "partial" || j2ExecStatus != "succeeded" || j2Blocked {
		t.Fatalf("job 2 must preserve historical partial with aggregate succeeded and unblocked: status=%s execStatus=%s blocked=%t", j2Status, j2ExecStatus, j2Blocked)
	}
}

func TestExecuteBatchJob_Regression_AbortedResumeAfterPriorCheckpoint_DurableDispositionAndRepeatedRecovery(t *testing.T) {
	e, platformDB, ownerDB := setupBatchTestEnv(t)
	setupBatchItemsFixture(t, e)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if _, err := e.exec.ExecContext(ctx, "INSERT INTO managed.batch_items (id, processed) VALUES (1, false), (2, false)"); err != nil {
		t.Fatalf("insert items: %v", err)
	}

	acquire := makeBatchAcquire(t, e.exec, e.prep)
	svc := New(platformDB)

	// 1. Initial Job 1 (Apply): batchSize = 1, commits batch 0, crashes authorising batch 1
	fix1 := createBatchFixtureWithDuration(t, platformDB, ownerDB, e.target, 1, executioncontract.Apply, nil, 100*time.Millisecond)
	cleanupBatchScope(t, e, fix1.executionID)

	deps1 := ExecutionDeps{
		Gate:    batchFakeGate(fix1.snapshot),
		Acquire: acquire,
		Timeout: 10 * time.Second,
		crashAfterBatchAuthorize: func(ctx context.Context, batchIndex int) error {
			if batchIndex == 1 {
				return errors.New("simulated crash authorising batch 1")
			}
			return nil
		},
	}
	outcome1, err := svc.ExecuteBatchJob(ctx, fix1.claim, deps1)
	if err == nil {
		t.Fatalf("expected crash on job 1, got outcome=%+v", outcome1)
	}
	time.Sleep(300 * time.Millisecond)

	recOutcome1, err := svc.ReconcileBatchJobByID(ctx, fix1.claim.JobID, ExecutionDeps{
		Gate:    batchFakeGate(fix1.snapshot),
		Acquire: acquire,
		Timeout: 10 * time.Second,
	})
	if err != nil || recOutcome1.Status != executioncontract.Partial {
		t.Fatalf("reconcile job 1: outcome=%+v err=%v", recOutcome1, err)
	}
	if _, err := platformDB.ExecContext(ctx, "UPDATE app.migration_proposals SET status = 'superseded' WHERE id = $1", fix1.proposalID); err != nil {
		t.Fatalf("supersede proposal 1: %v", err)
	}

	// 2. Job 2 (Resume): cleanly aborted before committing anything
	fix2 := createBatchFixtureWithDuration(t, platformDB, ownerDB, e.target, 1, executioncontract.Resume, &fix1, 100*time.Millisecond)
	deps2 := ExecutionDeps{
		Gate:    batchFakeGate(fix2.snapshot),
		Acquire: acquire,
		Timeout: 10 * time.Second,
		crashAfterBatchAuthorize: func(ctx context.Context, batchIndex int) error {
			return errors.New("simulated hard crash after authorization before target commit")
		},
	}
	outcome2, err := svc.ExecuteBatchJob(ctx, fix2.claim, deps2)
	if err == nil {
		t.Fatalf("expected crash on job 2, got outcome=%+v", outcome2)
	}
	time.Sleep(150 * time.Millisecond)

	// Verify attempt 2 committed 0 proofs
	var committedProofs int
	if err := platformDB.QueryRowContext(ctx, "SELECT count(*) FROM app.job_batch_commit_proofs WHERE attempt_id = $1", fix2.claim.AttemptID).Scan(&committedProofs); err != nil || committedProofs != 0 {
		t.Fatalf("expected 0 committed proofs for job 2 attempt, got %d err=%v", committedProofs, err)
	}

	// 3. Receipt-only recovery of Job 2: must reflect durable disposition (Failed, not silently scope Partial)
	restartSvc := New(platformDB)
	recoveryDeps := ExecutionDeps{
		Gate:    batchFakeGate(fix2.snapshot),
		Acquire: acquire,
		Timeout: 10 * time.Second,
	}
	recOutcome2, err := restartSvc.ReconcileBatchJobByID(ctx, fix2.claim.JobID, recoveryDeps)
	if err != nil || recOutcome2 == nil {
		t.Fatalf("ReconcileBatchJobByID job 2: outcome=%+v err=%v", recOutcome2, err)
	}
	if recOutcome2.Status != executioncontract.Failed || !recOutcome2.Reconciled {
		t.Fatalf("recovery outcome must reflect durable failed disposition for aborted resume, got: %+v", recOutcome2)
	}

	// Verify job 2 durable disposition in app.jobs
	var j2Status, j2ExecStatus string
	var j2Blocked bool
	if err := platformDB.QueryRowContext(ctx, "SELECT status, execution_status, blocks_database FROM app.jobs WHERE id = $1", fix2.claim.JobID).Scan(&j2Status, &j2ExecStatus, &j2Blocked); err != nil {
		t.Fatalf("query job 2: %v", err)
	}
	if j2Status != "failed" || j2ExecStatus != "failed" || j2Blocked {
		t.Fatalf("job 2 must be failed and blocks_database=false: status=%s execStatus=%s blocked=%t", j2Status, j2ExecStatus, j2Blocked)
	}

	// Scope aggregate partial/progress/block remains demonstrably intact through prior job:
	var j1Status, j1ExecStatus string
	var j1Blocked bool
	if err := platformDB.QueryRowContext(ctx, "SELECT status, execution_status, blocks_database FROM app.jobs WHERE id = $1", fix1.claim.JobID).Scan(&j1Status, &j1ExecStatus, &j1Blocked); err != nil {
		t.Fatalf("query job 1: %v", err)
	}
	if j1Status != "partial" || j1ExecStatus != "partial" || !j1Blocked {
		t.Fatalf("job 1 must retain scope partial and block: status=%s execStatus=%s blocked=%t", j1Status, j1ExecStatus, j1Blocked)
	}

	// 4. Repeated recovery must be idempotent Failed
	repeatedOutcome2, err := restartSvc.ReconcileBatchJobByID(ctx, fix2.claim.JobID, recoveryDeps)
	if err != nil || repeatedOutcome2 == nil {
		t.Fatalf("repeated recovery on job 2: outcome=%+v err=%v", repeatedOutcome2, err)
	}
	if repeatedOutcome2.Status != executioncontract.Failed || !repeatedOutcome2.Reconciled {
		t.Fatalf("repeated recovery must be idempotent failed: %+v", repeatedOutcome2)
	}
}

func TestExecuteBatchJob_Regression_LineageBlockers_MissingOrInconsistentAuthority_StaysBlocked(t *testing.T) {
	e, platformDB, ownerDB := setupBatchTestEnv(t)
	setupBatchItemsFixture(t, e)

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	if _, err := e.exec.ExecContext(ctx, "INSERT INTO managed.batch_items (id, processed) VALUES (1, false), (2, false)"); err != nil {
		t.Fatalf("insert items: %v", err)
	}

	acquire := makeBatchAcquire(t, e.exec, e.prep)
	svc := New(platformDB)

	// 1. Initial Job 1 (Apply): batchSize = 1, commits batch 0, crashes during batch 1 authorize
	fix1 := createBatchFixtureWithDuration(t, platformDB, ownerDB, e.target, 1, executioncontract.Apply, nil, 30*time.Second)
	cleanupBatchScope(t, e, fix1.executionID)

	crashFired := false
	deps1 := ExecutionDeps{
		Gate:    batchFakeGate(fix1.snapshot),
		Acquire: acquire,
		Timeout: 10 * time.Second,
		crashAfterBatchAuthorize: func(ctx context.Context, batchIndex int) error {
			if batchIndex == 1 {
				crashFired = true
				return errors.New("simulated crash during batch 1 authorize")
			}
			return nil
		},
	}
	outcome1, err := svc.ExecuteBatchJob(ctx, fix1.claim, deps1)
	if err == nil || !crashFired {
		t.Fatalf("expected batch 1 authorize crash, fired=%t outcome=%+v err=%v", crashFired, outcome1, err)
	}
	if _, err := platformDB.ExecContext(ctx, "UPDATE app.jobs SET lease_expires_at = clock_timestamp() - interval '5 seconds' WHERE id = $1", fix1.claim.JobID); err != nil {
		t.Fatalf("expire job 1 lease: %v", err)
	}
	recOutcome1, err := svc.ReconcileBatchJobByID(ctx, fix1.claim.JobID, ExecutionDeps{
		Gate:    batchFakeGate(fix1.snapshot),
		Acquire: acquire,
		Timeout: 10 * time.Second,
	})
	if err != nil || recOutcome1.Status != executioncontract.Partial {
		t.Fatalf("reconcile job 1: outcome=%+v err=%v", recOutcome1, err)
	}
	if _, err := platformDB.ExecContext(ctx, "UPDATE app.migration_proposals SET status = 'superseded' WHERE id = $1", fix1.proposalID); err != nil {
		t.Fatalf("supersede proposal 1: %v", err)
	}

	// 2. Insert earlier blocked jobs in same lineage with missing or inconsistent authority:
	// Job A: Missing authority (attempt sql_entered=true, but NO step authorizations)
	jobAID := uuid.NewString()
	attAID := uuid.NewString()
	if _, err := platformDB.ExecContext(ctx, `
		INSERT INTO app.jobs (id, execution_id, database_id, target_id, request_id, proposal_id, migration_id, revision, actor_id, action, direction, mode, status, execution_status, blocks_database, snapshot_hash, snapshot, attempt_no, source_execution_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 1, $8, 'resume', 'up', 'batch', 'partial', 'partial', true, $9, '{"batch":{"batch_size":1,"schema":"managed","table":"batch_items","cursor_column":"id"}}'::jsonb, 1, $10)
	`, jobAID, uuid.NewString(), fix1.dbID, e.target, uuid.NewString(), fix1.proposalID, fix1.draftID, fix1.actorID, fix1.claim.SnapshotHash, fix1.executionID); err != nil {
		t.Fatalf("insert job A: %v", err)
	}
	if _, err := platformDB.ExecContext(ctx, `
		INSERT INTO app.job_attempts (id, job_id, execution_id, database_id, migration_id, revision, actor_id, attempt_no, status, sql_entered)
		VALUES ($1, $2, $3, $4, $5, 1, $6, 1, 'partial', true)
	`, attAID, jobAID, fix1.executionID, fix1.dbID, fix1.draftID, fix1.actorID); err != nil {
		t.Fatalf("insert attempt A: %v", err)
	}

	// Job B: Inconsistent authority (step authorization with direction = 'down' instead of 'up')
	jobBID := uuid.NewString()
	attBID := uuid.NewString()
	authBID := uuid.NewString()
	if _, err := platformDB.ExecContext(ctx, `
		INSERT INTO app.jobs (id, execution_id, database_id, target_id, request_id, proposal_id, migration_id, revision, actor_id, action, direction, mode, status, execution_status, blocks_database, snapshot_hash, snapshot, attempt_no, source_execution_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 1, $8, 'resume', 'up', 'batch', 'partial', 'partial', true, $9, '{"batch":{"batch_size":1,"schema":"managed","table":"batch_items","cursor_column":"id"}}'::jsonb, 1, $10)
	`, jobBID, uuid.NewString(), fix1.dbID, e.target, uuid.NewString(), fix1.proposalID, fix1.draftID, fix1.actorID, fix1.claim.SnapshotHash, fix1.executionID); err != nil {
		t.Fatalf("insert job B: %v", err)
	}
	if _, err := platformDB.ExecContext(ctx, `
		INSERT INTO app.job_attempts (id, job_id, execution_id, database_id, migration_id, revision, actor_id, attempt_no, status, sql_entered)
		VALUES ($1, $2, $3, $4, $5, 1, $6, 1, 'partial', true)
	`, attBID, jobBID, fix1.executionID, fix1.dbID, fix1.draftID, fix1.actorID); err != nil {
		t.Fatalf("insert attempt B: %v", err)
	}
	hashB := sha256.Sum256([]byte("fake sql"))
	if _, err := ownerDB.ExecContext(ctx, `
		INSERT INTO app.job_batch_step_authorizations
		    (id, job_id, attempt_id, actor_id, job_execution_id, scope_execution_id,
		     physical_target_id, direction, batch_index, ordinal, chunk_ids, plan, sql_text, sql_sha256,
		     target_xid, target_version)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'down', 0, 1, '[1]'::jsonb, '{}'::jsonb, 'fake sql', $8, '99999', 1)
	`, authBID, jobBID, attBID, fix1.actorID, fix1.executionID, fix1.executionID, e.target, hashB[:]); err != nil {
		t.Fatalf("insert auth B: %v", err)
	}

	// Job C: Inconsistent physical target
	var otherTargetID string
	if err := platformDB.QueryRowContext(ctx, "SELECT id::text FROM app.migration_target_state WHERE id <> $1 LIMIT 1", e.target).Scan(&otherTargetID); err != nil {
		t.Fatalf("query other target: %v", err)
	}
	jobCID := uuid.NewString()
	attCID := uuid.NewString()
	authCID := uuid.NewString()
	if _, err := platformDB.ExecContext(ctx, `
		INSERT INTO app.jobs (id, execution_id, database_id, target_id, request_id, proposal_id, migration_id, revision, actor_id, action, direction, mode, status, execution_status, blocks_database, snapshot_hash, snapshot, attempt_no, source_execution_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 1, $8, 'resume', 'up', 'batch', 'partial', 'partial', true, $9, '{"batch":{"batch_size":1,"schema":"managed","table":"batch_items","cursor_column":"id"}}'::jsonb, 1, $10)
	`, jobCID, uuid.NewString(), fix1.dbID, e.target, uuid.NewString(), fix1.proposalID, fix1.draftID, fix1.actorID, fix1.claim.SnapshotHash, fix1.executionID); err != nil {
		t.Fatalf("insert job C: %v", err)
	}
	if _, err := platformDB.ExecContext(ctx, `
		INSERT INTO app.job_attempts (id, job_id, execution_id, database_id, migration_id, revision, actor_id, attempt_no, status, sql_entered)
		VALUES ($1, $2, $3, $4, $5, 1, $6, 1, 'partial', true)
	`, attCID, jobCID, fix1.executionID, fix1.dbID, fix1.draftID, fix1.actorID); err != nil {
		t.Fatalf("insert attempt C: %v", err)
	}
	hashC := sha256.Sum256([]byte("fake sql c"))
	if _, err := ownerDB.ExecContext(ctx, `
		INSERT INTO app.job_batch_step_authorizations
		    (id, job_id, attempt_id, actor_id, job_execution_id, scope_execution_id,
		     physical_target_id, direction, batch_index, ordinal, chunk_ids, plan, sql_text, sql_sha256,
		     target_xid, target_version)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'up', 1, 2, '[2]'::jsonb, '{}'::jsonb, 'fake sql c', $8, '88888', 1)
	`, authCID, jobCID, attCID, fix1.actorID, fix1.executionID, fix1.executionID, otherTargetID, hashC[:]); err != nil {
		t.Fatalf("insert auth C: %v", err)
	}

	// 3. Final Resume executes to completion (commits remaining batch 1)
	fixFinal := createBatchFixtureWithDuration(t, platformDB, ownerDB, e.target, 1, executioncontract.Resume, &fix1, 30*time.Second)
	finalSvc := New(platformDB)
	finalDeps := ExecutionDeps{
		Gate:    batchFakeGate(fixFinal.snapshot),
		Acquire: acquire,
		Timeout: 10 * time.Second,
	}
	finalOutcome, err := finalSvc.ExecuteBatchJob(ctx, fixFinal.claim, finalDeps)
	if err != nil || finalOutcome == nil || finalOutcome.Status != executioncontract.Succeeded {
		t.Fatalf("final resume execution failed: outcome=%+v err=%v", finalOutcome, err)
	}

	// 4. Assert:
	// fix1 (valid contributor with full proof resolution) must be unblocked and execution_status=succeeded:
	var j1Status, j1ExecStatus string
	var j1Blocked bool
	if err := platformDB.QueryRowContext(ctx, "SELECT status, execution_status, blocks_database FROM app.jobs WHERE id = $1", fix1.claim.JobID).Scan(&j1Status, &j1ExecStatus, &j1Blocked); err != nil {
		t.Fatalf("query job 1: %v", err)
	}
	if j1Status != "partial" || j1ExecStatus != "succeeded" || j1Blocked {
		t.Fatalf("job 1 must be unblocked and execution_status=succeeded: status=%s execStatus=%s blocked=%t", j1Status, j1ExecStatus, j1Blocked)
	}

	// Job A (missing authority) MUST REMAIN BLOCKED:
	var jABlocked bool
	var jAExecStatus string
	if err := platformDB.QueryRowContext(ctx, "SELECT execution_status, blocks_database FROM app.jobs WHERE id = $1", jobAID).Scan(&jAExecStatus, &jABlocked); err != nil {
		t.Fatalf("query job A: %v", err)
	}
	if !jABlocked || jAExecStatus == "succeeded" {
		t.Fatalf("job A with missing authority must remain blocked: blocked=%t execStatus=%s", jABlocked, jAExecStatus)
	}

	// Job B (inconsistent authority - wrong direction) MUST REMAIN BLOCKED:
	var jBBlocked bool
	var jBExecStatus string
	if err := platformDB.QueryRowContext(ctx, "SELECT execution_status, blocks_database FROM app.jobs WHERE id = $1", jobBID).Scan(&jBExecStatus, &jBBlocked); err != nil {
		t.Fatalf("query job B: %v", err)
	}
	if !jBBlocked || jBExecStatus == "succeeded" {
		t.Fatalf("job B with inconsistent authority must remain blocked: blocked=%t execStatus=%s", jBBlocked, jBExecStatus)
	}

	// Job C (incomplete proof) MUST REMAIN BLOCKED:
	var jCBlocked bool
	var jCExecStatus string
	if err := platformDB.QueryRowContext(ctx, "SELECT execution_status, blocks_database FROM app.jobs WHERE id = $1", jobCID).Scan(&jCExecStatus, &jCBlocked); err != nil {
		t.Fatalf("query job C: %v", err)
	}
	if !jCBlocked || jCExecStatus == "succeeded" {
		t.Fatalf("job C with incomplete proof must remain blocked: blocked=%t execStatus=%s", jCBlocked, jCExecStatus)
	}
}

func TestExecuteBatchJob_Regression_ManyJobClosureBound_BoundedMemoryAndCancellation(t *testing.T) {
	e, platformDB, ownerDB := setupBatchTestEnv(t)
	setupBatchItemsFixture(t, e)

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	if _, err := e.exec.ExecContext(ctx, "INSERT INTO managed.batch_items (id, processed) VALUES (1, false), (2, false)"); err != nil {
		t.Fatalf("insert items: %v", err)
	}

	acquire := makeBatchAcquire(t, e.exec, e.prep)

	fix1 := createBatchFixtureWithDuration(t, platformDB, ownerDB, e.target, 2, executioncontract.Apply, nil, 100*time.Millisecond)
	cleanupBatchScope(t, e, fix1.executionID)

	ident := BatchScopeIdentity{
		ExecutionID: fix1.executionID,
		TargetID:    fix1.targetID,
		DatabaseID:  fix1.dbID,
		ActorID:     fix1.actorID,
	}
	svcScope := New(platformDB)
	if _, err := svcScope.FreezeBatchScope(ctx, e.prep, ident); err != nil {
		t.Fatalf("freeze batch scope: %v", err)
	}

	// Set small page limit = 2 to enforce bounded multi-item pages
	oldRecLimit := recoveryPageLimit
	recoveryPageLimit = 2
	t.Cleanup(func() {
		recoveryPageLimit = oldRecLimit
	})

	// Insert 5 originating jobs with expired leases under the scope
	jobIDs := make([]string, 5)
	attIDs := make([]string, 5)
	for i := 0; i < 5; i++ {
		jID := uuid.NewString()
		aID := uuid.NewString()
		jobIDs[i] = jID
		attIDs[i] = aID
		if _, err := platformDB.ExecContext(ctx, `
			INSERT INTO app.jobs (id, execution_id, database_id, target_id, request_id, proposal_id, migration_id, revision, actor_id, action, direction, mode, status, execution_status, blocks_database, snapshot_hash, snapshot, attempt_no, source_execution_id, lease_expires_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, 1, $8, 'resume', 'up', 'batch', 'running', 'running', true, $9, '{"batch":{"batch_size":2,"schema":"managed","table":"batch_items","cursor_column":"id"}}'::jsonb, 1, $10, clock_timestamp() - interval '1 second')
		`, jID, uuid.NewString(), fix1.dbID, e.target, uuid.NewString(), fix1.proposalID, fix1.draftID, fix1.actorID, fix1.claim.SnapshotHash, fix1.executionID); err != nil {
			t.Fatalf("insert job %d: %v", i, err)
		}
		if _, err := platformDB.ExecContext(ctx, `
			INSERT INTO app.job_attempts (id, job_id, execution_id, database_id, migration_id, revision, actor_id, attempt_no, status, sql_entered)
			VALUES ($1, $2, $3, $4, $5, 1, $6, 1, 'running', false)
		`, aID, jID, fix1.executionID, fix1.dbID, fix1.draftID, fix1.actorID); err != nil {
			t.Fatalf("insert attempt %d: %v", i, err)
		}
	}

	txA, err := e.exec.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin txA: %v", err)
	}
	var xidA string
	if err := txA.QueryRowContext(ctx, "SELECT pg_current_xact_id()::text").Scan(&xidA); err != nil {
		t.Fatalf("query xidA: %v", err)
	}
	if err := txA.Rollback(); err != nil {
		t.Fatalf("rollback txA: %v", err)
	}

	planJSON, _ := json.Marshal(executioncontract.ExecutionPlan{
		Snapshot: fix1.snapshot,
		Version:  "1",
	})
	hash := sha256.Sum256([]byte("UPDATE managed.batch_items SET processed = true WHERE id = ANY($1::bigint[]);"))
	if _, err := ownerDB.ExecContext(ctx, `
		INSERT INTO app.job_batch_step_authorizations
		    (job_id, attempt_id, actor_id, job_execution_id, scope_execution_id,
		     physical_target_id, direction, batch_index, ordinal, chunk_ids, plan, sql_text, sql_sha256,
		     target_xid, target_version)
		VALUES ($1, $2, $3, $4, $5, $6, 'up', 0, 1, '[1, 2]'::jsonb, $7::jsonb, $8, $9, $10, 1)
	`, jobIDs[0], attIDs[0], fix1.claim.ActorID, fix1.claim.ExecutionID, fix1.executionID,
		e.target, string(planJSON),
		"UPDATE managed.batch_items SET processed = true WHERE id = ANY($1::bigint[]);", hash[:],
		xidA); err != nil {
		t.Fatalf("insert auth: %v", err)
	}
	if _, err := ownerDB.ExecContext(ctx, `
		INSERT INTO app.job_statement_evidence
		    (attempt_id, ordinal, batch_index, sql_text, safe_parameters, outcome, executed_at)
		VALUES ($1::uuid, 1, 0, $2, '{"ids": [1, 2]}'::jsonb, 'unknown', clock_timestamp())
	`, attIDs[0], "UPDATE managed.batch_items SET processed = true WHERE id = ANY($1::bigint[]);"); err != nil {
		t.Fatalf("insert statement evidence: %v", err)
	}
	time.Sleep(300 * time.Millisecond)

	restartSvc := New(platformDB)
	// Phase 1: Test cancellation on page 1 (second page of attempts)
	var (
		pagesSeen []int
		maxPageSz int
	)
	runCtx, runCancel := context.WithCancel(ctx)
	depsCancel := ExecutionDeps{
		Gate:    batchFakeGate(fix1.snapshot),
		Acquire: acquire,
		Timeout: 10 * time.Second,
		beforeRecoveryClosurePage: func(c context.Context, kind string, pageIndex int, pageSize int) error {
			if pageSize > maxPageSz {
				maxPageSz = pageSize
			}
			if pageSize > 2 {
				t.Fatalf("expected bounded page size <= 2, got %d", pageSize)
			}
			if kind == "attempts" {
				pagesSeen = append(pagesSeen, pageIndex)
				if pageIndex == 1 {
					runCancel()
				}
			}
			return nil
		},
	}

	outcome, err := restartSvc.ReconcileBatchJobByID(runCtx, jobIDs[0], depsCancel)
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got outcome=%+v err=%v", outcome, err)
	}
	if len(pagesSeen) < 2 {
		t.Fatalf("expected at least 2 pages visited before cancel, got %v", pagesSeen)
	}
	if maxPageSz > 2 {
		t.Fatalf("page size exceeded bound 2: max=%d", maxPageSz)
	}

	// Verify rollback: all jobs remain 'running'
	for i, id := range jobIDs {
		var st string
		if err := platformDB.QueryRowContext(ctx, "SELECT status FROM app.jobs WHERE id = $1", id).Scan(&st); err != nil || st != "running" {
			t.Fatalf("job %d must remain running after rollback, got status=%s err=%v", i, st, err)
		}
	}

	// Phase 2: Run to completion without cancellation
	var totalPagesVisited int
	depsFull := ExecutionDeps{
		Gate:    batchFakeGate(fix1.snapshot),
		Acquire: acquire,
		Timeout: 10 * time.Second,
		beforeRecoveryClosurePage: func(c context.Context, kind string, pageIndex int, pageSize int) error {
			if pageSize > 2 {
				t.Fatalf("page size exceeded bound 2 during full run: %d", pageSize)
			}
			if kind == "attempts" {
				totalPagesVisited++
			}
			return nil
		},
	}

	outcomeFull, err := restartSvc.ReconcileBatchJobByID(ctx, jobIDs[0], depsFull)
	if err != nil || outcomeFull == nil || outcomeFull.Status != executioncontract.Failed {
		t.Fatalf("expected successful reconcile with failed disposition, got: outcome=%+v err=%v", outcomeFull, err)
	}
	if totalPagesVisited < 3 {
		t.Fatalf("expected at least 3 pages visited for 5+ jobs with limit 2, got %d", totalPagesVisited)
	}

	// All 5 jobs must now be cleanly closed as 'failed'
	for i, id := range jobIDs {
		var st string
		var bl bool
		if err := platformDB.QueryRowContext(ctx, "SELECT status, blocks_database FROM app.jobs WHERE id = $1", id).Scan(&st, &bl); err != nil || st != "failed" || bl {
			t.Fatalf("job %d must be cleanly closed failed and unblocked, got status=%s blocked=%t err=%v", i, st, bl, err)
		}
	}
}

func TestExecuteBatchJob_Regression_Terra1_ExtraUnknownStatementRetainsBlockAndTargetGuards(t *testing.T) {
	e, platformDB, ownerDB := setupBatchTestEnv(t)
	setupBatchItemsFixture(t, e)

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	if _, err := e.exec.ExecContext(ctx, "INSERT INTO managed.batch_items (id, processed) VALUES (1, false), (2, false)"); err != nil {
		t.Fatalf("insert items: %v", err)
	}

	acquire := makeBatchAcquire(t, e.exec, e.prep)
	svc := New(platformDB)

	// 1. Initial Job 1 (Apply): batchSize = 1, commits batch 0, crashes during batch 1 authorize
	fix1 := createBatchFixtureWithDuration(t, platformDB, ownerDB, e.target, 1, executioncontract.Apply, nil, 100*time.Millisecond)
	cleanupBatchScope(t, e, fix1.executionID)

	deps1 := ExecutionDeps{
		Gate:    batchFakeGate(fix1.snapshot),
		Acquire: acquire,
		Timeout: 10 * time.Second,
		crashAfterBatchAuthorize: func(ctx context.Context, batchIndex int) error {
			if batchIndex == 1 {
				return errors.New("simulated crash during batch 1 authorize")
			}
			return nil
		},
	}
	outcome1, err := svc.ExecuteBatchJob(ctx, fix1.claim, deps1)
	if err == nil {
		t.Fatalf("expected crash on job 1, got outcome=%+v", outcome1)
	}
	time.Sleep(150 * time.Millisecond)

	// Reconcile job 1 to partial
	recOutcome1, err := svc.ReconcileBatchJobByID(ctx, fix1.claim.JobID, ExecutionDeps{
		Gate:    batchFakeGate(fix1.snapshot),
		Acquire: acquire,
		Timeout: 10 * time.Second,
	})
	if err != nil || recOutcome1.Status != executioncontract.Partial {
		t.Fatalf("reconcile job 1: outcome=%+v err=%v", recOutcome1, err)
	}
	if _, err := platformDB.ExecContext(ctx, "UPDATE app.migration_proposals SET status = 'superseded' WHERE id = $1", fix1.proposalID); err != nil {
		t.Fatalf("supersede proposal 1: %v", err)
	}

	// 2. Insert Job X in same lineage: has 1 resolved step authorization (with proof)
	// BUT also has 1 extra unmatched unknown statement in app.job_statement_evidence!
	jobXID := uuid.NewString()
	attXID := uuid.NewString()
	authXID := uuid.NewString()
	if _, err := platformDB.ExecContext(ctx, `
		INSERT INTO app.jobs (id, execution_id, database_id, target_id, request_id, proposal_id, migration_id, revision, actor_id, action, direction, mode, status, execution_status, blocks_database, snapshot_hash, snapshot, attempt_no, source_execution_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 1, $8, 'resume', 'up', 'batch', 'partial', 'partial', true, $9, '{"batch":{"batch_size":1,"schema":"managed","table":"batch_items","cursor_column":"id"}}'::jsonb, 1, $10)
	`, jobXID, uuid.NewString(), fix1.dbID, e.target, uuid.NewString(), fix1.proposalID, fix1.draftID, fix1.actorID, fix1.claim.SnapshotHash, fix1.executionID); err != nil {
		t.Fatalf("insert job X: %v", err)
	}
	if _, err := platformDB.ExecContext(ctx, `
		INSERT INTO app.job_attempts (id, job_id, execution_id, database_id, migration_id, revision, actor_id, attempt_no, status, sql_entered)
		VALUES ($1, $2, $3, $4, $5, 1, $6, 1, 'partial', true)
	`, attXID, jobXID, fix1.executionID, fix1.dbID, fix1.draftID, fix1.actorID); err != nil {
		t.Fatalf("insert attempt X: %v", err)
	}
	var realXID string
	tx, err := e.exec.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin target tx: %v", err)
	}
	if err := tx.QueryRowContext(ctx, "SELECT pg_catalog.pg_current_xact_id()::text").Scan(&realXID); err != nil {
		t.Fatalf("query real xid: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback target tx: %v", err)
	}

	hashX := sha256.Sum256([]byte("UPDATE managed.batch_items SET processed = TRUE WHERE id = ANY($1::bigint[]);"))
	if _, err := ownerDB.ExecContext(ctx, `
		INSERT INTO app.job_batch_step_authorizations
		    (id, job_id, attempt_id, actor_id, job_execution_id, scope_execution_id,
		     physical_target_id, direction, batch_index, ordinal, chunk_ids, plan, sql_text, sql_sha256,
		     target_xid, target_version)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'up', 0, 1, '[1]'::jsonb, '{}'::jsonb, 'UPDATE managed.batch_items SET processed = TRUE WHERE id = ANY($1::bigint[]);', $8, $9, 1)
	`, authXID, jobXID, attXID, fix1.actorID, fix1.executionID, fix1.executionID, e.target, hashX[:], realXID); err != nil {
		t.Fatalf("insert auth X: %v", err)
	}
	// Statement evidence 1: matches step 0, ordinal 1, outcome = failed (documented confirmed abort)
	if _, err := platformDB.ExecContext(ctx, `
		INSERT INTO app.job_statement_evidence (attempt_id, ordinal, batch_index, sql_text, safe_parameters, outcome, row_count)
		VALUES ($1, 1, 0, 'UPDATE managed.batch_items SET processed = TRUE WHERE id = ANY($1::bigint[]);', '{"ids":[1]}'::jsonb, 'failed', 0)
	`, attXID); err != nil {
		t.Fatalf("insert statement 1 for X: %v", err)
	}
	// Statement evidence 2: EXTRA UNMATCHED statement with outcome = 'unknown'
	if _, err := platformDB.ExecContext(ctx, `
		INSERT INTO app.job_statement_evidence (attempt_id, ordinal, batch_index, sql_text, safe_parameters, outcome, row_count)
		VALUES ($1, 2, 1, 'UPDATE managed.batch_items SET processed = TRUE WHERE id = ANY($1::bigint[]);', '{"ids":[2]}'::jsonb, 'unknown', 1)
	`, attXID); err != nil {
		t.Fatalf("insert extra unknown statement for X: %v", err)
	}

	// Verify Job X blocks claim and dispatch right now:
	unrelatedJobID := uuid.NewString()
	unrelatedAttemptID := uuid.NewString()
	unrelatedLeaseToken := uuid.NewString()
	if _, err := platformDB.ExecContext(ctx, `
		INSERT INTO app.jobs (id, execution_id, database_id, target_id, request_id, proposal_id, migration_id, revision, actor_id, action, direction, mode, status, execution_status, blocks_database, snapshot_hash, snapshot, attempt_no, lease_token, lease_expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 1, $8, 'apply', 'up', 'batch', 'running', 'running', false, $9, '{}'::jsonb, 1, $10, clock_timestamp() + interval '30 seconds')
	`, unrelatedJobID, uuid.NewString(), fix1.dbID, e.target, uuid.NewString(), fix1.proposalID, fix1.draftID, fix1.actorID, fix1.claim.SnapshotHash, unrelatedLeaseToken); err != nil {
		t.Fatalf("insert running unrelated job: %v", err)
	}
	if _, err := platformDB.ExecContext(ctx, `
		INSERT INTO app.job_attempts (id, job_id, execution_id, database_id, migration_id, revision, actor_id, attempt_no, status, sql_entered)
		VALUES ($1, $2, $3, $4, $5, 1, $6, 1, 'running', false)
	`, unrelatedAttemptID, unrelatedJobID, uuid.NewString(), fix1.dbID, fix1.draftID, fix1.actorID); err != nil {
		t.Fatalf("insert running unrelated attempt: %v", err)
	}

	unrelatedClaim := &Claim{
		JobID:             unrelatedJobID,
		ExecutionID:       uuid.NewString(),
		DatabaseID:        fix1.dbID,
		TargetID:          e.target,
		ProposalID:        fix1.proposalID,
		MigrationID:       fix1.draftID,
		Revision:          1,
		ActorID:           fix1.actorID,
		Action:            executioncontract.Apply,
		Direction:         executioncontract.Up,
		Mode:              executioncontract.Batch,
		LeaseToken:        unrelatedLeaseToken,
		AttemptNo:         1,
		AttemptID:         unrelatedAttemptID,
		SnapshotHash:      fix1.claim.SnapshotHash,
	}

	dispatchErr := svc.checkBatchLeaseAndTargetBlock(ctx, unrelatedClaim)
	if dispatchErr == nil {
		t.Fatalf("dispatch guard must reject when Job X has extra unknown statement, expected TARGET_BLOCKED")
	}
	var cErr executioncontract.ContractError
	if !errors.As(dispatchErr, &cErr) || cErr.Code != "TARGET_BLOCKED" {
		t.Fatalf("expected TARGET_BLOCKED, got %v", dispatchErr)
	}

	// Also verify a queued apply job cannot be claimed:
	queuedJobID := uuid.NewString()
	if _, err := platformDB.ExecContext(ctx, `
		INSERT INTO app.jobs (id, execution_id, database_id, target_id, request_id, proposal_id, migration_id, revision, actor_id, action, direction, mode, status, execution_status, blocks_database, snapshot_hash, snapshot, attempt_no)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 1, $8, 'apply', 'up', 'batch', 'queued', 'queued', false, $9, '{}'::jsonb, 1)
	`, queuedJobID, uuid.NewString(), fix1.dbID, e.target, uuid.NewString(), fix1.proposalID, fix1.draftID, fix1.actorID, fix1.claim.SnapshotHash); err != nil {
		t.Fatalf("insert queued apply job: %v", err)
	}
	claimed, claimErr := svc.ClaimJob(ctx, ClaimRequest{WorkerID: "worker-test", LeaseDuration: 10 * time.Second, MaxAttempts: 5})
	if claimErr != nil {
		t.Fatalf("ClaimJob error: %v", claimErr)
	}
	if claimed != nil && claimed.JobID == queuedJobID {
		t.Fatalf("claim must not bypass target block from Job X with extra unknown statement")
	}
	if _, err := platformDB.ExecContext(ctx, `UPDATE app.jobs SET status = 'failed', execution_status = 'failed', finished_at = clock_timestamp() WHERE id IN ($1, $2)`, unrelatedJobID, queuedJobID); err != nil {
		t.Fatalf("close probe jobs: %v", err)
	}
	if _, err := platformDB.ExecContext(ctx, `UPDATE app.job_attempts SET status = 'failed', finished_at = clock_timestamp() WHERE id = $1`, unrelatedAttemptID); err != nil {
		t.Fatalf("close probe attempt: %v", err)
	}
	// 3. Final Resume executes to completion (commits remaining batch 1)
	fixFinal := createBatchFixtureWithDuration(t, platformDB, ownerDB, e.target, 1, executioncontract.Resume, &fix1, 30*time.Second)
	finalSvc := New(platformDB)
	finalDeps := ExecutionDeps{
		Gate:    batchFakeGate(fixFinal.snapshot),
		Acquire: acquire,
		Timeout: 10 * time.Second,
	}
	finalOutcome, err := finalSvc.ExecuteBatchJob(ctx, fixFinal.claim, finalDeps)
	if err != nil || finalOutcome == nil || finalOutcome.Status != executioncontract.Succeeded {
		t.Fatalf("final resume execution failed: outcome=%+v err=%v", finalOutcome, err)
	}

	// 4. Assert Job X MUST REMAIN BLOCKED (must not have block cleared):
	var jXBlocked bool
	var jXExecStatus string
	if err := platformDB.QueryRowContext(ctx, "SELECT execution_status, blocks_database FROM app.jobs WHERE id = $1", jobXID).Scan(&jXExecStatus, &jXBlocked); err != nil {
		t.Fatalf("query job X: %v", err)
	}
	if !jXBlocked || jXExecStatus == "succeeded" {
		t.Fatalf("job X with extra unmatched unknown statement must remain blocked: blocked=%t execStatus=%s", jXBlocked, jXExecStatus)
	}
}

func TestExecuteBatchJob_Regression_Terra2_CleanFailedSourceReflectsCompletedScopeProgress(t *testing.T) {
	e, platformDB, ownerDB := setupBatchTestEnv(t)
	setupBatchItemsFixture(t, e)

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	if _, err := e.exec.ExecContext(ctx, "INSERT INTO managed.batch_items (id, processed) VALUES (1, false), (2, false)"); err != nil {
		t.Fatalf("insert items: %v", err)
	}

	acquire := makeBatchAcquire(t, e.exec, e.prep)
	svc := New(platformDB)

	// 1. Initial Job 1 (Apply): cleanly fails before any commit (blocks_database=false, status=failed, execution_status=failed)
	fix1 := createBatchFixtureWithDuration(t, platformDB, ownerDB, e.target, 1, executioncontract.Apply, nil, 100*time.Millisecond)
	cleanupBatchScope(t, e, fix1.executionID)
	ident := BatchScopeIdentity{
		ExecutionID: fix1.executionID,
		TargetID:    fix1.targetID,
		DatabaseID:  fix1.dbID,
		ActorID:     fix1.actorID,
	}
	if _, err := svc.FreezeBatchScope(ctx, e.prep, ident); err != nil {
		t.Fatalf("freeze batch scope: %v", err)
	}

	// Fail Job 1 cleanly
	if _, err := platformDB.ExecContext(ctx, `
		UPDATE app.jobs
		SET status = 'failed',
		    execution_status = 'failed',
		    blocks_database = false,
		    error_code = 'PREFLIGHT_FAILED',
		    error_message = 'clean failure before commit',
		    finished_at = clock_timestamp(),
		    updated_at = clock_timestamp()
		WHERE id = $1
	`, fix1.claim.JobID); err != nil {
		t.Fatalf("fail job 1 cleanly: %v", err)
	}
	if _, err := platformDB.ExecContext(ctx, `
		UPDATE app.job_attempts
		SET status = 'failed',
		    error_code = 'PREFLIGHT_FAILED',
		    error_message = 'clean failure before commit',
		    finished_at = clock_timestamp()
		WHERE id = $1
	`, fix1.claim.AttemptID); err != nil {
		t.Fatalf("fail attempt 1 cleanly: %v", err)
	}

	// 2. Also insert an unrelated independent blocked job on another target to verify independently unresolved blocks are preserved
	otherTargetID := uuid.NewString()
	if _, err := ownerDB.ExecContext(ctx, "INSERT INTO app.migration_target_state (id, next_version) VALUES ($1, 1)", otherTargetID); err != nil {
		t.Fatalf("insert other target state: %v", err)
	}
	t.Cleanup(func() {
		_, _ = ownerDB.ExecContext(context.Background(), "DELETE FROM app.migration_target_state WHERE id = $1", otherTargetID)
	})

	unrelatedJobID := uuid.NewString()
	if _, err := platformDB.ExecContext(ctx, `
		INSERT INTO app.jobs (id, execution_id, database_id, target_id, request_id, proposal_id, migration_id, revision, actor_id, action, direction, mode, status, execution_status, blocks_database, snapshot_hash, snapshot, attempt_no, source_execution_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 1, $8, 'apply', 'up', 'batch', 'partial', 'partial', true, 'unrelatedhash', '{}'::jsonb, 1, $9)
	`, unrelatedJobID, uuid.NewString(), fix1.dbID, otherTargetID, uuid.NewString(), fix1.proposalID, fix1.draftID, fix1.actorID, fix1.executionID); err != nil {
		t.Fatalf("insert unrelated job: %v", err)
	}
	// 3. Resume Job 2 executes and completes the entire scope (both batches 0 and 1)
	fix2 := createBatchFixtureWithDuration(t, platformDB, ownerDB, e.target, 1, executioncontract.Resume, &fix1, 30*time.Second)
	outcome2, err := svc.ExecuteBatchJob(ctx, fix2.claim, ExecutionDeps{
		Gate:    batchFakeGate(fix2.snapshot),
		Acquire: acquire,
		Timeout: 10 * time.Second,
	})
	if err != nil || outcome2 == nil || outcome2.Status != executioncontract.Succeeded {
		t.Fatalf("resume execution failed: outcome=%+v err=%v", outcome2, err)
	}

	// 4. Assert:
	// Source Job 1 (cleanly failed and already unblocked) MUST reflect completed scope:
	var j1Status, j1ExecStatus string
	var j1Blocked bool
	var j1Progress []byte
	if err := platformDB.QueryRowContext(ctx, `
		SELECT status, execution_status, blocks_database, progress
		FROM app.jobs WHERE id = $1
	`, fix1.claim.JobID).Scan(&j1Status, &j1ExecStatus, &j1Blocked, &j1Progress); err != nil {
		t.Fatalf("query job 1: %v", err)
	}
	if j1Status != "failed" {
		t.Fatalf("job 1 historical disposition must remain 'failed', got: %s", j1Status)
	}
	if j1ExecStatus != "succeeded" {
		t.Fatalf("job 1 aggregate execution_status must be 'succeeded', got: %s", j1ExecStatus)
	}
	if j1Blocked {
		t.Fatalf("job 1 must remain unblocked, got blocked=%t", j1Blocked)
	}
	if len(j1Progress) == 0 {
		t.Fatalf("job 1 progress must reflect completed scope, got empty")
	}
	var p executioncontract.Progress
	if err := json.Unmarshal(j1Progress, &p); err != nil || p.BatchesCommitted != "2" {
		t.Fatalf("expected 2 batches committed in job 1 progress, got: %+v (err=%v)", p, err)
	}

	// Unrelated independently unresolved job MUST PRESERVE ITS BLOCK:
	var unrlStatus, unrlExecStatus string
	var unrlBlocked bool
	if err := platformDB.QueryRowContext(ctx, `
		SELECT status, execution_status, blocks_database
		FROM app.jobs WHERE id = $1
	`, unrelatedJobID).Scan(&unrlStatus, &unrlExecStatus, &unrlBlocked); err != nil {
		t.Fatalf("query unrelated job: %v", err)
	}
	if !unrlBlocked || unrlExecStatus == "succeeded" {
		t.Fatalf("unrelated job must preserve its unresolved block: blocked=%t execStatus=%s", unrlBlocked, unrlExecStatus)
	}
}

func TestExecuteBatchJob_Regression_Terra3_RepeatedReceiptRecoveryReturnsDurableStatus(t *testing.T) {
	e, platformDB, ownerDB := setupBatchTestEnv(t)
	setupBatchItemsFixture(t, e)

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	if _, err := e.exec.ExecContext(ctx, "INSERT INTO managed.batch_items (id, processed) VALUES (1, false), (2, false), (3, false)"); err != nil {
		t.Fatalf("insert items: %v", err)
	}

	acquire := makeBatchAcquire(t, e.exec, e.prep)
	svc := New(platformDB)

	// 1. Job 1 (Apply): commits batch 0, crashes during batch 1 -> closed as partial
	fix1 := createBatchFixtureWithDuration(t, platformDB, ownerDB, e.target, 1, executioncontract.Apply, nil, 30*time.Second)
	cleanupBatchScope(t, e, fix1.executionID)

	var crashRan bool
	deps1 := ExecutionDeps{
		Gate:    batchFakeGate(fix1.snapshot),
		Acquire: acquire,
		Timeout: 10 * time.Second,
		crashAfterBatchAuthorize: func(ctx context.Context, batchIndex int) error {
			if batchIndex == 1 {
				crashRan = true
				return errors.New("simulated crash during batch 1")
			}
			return nil
		},
	}
	if outcome, err := svc.ExecuteBatchJob(ctx, fix1.claim, deps1); err == nil || !crashRan {
		t.Fatalf("expected batch 1 crash hook, got outcome=%+v err=%v hook=%t", outcome, err, crashRan)
	}
	if _, err := platformDB.ExecContext(ctx, "UPDATE app.jobs SET lease_expires_at = clock_timestamp() - interval '5 seconds' WHERE id = $1", fix1.claim.JobID); err != nil {
		t.Fatalf("expire job lease: %v", err)
	}

	rec1, err := svc.ReconcileBatchJobByID(ctx, fix1.claim.JobID, ExecutionDeps{Gate: batchFakeGate(fix1.snapshot), Acquire: acquire, Timeout: 10 * time.Second})
	if err != nil || rec1.Status != executioncontract.Partial {
		t.Fatalf("expected partial for job 1, got outcome=%+v err=%v", rec1, err)
	}
	if _, err := platformDB.ExecContext(ctx, "UPDATE app.migration_proposals SET status = 'superseded' WHERE id = $1", fix1.proposalID); err != nil {
		t.Fatalf("supersede proposal 1: %v", err)
	}

	// 2. Job 2 (Resume 1): cleanly fails before any commit (clean failed contributor)
	fix2 := createBatchFixtureWithDuration(t, platformDB, ownerDB, e.target, 1, executioncontract.Resume, &fix1, 100*time.Millisecond)
	if _, err := platformDB.ExecContext(ctx, `
		UPDATE app.jobs
		SET status = 'failed', execution_status = 'failed', blocks_database = false,
		    error_code = 'PREFLIGHT_ERROR', error_message = 'clean failure on resume 1',
		    finished_at = clock_timestamp(), updated_at = clock_timestamp()
		WHERE id = $1
	`, fix2.claim.JobID); err != nil {
		t.Fatalf("set job 2 failed: %v", err)
	}
	if _, err := platformDB.ExecContext(ctx, `
		UPDATE app.job_attempts
		SET status = 'failed', error_code = 'PREFLIGHT_ERROR', error_message = 'clean failure on resume 1',
		    finished_at = clock_timestamp()
		WHERE id = $1
	`, fix2.claim.AttemptID); err != nil {
		t.Fatalf("set attempt 2 failed: %v", err)
	}
	if _, err := platformDB.ExecContext(ctx, "UPDATE app.migration_proposals SET status = 'superseded' WHERE id = $1", fix2.proposalID); err != nil {
		t.Fatalf("supersede proposal 2: %v", err)
	}

	// 3. Job 3 (Resume 2): commits remaining batches 1 and 2, completing the scope!
	fix3 := createBatchFixtureWithDuration(t, platformDB, ownerDB, e.target, 1, executioncontract.Resume, &fix1, 30*time.Second)
	outcome3, err := svc.ExecuteBatchJob(ctx, fix3.claim, ExecutionDeps{
		Gate:    batchFakeGate(fix3.snapshot),
		Acquire: acquire,
		Timeout: 10 * time.Second,
	})
	if err != nil || outcome3 == nil || outcome3.Status != executioncontract.Succeeded {
		t.Fatalf("final resume execution failed: outcome=%+v err=%v", outcome3, err)
	}

	// Verify DB state:
	var j1St, j1Ex string
	if err := platformDB.QueryRowContext(ctx, "SELECT status, execution_status FROM app.jobs WHERE id = $1", fix1.claim.JobID).Scan(&j1St, &j1Ex); err != nil {
		t.Fatalf("query j1: %v", err)
	}
	if j1St != "partial" || j1Ex != "succeeded" {
		t.Fatalf("j1 expected status=partial, exec_status=succeeded, got %s / %s", j1St, j1Ex)
	}

	var j2St, j2Ex string
	if err := platformDB.QueryRowContext(ctx, "SELECT status, execution_status FROM app.jobs WHERE id = $1", fix2.claim.JobID).Scan(&j2St, &j2Ex); err != nil {
		t.Fatalf("query j2: %v", err)
	}
	if j2St != "failed" || j2Ex != "succeeded" {
		t.Fatalf("j2 expected status=failed, exec_status=succeeded, got %s / %s", j2St, j2Ex)
	}

	// 4. Repeated receipt-only recovery by Job ID:
	recoveryDeps := ExecutionDeps{Timeout: 10 * time.Second}

	// Job 1 (partial contributor) MUST return Partial:
	recJob1_1, err := svc.ReconcileBatchJobByID(ctx, fix1.claim.JobID, recoveryDeps)
	if err != nil || recJob1_1 == nil || recJob1_1.Status != executioncontract.Partial || !recJob1_1.Reconciled {
		t.Fatalf("job 1 recovery must return durable Partial, got: %+v err=%v", recJob1_1, err)
	}
	recJob1_2, err := svc.ReconcileBatchJobByID(ctx, fix1.claim.JobID, recoveryDeps)
	if err != nil || recJob1_2 == nil || recJob1_2.Status != executioncontract.Partial || !recJob1_2.Reconciled {
		t.Fatalf("repeated job 1 recovery must remain durable Partial, got: %+v err=%v", recJob1_2, err)
	}

	// Job 2 (clean failed contributor) MUST return Failed:
	recJob2_1, err := svc.ReconcileBatchJobByID(ctx, fix2.claim.JobID, recoveryDeps)
	if err != nil || recJob2_1 == nil || recJob2_1.Status != executioncontract.Failed || !recJob2_1.Reconciled {
		t.Fatalf("job 2 recovery must return durable Failed, got: %+v err=%v", recJob2_1, err)
	}
	recJob2_2, err := svc.ReconcileBatchJobByID(ctx, fix2.claim.JobID, recoveryDeps)
	if err != nil || recJob2_2 == nil || recJob2_2.Status != executioncontract.Failed || !recJob2_2.Reconciled {
		t.Fatalf("repeated job 2 recovery must remain durable Failed, got: %+v err=%v", recJob2_2, err)
	}

	// Job 3 (succeeded job) MUST return Succeeded:
	recJob3, err := svc.ReconcileBatchJobByID(ctx, fix3.claim.JobID, recoveryDeps)
	if err != nil || recJob3 == nil || recJob3.Status != executioncontract.Succeeded || !recJob3.Reconciled {
		t.Fatalf("job 3 recovery must return Succeeded, got: %+v err=%v", recJob3, err)
	}
}

func TestExecuteBatchJob_Regression_Terra4_BoundInconsistentStatementMemoryAndRejectDuplicates(t *testing.T) {
	e, platformDB, ownerDB := setupBatchTestEnv(t)
	setupBatchItemsFixture(t, e)

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	if _, err := e.exec.ExecContext(ctx, "INSERT INTO managed.batch_items (id, processed) VALUES (1, false), (2, false)"); err != nil {
		t.Fatalf("insert items: %v", err)
	}

	acquire := makeBatchAcquire(t, e.exec, e.prep)
	svc := New(platformDB)

	// Subtest 1: Duplicate / multiple candidate rows for originating batch -> fails closed with TARGET_BLOCKED
	t.Run("DuplicateCandidateRows_BoundedMemoryAndRejected", func(t *testing.T) {
		fix := createBatchFixtureWithDuration(t, platformDB, ownerDB, e.target, 1, executioncontract.Apply, nil, 100*time.Millisecond)
		cleanupBatchScope(t, e, fix.executionID)

		ident := BatchScopeIdentity{
			ExecutionID: fix.executionID,
			TargetID:    fix.targetID,
			DatabaseID:  fix.dbID,
			ActorID:     fix.actorID,
		}
		if _, err := svc.FreezeBatchScope(ctx, e.prep, ident); err != nil {
			t.Fatalf("freeze batch scope: %v", err)
		}
		if _, err := platformDB.ExecContext(ctx, "UPDATE app.jobs SET blocks_database = true WHERE id = $1", fix.claim.JobID); err != nil {
			t.Fatalf("update blocks_database: %v", err)
		}
		if _, err := platformDB.ExecContext(ctx, "UPDATE app.job_attempts SET sql_entered = true WHERE id = $1", fix.claim.AttemptID); err != nil {
			t.Fatalf("update sql_entered: %v", err)
		}

		var realXID string
		tx, err := e.exec.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("begin target tx: %v", err)
		}
		if err := tx.QueryRowContext(ctx, "SELECT pg_catalog.pg_current_xact_id()::text").Scan(&realXID); err != nil {
			t.Fatalf("query real xid: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit target tx: %v", err)
		}

		// Create checkpoint on target for batch 0
		if _, err := e.cleanup.ExecContext(ctx, `
			INSERT INTO wbd_auth.batch_checkpoints (execution_id, physical_target_id, target_xid, batch_index, direction, rows_affected)
			VALUES ($1::uuid, $2::uuid, $3::xid8, 0, 'up', 1)
		`, fix.executionID, e.target, realXID); err != nil {
			t.Fatalf("insert checkpoint: %v", err)
		}

		// Insert batch step auth for batch 0, ordinal 1, target_xid realXID
		authID := uuid.NewString()
		hash := sha256.Sum256([]byte("UPDATE managed.batch_items SET processed = TRUE WHERE id = ANY($1::bigint[]);"))
		if _, err := ownerDB.ExecContext(ctx, `
			INSERT INTO app.job_batch_step_authorizations
			    (id, job_id, attempt_id, actor_id, job_execution_id, scope_execution_id,
			     physical_target_id, direction, batch_index, ordinal, chunk_ids, plan, sql_text, sql_sha256,
			     target_xid, target_version)
			VALUES ($1, $2, $3, $4, $5, $6, $7, 'up', 0, 1, '[1]'::jsonb, '{}'::jsonb, 'UPDATE managed.batch_items SET processed = TRUE WHERE id = ANY($1::bigint[]);', $8, $9, 1)
		`, authID, fix.claim.JobID, fix.claim.AttemptID, fix.actorID, fix.executionID, fix.executionID, e.target, hash[:], realXID); err != nil {
			t.Fatalf("insert auth: %v", err)
		}

		// Insert commit proof for batch 0
		if _, err := ownerDB.ExecContext(ctx, `
			INSERT INTO app.job_batch_commit_proofs
			    (id, step_auth_id, job_id, attempt_id, scope_execution_id, physical_target_id, direction, batch_index, target_xid, row_count, committed_at)
			VALUES ($1, $2, $3, $4, $5, $6, 'up', 0, $7, 1, clock_timestamp())
		`, uuid.NewString(), authID, fix.claim.JobID, fix.claim.AttemptID, fix.executionID, e.target, realXID); err != nil {
			t.Fatalf("insert commit proof: %v", err)
		}

		// Insert 2 candidate rows in job_statement_evidence for this attempt!
		if _, err := platformDB.ExecContext(ctx, `
			INSERT INTO app.job_statement_evidence (attempt_id, ordinal, batch_index, sql_text, safe_parameters, outcome, row_count)
			VALUES ($1, 1, 0, 'UPDATE managed.batch_items SET processed = TRUE WHERE id = ANY($1::bigint[]);', '{"ids":[1]}'::jsonb, 'succeeded', 1)
		`, fix.claim.AttemptID); err != nil {
			t.Fatalf("insert statement 1: %v", err)
		}
		if _, err := platformDB.ExecContext(ctx, `
			INSERT INTO app.job_statement_evidence (attempt_id, ordinal, batch_index, sql_text, safe_parameters, outcome, row_count)
			VALUES ($1, 2, 0, 'UPDATE managed.batch_items SET processed = TRUE WHERE id = ANY($1::bigint[]);', '{"ids":[1]}'::jsonb, 'succeeded', 1)
		`, fix.claim.AttemptID); err != nil {
			t.Fatalf("insert statement 2: %v", err)
		}

		time.Sleep(150 * time.Millisecond)

		recOutcome, err := svc.ReconcileBatchJobByID(ctx, fix.claim.JobID, ExecutionDeps{
			Gate:    batchFakeGate(fix.snapshot),
			Acquire: acquire,
			Timeout: 10 * time.Second,
		})
		if err != nil {
			t.Fatalf("ReconcileBatchJobByID unexpected err: %v", err)
		}
		if recOutcome == nil || recOutcome.Status != executioncontract.Failed {
			t.Fatalf("expected TARGET_BLOCKED failure on duplicate candidate rows, got: outcome=%+v", recOutcome)
		}
		var cErr executioncontract.ContractError
		if !errors.As(recOutcome.Error, &cErr) || cErr.Code != "TARGET_BLOCKED" {
			t.Fatalf("expected TARGET_BLOCKED, got %v", recOutcome.Error)
		}
	})

	// Subtest 2: Mismatched extra ordinal/index in single candidate row -> fails closed with TARGET_BLOCKED
	t.Run("MismatchedOrdinalIndex_Rejected", func(t *testing.T) {
		fix := createBatchFixtureWithDuration(t, platformDB, ownerDB, e.target, 1, executioncontract.Apply, nil, 100*time.Millisecond)
		cleanupBatchScope(t, e, fix.executionID)

		ident := BatchScopeIdentity{
			ExecutionID: fix.executionID,
			TargetID:    fix.targetID,
			DatabaseID:  fix.dbID,
			ActorID:     fix.actorID,
		}
		if _, err := svc.FreezeBatchScope(ctx, e.prep, ident); err != nil {
			t.Fatalf("freeze batch scope: %v", err)
		}
		if _, err := platformDB.ExecContext(ctx, "UPDATE app.jobs SET blocks_database = true WHERE id = $1", fix.claim.JobID); err != nil {
			t.Fatalf("update blocks_database: %v", err)
		}
		if _, err := platformDB.ExecContext(ctx, "UPDATE app.job_attempts SET sql_entered = true WHERE id = $1", fix.claim.AttemptID); err != nil {
			t.Fatalf("update sql_entered: %v", err)
		}

		var realXID string
		tx, err := e.exec.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("begin target tx: %v", err)
		}
		if err := tx.QueryRowContext(ctx, "SELECT pg_catalog.pg_current_xact_id()::text").Scan(&realXID); err != nil {
			t.Fatalf("query real xid: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit target tx: %v", err)
		}

		// Create checkpoint on target for batch 0
		if _, err := e.cleanup.ExecContext(ctx, `
			INSERT INTO wbd_auth.batch_checkpoints (execution_id, physical_target_id, target_xid, batch_index, direction, rows_affected)
			VALUES ($1::uuid, $2::uuid, $3::xid8, 0, 'up', 1)
		`, fix.executionID, e.target, realXID); err != nil {
			t.Fatalf("insert checkpoint: %v", err)
		}

		// Insert batch step auth for batch 0, ordinal 1, target_xid realXID
		authID := uuid.NewString()
		hash := sha256.Sum256([]byte("UPDATE managed.batch_items SET processed = TRUE WHERE id = ANY($1::bigint[]);"))
		if _, err := ownerDB.ExecContext(ctx, `
			INSERT INTO app.job_batch_step_authorizations
			    (id, job_id, attempt_id, actor_id, job_execution_id, scope_execution_id,
			     physical_target_id, direction, batch_index, ordinal, chunk_ids, plan, sql_text, sql_sha256,
			     target_xid, target_version)
			VALUES ($1, $2, $3, $4, $5, $6, $7, 'up', 0, 1, '[1]'::jsonb, '{}'::jsonb, 'UPDATE managed.batch_items SET processed = TRUE WHERE id = ANY($1::bigint[]);', $8, $9, 1)
		`, authID, fix.claim.JobID, fix.claim.AttemptID, fix.actorID, fix.executionID, fix.executionID, e.target, hash[:], realXID); err != nil {
			t.Fatalf("insert auth: %v", err)
		}

		// Insert commit proof for batch 0
		if _, err := ownerDB.ExecContext(ctx, `
			INSERT INTO app.job_batch_commit_proofs
			    (id, step_auth_id, job_id, attempt_id, scope_execution_id, physical_target_id, direction, batch_index, target_xid, row_count, committed_at)
			VALUES ($1, $2, $3, $4, $5, $6, 'up', 0, $7, 1, clock_timestamp())
		`, uuid.NewString(), authID, fix.claim.JobID, fix.claim.AttemptID, fix.executionID, e.target, realXID); err != nil {
			t.Fatalf("insert commit proof: %v", err)
		}

		// Insert 1 statement evidence row where batch_index = 0, but ordinal = 99 (mismatched ordinal!)
		if _, err := platformDB.ExecContext(ctx, `
			INSERT INTO app.job_statement_evidence (attempt_id, ordinal, batch_index, sql_text, safe_parameters, outcome, row_count)
			VALUES ($1, 99, 0, 'UPDATE managed.batch_items SET processed = TRUE WHERE id = ANY($1::bigint[]);', '{"ids":[1]}'::jsonb, 'succeeded', 1)
		`, fix.claim.AttemptID); err != nil {
			t.Fatalf("insert mismatched statement: %v", err)
		}

		time.Sleep(150 * time.Millisecond)

		recOutcome, err := svc.ReconcileBatchJobByID(ctx, fix.claim.JobID, ExecutionDeps{
			Gate:    batchFakeGate(fix.snapshot),
			Acquire: acquire,
			Timeout: 10 * time.Second,
		})
		if err != nil {
			t.Fatalf("ReconcileBatchJobByID unexpected err: %v", err)
		}
		if recOutcome == nil || recOutcome.Status != executioncontract.Failed {
			t.Fatalf("expected TARGET_BLOCKED failure on mismatched ordinal, got: outcome=%+v", recOutcome)
		}
		var cErr executioncontract.ContractError
		if !errors.As(recOutcome.Error, &cErr) || cErr.Code != "TARGET_BLOCKED" {
			t.Fatalf("expected TARGET_BLOCKED, got %v", recOutcome.Error)
		}
	})
}

func TestExecuteBatchJob_Regression_Terra5_SameSourceIncompatibleLineage_NotClosed(t *testing.T) {
	e, platformDB, ownerDB := setupBatchTestEnv(t)
	setupBatchItemsFixture(t, e)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := e.exec.ExecContext(ctx, "INSERT INTO managed.batch_items (id, processed) VALUES (1, false), (2, false), (3, false), (4, false)"); err != nil {
		t.Fatalf("insert items: %v", err)
	}

	fix1 := createBatchFixture(t, platformDB, ownerDB, e.target, 2, executioncontract.Apply, nil)
	cleanupBatchScope(t, e, fix1.executionID)

	acquire := makeBatchAcquire(t, e.exec, e.prep)
	svc := New(platformDB)

	// Crash after batch 0 so fix1 has committed batch 0, is partial, and blocks_database = true
	deps1 := ExecutionDeps{
		Gate:    batchFakeGate(fix1.snapshot),
		Acquire: acquire,
		Timeout: 10 * time.Second,
		afterBatchCommit: func(ctx context.Context, batchIndex int) error {
			if batchIndex == 0 {
				return errors.New("simulated crash after batch 0")
			}
			return nil
		},
	}
	_, _ = svc.ExecuteBatchJob(ctx, fix1.claim, deps1)

	// Claim resume job for valid lineage
	fixResume := createBatchFixture(t, platformDB, ownerDB, e.target, 2, executioncontract.Resume, &fix1)
	cleanupBatchScope(t, e, fixResume.executionID)

	// Insert an incompatible job that references the same source_execution_id, but with mode = 'standard' and direction = 'down'
	incompatibleJobID := uuid.NewString()
	incompatibleExecID := uuid.NewString()
	incompatibleReqID := uuid.NewString()
	incompatibleAttID := uuid.NewString()
	if _, err := platformDB.ExecContext(ctx, `
		INSERT INTO app.jobs (
			id, request_id, database_id, target_id, proposal_id, migration_id, revision,
			execution_id, actor_id, action, direction, mode, status, execution_status,
			attempt_no, snapshot, snapshot_hash, blocks_database, source_execution_id,
			lease_expires_at
		) VALUES (
			$1, $2, $3, $4, $5, $6, 1,
			$7, $8, 'resume', 'down', 'batch', 'running', 'running',
			1, '{}'::jsonb, repeat('0', 64), false, $9,
			clock_timestamp() - interval '10 seconds'
		)
	`, incompatibleJobID, incompatibleReqID, fix1.dbID, e.target, fix1.proposalID, fix1.draftID,
		incompatibleExecID, fix1.actorID, fix1.executionID); err != nil {
		t.Fatalf("insert incompatible job: %v", err)
	}
	if _, err := platformDB.ExecContext(ctx, `
		INSERT INTO app.job_attempts (
			id, job_id, execution_id, database_id, migration_id, revision, actor_id, attempt_no, status
		) VALUES ($1, $2, $3, $4, $5, 1, $6, 1, 'running')
	`, incompatibleAttID, incompatibleJobID, incompatibleExecID, fix1.dbID, fix1.draftID, fix1.actorID); err != nil {
		t.Fatalf("insert incompatible attempt: %v", err)
	}
	t.Cleanup(func() {
		_, _ = ownerDB.ExecContext(context.Background(), "DELETE FROM app.job_attempts WHERE job_id = $1", incompatibleJobID)
		_, _ = ownerDB.ExecContext(context.Background(), "DELETE FROM app.jobs WHERE id = $1", incompatibleJobID)
	})

	// Execute resume for fix1: it will complete batch 1 and run applyOriginatingClosuresTx
	depsResume := ExecutionDeps{
		Gate:    batchFakeGate(fixResume.snapshot),
		Acquire: acquire,
		Timeout: 10 * time.Second,
	}
	outcomeResume, err := svc.ExecuteBatchJob(ctx, fixResume.claim, depsResume)
	if err != nil || outcomeResume == nil || outcomeResume.Status != executioncontract.Succeeded {
		t.Fatalf("expected resume to succeed, got outcome=%+v err=%v", outcomeResume, err)
	}

	// Lineage predicates must prevent closing the incompatible job/attempt:
	var incompJobStatus, incompExecStatus string
	var incompAttStatus string
	if err := platformDB.QueryRowContext(ctx, "SELECT status, execution_status FROM app.jobs WHERE id = $1", incompatibleJobID).Scan(&incompJobStatus, &incompExecStatus); err != nil {
		t.Fatalf("query incompatible job: %v", err)
	}
	if err := platformDB.QueryRowContext(ctx, "SELECT status FROM app.job_attempts WHERE id = $1", incompatibleAttID).Scan(&incompAttStatus); err != nil {
		t.Fatalf("query incompatible attempt: %v", err)
	}

	if incompJobStatus != "running" || incompExecStatus != "running" || incompAttStatus != "running" {
		t.Fatalf("incompatible lineage job should not have been closed: jobStatus=%s execStatus=%s attStatus=%s",
			incompJobStatus, incompExecStatus, incompAttStatus)
	}
}

func TestExecuteBatchJob_Regression_Terra5_AbortedXIDWithMalformedStatement_RetainsBlock(t *testing.T) {
	e, platformDB, ownerDB := setupBatchTestEnv(t)
	setupBatchItemsFixture(t, e)

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	if _, err := e.exec.ExecContext(ctx, "INSERT INTO managed.batch_items (id, processed) VALUES (1, false)"); err != nil {
		t.Fatalf("insert items: %v", err)
	}

	fix := createBatchFixtureWithDuration(t, platformDB, ownerDB, e.target, 1, executioncontract.Apply, nil, 100*time.Millisecond)
	cleanupBatchScope(t, e, fix.executionID)
	if _, err := platformDB.ExecContext(ctx, "UPDATE app.jobs SET blocks_database = true WHERE id = $1", fix.claim.JobID); err != nil {
		t.Fatalf("set blocks_database: %v", err)
	}
	acquire := makeBatchAcquire(t, e.exec, e.prep)
	svc := New(platformDB)
	ident := BatchScopeIdentity{
		ExecutionID: fix.executionID,
		TargetID:    fix.targetID,
		DatabaseID:  fix.dbID,
		ActorID:     fix.actorID,
	}
	if _, err := svc.FreezeBatchScope(ctx, e.prep, ident); err != nil {
		t.Fatalf("freeze batch scope: %v", err)
	}
	// Produce a real aborted XID on target PostgreSQL
	tx, err := e.exec.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin target tx: %v", err)
	}
	var realXID string
	if err := tx.QueryRowContext(ctx, "SELECT pg_current_xact_id()::text").Scan(&realXID); err != nil {
		t.Fatalf("query target xid: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback target tx: %v", err)
	}

	var xactStatus string
	if err := e.prep.QueryRowContext(ctx, "SELECT pg_catalog.pg_xact_status($1::xid8)", realXID).Scan(&xactStatus); err != nil || xactStatus != "aborted" {
		t.Fatalf("expected aborted target status, got: %s err=%v", xactStatus, err)
	}

	authID := uuid.NewString()
	hash := sha256.Sum256([]byte("UPDATE managed.batch_items SET processed = TRUE WHERE id = ANY($1::bigint[]);"))
	if _, err := ownerDB.ExecContext(ctx, `
		INSERT INTO app.job_batch_step_authorizations
		    (id, job_id, attempt_id, actor_id, job_execution_id, scope_execution_id,
		     physical_target_id, direction, batch_index, ordinal, chunk_ids, plan, sql_text, sql_sha256,
		     target_xid, target_version)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'up', 0, 1, '[1]'::jsonb, '{}'::jsonb, 'UPDATE managed.batch_items SET processed = TRUE WHERE id = ANY($1::bigint[]);', $8, $9, 1)
	`, authID, fix.claim.JobID, fix.claim.AttemptID, fix.actorID, fix.executionID, fix.executionID, e.target, hash[:], realXID); err != nil {
		t.Fatalf("insert auth: %v", err)
	}

	// Insert statement evidence with matching ordinal and batch_index, BUT malformed parameter IDs:
	// bsa has [1], evidence has [999]!
	if _, err := platformDB.ExecContext(ctx, `
		INSERT INTO app.job_statement_evidence (attempt_id, ordinal, batch_index, sql_text, safe_parameters, outcome, row_count)
		VALUES ($1, 1, 0, 'UPDATE managed.batch_items SET processed = TRUE WHERE id = ANY($1::bigint[]);', '{"ids":[999]}'::jsonb, 'unknown', 0)
	`, fix.claim.AttemptID); err != nil {
		t.Fatalf("insert malformed statement: %v", err)
	}

	// Let job lease expire naturally
	time.Sleep(300 * time.Millisecond)
	recOutcome, err := svc.ReconcileBatchJobByID(ctx, fix.claim.JobID, ExecutionDeps{
		Gate:    batchFakeGate(fix.snapshot),
		Acquire: acquire,
		Timeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("ReconcileBatchJobByID unexpected err: %v", err)
	}
	if recOutcome == nil || recOutcome.Status != executioncontract.Failed {
		t.Fatalf("expected TARGET_BLOCKED failure on malformed aborted statement, got outcome=%+v", recOutcome)
	}
	var cErr executioncontract.ContractError
	if !errors.As(recOutcome.Error, &cErr) || cErr.Code != "TARGET_BLOCKED" {
		t.Fatalf("expected TARGET_BLOCKED error, got: %v", recOutcome.Error)
	}

	// Verify job remains blocked and evidence was NOT updated to 'failed':
	var blocked bool
	if err := platformDB.QueryRowContext(ctx, "SELECT blocks_database FROM app.jobs WHERE id = $1", fix.claim.JobID).Scan(&blocked); err != nil || !blocked {
		t.Fatalf("expected job to retain blocks_database=true: %t", blocked)
	}

	var evOutcome string
	if err := platformDB.QueryRowContext(ctx, "SELECT outcome FROM app.job_statement_evidence WHERE attempt_id = $1 AND ordinal = 1", fix.claim.AttemptID).Scan(&evOutcome); err != nil {
		t.Fatalf("query evidence outcome: %v", err)
	}
	if evOutcome != "unknown" {
		t.Fatalf("statement evidence should not have been updated to failed on malformed mismatch, got: %s", evOutcome)
	}
}

func TestExecuteBatchJob_Regression_Terra5_ConfirmedZeroCheckpointReturn_MatchesPersistedState(t *testing.T) {
	e, platformDB, ownerDB := setupBatchTestEnv(t)
	setupBatchItemsFixture(t, e)

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	if _, err := e.exec.ExecContext(ctx, "INSERT INTO managed.batch_items (id, processed) VALUES (1, false)"); err != nil {
		t.Fatalf("insert items: %v", err)
	}

	acquire := makeBatchAcquire(t, e.exec, e.prep)
	svc := New(platformDB)

	t.Run("CleanZeroCheckpoint_UnblockedDisposition", func(t *testing.T) {
		fixClean := createBatchFixtureWithDuration(t, platformDB, ownerDB, e.target, 1, executioncontract.Apply, nil, 100*time.Millisecond)
		cleanupBatchScope(t, e, fixClean.executionID)
		if _, err := platformDB.ExecContext(ctx, "UPDATE app.jobs SET blocks_database = true WHERE id = $1", fixClean.claim.JobID); err != nil {
			t.Fatalf("set blocks_database: %v", err)
		}
		cleanIdent := BatchScopeIdentity{
			ExecutionID: fixClean.executionID,
			TargetID:    fixClean.targetID,
			DatabaseID:  fixClean.dbID,
			ActorID:     fixClean.actorID,
		}
		if _, err := svc.FreezeBatchScope(ctx, e.prep, cleanIdent); err != nil {
			t.Fatalf("freeze batch scope: %v", err)
		}
		tx, err := e.exec.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("begin target tx: %v", err)
		}
		var cleanXID string
		if err := tx.QueryRowContext(ctx, "SELECT pg_current_xact_id()::text").Scan(&cleanXID); err != nil {
			t.Fatalf("query target xid: %v", err)
		}
		if err := tx.Rollback(); err != nil {
			t.Fatalf("rollback target tx: %v", err)
		}

		authID := uuid.NewString()
		hash := sha256.Sum256([]byte("UPDATE managed.batch_items SET processed = TRUE WHERE id = ANY($1::bigint[]);"))
		if _, err := ownerDB.ExecContext(ctx, `
			INSERT INTO app.job_batch_step_authorizations
			    (id, job_id, attempt_id, actor_id, job_execution_id, scope_execution_id,
			     physical_target_id, direction, batch_index, ordinal, chunk_ids, plan, sql_text, sql_sha256,
			     target_xid, target_version)
			VALUES ($1, $2, $3, $4, $5, $6, $7, 'up', 0, 1, '[1]'::jsonb, '{}'::jsonb, 'UPDATE managed.batch_items SET processed = TRUE WHERE id = ANY($1::bigint[]);', $8, $9, 1)
		`, authID, fixClean.claim.JobID, fixClean.claim.AttemptID, fixClean.actorID, fixClean.executionID, fixClean.executionID, e.target, hash[:], cleanXID); err != nil {
			t.Fatalf("insert auth: %v", err)
		}

		if _, err := platformDB.ExecContext(ctx, `
			INSERT INTO app.job_statement_evidence (attempt_id, ordinal, batch_index, sql_text, safe_parameters, outcome, row_count)
			VALUES ($1, 1, 0, 'UPDATE managed.batch_items SET processed = TRUE WHERE id = ANY($1::bigint[]);', '{"ids":[1]}'::jsonb, 'unknown', 0)
		`, fixClean.claim.AttemptID); err != nil {
			t.Fatalf("insert valid statement: %v", err)
		}

		if _, err := platformDB.ExecContext(ctx, "UPDATE app.jobs SET lease_expires_at = clock_timestamp() - interval '5 seconds' WHERE id = $1", fixClean.claim.JobID); err != nil {
			t.Fatalf("expire job: %v", err)
		}

		recOutcome, err := svc.ReconcileBatchJobByID(ctx, fixClean.claim.JobID, ExecutionDeps{
			Gate:    batchFakeGate(fixClean.snapshot),
			Acquire: acquire,
			Timeout: 10 * time.Second,
		})
		if err != nil {
			t.Fatalf("ReconcileBatchJobByID unexpected err: %v", err)
		}
		if recOutcome == nil || recOutcome.Status != executioncontract.Failed || recOutcome.Error != nil || !recOutcome.Reconciled {
			t.Fatalf("expected clean failed outcome with nil error, got: %+v", recOutcome)
		}

		var jStatus, jExecStatus string
		var jBlocked bool
		if err := platformDB.QueryRowContext(ctx, "SELECT status, execution_status, blocks_database FROM app.jobs WHERE id = $1", fixClean.claim.JobID).Scan(&jStatus, &jExecStatus, &jBlocked); err != nil {
			t.Fatalf("query clean job: %v", err)
		}
		if jStatus != "failed" || jExecStatus != "failed" || jBlocked {
			t.Fatalf("clean job must be failed and unblocked: status=%s execStatus=%s blocked=%t", jStatus, jExecStatus, jBlocked)
		}

		// Repeated recovery returns matching persisted clean state
		repOutcome, err := svc.ReconcileBatchJobByID(ctx, fixClean.claim.JobID, ExecutionDeps{
			Gate:    batchFakeGate(fixClean.snapshot),
			Acquire: acquire,
			Timeout: 10 * time.Second,
		})
		if err != nil || repOutcome == nil || repOutcome.Status != executioncontract.Failed || repOutcome.Error != nil {
			t.Fatalf("repeated clean recovery failed: %+v err=%v", repOutcome, err)
		}
	})

	t.Run("BlockedZeroCheckpoint_BlockedDispositionReturned", func(t *testing.T) {
		fixBlocked := createBatchFixtureWithDuration(t, platformDB, ownerDB, e.target, 1, executioncontract.Apply, nil, 100*time.Millisecond)
		cleanupBatchScope(t, e, fixBlocked.executionID)

		// Set persisted state to failed with blocks_database = true and custom error code
		if _, err := platformDB.ExecContext(ctx, `
			UPDATE app.jobs
			SET status = 'failed',
			    execution_status = 'failed',
			    blocks_database = true,
			    error_code = 'TARGET_BLOCKED',
			    error_message = 'target transaction aborted but blocked'
			WHERE id = $1
		`, fixBlocked.claim.JobID); err != nil {
			t.Fatalf("update job to blocked: %v", err)
		}
		if _, err := platformDB.ExecContext(ctx, `
			UPDATE app.job_attempts
			SET status = 'failed',
			    error_code = 'TARGET_BLOCKED',
			    error_message = 'target transaction aborted but blocked'
			WHERE job_id = $1
		`, fixBlocked.claim.JobID); err != nil {
			t.Fatalf("update attempt to failed: %v", err)
		}

		recOutcome, err := svc.ReconcileBatchJobByID(ctx, fixBlocked.claim.JobID, ExecutionDeps{
			Gate:    batchFakeGate(fixBlocked.snapshot),
			Acquire: acquire,
			Timeout: 10 * time.Second,
		})
		if err != nil {
			t.Fatalf("ReconcileBatchJobByID unexpected err: %v", err)
		}
		if recOutcome == nil || recOutcome.Status != executioncontract.Failed || !recOutcome.Reconciled {
			t.Fatalf("expected failed outcome, got: %+v", recOutcome)
		}
		var cErr executioncontract.ContractError
		if !errors.As(recOutcome.Error, &cErr) || cErr.Code != "TARGET_BLOCKED" || cErr.Message != "target transaction aborted but blocked" {
			t.Fatalf("expected TARGET_BLOCKED with custom message, got: %v", recOutcome.Error)
		}
	})
}

func TestExecuteBatchJob_ResumeFailsClosedWhenPriorCommitUncertain(t *testing.T) {
	e, platformDB, ownerDB := setupBatchTestEnv(t)
	setupBatchItemsFixture(t, e)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := e.exec.ExecContext(ctx, "INSERT INTO managed.batch_items (id, processed) VALUES (1, false), (2, false), (3, false), (4, false)"); err != nil {
		t.Fatalf("insert items: %v", err)
	}
	svc := New(platformDB)
	acquire := makeBatchAcquire(t, e.exec, e.prep)

	other := createBatchFixture(t, platformDB, ownerDB, e.target, 2, executioncontract.Apply, nil)
	if _, err := ownerDB.ExecContext(ctx, `UPDATE app.job_attempts SET status = 'failed', finished_at = clock_timestamp() WHERE id = $1`, other.claim.AttemptID); err != nil {
		t.Fatalf("park other attempt: %v", err)
	}
	if _, err := ownerDB.ExecContext(ctx, `
		UPDATE app.jobs SET status = 'failed', execution_status = 'failed', blocks_database = false,
		       claimed_by = NULL, claimed_at = NULL, lease_token = NULL, lease_expires_at = NULL, finished_at = clock_timestamp()
		WHERE id = $1`, other.claim.JobID); err != nil {
		t.Fatalf("park other job: %v", err)
	}

	source := createBatchFixtureOnConnection(t, platformDB, ownerDB, e.target, 2, &other)
	cleanupBatchScope(t, e, source.executionID)
	out, err := svc.ExecuteBatchJob(ctx, source.claim, ExecutionDeps{
		Gate: batchFakeGate(source.snapshot), Acquire: acquire, Timeout: 10 * time.Second,
		afterTicketReservation: func(_ context.Context, batchIndex int) error {
			if batchIndex == 1 {
				return errors.New("simulated batch 1 failure")
			}
			return nil
		},
	})
	if err != nil || out == nil || out.Status != executioncontract.Partial {
		t.Fatalf("source outcome=%+v err=%v", out, err)
	}

	resume := createBatchFixture(t, platformDB, ownerDB, e.target, 2, executioncontract.Resume, &source)
	cleanupBatchScope(t, e, resume.executionID)
	if _, err := ownerDB.ExecContext(ctx, `DELETE FROM app.job_attempts WHERE id = $1`, resume.claim.AttemptID); err != nil {
		t.Fatalf("drop resume attempt: %v", err)
	}
	if _, err := ownerDB.ExecContext(ctx, `
		UPDATE app.jobs SET status = 'queued', execution_status = 'queued',
		       claimed_by = NULL, claimed_at = NULL, lease_token = NULL, lease_expires_at = NULL
		WHERE id = $1`, resume.claim.JobID); err != nil {
		t.Fatalf("requeue resume: %v", err)
	}

	if _, err := ownerDB.ExecContext(ctx, `UPDATE app.job_attempts SET sql_entered = true WHERE id = $1`, other.claim.AttemptID); err != nil {
		t.Fatalf("mark other sql entered: %v", err)
	}
	if _, err := ownerDB.ExecContext(ctx, `UPDATE app.jobs SET status = 'unknown', execution_status = 'unknown', blocks_database = true WHERE id = $1`, other.claim.JobID); err != nil {
		t.Fatalf("mark other unknown: %v", err)
	}

	if blocked, err := svc.ClaimJob(ctx, ClaimRequest{WorkerID: "worker-guard", LeaseDuration: 30 * time.Second, MaxAttempts: 3}); err != nil || blocked != nil {
		t.Fatalf("claim guard should refuse the resume: claim=%+v err=%v", blocked, err)
	}
	claim := forceP3Claim(t, ownerDB, resume.claim.JobID, "worker-forced")

	dispatched := 0
	outcome, err := svc.ExecuteBatchJob(ctx, claim, ExecutionDeps{
		Gate: batchFakeGate(resume.snapshot), Acquire: acquire, Timeout: 10 * time.Second,
		afterTicketReservation: func(context.Context, int) error { dispatched++; return nil },
		afterBatchUpdate:       func(context.Context, int) error { dispatched++; return nil },
	})
	if err != nil || outcome == nil || outcome.Status == executioncontract.Succeeded {
		t.Fatalf("resume must fail closed, got outcome=%+v err=%v", outcome, err)
	}
	var cErr executioncontract.ContractError
	if !errors.As(outcome.Error, &cErr) || cErr.Code != "TARGET_BLOCKED" {
		t.Fatalf("expected TARGET_BLOCKED, got %v", outcome.Error)
	}
	if dispatched != 0 {
		t.Fatalf("resume reached target dispatch %d times while target was blocked", dispatched)
	}
	var evidence, auths int
	if err := platformDB.QueryRowContext(ctx, `SELECT
		(SELECT count(*) FROM app.job_statement_evidence WHERE attempt_id = $1),
		(SELECT count(*) FROM app.job_batch_step_authorizations WHERE attempt_id = $1)`, claim.AttemptID).Scan(&evidence, &auths); err != nil {
		t.Fatalf("count resume evidence: %v", err)
	}
	if evidence != 0 || auths != 0 {
		t.Fatalf("resume left SQL evidence=%d authorizations=%d", evidence, auths)
	}
	var p3, p4 bool
	if err := e.exec.QueryRowContext(ctx, "SELECT (SELECT processed FROM managed.batch_items WHERE id = 3), (SELECT processed FROM managed.batch_items WHERE id = 4)").Scan(&p3, &p4); err != nil {
		t.Fatalf("query items: %v", err)
	}
	if p3 || p4 {
		t.Fatalf("target rows changed while blocked: 3=%t 4=%t", p3, p4)
	}
}
