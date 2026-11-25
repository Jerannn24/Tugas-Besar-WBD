package jobs

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Jerannn24/Tugas-Besar-WBD.git/executioncontract"
	"github.com/google/uuid"
)

func createCompensateFixture(t *testing.T, platformDB, ownerDB *sql.DB, targetID string, batchSize int, sourceFixture *batchJobFixture) batchJobFixture {
	return createDownFixture(t, platformDB, ownerDB, targetID, batchSize, sourceFixture, executioncontract.Compensate)
}

func createDownFixture(t *testing.T, platformDB, ownerDB *sql.DB, targetID string, batchSize int, sourceFixture *batchJobFixture, action executioncontract.Action) batchJobFixture {
	t.Helper()
	ctx := context.Background()

	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	sqlUp := "UPDATE managed.batch_items SET processed = TRUE WHERE id = ANY($1::bigint[]);"
	sqlDown := "UPDATE managed.batch_items SET processed = FALSE WHERE id = ANY($1::bigint[]);"
	title := "Batch Migration " + suffix
	dbID := sourceFixture.dbID
	actorID := sourceFixture.actorID
	draftID := sourceFixture.draftID
	sourceExecutionID := &sourceFixture.executionID

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
	_, _ = platformDB.ExecContext(ctx, `
		UPDATE app.migration_proposals
		SET status = 'superseded'
		WHERE database_id = $1 AND migration_id = $2 AND action = $3 AND status <> 'superseded'
	`, dbID, draftID, string(action))
	if _, err := platformDB.ExecContext(ctx, `
		INSERT INTO app.migration_proposals (
			id, database_id, migration_id, revision, action, source_execution_id, status,
			snapshot, snapshot_hash, created_by
		) VALUES ($1, $2, $3, 1, $8, $4, 'draft', $5::jsonb, $6, $7)
	`, proposalID, dbID, draftID, sourceExecutionID, string(snapshotBytes), hashBytes[:], actorID, string(action)); err != nil {
		t.Fatalf("insert compensate proposal: %v", err)
	}

	svc := New(platformDB)
	tx, err := platformDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin compensate enqueue tx: %v", err)
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
		Direction:         executioncontract.Down,
		SourceExecutionID: sourceExecutionID,
		Snapshot:          snapshot,
		SnapshotHash:      hex.EncodeToString(hashBytes[:]),
	})
	if err != nil {
		t.Fatalf("enqueue compensate job: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit compensate enqueue: %v", err)
	}

	claim, err := svc.ClaimJob(ctx, ClaimRequest{
		WorkerID:      "worker-" + suffix,
		LeaseDuration: 30 * time.Second,
		MaxAttempts:   3,
	})
	if err != nil || claim == nil {
		t.Fatalf("claim compensate job: claim=%+v err=%v", claim, err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, q := range []string{
			"DELETE FROM app.job_batch_commit_proofs WHERE job_id = $1",
			"DELETE FROM app.job_batch_step_authorizations WHERE job_id = $1",
			"DELETE FROM app.job_statement_evidence WHERE attempt_id IN (SELECT id FROM app.job_attempts WHERE job_id = $1)",
			"DELETE FROM app.job_attempts WHERE job_id = $1",
			"DELETE FROM app.jobs WHERE id = $1",
			"DELETE FROM app.migration_proposals WHERE id = $1",
		} {
			_, _ = ownerDB.ExecContext(cleanupCtx, q, claim.JobID)
		}
	})
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

// 1. Focused test: Partial up -> down reverse order with no uncommitted rows touched
func TestExecuteBatchJob_Compensation_PartialUp_ReverseOrder_NoUncommittedRowsTouched(t *testing.T) {
	e, platformDB, ownerDB := setupBatchTestEnv(t)
	setupBatchItemsFixture(t, e)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// 6 items, batchSize=2 -> 3 batches total
	// Batch 0: items 1, 2
	// Batch 1: items 3, 4
	// Batch 2: items 5, 6
	if _, err := e.exec.ExecContext(ctx, `
		INSERT INTO managed.batch_items (id, processed)
		VALUES (1, false), (2, false), (3, false), (4, false), (5, false), (6, false)
	`); err != nil {
		t.Fatalf("insert batch items: %v", err)
	}

	upFix := createBatchFixture(t, platformDB, ownerDB, e.target, 2, executioncontract.Apply, nil)
	cleanupBatchScope(t, e, upFix.executionID)

	acquire := makeBatchAcquire(t, e.exec, e.prep)
	svc := New(platformDB)

	// Partial up: commit batch 0 and batch 1, then fail on batch 2 before dispatch
	upDeps := ExecutionDeps{
		Gate:    batchFakeGate(upFix.snapshot),
		Acquire: acquire,
		Timeout: 10 * time.Second,
		beforeBatchDispatch: func(ctx context.Context, batchIndex int) error {
			if batchIndex == 2 {
				return errors.New("simulated interrupt on batch 2")
			}
			return nil
		},
	}

	outcome, err := svc.ExecuteBatchJob(ctx, upFix.claim, upDeps)
	if outcome == nil || outcome.Status != executioncontract.Partial {
		t.Fatalf("expected up outcome partial, got: %+v, err: %v", outcome, err)
	}

	// Verify items 1..4 are true, items 5..6 are false
	var trueCount, falseCount int
	if err := e.exec.QueryRowContext(ctx, "SELECT count(*) FROM managed.batch_items WHERE processed = true").Scan(&trueCount); err != nil {
		t.Fatalf("query true items: %v", err)
	}
	if err := e.exec.QueryRowContext(ctx, "SELECT count(*) FROM managed.batch_items WHERE processed = false").Scan(&falseCount); err != nil {
		t.Fatalf("query false items: %v", err)
	}
	if trueCount != 4 || falseCount != 2 {
		t.Fatalf("expected 4 true and 2 false after partial up, got true=%d false=%d", trueCount, falseCount)
	}

	// Now start compensation
	compFix := createCompensateFixture(t, platformDB, ownerDB, e.target, 2, &upFix)

	var dispatchedDownBatches []int
	compDeps := ExecutionDeps{
		Gate:    batchFakeGate(compFix.snapshot),
		Acquire: acquire,
		Timeout: 10 * time.Second,
		beforeBatchDispatch: func(ctx context.Context, batchIndex int) error {
			dispatchedDownBatches = append(dispatchedDownBatches, batchIndex)
			return nil
		},
	}

	compOutcome, compErr := svc.ExecuteBatchJob(ctx, compFix.claim, compDeps)
	if compErr != nil || compOutcome == nil || compOutcome.Status != executioncontract.Succeeded {
		t.Fatalf("expected compensation succeeded, got outcome: %+v, err: %v", compOutcome, compErr)
	}

	// Verify reverse order: batch 1 was executed first, then batch 0!
	// Batch 2 was NEVER touched!
	if len(dispatchedDownBatches) != 2 || dispatchedDownBatches[0] != 1 || dispatchedDownBatches[1] != 0 {
		t.Fatalf("expected reverse order [1, 0], got: %v", dispatchedDownBatches)
	}

	// Verify all items are now processed = false
	var finalTrueCount int
	if err := e.exec.QueryRowContext(ctx, "SELECT count(*) FROM managed.batch_items WHERE processed = true").Scan(&finalTrueCount); err != nil {
		t.Fatalf("query final true count: %v", err)
	}
	if finalTrueCount != 0 {
		t.Fatalf("expected 0 true items after compensation, got %d", finalTrueCount)
	}

	// Verify database is cleanly unblocked
	var blocksDB bool
	var jobStatus string
	if err := platformDB.QueryRowContext(ctx, "SELECT status, blocks_database FROM app.jobs WHERE id = $1", compFix.claim.JobID).Scan(&jobStatus, &blocksDB); err != nil {
		t.Fatalf("query job: %v", err)
	}
	if blocksDB || jobStatus != "succeeded" {
		t.Fatalf("expected unblocked succeeded compensate job, got status=%s blocked=%t", jobStatus, blocksDB)
	}
}

