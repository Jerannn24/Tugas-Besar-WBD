package jobs

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Jerannn24/Tugas-Besar-WBD.git/executioncontract"
	"github.com/google/uuid"
)

func createClaimFixture(t *testing.T, db *sql.DB, ownerDB *sql.DB) fixture {
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

	title := "Create Items Table Claim"
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
			'{"title": "Create Items Table Claim", "sql": "CREATE TABLE items (id int);"}'::jsonb,
			sha256('Create Items Table Claim'::bytea),
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

func TestClaimJob_RevalidationRequest(t *testing.T) {
	platformDB, ownerDB := testDBs(t)
	svc := New(platformDB)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	source := uuid.NewString()
	for _, sourceID := range []*string{nil, &source} {
		f := createClaimFixture(t, platformDB, ownerDB)
		if sourceID != nil {
			createFixtureSource(t, ctx, platformDB, f, *sourceID)
		}
		proposalID := createFixtureProposal(t, ctx, platformDB, f, executioncontract.Rollback, sourceID)
		req := executioncontract.EnqueueRequest{
			DatabaseID: f.dbID, TargetID: f.targetID, RequestID: uuid.NewString(),
			ProposalID: proposalID, MigrationID: f.draftID, Revision: 1,
			ActorID: f.actorID, Action: executioncontract.Rollback, Direction: executioncontract.Down,
			SourceExecutionID: sourceID, Snapshot: f.snapshot, SnapshotHash: f.hash,
			ApprovalFingerprint: "nonempty-approval-fingerprint",
		}
		tx, err := platformDB.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		accepted, err := svc.EnqueueTx(ctx, tx, req)
		if err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		claim, err := svc.ClaimJob(ctx, ClaimRequest{WorkerID: "worker-revalidation"})
		if err != nil || claim == nil {
			t.Fatalf("claim: result=%+v error=%v", claim, err)
		}
		want := executioncontract.RevalidationRequest{
			JobID: accepted.JobID, ExecutionID: accepted.ExecutionID, ActorID: req.ActorID,
			DatabaseID: req.DatabaseID, TargetID: req.TargetID, MigrationID: req.MigrationID,
			ProposalID: req.ProposalID, Revision: req.Revision, Action: req.Action,
			Direction: req.Direction, SourceExecutionID: sourceID,
			SnapshotHash: req.SnapshotHash, ApprovalFingerprint: req.ApprovalFingerprint,
		}
		if got := claim.RevalidationRequest(); !reflect.DeepEqual(got, want) {
			t.Fatalf("revalidation request: got %+v, want %+v", got, want)
		}
	}
}

func TestClaimJob_ConcurrentWorkersCompetition(t *testing.T) {
	platformDB, ownerDB := testDBs(t)
	f := createClaimFixture(t, platformDB, ownerDB)
	svc := New(platformDB)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Enqueue and commit exactly one queued job
	tx, err := platformDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	req := executioncontract.EnqueueRequest{
		DatabaseID:          f.dbID,
		TargetID:            f.targetID,
		RequestID:           uuid.NewString(),
		ProposalID:          f.proposalID,
		MigrationID:         f.draftID,
		Revision:            1,
		ActorID:             f.actorID,
		Action:              executioncontract.Apply,
		Direction:           executioncontract.Up,
		Snapshot:            f.snapshot,
		SnapshotHash:        f.hash,
		ApprovalFingerprint: "fp_claim_competition",
	}
	res, err := svc.EnqueueTx(ctx, tx, req)
	if err != nil {
		tx.Rollback()
		t.Fatalf("EnqueueTx: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit tx: %v", err)
	}

	// Deterministic barrier:
	// Worker 1 acquires row lock via FOR UPDATE SKIP LOCKED.
	// While Worker 1 holds the lock in its open transaction, Worker 2 attempts to claim.
	// Worker 2 must find no work due to SKIP LOCKED.
	// Once Worker 2 finishes, Worker 1 completes attempt creation and commits.
	w1HoldingLock := make(chan struct{})
	w2Done := make(chan struct{})

	var (
		w1Claim *Claim
		w1Err   error
		w2Claim *Claim
		w2Err   error
		wg      sync.WaitGroup
	)

	wg.Add(2)

	// Worker 1
	go func() {
		defer wg.Done()
		w1Claim, w1Err = svc.ClaimJob(ctx, ClaimRequest{
			WorkerID:      "worker-1",
			LeaseDuration: 30 * time.Second,
			afterLock: func(ctx context.Context) error {
				close(w1HoldingLock)
				select {
				case <-w2Done:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			},
		})
	}()

	// Worker 2
	go func() {
		defer wg.Done()
		defer close(w2Done)

		select {
		case <-w1HoldingLock:
		case <-ctx.Done():
			w2Err = ctx.Err()
			return
		}

		w2Claim, w2Err = svc.ClaimJob(ctx, ClaimRequest{
			WorkerID:      "worker-2",
			LeaseDuration: 30 * time.Second,
		})
	}()

	wg.Wait()

	// Assert winner
	if w1Err != nil {
		t.Fatalf("worker-1 claim failed: %v", w1Err)
	}
	if w1Claim == nil {
		t.Fatal("expected worker-1 to win and claim the job, got nil")
	}

	// Assert loser: distinct no-work result
	if w2Err != nil {
		t.Fatalf("worker-2 encountered error instead of no-work: %v", w2Err)
	}
	if w2Claim != nil {
		t.Fatalf("expected worker-2 to get no work, got claim: %+v", w2Claim)
	}

	// Assert winner claim fields
	if w1Claim.JobID != res.JobID {
		t.Errorf("w1Claim.JobID = %s, want %s", w1Claim.JobID, res.JobID)
	}
	if w1Claim.ClaimedBy != "worker-1" {
		t.Errorf("w1Claim.ClaimedBy = %s, want worker-1", w1Claim.ClaimedBy)
	}
	if w1Claim.LeaseToken == "" {
		t.Errorf("w1Claim.LeaseToken is empty")
	}
	if w1Claim.AttemptID == "" {
		t.Errorf("w1Claim.AttemptID is empty")
	}
	if w1Claim.AttemptNo != 1 {
		t.Errorf("w1Claim.AttemptNo = %d, want 1", w1Claim.AttemptNo)
	}
	if !w1Claim.LeaseExpiresAt.After(time.Now()) {
		t.Errorf("w1Claim.LeaseExpiresAt = %v, expected in the future", w1Claim.LeaseExpiresAt)
	}

	// Verify durable running state and ownership persisted in app.jobs
	var (
		storedStatus     string
		storedExecStatus string
		storedClaimedBy  string
		storedToken      string
		storedExpiresAt  time.Time
		storedAttemptNo  int
	)
	err = platformDB.QueryRowContext(ctx, `
		SELECT status, execution_status, claimed_by, lease_token::text, lease_expires_at, attempt_no
		FROM app.jobs
		WHERE id = $1
	`, res.JobID).Scan(
		&storedStatus, &storedExecStatus, &storedClaimedBy, &storedToken, &storedExpiresAt, &storedAttemptNo,
	)
	if err != nil {
		t.Fatalf("query app.jobs: %v", err)
	}
	if storedStatus != "running" {
		t.Errorf("db jobs.status = %s, want running", storedStatus)
	}
	if storedExecStatus != "running" {
		t.Errorf("db jobs.execution_status = %s, want running", storedExecStatus)
	}
	if storedClaimedBy != "worker-1" {
		t.Errorf("db jobs.claimed_by = %s, want worker-1", storedClaimedBy)
	}
	if storedToken != w1Claim.LeaseToken {
		t.Errorf("db jobs.lease_token = %s, want %s", storedToken, w1Claim.LeaseToken)
	}
	if !storedExpiresAt.Equal(w1Claim.LeaseExpiresAt) {
		t.Errorf("db jobs.lease_expires_at = %v, want %v", storedExpiresAt, w1Claim.LeaseExpiresAt)
	}
	if storedAttemptNo != 1 {
		t.Errorf("db jobs.attempt_no = %d, want 1", storedAttemptNo)
	}

	// Verify durable running attempt persisted in app.job_attempts
	var (
		attemptID      string
		attemptJobID   string
		attemptExecID  string
		attemptDBID    string
		attemptDraftID string
		attemptRev     int
		attemptActorID string
		attemptNo      int
		attemptStatus  string
		sqlEntered     bool
	)
	err = platformDB.QueryRowContext(ctx, `
		SELECT id::text, job_id::text, execution_id::text, database_id::text, migration_id::text,
		       revision, actor_id::text, attempt_no, status, sql_entered
		FROM app.job_attempts
		WHERE job_id = $1
	`, res.JobID).Scan(
		&attemptID, &attemptJobID, &attemptExecID, &attemptDBID, &attemptDraftID,
		&attemptRev, &attemptActorID, &attemptNo, &attemptStatus, &sqlEntered,
	)
	if err != nil {
		t.Fatalf("query app.job_attempts: %v", err)
	}
	if attemptID != w1Claim.AttemptID {
		t.Errorf("attempt.id = %s, want %s", attemptID, w1Claim.AttemptID)
	}
	if attemptJobID != res.JobID {
		t.Errorf("attempt.job_id = %s, want %s", attemptJobID, res.JobID)
	}
	if attemptStatus != "running" {
		t.Errorf("attempt.status = %s, want running", attemptStatus)
	}
	if attemptNo != 1 {
		t.Errorf("attempt.attempt_no = %d, want 1", attemptNo)
	}
	if sqlEntered {
		t.Errorf("attempt.sql_entered = true, want false")
	}

	// Verify exactly one attempt row exists for this job
	var attemptCount int
	err = platformDB.QueryRowContext(ctx, "SELECT count(*) FROM app.job_attempts WHERE job_id = $1", res.JobID).Scan(&attemptCount)
	if err != nil {
		t.Fatalf("count app.job_attempts: %v", err)
	}
	if attemptCount != 1 {
		t.Errorf("attemptCount = %d, want 1", attemptCount)
	}

	// Subsequent claim attempt after winner has committed must also yield no work
	subsequentClaim, err := svc.ClaimJob(ctx, ClaimRequest{
		WorkerID:      "worker-2",
		LeaseDuration: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("subsequent claim failed: %v", err)
	}
	if subsequentClaim != nil {
		t.Fatalf("expected subsequent claim to find no work, got: %+v", subsequentClaim)
	}
}

func TestClaimJob_RejectsPositiveSubMillisecondLeaseDuration(t *testing.T) {
	ctx := context.Background()

	// Prove validation rejects before any DB access (closed DB would fail on BeginTx if reached)
	closedDB, err := sql.Open("pgx", "postgres://invalid:invalid@127.0.0.1:1/invalid")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	closedDB.Close()

	svcClosed := New(closedDB)
	claim, err := svcClosed.ClaimJob(ctx, ClaimRequest{
		WorkerID:      "worker-valid",
		LeaseDuration: 500 * time.Microsecond,
	})
	if claim != nil {
		t.Fatalf("expected nil claim, got %+v", claim)
	}
	var ce executioncontract.ContractError
	if !errors.As(err, &ce) {
		t.Fatalf("expected ContractError, got %v", err)
	}
	if ce.Code != "INVALID_REQUEST" {
		t.Errorf("ce.Code = %q, want INVALID_REQUEST", ce.Code)
	}
	if !strings.Contains(ce.Message, "lease_duration") {
		t.Errorf("ce.Message = %q, want mention of lease_duration", ce.Message)
	}

	// Prove against live DB with queued job that validation rejects and job is untouched
	platformDB, ownerDB := testDBs(t)
	f := createClaimFixture(t, platformDB, ownerDB)
	svc := New(platformDB)

	tx, err := platformDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	req := executioncontract.EnqueueRequest{
		DatabaseID:          f.dbID,
		TargetID:            f.targetID,
		RequestID:           uuid.NewString(),
		ProposalID:          f.proposalID,
		MigrationID:         f.draftID,
		Revision:            1,
		ActorID:             f.actorID,
		Action:              executioncontract.Apply,
		Direction:           executioncontract.Up,
		Snapshot:            f.snapshot,
		SnapshotHash:        f.hash,
		ApprovalFingerprint: "fp_sub_ms_test",
	}
	res, err := svc.EnqueueTx(ctx, tx, req)
	if err != nil {
		tx.Rollback()
		t.Fatalf("EnqueueTx: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit tx: %v", err)
	}

	claim, err = svc.ClaimJob(ctx, ClaimRequest{
		WorkerID:      "worker-valid",
		LeaseDuration: 500 * time.Microsecond,
	})
	if claim != nil {
		t.Fatalf("expected nil claim, got %+v", claim)
	}
	if !errors.As(err, &ce) || ce.Code != "INVALID_REQUEST" {
		t.Fatalf("expected INVALID_REQUEST, got %v", err)
	}

	var status string
	err = platformDB.QueryRowContext(ctx, "SELECT status FROM app.jobs WHERE id = $1", res.JobID).Scan(&status)
	if err != nil {
		t.Fatalf("query status: %v", err)
	}
	if status != "queued" {
		t.Errorf("job status = %s, want queued", status)
	}

	var attempts int
	err = platformDB.QueryRowContext(ctx, "SELECT count(*) FROM app.job_attempts WHERE job_id = $1", res.JobID).Scan(&attempts)
	if err != nil {
		t.Fatalf("count attempts: %v", err)
	}
	if attempts != 0 {
		t.Errorf("job attempts = %d, want 0", attempts)
	}
}

func TestClaimJob_Takeover_NoTakeoverBeforeExpiry(t *testing.T) {
	platformDB, ownerDB := testDBs(t)
	f := createClaimFixture(t, platformDB, ownerDB)
	svc := New(platformDB)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tx, err := platformDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	req := executioncontract.EnqueueRequest{
		DatabaseID:          f.dbID,
		TargetID:            f.targetID,
		RequestID:           uuid.NewString(),
		ProposalID:          f.proposalID,
		MigrationID:         f.draftID,
		Revision:            1,
		ActorID:             f.actorID,
		Action:              executioncontract.Apply,
		Direction:           executioncontract.Up,
		Snapshot:            f.snapshot,
		SnapshotHash:        f.hash,
		ApprovalFingerprint: "fp_no_takeover_before_expiry",
	}
	res, err := svc.EnqueueTx(ctx, tx, req)
	if err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	claim1, err := svc.ClaimJob(ctx, ClaimRequest{
		WorkerID:      "worker-1",
		LeaseDuration: 30 * time.Second,
	})
	if err != nil || claim1 == nil {
		t.Fatalf("worker-1 initial claim failed: claim=%+v, err=%v", claim1, err)
	}

	// Worker 2 attempts takeover while lease is active
	claim2, err := svc.ClaimJob(ctx, ClaimRequest{
		WorkerID:      "worker-2",
		LeaseDuration: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("worker-2 claim returned error: %v", err)
	}
	if claim2 != nil {
		t.Fatalf("expected worker-2 to get no work before expiry, got claim: %+v", claim2)
	}

	// Verify Worker 1 still owns the job in app.jobs
	var (
		dbClaimedBy string
		dbToken     string
		dbStatus    string
		dbAttemptNo int
	)
	err = platformDB.QueryRowContext(ctx, "SELECT claimed_by, lease_token::text, status, attempt_no FROM app.jobs WHERE id = $1", res.JobID).Scan(&dbClaimedBy, &dbToken, &dbStatus, &dbAttemptNo)
	if err != nil {
		t.Fatal(err)
	}
	if dbClaimedBy != "worker-1" || dbToken != claim1.LeaseToken || dbStatus != "running" || dbAttemptNo != 1 {
		t.Fatalf("job mutated unexpectedly: claimed_by=%s token=%s status=%s attempt_no=%d", dbClaimedBy, dbToken, dbStatus, dbAttemptNo)
	}

	// Verify only attempt 1 exists and is running
	var attemptCount int
	err = platformDB.QueryRowContext(ctx, "SELECT count(*) FROM app.job_attempts WHERE job_id = $1", res.JobID).Scan(&attemptCount)
	if err != nil {
		t.Fatal(err)
	}
	if attemptCount != 1 {
		t.Fatalf("attempt count = %d, want 1", attemptCount)
	}
}

func TestClaimJob_Takeover_ProvablyPreDispatchSuccess(t *testing.T) {
	platformDB, ownerDB := testDBs(t)
	f := createClaimFixture(t, platformDB, ownerDB)
	svc := New(platformDB)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tx, err := platformDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	req := executioncontract.EnqueueRequest{
		DatabaseID:          f.dbID,
		TargetID:            f.targetID,
		RequestID:           uuid.NewString(),
		ProposalID:          f.proposalID,
		MigrationID:         f.draftID,
		Revision:            1,
		ActorID:             f.actorID,
		Action:              executioncontract.Apply,
		Direction:           executioncontract.Up,
		Snapshot:            f.snapshot,
		SnapshotHash:        f.hash,
		ApprovalFingerprint: "fp_takeover_predispatch",
	}
	res, err := svc.EnqueueTx(ctx, tx, req)
	if err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	claim1, err := svc.ClaimJob(ctx, ClaimRequest{
		WorkerID:      "worker-1",
		LeaseDuration: 30 * time.Second,
	})
	if err != nil || claim1 == nil {
		t.Fatalf("worker-1 initial claim: claim=%+v err=%v", claim1, err)
	}

	// Fast-forward lease expiry using database platform clock without sleep
	_, err = platformDB.ExecContext(ctx, "UPDATE app.jobs SET lease_expires_at = clock_timestamp() - interval '5 seconds' WHERE id = $1", res.JobID)
	if err != nil {
		t.Fatal(err)
	}

	// Worker 2 calls ClaimJob after Worker 1 lease expiry: job transitions to retry_wait and is not due yet
	claimNotDue, err := svc.ClaimJob(ctx, ClaimRequest{
		WorkerID:      "worker-2",
		LeaseDuration: 45 * time.Second,
	})
	if err != nil {
		t.Fatalf("worker-2 claim returned error: %v", err)
	}
	if claimNotDue != nil {
		t.Fatalf("expected nil claim before retry is due, got: %+v", claimNotDue)
	}

	// Verify job transitioned to retry_wait with cleared ownership and future next_retry_at
	var (
		waitStatus     string
		waitExecStatus string
		waitClaimedBy  sql.NullString
		waitToken      sql.NullString
		waitExpiresAt  *time.Time
		waitNextRetry  *time.Time
		waitAttemptNo  int
		waitErrCode    sql.NullString
		waitRetryable  sql.NullBool
	)
	err = platformDB.QueryRowContext(ctx, `
		SELECT status, execution_status, claimed_by, lease_token::text, lease_expires_at, next_retry_at,
		       attempt_no, error_code, error_retryable
		FROM app.jobs
		WHERE id = $1
	`, res.JobID).Scan(&waitStatus, &waitExecStatus, &waitClaimedBy, &waitToken, &waitExpiresAt, &waitNextRetry, &waitAttemptNo, &waitErrCode, &waitRetryable)
	if err != nil {
		t.Fatal(err)
	}
	if waitStatus != "retry_wait" || waitExecStatus != "retry_wait" {
		t.Fatalf("job status = %s / %s, want retry_wait/retry_wait", waitStatus, waitExecStatus)
	}
	if waitClaimedBy.Valid || waitToken.Valid || waitExpiresAt != nil {
		t.Fatalf("ownership not cleared: claimed_by=%v token=%v expires_at=%v", waitClaimedBy, waitToken, waitExpiresAt)
	}
	if waitNextRetry == nil || !waitNextRetry.After(time.Now()) {
		t.Fatalf("expected next_retry_at in future, got %v", waitNextRetry)
	}
	if waitAttemptNo != 1 {
		t.Fatalf("attempt_no = %d, want 1 before due claim", waitAttemptNo)
	}
	if !waitErrCode.Valid || waitErrCode.String != "LEASE_EXPIRED" || !waitRetryable.Valid || !waitRetryable.Bool {
		t.Fatalf("unexpected error fields: code=%v retryable=%v", waitErrCode, waitRetryable)
	}

	// Fast-forward next_retry_at without sleeps so retry is due
	_, err = platformDB.ExecContext(ctx, "UPDATE app.jobs SET next_retry_at = clock_timestamp() - interval '1 second' WHERE id = $1", res.JobID)
	if err != nil {
		t.Fatal(err)
	}

	// Worker 2 claims the now-due scheduled retry
	claim2, err := svc.ClaimJob(ctx, ClaimRequest{
		WorkerID:      "worker-2",
		LeaseDuration: 45 * time.Second,
	})
	if err != nil {
		t.Fatalf("worker-2 due claim returned error: %v", err)
	}
	if claim2 == nil {
		t.Fatal("worker-2 due claim expected claim, got nil")
	}

	// Verify claim2 attributes
	if claim2.JobID != claim1.JobID {
		t.Errorf("claim2.JobID = %s, want %s", claim2.JobID, claim1.JobID)
	}
	if claim2.ExecutionID != claim1.ExecutionID {
		t.Errorf("claim2.ExecutionID = %s, want %s", claim2.ExecutionID, claim1.ExecutionID)
	}
	if claim2.ClaimedBy != "worker-2" {
		t.Errorf("claim2.ClaimedBy = %s, want worker-2", claim2.ClaimedBy)
	}
	if claim2.AttemptNo != 2 {
		t.Errorf("claim2.AttemptNo = %d, want 2", claim2.AttemptNo)
	}
	if claim2.AttemptID == claim1.AttemptID || claim2.AttemptID == "" {
		t.Errorf("claim2.AttemptID = %s, should be new non-empty UUID distinct from %s", claim2.AttemptID, claim1.AttemptID)
	}
	if claim2.LeaseToken == claim1.LeaseToken || claim2.LeaseToken == "" {
		t.Errorf("claim2.LeaseToken = %s, should be new non-empty UUID distinct from %s", claim2.LeaseToken, claim1.LeaseToken)
	}
	if !claim2.LeaseExpiresAt.After(time.Now()) {
		t.Errorf("claim2.LeaseExpiresAt = %v, expected in the future", claim2.LeaseExpiresAt)
	}

	// Verify app.jobs state
	var (
		dbStatus     string
		dbExecStatus string
		dbClaimedBy  string
		dbToken      string
		dbExpiresAt  time.Time
		dbAttemptNo  int
		dbErrorCode  sql.NullString
		dbErrorMsg   sql.NullString
		dbRetryable  sql.NullBool
	)
	err = platformDB.QueryRowContext(ctx, `
		SELECT status, execution_status, claimed_by, lease_token::text, lease_expires_at, attempt_no,
		       error_code, error_message, error_retryable
		FROM app.jobs
		WHERE id = $1
	`, res.JobID).Scan(&dbStatus, &dbExecStatus, &dbClaimedBy, &dbToken, &dbExpiresAt, &dbAttemptNo, &dbErrorCode, &dbErrorMsg, &dbRetryable)
	if err != nil {
		t.Fatal(err)
	}
	if dbStatus != "running" || dbExecStatus != "running" {
		t.Errorf("db jobs status = %s / %s, want running/running", dbStatus, dbExecStatus)
	}
	if dbClaimedBy != "worker-2" {
		t.Errorf("db jobs claimed_by = %s, want worker-2", dbClaimedBy)
	}
	if dbToken != claim2.LeaseToken {
		t.Errorf("db jobs lease_token = %s, want %s", dbToken, claim2.LeaseToken)
	}
	if dbAttemptNo != 2 {
		t.Errorf("db jobs attempt_no = %d, want 2", dbAttemptNo)
	}
	if !dbExpiresAt.Equal(claim2.LeaseExpiresAt) {
		t.Errorf("db jobs lease_expires_at = %v, want %v", dbExpiresAt, claim2.LeaseExpiresAt)
	}
	if dbErrorCode.Valid || dbErrorMsg.Valid || dbRetryable.Valid {
		t.Errorf("db jobs error fields should be null on running takeover: code=%v msg=%v retry=%v", dbErrorCode, dbErrorMsg, dbRetryable)
	}

	// Verify preservation of prior attempt (attempt 1) and new attempt (attempt 2)
	rows, err := platformDB.QueryContext(ctx, `
		SELECT id::text, attempt_no, status, sql_entered, finished_at IS NOT NULL,
		       COALESCE(error_code, ''), COALESCE(error_message, ''), COALESCE(error_retryable, false)
		FROM app.job_attempts
		WHERE job_id = $1
		ORDER BY attempt_no ASC
	`, res.JobID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	type attemptRecord struct {
		id          string
		attemptNo   int
		status      string
		sqlEntered  bool
		hasFinished bool
		errorCode   string
		errorMsg    string
		retryable   bool
	}
	var attempts []attemptRecord
	for rows.Next() {
		var a attemptRecord
		if err := rows.Scan(&a.id, &a.attemptNo, &a.status, &a.sqlEntered, &a.hasFinished, &a.errorCode, &a.errorMsg, &a.retryable); err != nil {
			t.Fatal(err)
		}
		attempts = append(attempts, a)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 2 {
		t.Fatalf("expected exactly 2 attempts, got %d", len(attempts))
	}

	// Prior attempt 1 verification
	att1 := attempts[0]
	if att1.id != claim1.AttemptID || att1.attemptNo != 1 {
		t.Errorf("att1 mismatch: id=%s (want %s), no=%d (want 1)", att1.id, claim1.AttemptID, att1.attemptNo)
	}
	if att1.status != "failed" {
		t.Errorf("att1.status = %s, want failed", att1.status)
	}
	if att1.sqlEntered {
		t.Errorf("att1.sqlEntered = true, want false")
	}
	if !att1.hasFinished {
		t.Errorf("att1.finished_at should not be null")
	}
	if att1.errorCode != "LEASE_EXPIRED" {
		t.Errorf("att1.errorCode = %s, want LEASE_EXPIRED", att1.errorCode)
	}
	if !att1.retryable {
		t.Errorf("att1.retryable = false, want true")
	}

	// New attempt 2 verification
	att2 := attempts[1]
	if att2.id != claim2.AttemptID || att2.attemptNo != 2 {
		t.Errorf("att2 mismatch: id=%s (want %s), no=%d (want 2)", att2.id, claim2.AttemptID, att2.attemptNo)
	}
	if att2.status != "running" {
		t.Errorf("att2.status = %s, want running", att2.status)
	}
	if att2.sqlEntered {
		t.Errorf("att2.sqlEntered = true, want false")
	}
	if att2.hasFinished {
		t.Errorf("att2.finished_at should be null for running attempt")
	}

	// Verify rejection of renewal by old token
	renewOld, err := svc.RenewLease(ctx, RenewLeaseRequest{
		JobID:         claim1.JobID,
		LeaseToken:    claim1.LeaseToken,
		LeaseDuration: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("RenewLease with old token returned error: %v", err)
	}
	if renewOld.Renewed {
		t.Errorf("expected renewal with old token to be rejected, got renewed=true")
	}

	// Verify new token CAN renew successfully
	renewNew, err := svc.RenewLease(ctx, RenewLeaseRequest{
		JobID:         claim2.JobID,
		LeaseToken:    claim2.LeaseToken,
		LeaseDuration: 60 * time.Second,
	})
	if err != nil {
		t.Fatalf("RenewLease with new token returned error: %v", err)
	}
	if !renewNew.Renewed {
		t.Errorf("expected renewal with new token to succeed, got renewed=false")
	}
}

func TestClaimJob_Takeover_LeavesEnteredOrUncertainSQLAlone(t *testing.T) {
	platformDB, ownerDB := testDBs(t)
	svc := New(platformDB)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	enqueueAndClaim := func(t *testing.T, tag string) (fixture, executioncontract.EnqueueResult, *Claim) {
		t.Helper()
		f := createClaimFixture(t, platformDB, ownerDB)
		tx, err := platformDB.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		req := executioncontract.EnqueueRequest{
			DatabaseID:          f.dbID,
			TargetID:            f.targetID,
			RequestID:           uuid.NewString(),
			ProposalID:          f.proposalID,
			MigrationID:         f.draftID,
			Revision:            1,
			ActorID:             f.actorID,
			Action:              executioncontract.Apply,
			Direction:           executioncontract.Up,
			Snapshot:            f.snapshot,
			SnapshotHash:        f.hash,
			ApprovalFingerprint: "fp_" + tag,
		}
		res, err := svc.EnqueueTx(ctx, tx, req)
		if err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		claim, err := svc.ClaimJob(ctx, ClaimRequest{
			WorkerID:      "worker-orig-" + tag,
			LeaseDuration: 30 * time.Second,
		})
		if err != nil || claim == nil {
			t.Fatalf("claim failed: %v", err)
		}
		return f, res, claim
	}

	t.Run("leaves job alone when sql_entered is true", func(t *testing.T) {
		_, res, claim := enqueueAndClaim(t, "sql_entered")

		// Mark attempt as having entered SQL
		_, err := platformDB.ExecContext(ctx, "UPDATE app.job_attempts SET sql_entered = true WHERE id = $1", claim.AttemptID)
		if err != nil {
			t.Fatal(err)
		}
		// Expire lease
		_, err = platformDB.ExecContext(ctx, "UPDATE app.jobs SET lease_expires_at = clock_timestamp() - interval '5 seconds' WHERE id = $1", res.JobID)
		if err != nil {
			t.Fatal(err)
		}

		takeoverClaim, err := svc.ClaimJob(ctx, ClaimRequest{
			WorkerID:      "worker-takeover",
			LeaseDuration: 30 * time.Second,
		})
		if err != nil {
			t.Fatalf("ClaimJob error: %v", err)
		}
		if takeoverClaim != nil {
			t.Fatalf("expected job with sql_entered=true to be left alone, but got claim: %+v", takeoverClaim)
		}

		// Verify job and attempt untouched
		var (
			claimedBy  string
			attemptNo  int
			sqlEntered bool
		)
		err = platformDB.QueryRowContext(ctx, "SELECT claimed_by, attempt_no FROM app.jobs WHERE id = $1", res.JobID).Scan(&claimedBy, &attemptNo)
		if err != nil {
			t.Fatal(err)
		}
		if claimedBy != claim.ClaimedBy || attemptNo != 1 {
			t.Errorf("app.jobs altered: claimed_by=%s attempt_no=%d", claimedBy, attemptNo)
		}
		err = platformDB.QueryRowContext(ctx, "SELECT sql_entered FROM app.job_attempts WHERE id = $1", claim.AttemptID).Scan(&sqlEntered)
		if err != nil {
			t.Fatal(err)
		}
		if !sqlEntered {
			t.Errorf("attempt sql_entered reset to false")
		}
	})

	t.Run("leaves job alone when statement evidence exists", func(t *testing.T) {
		_, res, claim := enqueueAndClaim(t, "evidence")

		// Insert statement evidence for this attempt
		_, err := platformDB.ExecContext(ctx, `
			INSERT INTO app.job_statement_evidence (attempt_id, ordinal, sql_text, outcome)
			VALUES ($1, 1, 'UPDATE items SET id = 1', 'succeeded')
		`, claim.AttemptID)
		if err != nil {
			t.Fatal(err)
		}
		// Expire lease
		_, err = platformDB.ExecContext(ctx, "UPDATE app.jobs SET lease_expires_at = clock_timestamp() - interval '5 seconds' WHERE id = $1", res.JobID)
		if err != nil {
			t.Fatal(err)
		}

		takeoverClaim, err := svc.ClaimJob(ctx, ClaimRequest{
			WorkerID:      "worker-takeover",
			LeaseDuration: 30 * time.Second,
		})
		if err != nil {
			t.Fatalf("ClaimJob error: %v", err)
		}
		if takeoverClaim != nil {
			t.Fatalf("expected job with statement evidence to be left alone, but got claim: %+v", takeoverClaim)
		}

		var claimedBy string
		err = platformDB.QueryRowContext(ctx, "SELECT claimed_by FROM app.jobs WHERE id = $1", res.JobID).Scan(&claimedBy)
		if err != nil {
			t.Fatal(err)
		}
		if claimedBy != claim.ClaimedBy {
			t.Errorf("app.jobs claimed_by altered: %s", claimedBy)
		}
	})

	t.Run("leaves job alone when attempt status is unknown", func(t *testing.T) {
		_, res, claim := enqueueAndClaim(t, "unknown_attempt")

		// Update attempt status to unknown
		_, err := platformDB.ExecContext(ctx, "UPDATE app.job_attempts SET status = 'unknown' WHERE id = $1", claim.AttemptID)
		if err != nil {
			t.Fatal(err)
		}
		// Expire lease
		_, err = platformDB.ExecContext(ctx, "UPDATE app.jobs SET lease_expires_at = clock_timestamp() - interval '5 seconds' WHERE id = $1", res.JobID)
		if err != nil {
			t.Fatal(err)
		}

		takeoverClaim, err := svc.ClaimJob(ctx, ClaimRequest{
			WorkerID:      "worker-takeover",
			LeaseDuration: 30 * time.Second,
		})
		if err != nil {
			t.Fatalf("ClaimJob error: %v", err)
		}
		if takeoverClaim != nil {
			t.Fatalf("expected job with unknown attempt status to be left alone, but got claim: %+v", takeoverClaim)
		}
	})

	t.Run("leaves job alone when execution_status is not running", func(t *testing.T) {
		_, res, _ := enqueueAndClaim(t, "exec_unknown")

		// Update job execution_status to unknown
		_, err := platformDB.ExecContext(ctx, "UPDATE app.jobs SET execution_status = 'unknown' WHERE id = $1", res.JobID)
		if err != nil {
			t.Fatal(err)
		}
		// Expire lease
		_, err = platformDB.ExecContext(ctx, "UPDATE app.jobs SET lease_expires_at = clock_timestamp() - interval '5 seconds' WHERE id = $1", res.JobID)
		if err != nil {
			t.Fatal(err)
		}

		takeoverClaim, err := svc.ClaimJob(ctx, ClaimRequest{
			WorkerID:      "worker-takeover",
			LeaseDuration: 30 * time.Second,
		})
		if err != nil {
			t.Fatalf("ClaimJob error: %v", err)
		}
		if takeoverClaim != nil {
			t.Fatalf("expected job with execution_status=unknown to be left alone, but got claim: %+v", takeoverClaim)
		}
	})
}

func TestClaimJob_SourceEvidenceBlocksQueuedResume(t *testing.T) {
	platformDB, ownerDB := testDBs(t)
	f := createClaimFixture(t, platformDB, ownerDB)
	svc := New(platformDB)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Enqueue and claim the source job
	tx1, err := platformDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	req1 := executioncontract.EnqueueRequest{
		DatabaseID:          f.dbID,
		TargetID:            f.targetID,
		RequestID:           uuid.NewString(),
		ProposalID:          f.proposalID,
		MigrationID:         f.draftID,
		Revision:            1,
		ActorID:             f.actorID,
		Action:              executioncontract.Apply,
		Direction:           executioncontract.Up,
		Snapshot:            f.snapshot,
		SnapshotHash:        f.hash,
		ApprovalFingerprint: "fp_source_evidence",
	}
	res1, err := svc.EnqueueTx(ctx, tx1, req1)
	if err != nil {
		tx1.Rollback()
		t.Fatal(err)
	}
	if err := tx1.Commit(); err != nil {
		t.Fatal(err)
	}

	claim1, err := svc.ClaimJob(ctx, ClaimRequest{
		WorkerID:      "worker-source",
		LeaseDuration: 30 * time.Second,
	})
	if err != nil || claim1 == nil {
		t.Fatalf("worker-source initial claim failed: claim=%+v, err=%v", claim1, err)
	}

	// Unproven source evidence keeps the physical target blocked.
	_, err = platformDB.ExecContext(ctx, `
		INSERT INTO app.job_statement_evidence (attempt_id, ordinal, sql_text, outcome)
		VALUES ($1, 1, 'CREATE TABLE items (id int);', 'succeeded')
	`, claim1.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = platformDB.ExecContext(ctx, "UPDATE app.job_attempts SET status = 'failed', finished_at = clock_timestamp() WHERE id = $1", claim1.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = platformDB.ExecContext(ctx, "UPDATE app.jobs SET status = 'failed', execution_status = 'failed', finished_at = clock_timestamp() WHERE id = $1", res1.JobID)
	if err != nil {
		t.Fatal(err)
	}

	// Enqueue resume job referencing source execution
	sourceExecID := claim1.ExecutionID
	createFixtureSource(t, ctx, platformDB, f, sourceExecID)
	resumeProposalID := createFixtureProposal(t, ctx, platformDB, f, executioncontract.Resume, &sourceExecID)
	tx2, err := platformDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	req2 := executioncontract.EnqueueRequest{
		DatabaseID:          f.dbID,
		TargetID:            f.targetID,
		RequestID:           uuid.NewString(),
		ProposalID:          resumeProposalID,
		MigrationID:         f.draftID,
		Revision:            1,
		ActorID:             f.actorID,
		Action:              executioncontract.Resume,
		Direction:           executioncontract.Up,
		SourceExecutionID:   &sourceExecID,
		Snapshot:            f.snapshot,
		SnapshotHash:        f.hash,
		ApprovalFingerprint: "fp_resume_with_source",
	}
	res2, err := svc.EnqueueTx(ctx, tx2, req2)
	if err != nil {
		tx2.Rollback()
		t.Fatal(err)
	}
	if err := tx2.Commit(); err != nil {
		t.Fatal(err)
	}

	claim2, err := svc.ClaimJob(ctx, ClaimRequest{
		WorkerID:      "worker-resume",
		LeaseDuration: 30 * time.Second,
	})
	if err != nil || claim2 != nil {
		t.Fatalf("source evidence must prevent dispatch: claim=%+v err=%v", claim2, err)
	}
	var status, executionStatus, source string
	var attemptNo, attempts, sourceEvidence int
	var blocked, hasLease bool
	err = platformDB.QueryRowContext(ctx, `
		SELECT status, execution_status, source_execution_id::text, attempt_no, blocks_database,
		       lease_token IS NOT NULL,
		       (SELECT count(*) FROM app.job_attempts WHERE job_id = $1),
		       (SELECT count(*) FROM app.job_statement_evidence WHERE attempt_id = $2)
		FROM app.jobs WHERE id = $1
	`, res2.JobID, claim1.AttemptID).Scan(&status, &executionStatus, &source, &attemptNo, &blocked, &hasLease, &attempts, &sourceEvidence)
	if err != nil || status != "queued" || executionStatus != "queued" || source != sourceExecID ||
		attemptNo != 1 || blocked || hasLease || attempts != 0 || sourceEvidence != 1 {
		t.Fatalf("queued resume changed: status=%s execution=%s source=%s attempt=%d blocked=%v lease=%v attempts=%d sourceEvidence=%d err=%v",
			status, executionStatus, source, attemptNo, blocked, hasLease, attempts, sourceEvidence, err)
	}
}

func TestClaimJob_ScheduledRetry_NotDue(t *testing.T) {
	platformDB, ownerDB := testDBs(t)
	f := createClaimFixture(t, platformDB, ownerDB)
	svc := New(platformDB)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tx, err := platformDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	req := executioncontract.EnqueueRequest{
		DatabaseID:          f.dbID,
		TargetID:            f.targetID,
		RequestID:           uuid.NewString(),
		ProposalID:          f.proposalID,
		MigrationID:         f.draftID,
		Revision:            1,
		ActorID:             f.actorID,
		Action:              executioncontract.Apply,
		Direction:           executioncontract.Up,
		Snapshot:            f.snapshot,
		SnapshotHash:        f.hash,
		ApprovalFingerprint: "fp_not_due",
	}
	res, err := svc.EnqueueTx(ctx, tx, req)
	if err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	claim1, err := svc.ClaimJob(ctx, ClaimRequest{
		WorkerID:      "worker-1",
		LeaseDuration: 30 * time.Second,
	})
	if err != nil || claim1 == nil {
		t.Fatalf("worker-1 initial claim failed: claim=%+v, err=%v", claim1, err)
	}

	// Expire lease
	_, err = platformDB.ExecContext(ctx, "UPDATE app.jobs SET lease_expires_at = clock_timestamp() - interval '5 seconds' WHERE id = $1", res.JobID)
	if err != nil {
		t.Fatal(err)
	}

	// Worker 2 calls ClaimJob: job should transition to retry_wait and return nil
	claim2, err := svc.ClaimJob(ctx, ClaimRequest{
		WorkerID:      "worker-2",
		LeaseDuration: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("worker-2 ClaimJob error: %v", err)
	}
	if claim2 != nil {
		t.Fatalf("expected nil claim before retry is due, got %+v", claim2)
	}

	// Worker 3 calls ClaimJob while next_retry_at is still in future: should still get nil
	claim3, err := svc.ClaimJob(ctx, ClaimRequest{
		WorkerID:      "worker-3",
		LeaseDuration: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("worker-3 ClaimJob error: %v", err)
	}
	if claim3 != nil {
		t.Fatalf("expected nil claim from worker-3 while not due, got %+v", claim3)
	}

	// Verify app.jobs state remains retry_wait with cleared ownership
	var (
		status      string
		claimedBy   sql.NullString
		leaseToken  sql.NullString
		nextRetryAt *time.Time
	)
	err = platformDB.QueryRowContext(ctx, "SELECT status, claimed_by, lease_token::text, next_retry_at FROM app.jobs WHERE id = $1", res.JobID).Scan(&status, &claimedBy, &leaseToken, &nextRetryAt)
	if err != nil {
		t.Fatal(err)
	}
	if status != "retry_wait" {
		t.Errorf("status = %s, want retry_wait", status)
	}
	if claimedBy.Valid || leaseToken.Valid {
		t.Errorf("ownership not cleared: claimed_by=%v lease_token=%v", claimedBy, leaseToken)
	}
	if nextRetryAt == nil || !nextRetryAt.After(time.Now()) {
		t.Errorf("expected next_retry_at in future, got %v", nextRetryAt)
	}
}

func TestClaimJob_ScheduledRetry_DueOpensSingleAttempt(t *testing.T) {
	platformDB, ownerDB := testDBs(t)
	f := createClaimFixture(t, platformDB, ownerDB)
	svc := New(platformDB)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tx, err := platformDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	req := executioncontract.EnqueueRequest{
		DatabaseID:          f.dbID,
		TargetID:            f.targetID,
		RequestID:           uuid.NewString(),
		ProposalID:          f.proposalID,
		MigrationID:         f.draftID,
		Revision:            1,
		ActorID:             f.actorID,
		Action:              executioncontract.Apply,
		Direction:           executioncontract.Up,
		Snapshot:            f.snapshot,
		SnapshotHash:        f.hash,
		ApprovalFingerprint: "fp_due_single",
	}
	res, err := svc.EnqueueTx(ctx, tx, req)
	if err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	claim1, err := svc.ClaimJob(ctx, ClaimRequest{
		WorkerID:      "worker-1",
		LeaseDuration: 30 * time.Second,
	})
	if err != nil || claim1 == nil {
		t.Fatalf("worker-1 initial claim failed: %v", err)
	}

	// Expire lease
	_, err = platformDB.ExecContext(ctx, "UPDATE app.jobs SET lease_expires_at = clock_timestamp() - interval '5 seconds' WHERE id = $1", res.JobID)
	if err != nil {
		t.Fatal(err)
	}
	// Trigger reclaim to retry_wait
	claimNotDue, err := svc.ClaimJob(ctx, ClaimRequest{WorkerID: "worker-2", LeaseDuration: 30 * time.Second})
	if err != nil || claimNotDue != nil {
		t.Fatalf("expected nil claim during reclaim, got claim=%+v err=%v", claimNotDue, err)
	}

	// Fast-forward next_retry_at to past (due)
	_, err = platformDB.ExecContext(ctx, "UPDATE app.jobs SET next_retry_at = clock_timestamp() - interval '2 seconds' WHERE id = $1", res.JobID)
	if err != nil {
		t.Fatal(err)
	}

	// Worker 2 claims the due retry
	claim2, err := svc.ClaimJob(ctx, ClaimRequest{
		WorkerID:      "worker-2",
		LeaseDuration: 30 * time.Second,
	})
	if err != nil || claim2 == nil {
		t.Fatalf("worker-2 claim due retry failed: claim=%+v, err=%v", claim2, err)
	}

	if claim2.AttemptNo != 2 {
		t.Errorf("claim2.AttemptNo = %d, want 2", claim2.AttemptNo)
	}
	if claim2.AttemptID == claim1.AttemptID || claim2.AttemptID == "" {
		t.Errorf("claim2.AttemptID = %s, want fresh UUID distinct from %s", claim2.AttemptID, claim1.AttemptID)
	}
	if claim2.LeaseToken == claim1.LeaseToken || claim2.LeaseToken == "" {
		t.Errorf("claim2.LeaseToken = %s, want fresh UUID distinct from %s", claim2.LeaseToken, claim1.LeaseToken)
	}

	// Verify exactly 2 attempts exist in app.job_attempts
	var attemptCount int
	err = platformDB.QueryRowContext(ctx, "SELECT count(*) FROM app.job_attempts WHERE job_id = $1", res.JobID).Scan(&attemptCount)
	if err != nil {
		t.Fatal(err)
	}
	if attemptCount != 2 {
		t.Fatalf("expected exactly 2 attempts in app.job_attempts, got %d", attemptCount)
	}
}

func TestClaimJob_ScheduledRetry_RepeatedExpiryBound(t *testing.T) {
	platformDB, ownerDB := testDBs(t)
	f := createClaimFixture(t, platformDB, ownerDB)
	svc := New(platformDB)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	tx, err := platformDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	req := executioncontract.EnqueueRequest{
		DatabaseID:          f.dbID,
		TargetID:            f.targetID,
		RequestID:           uuid.NewString(),
		ProposalID:          f.proposalID,
		MigrationID:         f.draftID,
		Revision:            1,
		ActorID:             f.actorID,
		Action:              executioncontract.Apply,
		Direction:           executioncontract.Up,
		Snapshot:            f.snapshot,
		SnapshotHash:        f.hash,
		ApprovalFingerprint: "fp_bound",
	}
	res, err := svc.EnqueueTx(ctx, tx, req)
	if err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	// Attempt 1: Claim and expire
	c1, err := svc.ClaimJob(ctx, ClaimRequest{WorkerID: "w1", LeaseDuration: 30 * time.Second})
	if err != nil || c1 == nil || c1.AttemptNo != 1 {
		t.Fatalf("c1 failed: %+v, %v", c1, err)
	}
	_, err = platformDB.ExecContext(ctx, "UPDATE app.jobs SET lease_expires_at = clock_timestamp() - interval '5 seconds' WHERE id = $1", res.JobID)
	if err != nil {
		t.Fatal(err)
	}
	// Reclaim attempt 1 -> retry_wait
	r1, err := svc.ClaimJob(ctx, ClaimRequest{WorkerID: "w2", LeaseDuration: 30 * time.Second})
	if err != nil || r1 != nil {
		t.Fatalf("expected nil claim on reclaim 1, got %+v, %v", r1, err)
	}

	// Make due and claim Attempt 2
	_, err = platformDB.ExecContext(ctx, "UPDATE app.jobs SET next_retry_at = clock_timestamp() - interval '1 second' WHERE id = $1", res.JobID)
	if err != nil {
		t.Fatal(err)
	}
	c2, err := svc.ClaimJob(ctx, ClaimRequest{WorkerID: "w2", LeaseDuration: 30 * time.Second})
	if err != nil || c2 == nil || c2.AttemptNo != 2 {
		t.Fatalf("c2 failed: %+v, %v", c2, err)
	}
	// Expire attempt 2
	_, err = platformDB.ExecContext(ctx, "UPDATE app.jobs SET lease_expires_at = clock_timestamp() - interval '5 seconds' WHERE id = $1", res.JobID)
	if err != nil {
		t.Fatal(err)
	}
	// Reclaim attempt 2 -> retry_wait
	r2, err := svc.ClaimJob(ctx, ClaimRequest{WorkerID: "w3", LeaseDuration: 30 * time.Second})
	if err != nil || r2 != nil {
		t.Fatalf("expected nil claim on reclaim 2, got %+v, %v", r2, err)
	}

	// Make due and claim Attempt 3 (DefaultMaxAttempts = 3)
	_, err = platformDB.ExecContext(ctx, "UPDATE app.jobs SET next_retry_at = clock_timestamp() - interval '1 second' WHERE id = $1", res.JobID)
	if err != nil {
		t.Fatal(err)
	}
	c3, err := svc.ClaimJob(ctx, ClaimRequest{WorkerID: "w3", LeaseDuration: 30 * time.Second})
	if err != nil || c3 == nil || c3.AttemptNo != 3 {
		t.Fatalf("c3 failed: %+v, %v", c3, err)
	}
	// Expire attempt 3
	_, err = platformDB.ExecContext(ctx, "UPDATE app.jobs SET lease_expires_at = clock_timestamp() - interval '5 seconds' WHERE id = $1", res.JobID)
	if err != nil {
		t.Fatal(err)
	}

	// Reclaim attempt 3: bound reached! Should transition to 'failed' rather than 'retry_wait'
	r3, err := svc.ClaimJob(ctx, ClaimRequest{WorkerID: "w4", LeaseDuration: 30 * time.Second})
	if err != nil || r3 != nil {
		t.Fatalf("expected nil claim on bound reached, got %+v, %v", r3, err)
	}

	// Verify terminal failed state on app.jobs
	var (
		jobStatus    string
		execStatus   string
		claimedBy    sql.NullString
		leaseToken   sql.NullString
		nextRetryAt  *time.Time
		finishedAt   *time.Time
		errCode      string
		errRetryable bool
	)
	err = platformDB.QueryRowContext(ctx, `
		SELECT status, execution_status, claimed_by, lease_token::text, next_retry_at, finished_at,
		       COALESCE(error_code, ''), COALESCE(error_retryable, true)
		FROM app.jobs
		WHERE id = $1
	`, res.JobID).Scan(&jobStatus, &execStatus, &claimedBy, &leaseToken, &nextRetryAt, &finishedAt, &errCode, &errRetryable)
	if err != nil {
		t.Fatal(err)
	}
	if jobStatus != "failed" || execStatus != "failed" {
		t.Errorf("job status = %s / %s, want failed/failed", jobStatus, execStatus)
	}
	if claimedBy.Valid || leaseToken.Valid {
		t.Errorf("ownership should be cleared: claimed_by=%v token=%v", claimedBy, leaseToken)
	}
	if nextRetryAt != nil {
		t.Errorf("next_retry_at should be null at bound, got %v", nextRetryAt)
	}
	if finishedAt == nil {
		t.Errorf("finished_at should not be null for terminal failure")
	}
	if errRetryable {
		t.Errorf("error_retryable = true, want false at bound")
	}
	if errCode != "LEASE_EXPIRED" {
		t.Errorf("errCode = %s, want LEASE_EXPIRED", errCode)
	}

	// Verify all 3 attempts recorded in app.job_attempts
	var attemptCount int
	err = platformDB.QueryRowContext(ctx, "SELECT count(*) FROM app.job_attempts WHERE job_id = $1 AND status = 'failed' AND error_code = 'LEASE_EXPIRED'", res.JobID).Scan(&attemptCount)
	if err != nil {
		t.Fatal(err)
	}
	if attemptCount != 3 {
		t.Fatalf("expected 3 failed attempts in app.job_attempts, got %d", attemptCount)
	}

	// Verify subsequent claim cannot claim the failed job
	cFinal, err := svc.ClaimJob(ctx, ClaimRequest{WorkerID: "w5", LeaseDuration: 30 * time.Second})
	if err != nil {
		t.Fatalf("cFinal error: %v", err)
	}
	if cFinal != nil {
		t.Fatalf("expected nil claim on permanently failed job, got %+v", cFinal)
	}
}

func TestClaimJob_ScheduledRetry_DuplicateOrAmbiguousAttempt(t *testing.T) {
	platformDB, ownerDB := testDBs(t)
	svc := New(platformDB)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	t.Run("duplicate running attempts prevents reclaim", func(t *testing.T) {
		f := createClaimFixture(t, platformDB, ownerDB)

		tx, err := platformDB.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		req := executioncontract.EnqueueRequest{
			DatabaseID:          f.dbID,
			TargetID:            f.targetID,
			RequestID:           uuid.NewString(),
			ProposalID:          f.proposalID,
			MigrationID:         f.draftID,
			Revision:            1,
			ActorID:             f.actorID,
			Action:              executioncontract.Apply,
			Direction:           executioncontract.Up,
			Snapshot:            f.snapshot,
			SnapshotHash:        f.hash,
			ApprovalFingerprint: "fp_dup_att",
		}
		res, err := svc.EnqueueTx(ctx, tx, req)
		if err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}

		c1, err := svc.ClaimJob(ctx, ClaimRequest{WorkerID: "w1", LeaseDuration: 30 * time.Second})
		if err != nil || c1 == nil {
			t.Fatalf("claim failed: %v", err)
		}

		// Inject duplicate running attempt for same job
		_, err = platformDB.ExecContext(ctx, `
			INSERT INTO app.job_attempts (
				job_id, execution_id, database_id, migration_id, revision,
				actor_id, attempt_no, status, sql_entered, started_at, created_at
			) VALUES (
				$1, $2, $3, $4, 1,
				$5, 1, 'running', false, clock_timestamp(), clock_timestamp()
			)
		`, res.JobID, c1.ExecutionID, f.dbID, f.draftID, f.actorID)
		if err != nil {
			t.Fatal(err)
		}

		// Expire lease
		_, err = platformDB.ExecContext(ctx, "UPDATE app.jobs SET lease_expires_at = clock_timestamp() - interval '5 seconds' WHERE id = $1", res.JobID)
		if err != nil {
			t.Fatal(err)
		}

		// Worker 2 calls ClaimJob: ambiguous attempt state must reject reclaim
		c2, err := svc.ClaimJob(ctx, ClaimRequest{WorkerID: "w2", LeaseDuration: 30 * time.Second})
		if err != nil {
			t.Fatalf("ClaimJob error: %v", err)
		}
		if c2 != nil {
			t.Fatalf("expected nil claim on ambiguous job, got %+v", c2)
		}

		// Job must remain untouched (running, original claimed_by)
		var status, claimedBy string
		err = platformDB.QueryRowContext(ctx, "SELECT status, claimed_by FROM app.jobs WHERE id = $1", res.JobID).Scan(&status, &claimedBy)
		if err != nil {
			t.Fatal(err)
		}
		if status != "running" || claimedBy != "w1" {
			t.Errorf("app.jobs altered on ambiguous attempt: status=%s claimed_by=%s", status, claimedBy)
		}
	})

	t.Run("ambiguous prior attempt state prevents reclaim", func(t *testing.T) {
		f := createClaimFixture(t, platformDB, ownerDB)

		tx, err := platformDB.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		req := executioncontract.EnqueueRequest{
			DatabaseID:          f.dbID,
			TargetID:            f.targetID,
			RequestID:           uuid.NewString(),
			ProposalID:          f.proposalID,
			MigrationID:         f.draftID,
			Revision:            1,
			ActorID:             f.actorID,
			Action:              executioncontract.Apply,
			Direction:           executioncontract.Up,
			Snapshot:            f.snapshot,
			SnapshotHash:        f.hash,
			ApprovalFingerprint: "fp_ambig_prior",
		}
		res, err := svc.EnqueueTx(ctx, tx, req)
		if err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}

		c1, err := svc.ClaimJob(ctx, ClaimRequest{WorkerID: "w1", LeaseDuration: 30 * time.Second})
		if err != nil || c1 == nil {
			t.Fatalf("claim 1 failed: %v", err)
		}

		// Transition to retry_wait and claim attempt 2
		_, err = platformDB.ExecContext(ctx, "UPDATE app.jobs SET lease_expires_at = clock_timestamp() - interval '5 seconds' WHERE id = $1", res.JobID)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = svc.ClaimJob(ctx, ClaimRequest{WorkerID: "w2", LeaseDuration: 30 * time.Second})
		_, err = platformDB.ExecContext(ctx, "UPDATE app.jobs SET next_retry_at = clock_timestamp() - interval '1 second' WHERE id = $1", res.JobID)
		if err != nil {
			t.Fatal(err)
		}
		c2, err := svc.ClaimJob(ctx, ClaimRequest{WorkerID: "w2", LeaseDuration: 30 * time.Second})
		if err != nil || c2 == nil || c2.AttemptNo != 2 {
			t.Fatalf("claim 2 failed: %+v, %v", c2, err)
		}

		// Mutate prior attempt (attempt 1) to an ambiguous state: status = 'unknown'
		_, err = platformDB.ExecContext(ctx, "UPDATE app.job_attempts SET status = 'unknown' WHERE id = $1", c1.AttemptID)
		if err != nil {
			t.Fatal(err)
		}

		// Expire attempt 2's lease
		_, err = platformDB.ExecContext(ctx, "UPDATE app.jobs SET lease_expires_at = clock_timestamp() - interval '5 seconds' WHERE id = $1", res.JobID)
		if err != nil {
			t.Fatal(err)
		}

		// Worker 3 calls ClaimJob: ambiguous prior attempt state must prevent reclaim
		c3, err := svc.ClaimJob(ctx, ClaimRequest{WorkerID: "w3", LeaseDuration: 30 * time.Second})
		if err != nil {
			t.Fatalf("ClaimJob error: %v", err)
		}
		if c3 != nil {
			t.Fatalf("expected nil claim on ambiguous prior attempt, got %+v", c3)
		}

		// Verify job remains in running on attempt 2
		var status string
		var attemptNo int
		err = platformDB.QueryRowContext(ctx, "SELECT status, attempt_no FROM app.jobs WHERE id = $1", res.JobID).Scan(&status, &attemptNo)
		if err != nil {
			t.Fatal(err)
		}
		if status != "running" || attemptNo != 2 {
			t.Errorf("job state mutated despite ambiguous prior attempt: status=%s attempt_no=%d", status, attemptNo)
		}
	})
}

func TestClaimJob_ScheduledRetry_Scope(t *testing.T) {
	platformDB, ownerDB := testDBs(t)
	svc := New(platformDB)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	f1 := createClaimFixture(t, platformDB, ownerDB)
	f2 := createClaimFixture(t, platformDB, ownerDB)

	tx1, err := platformDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	res1, err := svc.EnqueueTx(ctx, tx1, executioncontract.EnqueueRequest{
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
		ApprovalFingerprint: "fp_scope_1",
	})
	if err != nil {
		tx1.Rollback()
		t.Fatal(err)
	}
	if err := tx1.Commit(); err != nil {
		t.Fatal(err)
	}

	tx2, err := platformDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	res2, err := svc.EnqueueTx(ctx, tx2, executioncontract.EnqueueRequest{
		DatabaseID:          f2.dbID,
		TargetID:            f2.targetID,
		RequestID:           uuid.NewString(),
		ProposalID:          f2.proposalID,
		MigrationID:         f2.draftID,
		Revision:            1,
		ActorID:             f2.actorID,
		Action:              executioncontract.Apply,
		Direction:           executioncontract.Up,
		Snapshot:            f2.snapshot,
		SnapshotHash:        f2.hash,
		ApprovalFingerprint: "fp_scope_2",
	})
	if err != nil {
		tx2.Rollback()
		t.Fatal(err)
	}
	if err := tx2.Commit(); err != nil {
		t.Fatal(err)
	}

	// Worker 1 claims job 1
	c1, err := svc.ClaimJob(ctx, ClaimRequest{WorkerID: "w1", LeaseDuration: 30 * time.Second})
	if err != nil || c1 == nil || c1.JobID != res1.JobID {
		t.Fatalf("expected job 1 claim, got %+v, %v", c1, err)
	}

	// Expire job 1's lease
	_, err = platformDB.ExecContext(ctx, "UPDATE app.jobs SET lease_expires_at = clock_timestamp() - interval '5 seconds' WHERE id = $1", res1.JobID)
	if err != nil {
		t.Fatal(err)
	}

	// Worker 2 calls ClaimJob:
	// Job 1 is reclaimed to retry_wait (not due).
	// Job 2 (which is queued on db2) should now be claimed!
	c2, err := svc.ClaimJob(ctx, ClaimRequest{WorkerID: "w2", LeaseDuration: 30 * time.Second})
	if err != nil || c2 == nil {
		t.Fatalf("expected job 2 claim, got %+v, %v", c2, err)
	}
	if c2.JobID != res2.JobID || c2.DatabaseID != f2.dbID {
		t.Fatalf("claimed job mismatch: got jobID=%s dbID=%s, want %s on %s", c2.JobID, c2.DatabaseID, res2.JobID, f2.dbID)
	}

	// Scope read isolation:
	job1View, err := svc.GetJob(ctx, f1.dbID, res1.JobID)
	if err != nil {
		t.Fatalf("GetJob(f1.dbID, res1.JobID) error: %v", err)
	}
	if job1View.Status != executioncontract.RetryWait {
		t.Errorf("job 1 view status = %s, want retry_wait", job1View.Status)
	}
	if job1View.NextRetryAt == nil {
		t.Errorf("job 1 view next_retry_at should not be nil")
	}

	// GetJob on database 2 attempting to access job 1 MUST return NOT_FOUND (scoped)
	_, err = svc.GetJob(ctx, f2.dbID, res1.JobID)
	if err == nil {
		t.Fatalf("expected error reading job 1 with database 2 scope, got nil")
	}
	var contractErr executioncontract.ContractError
	if errors.As(err, &contractErr) && contractErr.Code != "NOT_FOUND" {
		t.Errorf("contract error code = %s, want NOT_FOUND", contractErr.Code)
	}
}

func TestReclaimExpiredJobsSkipsAmbiguousFirstCandidate(t *testing.T) {
	db, owner := testDBs(t)
	svc := New(db)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	first, second := createClaimFixture(t, db, owner), createClaimFixture(t, db, owner)
	enqueue := func(f fixture) {
		t.Helper()
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		_, err = svc.EnqueueTx(ctx, tx, executioncontract.EnqueueRequest{
			DatabaseID: f.dbID, TargetID: f.targetID, RequestID: uuid.NewString(),
			ProposalID: f.proposalID, MigrationID: f.draftID, Revision: 1,
			ActorID: f.actorID, Action: executioncontract.Apply, Direction: executioncontract.Up,
			Snapshot: f.snapshot, SnapshotHash: f.hash, ApprovalFingerprint: "fp",
		})
		if err != nil {
			t.Fatal(err)
		}
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	enqueue(first)
	a, err := svc.ClaimJob(ctx, ClaimRequest{WorkerID: "first"})
	if err != nil || a == nil {
		t.Fatalf("first claim: %v", err)
	}
	enqueue(second)
	b, err := svc.ClaimJob(ctx, ClaimRequest{WorkerID: "second"})
	if err != nil || b == nil {
		t.Fatalf("second claim: %v", err)
	}
	_, err = db.ExecContext(ctx, `INSERT INTO app.job_attempts
		(job_id, execution_id, database_id, migration_id, revision, actor_id, attempt_no, status)
		SELECT job_id, execution_id, database_id, migration_id, revision, actor_id, attempt_no, status
		FROM app.job_attempts WHERE id = $1`, a.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `UPDATE app.jobs SET lease_expires_at = clock_timestamp() - interval '1 second'
		WHERE id IN ($1, $2)`, a.JobID, b.JobID); err != nil {
		t.Fatal(err)
	}
	n, err := svc.ReclaimExpiredJobs(ctx, DefaultMaxAttempts, DefaultRetryDelay)
	if err != nil || n != 1 {
		t.Fatalf("reclaim count = %d, error = %v", n, err)
	}
	var firstStatus, secondStatus string
	if err = db.QueryRowContext(ctx, `SELECT status FROM app.jobs WHERE id = $1`, a.JobID).Scan(&firstStatus); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRowContext(ctx, `SELECT status FROM app.jobs WHERE id = $1`, b.JobID).Scan(&secondStatus); err != nil {
		t.Fatal(err)
	}
	if firstStatus != "running" || secondStatus != "retry_wait" {
		t.Fatalf("statuses = %s, %s", firstStatus, secondStatus)
	}
	if _, err = db.ExecContext(ctx, `UPDATE app.jobs SET next_retry_at = NULL WHERE id = $1`, b.JobID); err != nil {
		t.Fatal(err)
	}
	if c, err := svc.ClaimJob(ctx, ClaimRequest{WorkerID: "third"}); err != nil || c != nil {
		t.Fatalf("unscheduled retry claimed: claim=%+v err=%v", c, err)
	}
	if _, err = db.ExecContext(ctx, `UPDATE app.jobs SET next_retry_at = clock_timestamp() - interval '1 second' WHERE id = $1`, b.JobID); err != nil {
		t.Fatal(err)
	}
	if c, err := svc.ClaimJob(ctx, ClaimRequest{WorkerID: "third", MaxAttempts: 1}); err != nil || c != nil {
		t.Fatalf("retry exceeded configured bound: claim=%+v err=%v", c, err)
	}
	if c, err := svc.ClaimJob(ctx, ClaimRequest{WorkerID: "third"}); err != nil || c == nil || c.JobID != b.JobID {
		t.Fatalf("eligible retry was not claimed: claim=%+v err=%v", c, err)
	}
}
