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
	// Set targetID equal to dbID so both database_connections and migration_target_state FK are satisfied
	targetID := dbID
	actorID := uuid.NewString()
	draftID := uuid.NewString()
	proposalID := uuid.NewString()

	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = ownerDB.ExecContext(cleanupCtx, "DELETE FROM app.jobs WHERE database_id = $1", dbID)
		_, _ = ownerDB.ExecContext(cleanupCtx, "DELETE FROM app.migration_proposals WHERE database_id = $1", dbID)
		_, _ = ownerDB.ExecContext(cleanupCtx, "DELETE FROM app.migration_drafts WHERE database_id = $1", dbID)
		_, _ = ownerDB.ExecContext(cleanupCtx, "DELETE FROM app.database_connections WHERE id = $1", dbID)
		_, _ = ownerDB.ExecContext(cleanupCtx, "DELETE FROM app.migration_target_state WHERE id = $1", targetID)
		_, _ = ownerDB.ExecContext(cleanupCtx, "DELETE FROM app.users WHERE id = $1", actorID)
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
		req := executioncontract.EnqueueRequest{
			DatabaseID: f.dbID, TargetID: f.targetID, RequestID: uuid.NewString(),
			ProposalID: f.proposalID, MigrationID: f.draftID, Revision: 1,
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
			AfterLock: func(ctx context.Context) error {
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