// 2. Focused test: Failure mid-down and recovery/resume without replay
func TestExecuteBatchJob_Compensation_FailureMidDown_ResumeRecoveryWithoutReplay(t *testing.T) {
	e, platformDB, ownerDB := setupBatchTestEnv(t)
	setupBatchItemsFixture(t, e)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// 6 items, batchSize=2 -> 3 batches (0, 1, 2)
	if _, err := e.exec.ExecContext(ctx, `
		INSERT INTO managed.batch_items (id, processed)
		VALUES (1, false), (2, false), (3, false), (4, false), (5, false), (6, false)
	`); err != nil {
		t.Fatalf("insert batch items: %v", err)
	}

	upFix := createBatchFixture(t, platformDB, ownerDB, e.target, 2, executioncontract.Apply, nil)
	cleanupBatchScope(t, e, upFix.executionID)

	acquire := makeBatchAcquire(t, e.exec, e.prep)
	svc := New(platformDB)

	// Up succeeds completely (all 3 batches commit)
	upDeps := ExecutionDeps{
		Gate:    batchFakeGate(upFix.snapshot),
		Acquire: acquire,
		Timeout: 10 * time.Second,
	}
	upOutcome, err := svc.ExecuteBatchJob(ctx, upFix.claim, upDeps)
	if err != nil || upOutcome == nil || upOutcome.Status != executioncontract.Succeeded {
		t.Fatalf("expected up succeeded, got: %+v, err: %v", upOutcome, err)
	}

	// Compensation attempt 1: reverts batch 2, then fails on batch 1 before dispatch
	compFix1 := createCompensateFixture(t, platformDB, ownerDB, e.target, 2, &upFix)

	compDeps1 := ExecutionDeps{
		Gate:    batchFakeGate(compFix1.snapshot),
		Acquire: acquire,
		Timeout: 10 * time.Second,
		beforeBatchDispatch: func(ctx context.Context, batchIndex int) error {
			if batchIndex == 1 {
				return errors.New("simulated failure mid-down on batch 1")
			}
			return nil
		},
	}

	outcome1, err1 := svc.ExecuteBatchJob(ctx, compFix1.claim, compDeps1)
	if outcome1 == nil || outcome1.Status != executioncontract.PartiallyReverted {
		t.Fatalf("expected outcome partially_reverted, got outcome=%+v err=%v", outcome1, err1)
	}

	// Verify database is still blocked because compensation failed mid-down
	var blocksDB bool
	var jobStatus string
	if err := platformDB.QueryRowContext(ctx, "SELECT status, blocks_database FROM app.jobs WHERE id = $1", compFix1.claim.JobID).Scan(&jobStatus, &blocksDB); err != nil {
		t.Fatalf("query job: %v", err)
	}
	if !blocksDB || jobStatus != "partially_reverted" {
		t.Fatalf("expected blocked partially_reverted job, got status=%s blocked=%t", jobStatus, blocksDB)
	}

	// Verify batch 2 (items 5, 6) is false, but items 1..4 are still true
	var batch2Processed bool
	if err := e.exec.QueryRowContext(ctx, "SELECT bool_or(processed) FROM managed.batch_items WHERE id IN (5, 6)").Scan(&batch2Processed); err != nil {
		t.Fatalf("query items 5, 6: %v", err)
	}
	if batch2Processed {
		t.Fatalf("expected batch 2 items to be reverted to false, but found processed=true")
	}
	// Resume the interrupted compensation through the queue.
	compFix2 := createDownFixture(t, platformDB, ownerDB, e.target, 2, &compFix1, executioncontract.Resume)

	var comp2Dispatched []int
	compDeps2 := ExecutionDeps{
		Gate:    batchFakeGate(compFix2.snapshot),
		Acquire: acquire,
		Timeout: 10 * time.Second,
		beforeBatchDispatch: func(ctx context.Context, batchIndex int) error {
			comp2Dispatched = append(comp2Dispatched, batchIndex)
			return nil
		},
	}

	outcome2, err2 := svc.ExecuteBatchJob(ctx, compFix2.claim, compDeps2)
	if err2 != nil || outcome2 == nil || outcome2.Status != executioncontract.Succeeded {
		t.Fatalf("expected resume compensation succeeded, got outcome=%+v err=%v", outcome2, err2)
	}

	// Verify NO REPLAY of batch 2!
	// Only batches 1 and 0 should have been dispatched!
	if len(comp2Dispatched) != 2 || comp2Dispatched[0] != 1 || comp2Dispatched[1] != 0 {
		t.Fatalf("expected dispatched [1, 0] without replay of 2, got: %v", comp2Dispatched)
	}

	// Verify all items are now processed = false
	var finalTrueCount int
	if err := e.exec.QueryRowContext(ctx, "SELECT count(*) FROM managed.batch_items WHERE processed = true").Scan(&finalTrueCount); err != nil {
		t.Fatalf("query final count: %v", err)
	}
	if finalTrueCount != 0 {
		t.Fatalf("expected all items false, got %d true", finalTrueCount)
	}

	// Database is now cleanly unblocked
	if err := platformDB.QueryRowContext(ctx, "SELECT status, blocks_database FROM app.jobs WHERE id = $1", compFix2.claim.JobID).Scan(&jobStatus, &blocksDB); err != nil {
		t.Fatalf("query job: %v", err)
	}
	if blocksDB || jobStatus != "succeeded" {
		t.Fatalf("expected unblocked succeeded job, got status=%s blocked=%t", jobStatus, blocksDB)
	}
	if err := platformDB.QueryRowContext(ctx, "SELECT status, blocks_database FROM app.jobs WHERE id = $1", compFix1.claim.JobID).Scan(&jobStatus, &blocksDB); err != nil {
		t.Fatalf("query compensation source: %v", err)
	}
	if blocksDB || jobStatus != "partially_reverted" {
		t.Fatalf("expected unblocked partially_reverted source, got status=%s blocked=%t", jobStatus, blocksDB)
	}
}

// 3. Focused test: External writer causes BATCH_UNSAFE and block intact
func TestExecuteBatchJob_Compensation_ExternalWriter_BatchUnsafe_BlockIntact(t *testing.T) {
	e, platformDB, ownerDB := setupBatchTestEnv(t)
	setupBatchItemsFixture(t, e)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if _, err := e.exec.ExecContext(ctx, "INSERT INTO managed.batch_items (id, processed) VALUES (1, false), (2, false)"); err != nil {
		t.Fatalf("insert items: %v", err)
	}

	upFix := createBatchFixture(t, platformDB, ownerDB, e.target, 2, executioncontract.Apply, nil)
	cleanupBatchScope(t, e, upFix.executionID)

	acquire := makeBatchAcquire(t, e.exec, e.prep)
	svc := New(platformDB)

	// Up succeeds
	upDeps := ExecutionDeps{
		Gate:    batchFakeGate(upFix.snapshot),
		Acquire: acquire,
		Timeout: 10 * time.Second,
	}
	upOutcome, err := svc.ExecuteBatchJob(ctx, upFix.claim, upDeps)
	if err != nil || upOutcome == nil || upOutcome.Status != executioncontract.Succeeded {
		t.Fatalf("up execution failed: %+v, err: %v", upOutcome, err)
	}

	// External writer modifies row 1 while PRESERVING processed = true!
	// In PostgreSQL, this allocates a new tuple version and changes xmin and ctid!
	if _, err := e.exec.ExecContext(ctx, "UPDATE managed.batch_items SET processed = true WHERE id = 1"); err != nil {
		t.Fatalf("external writer update: %v", err)
	}

	compFix := createCompensateFixture(t, platformDB, ownerDB, e.target, 2, &upFix)
	compDeps := ExecutionDeps{
		Gate:    batchFakeGate(compFix.snapshot),
		Acquire: acquire,
		Timeout: 10 * time.Second,
	}

	outcome, err := svc.ExecuteBatchJob(ctx, compFix.claim, compDeps)
	if outcome == nil || outcome.Status != executioncontract.Failed {
		t.Fatalf("expected failed outcome from BATCH_UNSAFE, got: outcome=%+v err=%v", outcome, err)
	}

	// Verify error code is BATCH_UNSAFE
	var cErr executioncontract.ContractError
	if !errors.As(outcome.Error, &cErr) || cErr.Code != "BATCH_UNSAFE" {
		t.Fatalf("expected BATCH_UNSAFE error code, got: %v", outcome.Error)
	}

	// Invariant: external writer causes BATCH_UNSAFE and database block remains INTACT!
	var blocksDB bool
	var jobStatus string
	if err := platformDB.QueryRowContext(ctx, "SELECT status, blocks_database FROM app.jobs WHERE id = $1", compFix.claim.JobID).Scan(&jobStatus, &blocksDB); err != nil {
		t.Fatalf("query job: %v", err)
	}
	if !blocksDB || jobStatus != "failed" {
		t.Fatalf("expected blocked failed job with block intact, got status=%s blocked=%t", jobStatus, blocksDB)
	}
}

// 4. Focused test: Approved artifact only
func TestExecuteBatchJob_Compensation_ApprovedArtifactOnly(t *testing.T) {
	e, platformDB, ownerDB := setupBatchTestEnv(t)
	setupBatchItemsFixture(t, e)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if _, err := e.exec.ExecContext(ctx, "INSERT INTO managed.batch_items (id, processed) VALUES (1, false), (2, false)"); err != nil {
		t.Fatalf("insert items: %v", err)
	}

	upFix := createBatchFixture(t, platformDB, ownerDB, e.target, 2, executioncontract.Apply, nil)
	cleanupBatchScope(t, e, upFix.executionID)

	acquire := makeBatchAcquire(t, e.exec, e.prep)
	svc := New(platformDB)

	// Up succeeds
	upDeps := ExecutionDeps{
		Gate:    batchFakeGate(upFix.snapshot),
		Acquire: acquire,
		Timeout: 10 * time.Second,
	}
	if _, err := svc.ExecuteBatchJob(ctx, upFix.claim, upDeps); err != nil {
		t.Fatalf("up execution failed: %v", err)
	}

	compFix := createCompensateFixture(t, platformDB, ownerDB, e.target, 2, &upFix)

	// Gate returns an unapproved down SQL pattern (e.g. without parameter, or arbitrary update)
	unapprovedSQL := "UPDATE managed.batch_items SET processed = FALSE WHERE id > 0;"
	badSnapshot := compFix.snapshot
	badSnapshot.SQLDown = &unapprovedSQL

	compDeps := ExecutionDeps{
		Gate:    batchFakeGate(badSnapshot),
		Acquire: acquire,
		Timeout: 10 * time.Second,
	}

	outcome, err := svc.ExecuteBatchJob(ctx, compFix.claim, compDeps)
	if outcome == nil || outcome.Status != executioncontract.Failed {
		t.Fatalf("expected failed outcome for unapproved artifact, got: outcome=%+v err=%v", outcome, err)
	}

	// Verify ZERO target down checkpoints were recorded
	var downCheckpoints int
	if err := e.prep.QueryRowContext(ctx, `
		SELECT count(*) FROM wbd_auth.batch_checkpoints
		WHERE execution_id = $1::uuid AND direction = 'down'
	`, upFix.executionID).Scan(&downCheckpoints); err != nil {
		t.Fatalf("query down checkpoints: %v", err)
	}
	if downCheckpoints != 0 {
		t.Fatalf("expected 0 down checkpoints from unapproved artifact, got %d", downCheckpoints)
	}
}

func TestExecuteBatchJob_Compensation_Regression_MismatchedSource(t *testing.T) {
	e, platformDB, ownerDB := setupBatchTestEnv(t)
	setupBatchItemsFixture(t, e)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if _, err := e.exec.ExecContext(ctx, "INSERT INTO managed.batch_items (id, processed) VALUES (1, false), (2, false)"); err != nil {
		t.Fatalf("insert items: %v", err)
	}

	upFix := createBatchFixture(t, platformDB, ownerDB, e.target, 2, executioncontract.Apply, nil)
	cleanupBatchScope(t, e, upFix.executionID)

	acquire := makeBatchAcquire(t, e.exec, e.prep)
	svc := New(platformDB)

	upDeps := ExecutionDeps{
		Gate:    batchFakeGate(upFix.snapshot),
		Acquire: acquire,
		Timeout: 10 * time.Second,
	}
	if _, err := svc.ExecuteBatchJob(ctx, upFix.claim, upDeps); err != nil {
		t.Fatalf("up failed: %v", err)
	}

	// 1. Enqueue compensate job with mismatched migration_id
	compFix := createCompensateFixture(t, platformDB, ownerDB, e.target, 2, &upFix)

	// Tamper with revision to mismatch
	differentRevision := 999
	if _, err := platformDB.ExecContext(ctx, "UPDATE app.jobs SET revision = $1 WHERE id = $2", differentRevision, compFix.claim.JobID); err != nil {
		t.Fatalf("update revision: %v", err)
	}
	// Also reset to queued for admission test
	if _, err := platformDB.ExecContext(ctx, "UPDATE app.jobs SET status = 'queued', execution_status = 'queued', claimed_by = NULL, claimed_at = NULL, lease_token = NULL, lease_expires_at = NULL WHERE id = $1", compFix.claim.JobID); err != nil {
		t.Fatalf("reset to queued: %v", err)
	}

	// Admission: ClaimJob MUST NOT admit this mismatched queued job!
	admitClaim, err := svc.ClaimJob(ctx, ClaimRequest{WorkerID: "test-worker", LeaseDuration: 30 * time.Second})
	if err != nil {
		t.Fatalf("claim job err: %v", err)
	}
	if admitClaim != nil && admitClaim.JobID == compFix.claim.JobID {
		t.Fatalf("expected mismatched queued job to be rejected by ClaimJob, but got claimed")
	}

	// Runtime: Direct execute of mismatched job must fail with REVISION_MISMATCH
	if _, err := platformDB.ExecContext(ctx, `
		UPDATE app.jobs
		SET status = 'running', execution_status = 'running',
		    claimed_by = 'test-worker', claimed_at = clock_timestamp(),
		    lease_token = $1, lease_expires_at = clock_timestamp() + interval '30 seconds'
		WHERE id = $2
	`, compFix.claim.LeaseToken, compFix.claim.JobID); err != nil {
		t.Fatalf("reset to running: %v", err)
	}
	compFix.claim.Revision = differentRevision
	compDeps := ExecutionDeps{
		Gate:    batchFakeGate(compFix.snapshot),
		Acquire: acquire,
		Timeout: 10 * time.Second,
	}
	outcome, err := svc.ExecuteBatchJob(ctx, compFix.claim, compDeps)
	if outcome == nil || outcome.Status != executioncontract.Failed {
		t.Fatalf("expected failed outcome, got: %+v err: %v", outcome, err)
	}
	var errCode string
	if err := platformDB.QueryRowContext(ctx, "SELECT error_code FROM app.jobs WHERE id = $1", compFix.claim.JobID).Scan(&errCode); err != nil {
		t.Fatalf("query error_code: %v", err)
	}
	if errCode != "REVISION_MISMATCH" {
		t.Fatalf("expected REVISION_MISMATCH error_code, got: %s", errCode)
	}
}

func TestExecuteBatchJob_Compensation_Regression_ZeroProof(t *testing.T) {
	e, platformDB, ownerDB := setupBatchTestEnv(t)
	setupBatchItemsFixture(t, e)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if _, err := e.exec.ExecContext(ctx, "INSERT INTO managed.batch_items (id, processed) VALUES (1, false), (2, false)"); err != nil {
		t.Fatalf("insert items: %v", err)
	}

	upFix := createBatchFixture(t, platformDB, ownerDB, e.target, 2, executioncontract.Apply, nil)
	cleanupBatchScope(t, e, upFix.executionID)

	acquire := makeBatchAcquire(t, e.exec, e.prep)
	svc := New(platformDB)

	upDeps := ExecutionDeps{
		Gate:    batchFakeGate(upFix.snapshot),
		Acquire: acquire,
		Timeout: 10 * time.Second,
	}
	if _, err := svc.ExecuteBatchJob(ctx, upFix.claim, upDeps); err != nil {
		t.Fatalf("up failed: %v", err)
	}

	compFix := createCompensateFixture(t, platformDB, ownerDB, e.target, 2, &upFix)

	// Reset to queued to test admission
	if _, err := platformDB.ExecContext(ctx, "UPDATE app.jobs SET status = 'queued', execution_status = 'queued', claimed_by = NULL, claimed_at = NULL, lease_token = NULL, lease_expires_at = NULL WHERE id = $1", compFix.claim.JobID); err != nil {
		t.Fatalf("reset to queued: %v", err)
	}

	// Delete platform commit proofs (leaving target checkpoints intact -> target-only evidence!)
	if _, err := ownerDB.ExecContext(ctx, "DELETE FROM app.job_batch_commit_proofs WHERE scope_execution_id = $1", upFix.executionID); err != nil {
		t.Fatalf("delete platform commit proofs: %v", err)
	}

	// Admission: ClaimJob MUST NOT admit compensation when source has zero platform proofs!
	admitClaim, err := svc.ClaimJob(ctx, ClaimRequest{WorkerID: "test-worker", LeaseDuration: 30 * time.Second})
	if err != nil {
		t.Fatalf("claim err: %v", err)
	}
	if admitClaim != nil && admitClaim.JobID == compFix.claim.JobID {
		t.Fatalf("expected ClaimJob to reject compensation with zero platform proofs")
	}

	// Runtime: Direct execute must reject target-only evidence
	if _, err := platformDB.ExecContext(ctx, `
		UPDATE app.jobs
		SET status = 'running', execution_status = 'running',
		    claimed_by = 'test-worker', claimed_at = clock_timestamp(),
		    lease_token = $1, lease_expires_at = clock_timestamp() + interval '30 seconds'
		WHERE id = $2
	`, compFix.claim.LeaseToken, compFix.claim.JobID); err != nil {
		t.Fatalf("reset to running: %v", err)
	}
	compDeps := ExecutionDeps{
		Gate:    batchFakeGate(compFix.snapshot),
		Acquire: acquire,
		Timeout: 10 * time.Second,
	}
	outcome, _ := svc.ExecuteBatchJob(ctx, compFix.claim, compDeps)
	if outcome == nil || outcome.Status != executioncontract.Failed {
		t.Fatalf("expected failed outcome for zero platform proofs, got: %+v", outcome)
	}
}

func TestExecuteBatchJob_Compensation_Regression_ExpiredOrStaleRecovery_ZeroDispatch(t *testing.T) {
	e, platformDB, ownerDB := setupBatchTestEnv(t)
	setupBatchItemsFixture(t, e)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if _, err := e.exec.ExecContext(ctx, "INSERT INTO managed.batch_items (id, processed) VALUES (1, false), (2, false), (3, false), (4, false)"); err != nil {
		t.Fatalf("insert items: %v", err)
	}

	upFix := createBatchFixture(t, platformDB, ownerDB, e.target, 2, executioncontract.Apply, nil)
	cleanupBatchScope(t, e, upFix.executionID)

	acquire := makeBatchAcquire(t, e.exec, e.prep)
	svc := New(platformDB)

	// Up succeeds
	upDeps := ExecutionDeps{
		Gate:    batchFakeGate(upFix.snapshot),
		Acquire: acquire,
		Timeout: 10 * time.Second,
	}
	if _, err := svc.ExecuteBatchJob(ctx, upFix.claim, upDeps); err != nil {
		t.Fatalf("up failed: %v", err)
	}

	// Create compensate job, but expire its lease before it runs any down batches
	compFix := createCompensateFixture(t, platformDB, ownerDB, e.target, 2, &upFix)
	if _, err := platformDB.ExecContext(ctx, "UPDATE app.jobs SET lease_expires_at = clock_timestamp() - interval '5 seconds', status = 'running', execution_status = 'running', blocks_database = true WHERE id = $1", compFix.claim.JobID); err != nil {
		t.Fatalf("expire job: %v", err)
	}

	// Track whether any down dispatch hook happens during reconcile
	dispatched := false
	reconGate := batchFakeGate(compFix.snapshot)
	reconDeps := ExecutionDeps{
		Gate:    reconGate,
		Acquire: acquire,
		Timeout: 10 * time.Second,
		beforeBatchDispatch: func(ctx context.Context, batchIndex int) error {
			dispatched = true
			return nil
		},
	}

	// Reconcile must be receipt-only: zero dispatch!
	outcome, err := svc.ReconcileBatchJobByID(ctx, compFix.claim.JobID, reconDeps)
	if outcome == nil || outcome.Status != executioncontract.PartiallyReverted {
		t.Fatalf("expected partially_reverted outcome from zero-dispatch reconcile, got outcome=%+v err=%v", outcome, err)
	}
	if dispatched {
		t.Fatalf("expected ZERO dispatch during reconcile, but dispatch hook was called!")
	}

	// Invariant: database block must remain INTACT
	var blocksDB bool
	if err := platformDB.QueryRowContext(ctx, "SELECT blocks_database FROM app.jobs WHERE id = $1", compFix.claim.JobID).Scan(&blocksDB); err != nil {
		t.Fatalf("query blocks_database: %v", err)
	}
	if !blocksDB {
		t.Fatalf("expected database block to remain intact (true), got false")
	}
}

func TestExecuteBatchJob_Compensation_Regression_ExactOriginalActor_OnePublicationPerCommit(t *testing.T) {
	e, platformDB, ownerDB := setupBatchTestEnv(t)
	setupBatchItemsFixture(t, e)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if _, err := e.exec.ExecContext(ctx, "INSERT INTO managed.batch_items (id, processed) VALUES (1, false), (2, false), (3, false), (4, false)"); err != nil {
		t.Fatalf("insert items: %v", err)
	}

	upFix := createBatchFixture(t, platformDB, ownerDB, e.target, 2, executioncontract.Apply, nil)
	cleanupBatchScope(t, e, upFix.executionID)

	acquire := makeBatchAcquire(t, e.exec, e.prep)
	svc := New(platformDB)

	// Up succeeds
	upDeps := ExecutionDeps{
		Gate:    batchFakeGate(upFix.snapshot),
		Acquire: acquire,
		Timeout: 10 * time.Second,
	}
	if _, err := svc.ExecuteBatchJob(ctx, upFix.claim, upDeps); err != nil {
		t.Fatalf("up failed: %v", err)
	}

	compFix := createCompensateFixture(t, platformDB, ownerDB, e.target, 2, &upFix)

	var recordedCommits []executioncontract.CommitEvidence
	trackingGate := batchFakeGate(compFix.snapshot)
	trackingGate.RecordCommitFunc = func(ctx context.Context, tx *sql.Tx, in executioncontract.CommitEvidence) error {
		recordedCommits = append(recordedCommits, in)
		return nil
	}

	compDeps := ExecutionDeps{
		Gate:    trackingGate,
		Acquire: acquire,
		Timeout: 10 * time.Second,
	}

	outcome, err := svc.ExecuteBatchJob(ctx, compFix.claim, compDeps)
	if err != nil || outcome == nil || outcome.Status != executioncontract.Succeeded {
		t.Fatalf("expected compensation success, got: %+v err: %v", outcome, err)
	}

	// 2 batches total (batch 1, batch 0) -> exactly 2 commits recorded!
	if len(recordedCommits) != 2 {
		t.Fatalf("expected exactly 2 commits recorded, got %d", len(recordedCommits))
	}

	// First commit: batch 1, FinalStep MUST be false
	if recordedCommits[0].FinalStep {
		t.Fatalf("expected batch 1 FinalStep to be false, got true")
	}
	if recordedCommits[0].ActorID != compFix.claim.ActorID || recordedCommits[0].Batch.ExecutedBy.ID != compFix.claim.ActorID {
		t.Fatalf("expected actor attribution to originating actor %s, got %s / %s",
			compFix.claim.ActorID, recordedCommits[0].ActorID, recordedCommits[0].Batch.ExecutedBy.ID)
	}

	// Second commit: batch 0 (last eligible chunk), FinalStep MUST be true
	if !recordedCommits[1].FinalStep {
		t.Fatalf("expected batch 0 FinalStep to be true, got false")
	}
	if recordedCommits[1].ActorID != compFix.claim.ActorID || recordedCommits[1].Batch.ExecutedBy.ID != compFix.claim.ActorID {
		t.Fatalf("expected actor attribution to originating actor %s, got %s / %s",
			compFix.claim.ActorID, recordedCommits[1].ActorID, recordedCommits[1].Batch.ExecutedBy.ID)
	}

	// Verify exact statement evidence in app.job_statement_evidence is marked 'succeeded'
	var succEvidenceCount int
	if err := platformDB.QueryRowContext(ctx, `
		SELECT count(*) FROM app.job_statement_evidence
		WHERE attempt_id = $1 AND outcome = 'succeeded'
	`, compFix.claim.AttemptID).Scan(&succEvidenceCount); err != nil {
		t.Fatalf("query statement evidence: %v", err)
	}
	if succEvidenceCount != 2 {
		t.Fatalf("expected 2 succeeded statement evidence rows, got %d", succEvidenceCount)
	}

	// Idempotency: Reconcile completed job again -> MUST NOT duplicate publications!
	countBefore := len(recordedCommits)
	reconOutcome, reconErr := svc.ReconcileBatchJob(ctx, compFix.claim, compDeps)
	if reconErr != nil || reconOutcome == nil || reconOutcome.Status != executioncontract.Succeeded {
		t.Fatalf("expected reconcile success, got: %+v err: %v", reconOutcome, reconErr)
	}
	if len(recordedCommits) != countBefore {
		t.Fatalf("expected zero additional publications during reconcile, but got %d new publications", len(recordedCommits)-countBefore)
	}
}

func TestExecuteBatchJob_Compensation_Regression_PreservingUnrelatedBlocks(t *testing.T) {
	e, platformDB, ownerDB := setupBatchTestEnv(t)
	setupBatchItemsFixture(t, e)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if _, err := e.exec.ExecContext(ctx, "INSERT INTO managed.batch_items (id, processed) VALUES (1, false), (2, false)"); err != nil {
		t.Fatalf("insert items: %v", err)
	}

	upFix := createBatchFixture(t, platformDB, ownerDB, e.target, 2, executioncontract.Apply, nil)
	cleanupBatchScope(t, e, upFix.executionID)

	acquire := makeBatchAcquire(t, e.exec, e.prep)
	svc := New(platformDB)

	// Up succeeds
	upDeps := ExecutionDeps{
		Gate:    batchFakeGate(upFix.snapshot),
		Acquire: acquire,
		Timeout: 10 * time.Second,
	}
	if _, err := svc.ExecuteBatchJob(ctx, upFix.claim, upDeps); err != nil {
		t.Fatalf("up failed: %v", err)
	}

	// Create and claim compensate fixture before target is blocked by unrelated job
	compFix := createCompensateFixture(t, platformDB, ownerDB, e.target, 2, &upFix)

	// Set source up job to partial/blocked
	if _, err := platformDB.ExecContext(ctx, "UPDATE app.jobs SET status = 'partial', execution_status = 'partial', blocks_database = true WHERE id = $1", upFix.claim.JobID); err != nil {
		t.Fatalf("update up job to partial: %v", err)
	}

	// Insert an UNRELATED job with blocks_database = true
	unrelatedDBID := uuid.NewString()
	unrelatedJobID := uuid.NewString()
	unrelatedExecID := uuid.NewString()
	unrelatedTargetUUID := uuid.NewString()
	if _, err := ownerDB.ExecContext(ctx, "INSERT INTO app.migration_target_state (id, next_version) VALUES ($1, 1) ON CONFLICT (id) DO NOTHING", unrelatedTargetUUID); err != nil {
		t.Fatalf("insert unrelated target state: %v", err)
	}
	if _, err := platformDB.ExecContext(ctx, `
		INSERT INTO app.database_connections
			(id, name, host, port, database_name, username, password_ciphertext, credential_key_id, ssl_mode, target_key, execution_target_id)
		VALUES ($1::uuid, 'unrelated_' || $1::text, 'postgres-test', 5432, 'test_target_unrelated', 'batch_key', '\x01', 'key1', 'disable', 'tkey_' || $1::text, $2::uuid)
	`, unrelatedDBID, unrelatedTargetUUID); err != nil {
		t.Fatalf("insert unrelated conn: %v", err)
	}
	t.Cleanup(func() {
		_, _ = ownerDB.ExecContext(context.Background(), "DELETE FROM app.jobs WHERE id = $1", unrelatedJobID)
		_, _ = ownerDB.ExecContext(context.Background(), "DELETE FROM app.database_connections WHERE id = $1", unrelatedDBID)
		_, _ = ownerDB.ExecContext(context.Background(), "DELETE FROM app.migration_target_state WHERE id = $1", unrelatedTargetUUID)
	})
	if _, err := platformDB.ExecContext(ctx, `
		INSERT INTO app.jobs (
			id, execution_id, database_id, target_id, proposal_id, migration_id, revision,
			actor_id, action, direction, mode, status, execution_status, blocks_database,
			snapshot_hash, snapshot, attempt_no, request_id
		) VALUES (
			$1, $2, $3, $4, $5, $6, 1,
			$7, 'apply', 'up', 'batch', 'failed', 'failed', true,
			'different_unrelated_hash', '{}'::jsonb, 1, gen_random_uuid()
		)
	`, unrelatedJobID, unrelatedExecID, unrelatedDBID, unrelatedTargetUUID, upFix.proposalID, upFix.claim.MigrationID, upFix.actorID); err != nil {
		t.Fatalf("insert unrelated job: %v", err)
	}
	compDeps := ExecutionDeps{
		Gate:    batchFakeGate(compFix.snapshot),
		Acquire: acquire,
		Timeout: 10 * time.Second,
	}

	outcome, err := svc.ExecuteBatchJob(ctx, compFix.claim, compDeps)
	if err != nil || outcome == nil || outcome.Status != executioncontract.Succeeded {
		t.Fatalf("compensation failed: %+v err: %v", outcome, err)
	}

	// 1. Compensate job itself succeeded and unblocked
	var compStatus string
	var compBlocked bool
	if err := platformDB.QueryRowContext(ctx, "SELECT status, blocks_database FROM app.jobs WHERE id = $1", compFix.claim.JobID).Scan(&compStatus, &compBlocked); err != nil {
		t.Fatalf("query comp job: %v", err)
	}
	if compStatus != "succeeded" || compBlocked {
		t.Fatalf("expected compensate job succeeded and unblocked, got status=%s blocked=%t", compStatus, compBlocked)
	}

	// 2. Verified matching source up job: blocks_database is cleared (false), but own status and execution_status MUST BE KEPT UNTOUCHED ('partial')!
	var upStatus, upExecStatus string
	var upBlocked bool
	if err := platformDB.QueryRowContext(ctx, "SELECT status, execution_status, blocks_database FROM app.jobs WHERE id = $1", upFix.claim.JobID).Scan(&upStatus, &upExecStatus, &upBlocked); err != nil {
		t.Fatalf("query up job: %v", err)
	}
	if upBlocked {
		t.Fatalf("expected matching source up job to be unblocked, got blocks_database=true")
	}
	if upStatus != "partial" || upExecStatus != "partial" {
		t.Fatalf("expected source up job individual outcome kept untouched ('partial'), got status=%s execStatus=%s", upStatus, upExecStatus)
	}

	// 3. Unrelated job MUST REMAIN BLOCKED (blocks_database = true) and status = 'failed'!
	var unrelStatus string
	var unrelBlocked bool
	if err := platformDB.QueryRowContext(ctx, "SELECT status, blocks_database FROM app.jobs WHERE id = $1", unrelatedJobID).Scan(&unrelStatus, &unrelBlocked); err != nil {
		t.Fatalf("query unrelated job: %v", err)
	}
	if !unrelBlocked || unrelStatus != "failed" {
		t.Fatalf("expected unrelated job to preserve block and status (blocked=true status=failed), got blocked=%t status=%s", unrelBlocked, unrelStatus)
	}
}

func enqueueUnrelatedBatchApply(t *testing.T, ctx context.Context, platformDB *sql.DB, svc *Service, dbID, targetID, actorID string, batchSize int) (string, executioncontract.Draft) {
	t.Helper()
	draftID := uuid.NewString()
	proposalID := uuid.NewString()
	sqlUp := "UPDATE managed.batch_items SET processed = TRUE WHERE id = ANY($1::bigint[]);"
	sqlDown := "UPDATE managed.batch_items SET processed = FALSE WHERE id = ANY($1::bigint[]);"
	if _, err := platformDB.ExecContext(ctx, `
		INSERT INTO app.migration_drafts (id, database_id, revision, title, sql, sql_down, created_by)
		VALUES ($1, $2, 1, 'Unrelated Next Migration', $3, $4, $5)
	`, draftID, dbID, sqlUp, sqlDown, actorID); err != nil {
		t.Fatalf("insert unrelated draft: %v", err)
	}
	snapshot := executioncontract.Draft{
		MigrationID: draftID,
		DatabaseID:  dbID,
		Revision:    1,
		Title:       "Unrelated Next Migration",
		SQLUp:       sqlUp,
		SQLDown:     &sqlDown,
		Mode:        executioncontract.Batch,
		Batch: &executioncontract.BatchConfig{
			Schema:       "managed",
			Table:        "batch_items",
			CursorColumn: "id",
			BatchSize:    batchSize,
		},
	}
	hashBytes := sha256.Sum256([]byte(sqlUp))
	snapJSON, _ := json.Marshal(snapshot)
	if _, err := platformDB.ExecContext(ctx, `
		INSERT INTO app.migration_proposals (id, database_id, migration_id, revision, action, status, snapshot, snapshot_hash, created_by)
		VALUES ($1, $2, $3, 1, 'apply', 'draft', $4::jsonb, $5, $6)
	`, proposalID, dbID, draftID, string(snapJSON), hashBytes[:], actorID); err != nil {
		t.Fatalf("insert unrelated proposal: %v", err)
	}
	tx, err := platformDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback()
	res, err := svc.EnqueueTx(ctx, tx, executioncontract.EnqueueRequest{
		TargetID:     targetID,
		RequestID:    uuid.NewString(),
		ActorID:      actorID,
		DatabaseID:   dbID,
		ProposalID:   proposalID,
		MigrationID:  draftID,
		Revision:     1,
		Action:       executioncontract.Apply,
		Direction:    executioncontract.Up,
		Snapshot:     snapshot,
		SnapshotHash: hex.EncodeToString(hashBytes[:]),
	})
	if err != nil {
		t.Fatalf("enqueue unrelated apply: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit unrelated apply: %v", err)
	}
	return res.JobID, snapshot
}

func TestExecuteBatchJob_Compensation_ClaimUnblocksOnlyProvenLineage(t *testing.T) {
	e, platformDB, ownerDB := setupBatchTestEnv(t)
	acquire := makeBatchAcquire(t, e.exec, e.prep)
	svc := New(platformDB)

	t.Run("partial_up_then_proven_compensation_unblocks_next_job", func(t *testing.T) {
		setupBatchItemsFixture(t, e)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		if _, err := e.exec.ExecContext(ctx, `
			INSERT INTO managed.batch_items (id, processed)
			VALUES (1, false), (2, false), (3, false), (4, false)
		`); err != nil {
			t.Fatalf("insert batch items: %v", err)
		}

		upFix := createBatchFixture(t, platformDB, ownerDB, e.target, 2, executioncontract.Apply, nil)
		cleanupBatchScope(t, e, upFix.executionID)

		upOutcome, err := svc.ExecuteBatchJob(ctx, upFix.claim, ExecutionDeps{
			Gate:    batchFakeGate(upFix.snapshot),
			Acquire: acquire,
			Timeout: 10 * time.Second,
			beforeBatchDispatch: func(ctx context.Context, batchIndex int) error {
				if batchIndex == 1 {
					return errors.New("simulated failure on batch 1")
				}
				return nil
			},
		})
		if upOutcome == nil || upOutcome.Status != executioncontract.Partial {
			t.Fatalf("expected partial up outcome, got %+v err=%v", upOutcome, err)
		}

		nextJobID, nextSnap := enqueueUnrelatedBatchApply(t, ctx, platformDB, svc, upFix.dbID, e.target, upFix.actorID, 2)
		blockedClaim, err := svc.ClaimJob(ctx, ClaimRequest{WorkerID: "worker-blocked-before-comp", LeaseDuration: 30 * time.Second})
		if err != nil || blockedClaim != nil {
			t.Fatalf("expected claim blocked before compensation, got claim=%+v err=%v", blockedClaim, err)
		}

		compFix := createCompensateFixture(t, platformDB, ownerDB, e.target, 2, &upFix)
		compOutcome, err := svc.ExecuteBatchJob(ctx, compFix.claim, ExecutionDeps{
			Gate:    batchFakeGate(compFix.snapshot),
			Acquire: acquire,
			Timeout: 10 * time.Second,
		})
		if err != nil || compOutcome == nil || compOutcome.Status != executioncontract.Succeeded {
			t.Fatalf("expected compensation succeeded, got %+v err=%v", compOutcome, err)
		}

		nextClaim, err := svc.ClaimJob(ctx, ClaimRequest{WorkerID: "worker-after-comp", LeaseDuration: 30 * time.Second})
		if err != nil || nextClaim == nil || nextClaim.JobID != nextJobID {
			t.Fatalf("expected next job %s claimed after compensation, got claim=%+v err=%v", nextJobID, nextClaim, err)
		}
		cleanupBatchScope(t, e, nextClaim.ExecutionID)

		nextOutcome, err := svc.ExecuteBatchJob(ctx, nextClaim, ExecutionDeps{
			Gate:    batchFakeGate(nextSnap),
			Acquire: acquire,
			Timeout: 10 * time.Second,
		})
		if err != nil || nextOutcome == nil || nextOutcome.Status != executioncontract.Succeeded {
			t.Fatalf("expected next job succeeded, got %+v err=%v", nextOutcome, err)
		}

		var trueCount int
		if err := e.exec.QueryRowContext(ctx, "SELECT count(*) FROM managed.batch_items WHERE processed = true").Scan(&trueCount); err != nil {
			t.Fatalf("query processed items: %v", err)
		}
		if trueCount != 4 {
			t.Fatalf("expected 4 processed items after next job, got %d", trueCount)
		}

		var upStatus string
		var upBlocked bool
		var upProofs, downProofs int
		if err := platformDB.QueryRowContext(ctx, "SELECT status, blocks_database FROM app.jobs WHERE id = $1", upFix.claim.JobID).Scan(&upStatus, &upBlocked); err != nil {
			t.Fatalf("query up job: %v", err)
		}
		if upStatus != "partial" || upBlocked {
			t.Fatalf("expected up job historical status='partial' and blocks_database=false, got status=%s blocked=%t", upStatus, upBlocked)
		}
		if err := platformDB.QueryRowContext(ctx, "SELECT count(*) FROM app.job_batch_commit_proofs WHERE scope_execution_id = $1 AND direction = 'up'", upFix.executionID).Scan(&upProofs); err != nil {
			t.Fatalf("query up proofs: %v", err)
		}
		if err := platformDB.QueryRowContext(ctx, "SELECT count(*) FROM app.job_batch_commit_proofs WHERE scope_execution_id = $1 AND direction = 'down'", upFix.executionID).Scan(&downProofs); err != nil {
			t.Fatalf("query down proofs: %v", err)
		}
		if upProofs != 1 || downProofs != 1 {
			t.Fatalf("expected 1 up proof and 1 down proof preserved without replay, got up=%d down=%d", upProofs, downProofs)
		}
	})

	t.Run("partial_up_then_interrupted_comp_then_down_resume_unblocks_next_job", func(t *testing.T) {
		setupBatchItemsFixture(t, e)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		if _, err := e.exec.ExecContext(ctx, `
			INSERT INTO managed.batch_items (id, processed)
			VALUES (1, false), (2, false), (3, false), (4, false), (5, false), (6, false)
		`); err != nil {
			t.Fatalf("insert batch items: %v", err)
		}

		upFix := createBatchFixture(t, platformDB, ownerDB, e.target, 2, executioncontract.Apply, nil)
		cleanupBatchScope(t, e, upFix.executionID)

		upOutcome, err := svc.ExecuteBatchJob(ctx, upFix.claim, ExecutionDeps{
			Gate:    batchFakeGate(upFix.snapshot),
			Acquire: acquire,
			Timeout: 10 * time.Second,
			beforeBatchDispatch: func(ctx context.Context, batchIndex int) error {
				if batchIndex == 2 {
					return errors.New("simulated failure on batch 2")
				}
				return nil
			},
		})
		if upOutcome == nil || upOutcome.Status != executioncontract.Partial {
			t.Fatalf("expected partial up outcome, got %+v err=%v", upOutcome, err)
		}

		compFix1 := createCompensateFixture(t, platformDB, ownerDB, e.target, 2, &upFix)
		comp1Outcome, err := svc.ExecuteBatchJob(ctx, compFix1.claim, ExecutionDeps{
			Gate:    batchFakeGate(compFix1.snapshot),
			Acquire: acquire,
			Timeout: 10 * time.Second,
			beforeBatchDispatch: func(ctx context.Context, batchIndex int) error {
				if batchIndex == 0 {
					return errors.New("simulated failure mid-down on batch 0")
				}
				return nil
			},
		})
		if comp1Outcome == nil || comp1Outcome.Status != executioncontract.PartiallyReverted {
			t.Fatalf("expected partially_reverted outcome, got %+v err=%v", comp1Outcome, err)
		}

		nextJobID, nextSnap := enqueueUnrelatedBatchApply(t, ctx, platformDB, svc, upFix.dbID, e.target, upFix.actorID, 2)
		blockedClaim, err := svc.ClaimJob(ctx, ClaimRequest{WorkerID: "worker-blocked-mid-down", LeaseDuration: 30 * time.Second})
		if err != nil || blockedClaim != nil {
			t.Fatalf("expected claim blocked while partially_reverted, got claim=%+v err=%v", blockedClaim, err)
		}

		compFix2 := createDownFixture(t, platformDB, ownerDB, e.target, 2, &compFix1, executioncontract.Resume)
		var resumedDownBatches []int
		comp2Outcome, err := svc.ExecuteBatchJob(ctx, compFix2.claim, ExecutionDeps{
			Gate:    batchFakeGate(compFix2.snapshot),
			Acquire: acquire,
			Timeout: 10 * time.Second,
			beforeBatchDispatch: func(ctx context.Context, batchIndex int) error {
				resumedDownBatches = append(resumedDownBatches, batchIndex)
				return nil
			},
		})
		if err != nil || comp2Outcome == nil || comp2Outcome.Status != executioncontract.Succeeded {
			t.Fatalf("expected down resume succeeded, got %+v err=%v", comp2Outcome, err)
		}
		if len(resumedDownBatches) != 1 || resumedDownBatches[0] != 0 {
			t.Fatalf("expected down resume to dispatch only [0] without replaying batch 1, got %v", resumedDownBatches)
		}

		nextClaim, err := svc.ClaimJob(ctx, ClaimRequest{WorkerID: "worker-after-down-resume", LeaseDuration: 30 * time.Second})
		if err != nil || nextClaim == nil || nextClaim.JobID != nextJobID {
			t.Fatalf("expected next job %s claimed after down resume, got claim=%+v err=%v", nextJobID, nextClaim, err)
		}
		cleanupBatchScope(t, e, nextClaim.ExecutionID)

		nextOutcome, err := svc.ExecuteBatchJob(ctx, nextClaim, ExecutionDeps{
			Gate:    batchFakeGate(nextSnap),
			Acquire: acquire,
			Timeout: 10 * time.Second,
		})
		if err != nil || nextOutcome == nil || nextOutcome.Status != executioncontract.Succeeded {
			t.Fatalf("expected next job succeeded, got %+v err=%v", nextOutcome, err)
		}

		var trueCount int
		if err := e.exec.QueryRowContext(ctx, "SELECT count(*) FROM managed.batch_items WHERE processed = true").Scan(&trueCount); err != nil {
			t.Fatalf("query processed items: %v", err)
		}
		if trueCount != 6 {
			t.Fatalf("expected 6 processed items after next job, got %d", trueCount)
		}

		var upStatus, comp1Status string
		var upBlocked, comp1Blocked bool
		if err := platformDB.QueryRowContext(ctx, "SELECT status, blocks_database FROM app.jobs WHERE id = $1", upFix.claim.JobID).Scan(&upStatus, &upBlocked); err != nil {
			t.Fatalf("query up job: %v", err)
		}
		if err := platformDB.QueryRowContext(ctx, "SELECT status, blocks_database FROM app.jobs WHERE id = $1", compFix1.claim.JobID).Scan(&comp1Status, &comp1Blocked); err != nil {
			t.Fatalf("query comp1 job: %v", err)
		}
		if upStatus != "partial" || upBlocked || comp1Status != "partially_reverted" || comp1Blocked {
			t.Fatalf("expected historical statuses preserved and blocks cleared, up=(%s,%t) comp1=(%s,%t)", upStatus, upBlocked, comp1Status, comp1Blocked)
		}
	})

	t.Run("status_only_down_without_proofs_still_blocks", func(t *testing.T) {
		setupBatchItemsFixture(t, e)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		if _, err := e.exec.ExecContext(ctx, "INSERT INTO managed.batch_items (id, processed) VALUES (1, false), (2, false), (3, false), (4, false)"); err != nil {
			t.Fatalf("insert batch items: %v", err)
		}

		upFix := createBatchFixture(t, platformDB, ownerDB, e.target, 2, executioncontract.Apply, nil)
		cleanupBatchScope(t, e, upFix.executionID)

		upOutcome, err := svc.ExecuteBatchJob(ctx, upFix.claim, ExecutionDeps{
			Gate:    batchFakeGate(upFix.snapshot),
			Acquire: acquire,
			Timeout: 10 * time.Second,
			beforeBatchDispatch: func(ctx context.Context, batchIndex int) error {
				if batchIndex == 1 {
					return errors.New("simulated failure on batch 1")
				}
				return nil
			},
		})
		if upOutcome == nil || upOutcome.Status != executioncontract.Partial {
			t.Fatalf("expected partial up outcome, got %+v err=%v", upOutcome, err)
		}

		compFix := createCompensateFixture(t, platformDB, ownerDB, e.target, 2, &upFix)
		if _, err := ownerDB.ExecContext(ctx, `
			UPDATE app.jobs SET status = 'succeeded', execution_status = 'succeeded', blocks_database = false, finished_at = clock_timestamp()
			WHERE id = $1
		`, compFix.claim.JobID); err != nil {
			t.Fatalf("fabricate comp status: %v", err)
		}
		if _, err := ownerDB.ExecContext(ctx, `
			UPDATE app.job_attempts SET status = 'succeeded', finished_at = clock_timestamp() WHERE id = $1
		`, compFix.claim.AttemptID); err != nil {
			t.Fatalf("fabricate comp attempt status: %v", err)
		}
		if _, err := ownerDB.ExecContext(ctx, `
			UPDATE app.jobs SET blocks_database = false WHERE id = $1
		`, upFix.claim.JobID); err != nil {
			t.Fatalf("clear up blocks_database: %v", err)
		}

		enqueueUnrelatedBatchApply(t, ctx, platformDB, svc, upFix.dbID, e.target, upFix.actorID, 2)
		claim, err := svc.ClaimJob(ctx, ClaimRequest{WorkerID: "worker-unproven-down", LeaseDuration: 30 * time.Second})
		if err != nil || claim != nil {
			t.Fatalf("expected status-only down without proofs to keep target blocked, got claim=%+v err=%v", claim, err)
		}
	})

	t.Run("incomplete_down_resume_missing_proof_still_blocks", func(t *testing.T) {
		setupBatchItemsFixture(t, e)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		if _, err := e.exec.ExecContext(ctx, `
			INSERT INTO managed.batch_items (id, processed)
			VALUES (1, false), (2, false), (3, false), (4, false), (5, false), (6, false)
		`); err != nil {
			t.Fatalf("insert batch items: %v", err)
		}

		upFix := createBatchFixture(t, platformDB, ownerDB, e.target, 2, executioncontract.Apply, nil)
		cleanupBatchScope(t, e, upFix.executionID)

		upOutcome, err := svc.ExecuteBatchJob(ctx, upFix.claim, ExecutionDeps{
			Gate:    batchFakeGate(upFix.snapshot),
			Acquire: acquire,
			Timeout: 10 * time.Second,
			beforeBatchDispatch: func(ctx context.Context, batchIndex int) error {
				if batchIndex == 2 {
					return errors.New("simulated failure on batch 2")
				}
				return nil
			},
		})
		if upOutcome == nil || upOutcome.Status != executioncontract.Partial {
			t.Fatalf("expected partial up outcome, got %+v err=%v", upOutcome, err)
		}

		compFix1 := createCompensateFixture(t, platformDB, ownerDB, e.target, 2, &upFix)
		comp1Outcome, err := svc.ExecuteBatchJob(ctx, compFix1.claim, ExecutionDeps{
			Gate:    batchFakeGate(compFix1.snapshot),
			Acquire: acquire,
			Timeout: 10 * time.Second,
			beforeBatchDispatch: func(ctx context.Context, batchIndex int) error {
				if batchIndex == 0 {
					return errors.New("simulated failure mid-down on batch 0")
				}
				return nil
			},
		})
		if comp1Outcome == nil || comp1Outcome.Status != executioncontract.PartiallyReverted {
			t.Fatalf("expected partially_reverted outcome, got %+v err=%v", comp1Outcome, err)
		}

		compFix2 := createDownFixture(t, platformDB, ownerDB, e.target, 2, &compFix1, executioncontract.Resume)
		if _, err := ownerDB.ExecContext(ctx, `
			UPDATE app.jobs SET status = 'succeeded', execution_status = 'succeeded', blocks_database = false, finished_at = clock_timestamp()
			WHERE id = $1
		`, compFix2.claim.JobID); err != nil {
			t.Fatalf("fabricate down resume status: %v", err)
		}
		if _, err := ownerDB.ExecContext(ctx, `
			UPDATE app.job_attempts SET status = 'succeeded', finished_at = clock_timestamp() WHERE id = $1
		`, compFix2.claim.AttemptID); err != nil {
			t.Fatalf("fabricate down resume attempt status: %v", err)
		}
		if _, err := ownerDB.ExecContext(ctx, `
			UPDATE app.jobs SET blocks_database = false WHERE id IN ($1, $2)
		`, upFix.claim.JobID, compFix1.claim.JobID); err != nil {
			t.Fatalf("clear lineage blocks_database: %v", err)
		}

		enqueueUnrelatedBatchApply(t, ctx, platformDB, svc, upFix.dbID, e.target, upFix.actorID, 2)
		claim, err := svc.ClaimJob(ctx, ClaimRequest{WorkerID: "worker-incomplete-down-resume", LeaseDuration: 30 * time.Second})
		if err != nil || claim != nil {
			t.Fatalf("expected down resume missing batch 0 proof to keep target blocked, got claim=%+v err=%v", claim, err)
		}
	})

	t.Run("proven_compensation_does_not_unblock_separate_unresolved_job", func(t *testing.T) {
		setupBatchItemsFixture(t, e)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		if _, err := e.exec.ExecContext(ctx, "INSERT INTO managed.batch_items (id, processed) VALUES (1, false), (2, false), (3, false), (4, false)"); err != nil {
			t.Fatalf("insert batch items: %v", err)
		}

		upA := createBatchFixture(t, platformDB, ownerDB, e.target, 2, executioncontract.Apply, nil)
		cleanupBatchScope(t, e, upA.executionID)

		upAOutcome, err := svc.ExecuteBatchJob(ctx, upA.claim, ExecutionDeps{
			Gate:    batchFakeGate(upA.snapshot),
			Acquire: acquire,
			Timeout: 10 * time.Second,
			beforeBatchDispatch: func(ctx context.Context, batchIndex int) error {
				if batchIndex == 1 {
					return errors.New("simulated failure on batch 1")
				}
				return nil
			},
		})
		if upAOutcome == nil || upAOutcome.Status != executioncontract.Partial {
			t.Fatalf("expected partial upA outcome, got %+v err=%v", upAOutcome, err)
		}

		compA := createCompensateFixture(t, platformDB, ownerDB, e.target, 2, &upA)
		compAOutcome, err := svc.ExecuteBatchJob(ctx, compA.claim, ExecutionDeps{
			Gate:    batchFakeGate(compA.snapshot),
			Acquire: acquire,
			Timeout: 10 * time.Second,
		})
		if err != nil || compAOutcome == nil || compAOutcome.Status != executioncontract.Succeeded {
			t.Fatalf("expected compA succeeded, got %+v err=%v", compAOutcome, err)
		}

		upB := createBatchFixture(t, platformDB, ownerDB, e.target, 2, executioncontract.Apply, nil)
		cleanupBatchScope(t, e, upB.executionID)

		upBOutcome, err := svc.ExecuteBatchJob(ctx, upB.claim, ExecutionDeps{
			Gate:    batchFakeGate(upB.snapshot),
			Acquire: acquire,
			Timeout: 10 * time.Second,
			beforeBatchDispatch: func(ctx context.Context, batchIndex int) error {
				if batchIndex == 1 {
					return errors.New("simulated failure on batch 1")
				}
				return nil
			},
		})
		if upBOutcome == nil || upBOutcome.Status != executioncontract.Partial {
			t.Fatalf("expected partial upB outcome, got %+v err=%v", upBOutcome, err)
		}

		enqueueUnrelatedBatchApply(t, ctx, platformDB, svc, upA.dbID, e.target, upA.actorID, 2)
		claim, err := svc.ClaimJob(ctx, ClaimRequest{WorkerID: "worker-other-unresolved", LeaseDuration: 30 * time.Second})
		if err != nil || claim != nil {
			t.Fatalf("expected separate unresolved partial job to keep target blocked, got claim=%+v err=%v", claim, err)
		}

		if _, err := ownerDB.ExecContext(ctx, "UPDATE app.jobs SET blocks_database = false WHERE id = $1", upB.claim.JobID); err != nil {
			t.Fatalf("clear upB blocks_database: %v", err)
		}
		claim, err = svc.ClaimJob(ctx, ClaimRequest{WorkerID: "worker-other-unresolved-2", LeaseDuration: 30 * time.Second})
		if err != nil || claim != nil {
			t.Fatalf("expected separate unresolved partial status to keep target blocked, got claim=%+v err=%v", claim, err)
		}
	})
}
