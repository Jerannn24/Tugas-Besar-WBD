package jobs

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/Jerannn24/Tugas-Besar-WBD.git/executioncontract"
	"github.com/google/uuid"
)

func TestRenewLease_Success(t *testing.T) {
	platformDB, ownerDB := testDBs(t)
	f1 := createClaimFixture(t, platformDB, ownerDB)
	f2 := createClaimFixture(t, platformDB, ownerDB)
	svc := New(platformDB)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Enqueue job 1
	tx1, err := platformDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx1: %v", err)
	}
	req1 := executioncontract.EnqueueRequest{
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
		ApprovalFingerprint: "fp_renew_1",
	}
	_, err = svc.EnqueueTx(ctx, tx1, req1)
	if err != nil {
		tx1.Rollback()
		t.Fatalf("EnqueueTx 1: %v", err)
	}
	if err := tx1.Commit(); err != nil {
		t.Fatalf("commit tx1: %v", err)
	}

	// Enqueue unrelated job 2
	tx2, err := platformDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx2: %v", err)
	}
	req2 := executioncontract.EnqueueRequest{
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
		ApprovalFingerprint: "fp_renew_2",
	}
	_, err = svc.EnqueueTx(ctx, tx2, req2)
	if err != nil {
		tx2.Rollback()
		t.Fatalf("EnqueueTx 2: %v", err)
	}
	if err := tx2.Commit(); err != nil {
		t.Fatalf("commit tx2: %v", err)
	}

	// Claim both jobs
	claim1, err := svc.ClaimJob(ctx, ClaimRequest{
		WorkerID:      "worker-renew-1",
		LeaseDuration: 30 * time.Second,
	})
	if err != nil || claim1 == nil {
		t.Fatalf("claim 1 failed: %v", err)
	}

	claim2, err := svc.ClaimJob(ctx, ClaimRequest{
		WorkerID:      "worker-renew-2",
		LeaseDuration: 30 * time.Second,
	})
	if err != nil || claim2 == nil {
		t.Fatalf("claim 2 failed: %v", err)
	}

	var beforeRenewal time.Time
	if err := platformDB.QueryRowContext(ctx, "SELECT clock_timestamp()").Scan(&beforeRenewal); err != nil {
		t.Fatalf("query database time: %v", err)
	}
	// Renew lease for job 1
	res, err := svc.RenewLease(ctx, RenewLeaseRequest{
		JobID:         claim1.JobID,
		LeaseToken:    claim1.LeaseToken,
		LeaseDuration: 60 * time.Second,
	})
	if err != nil {
		t.Fatalf("RenewLease error: %v", err)
	}
	if res == nil || !res.Renewed {
		t.Fatalf("expected lease to be renewed, got: %+v", res)
	}
	if !res.LeaseExpiresAt.After(claim1.LeaseExpiresAt) {
		t.Fatalf("expected new lease_expires_at (%v) to be after old (%v)", res.LeaseExpiresAt, claim1.LeaseExpiresAt)
	}
	if res.LeaseExpiresAt.Before(beforeRenewal.Add(60 * time.Second)) {
		t.Fatalf("renewed expiry %v is before database time plus duration %v", res.LeaseExpiresAt, beforeRenewal.Add(60*time.Second))
	}

	shortRenewal, err := svc.RenewLease(ctx, RenewLeaseRequest{
		JobID: claim1.JobID, LeaseToken: claim1.LeaseToken, LeaseDuration: time.Second,
	})
	if err != nil || shortRenewal == nil || !shortRenewal.Renewed {
		t.Fatalf("short renewal: result=%+v error=%v", shortRenewal, err)
	}
	if !shortRenewal.LeaseExpiresAt.Equal(res.LeaseExpiresAt) {
		t.Fatalf("short renewal changed expiry: got %v, want %v", shortRenewal.LeaseExpiresAt, res.LeaseExpiresAt)
	}

	// Verify job 1 state in PostgreSQL
	var (
		dbToken     string
		dbExpiresAt time.Time
		dbAttemptNo int
		dbStatus    string
		dbClaimedBy string
	)
	err = platformDB.QueryRowContext(ctx, `
		SELECT lease_token::text, lease_expires_at, attempt_no, status, claimed_by
		FROM app.jobs
		WHERE id = $1
	`, claim1.JobID).Scan(&dbToken, &dbExpiresAt, &dbAttemptNo, &dbStatus, &dbClaimedBy)
	if err != nil {
		t.Fatalf("query job 1: %v", err)
	}
	if dbToken != claim1.LeaseToken {
		t.Errorf("lease_token mutated: got %s, want %s", dbToken, claim1.LeaseToken)
	}
	if !dbExpiresAt.Equal(res.LeaseExpiresAt) {
		t.Errorf("db lease_expires_at (%v) != returned (%v)", dbExpiresAt, res.LeaseExpiresAt)
	}
	if dbAttemptNo != claim1.AttemptNo {
		t.Errorf("attempt_no mutated: got %d, want %d", dbAttemptNo, claim1.AttemptNo)
	}
	if dbStatus != "running" {
		t.Errorf("status changed: got %s, want running", dbStatus)
	}
	if dbClaimedBy != claim1.ClaimedBy {
		t.Errorf("claimed_by mutated: got %s, want %s", dbClaimedBy, claim1.ClaimedBy)
	}

	// Verify attempt count in app.job_attempts remains exactly 1
	var attemptCount int
	err = platformDB.QueryRowContext(ctx, "SELECT count(*) FROM app.job_attempts WHERE job_id = $1", claim1.JobID).Scan(&attemptCount)
	if err != nil {
		t.Fatalf("query attempt count: %v", err)
	}
	if attemptCount != 1 {
		t.Errorf("expected exactly 1 attempt for job 1, got %d", attemptCount)
	}

	// Verify unrelated job 2 state was NOT mutated
	var (
		job2Token     string
		job2ExpiresAt time.Time
	)
	err = platformDB.QueryRowContext(ctx, `
		SELECT lease_token::text, lease_expires_at
		FROM app.jobs
		WHERE id = $1
	`, claim2.JobID).Scan(&job2Token, &job2ExpiresAt)
	if err != nil {
		t.Fatalf("query job 2: %v", err)
	}
	if job2Token != claim2.LeaseToken {
		t.Errorf("unrelated job 2 token mutated: got %s, want %s", job2Token, claim2.LeaseToken)
	}
	if !job2ExpiresAt.Equal(claim2.LeaseExpiresAt) {
		t.Errorf("unrelated job 2 expires_at mutated: got %v, want %v", job2ExpiresAt, claim2.LeaseExpiresAt)
	}
}

func TestRenewLease_RejectsWrongToken(t *testing.T) {
	platformDB, ownerDB := testDBs(t)
	f := createClaimFixture(t, platformDB, ownerDB)
	svc := New(platformDB)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

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
		ApprovalFingerprint: "fp_renew_wrong_token",
	}
	_, err = svc.EnqueueTx(ctx, tx, req)
	if err != nil {
		tx.Rollback()
		t.Fatalf("EnqueueTx: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit tx: %v", err)
	}

	claim, err := svc.ClaimJob(ctx, ClaimRequest{
		WorkerID:      "worker-wrong-token",
		LeaseDuration: 30 * time.Second,
	})
	if err != nil || claim == nil {
		t.Fatalf("claim failed: %v", err)
	}

	wrongToken := uuid.NewString()
	res, err := svc.RenewLease(ctx, RenewLeaseRequest{
		JobID:         claim.JobID,
		LeaseToken:    wrongToken,
		LeaseDuration: 60 * time.Second,
	})
	if err != nil {
		t.Fatalf("RenewLease returned database error: %v", err)
	}
	if res == nil {
		t.Fatal("RenewLease returned nil result")
	}
	if res.Renewed {
		t.Fatal("expected renewal to be rejected for wrong token, got renewed = true")
	}

	// Verify PostgreSQL row was not mutated
	var (
		dbToken     string
		dbExpiresAt time.Time
	)
	err = platformDB.QueryRowContext(ctx, "SELECT lease_token::text, lease_expires_at FROM app.jobs WHERE id = $1", claim.JobID).Scan(&dbToken, &dbExpiresAt)
	if err != nil {
		t.Fatalf("query job: %v", err)
	}
	if dbToken != claim.LeaseToken {
		t.Errorf("lease_token changed in db: got %s, want %s", dbToken, claim.LeaseToken)
	}
	if !dbExpiresAt.Equal(claim.LeaseExpiresAt) {
		t.Errorf("lease_expires_at mutated: got %v, want %v", dbExpiresAt, claim.LeaseExpiresAt)
	}
}

func TestRenewLease_RejectsExpiredLease(t *testing.T) {
	platformDB, ownerDB := testDBs(t)
	f := createClaimFixture(t, platformDB, ownerDB)
	svc := New(platformDB)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

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
		ApprovalFingerprint: "fp_renew_expired",
	}
	_, err = svc.EnqueueTx(ctx, tx, req)
	if err != nil {
		tx.Rollback()
		t.Fatalf("EnqueueTx: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit tx: %v", err)
	}

	claim, err := svc.ClaimJob(ctx, ClaimRequest{
		WorkerID:      "worker-expired",
		LeaseDuration: 30 * time.Second,
	})
	if err != nil || claim == nil {
		t.Fatalf("claim failed: %v", err)
	}

	// Force lease to be expired in PostgreSQL using PostgreSQL clock_timestamp (no sleeps)
	_, err = platformDB.ExecContext(ctx, `
		UPDATE app.jobs
		SET lease_expires_at = clock_timestamp() - interval '10 seconds'
		WHERE id = $1
	`, claim.JobID)
	if err != nil {
		t.Fatalf("update lease_expires_at to past: %v", err)
	}

	res, err := svc.RenewLease(ctx, RenewLeaseRequest{
		JobID:         claim.JobID,
		LeaseToken:    claim.LeaseToken,
		LeaseDuration: 60 * time.Second,
	})
	if err != nil {
		t.Fatalf("RenewLease returned database error: %v", err)
	}
	if res == nil {
		t.Fatal("RenewLease returned nil result")
	}
	if res.Renewed {
		t.Fatal("expected renewal to be rejected for expired lease, got renewed = true")
	}

	// Verify lease in DB remains expired
	var (
		dbExpiresAt time.Time
		now         time.Time
	)
	err = platformDB.QueryRowContext(ctx, "SELECT lease_expires_at, clock_timestamp() FROM app.jobs WHERE id = $1", claim.JobID).Scan(&dbExpiresAt, &now)
	if err != nil {
		t.Fatalf("query job: %v", err)
	}
	if !dbExpiresAt.Before(now) {
		t.Fatalf("db lease_expires_at (%v) resurrected to >= now (%v)", dbExpiresAt, now)
	}
}

func TestRenewLease_WaitingOnLockCannotResurrectExpiredLease(t *testing.T) {
	platformDB, ownerDB := testDBs(t)
	f := createClaimFixture(t, platformDB, ownerDB)
	svc := New(platformDB)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

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
		ApprovalFingerprint: "fp_renew_resurrect",
	}
	_, err = svc.EnqueueTx(ctx, tx, req)
	if err != nil {
		tx.Rollback()
		t.Fatalf("EnqueueTx: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit tx: %v", err)
	}

	claim, err := svc.ClaimJob(ctx, ClaimRequest{
		WorkerID:      "worker-lock-1",
		LeaseDuration: 30 * time.Second,
	})
	if err != nil || claim == nil {
		t.Fatalf("ClaimJob: %v", err)
	}

	// Tx1 holds row lock on app.jobs
	tx1, err := platformDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx1: %v", err)
	}
	defer tx1.Rollback()

	var dummy string
	err = tx1.QueryRowContext(ctx, "SELECT id FROM app.jobs WHERE id = $1 FOR UPDATE", claim.JobID).Scan(&dummy)
	if err != nil {
		t.Fatalf("lock job in tx1: %v", err)
	}

	// Goroutine 2 calls RenewLease and will block waiting for row lock
	renewDone := make(chan struct{})
	var (
		res  *RenewResult
		rErr error
	)
	go func() {
		defer close(renewDone)
		res, rErr = svc.RenewLease(ctx, RenewLeaseRequest{
			JobID:         claim.JobID,
			LeaseToken:    claim.LeaseToken,
			LeaseDuration: 30 * time.Second,
		})
	}()

	// Wait until Goroutine 2 is waiting on lock in Postgres (no sleep, query pg_locks)
	for {
		if ctx.Err() != nil {
			t.Fatalf("timed out waiting for lock contention: %v", ctx.Err())
		}
		var waiting int
		_ = ownerDB.QueryRowContext(ctx, "SELECT count(*) FROM pg_locks WHERE granted = false").Scan(&waiting)
		if waiting > 0 {
			break
		}
		runtime.Gosched()
	}

	// While Goroutine 2 is blocked waiting on lock, expire the lease in tx1
	_, err = tx1.ExecContext(ctx, `
		UPDATE app.jobs
		SET lease_expires_at = clock_timestamp() - interval '5 seconds'
		WHERE id = $1
	`, claim.JobID)
	if err != nil {
		t.Fatalf("expire lease in tx1: %v", err)
	}

	// Commit tx1, releasing row lock
	if err := tx1.Commit(); err != nil {
		t.Fatalf("commit tx1: %v", err)
	}

	// Goroutine 2 should now acquire lock and finish
	select {
	case <-renewDone:
	case <-ctx.Done():
		t.Fatalf("timed out waiting for RenewLease to finish: %v", ctx.Done())
	}

	if rErr != nil {
		t.Fatalf("RenewLease returned unexpected database error: %v", rErr)
	}
	if res == nil {
		t.Fatal("RenewLease returned nil result")
	}
	if res.Renewed {
		t.Fatalf("RenewLease resurrected an expired lease! result = %+v", res)
	}

	// Verify in DB that lease_expires_at was not updated to future
	var (
		dbExpiresAt time.Time
		now         time.Time
	)
	err = platformDB.QueryRowContext(ctx, "SELECT lease_expires_at, clock_timestamp() FROM app.jobs WHERE id = $1", claim.JobID).Scan(&dbExpiresAt, &now)
	if err != nil {
		t.Fatalf("query job: %v", err)
	}
	if !dbExpiresAt.Before(now) {
		t.Fatalf("db lease_expires_at = %v, expected in the past before %v", dbExpiresAt, now)
	}
}

func TestRenewLease_RejectsNonRunningOrMissingJob(t *testing.T) {
	platformDB, ownerDB := testDBs(t)
	f := createClaimFixture(t, platformDB, ownerDB)
	svc := New(platformDB)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// 1. Missing job
	res, err := svc.RenewLease(ctx, RenewLeaseRequest{
		JobID:         uuid.NewString(),
		LeaseToken:    uuid.NewString(),
		LeaseDuration: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("RenewLease on missing job returned database error: %v", err)
	}
	if res == nil || res.Renewed {
		t.Fatalf("expected missing job renewal to be rejected, got %+v", res)
	}

	// 2. Non-running (queued) job
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
		ApprovalFingerprint: "fp_renew_queued",
	}
	enqueueRes, err := svc.EnqueueTx(ctx, tx, req)
	if err != nil {
		tx.Rollback()
		t.Fatalf("EnqueueTx: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit tx: %v", err)
	}

	res, err = svc.RenewLease(ctx, RenewLeaseRequest{
		JobID:         enqueueRes.JobID,
		LeaseToken:    uuid.NewString(),
		LeaseDuration: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("RenewLease on queued job returned database error: %v", err)
	}
	if res == nil || res.Renewed {
		t.Fatalf("expected queued job renewal to be rejected, got %+v", res)
	}

	// Verify status in DB remains 'queued'
	var dbStatus string
	err = platformDB.QueryRowContext(ctx, "SELECT status FROM app.jobs WHERE id = $1", enqueueRes.JobID).Scan(&dbStatus)
	if err != nil {
		t.Fatalf("query job status: %v", err)
	}
	if dbStatus != "queued" {
		t.Fatalf("job status mutated from queued to %s", dbStatus)
	}
}

func TestRenewLease_RejectsSubMillisecondDuration(t *testing.T) {
	platformDB, ownerDB := testDBs(t)
	f := createClaimFixture(t, platformDB, ownerDB)
	svc := New(platformDB)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

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
		ApprovalFingerprint: "fp_renew_subms",
	}
	_, err = svc.EnqueueTx(ctx, tx, req)
	if err != nil {
		tx.Rollback()
		t.Fatalf("EnqueueTx: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit tx: %v", err)
	}

	claim, err := svc.ClaimJob(ctx, ClaimRequest{
		WorkerID:      "worker-subms",
		LeaseDuration: 30 * time.Second,
	})
	if err != nil || claim == nil {
		t.Fatalf("claim failed: %v", err)
	}

	_, err = svc.RenewLease(ctx, RenewLeaseRequest{
		JobID:         claim.JobID,
		LeaseToken:    claim.LeaseToken,
		LeaseDuration: 500 * time.Microsecond,
	})
	if err == nil {
		t.Fatal("expected error for sub-millisecond lease duration, got nil")
	}

	contractErr, ok := err.(executioncontract.ContractError)
	if !ok {
		t.Fatalf("expected executioncontract.ContractError, got %T: %v", err, err)
	}
	if contractErr.Code != "INVALID_REQUEST" {
		t.Errorf("expected INVALID_REQUEST, got %s", contractErr.Code)
	}

	// Verify DB state unchanged
	var dbExpiresAt time.Time
	err = platformDB.QueryRowContext(ctx, "SELECT lease_expires_at FROM app.jobs WHERE id = $1", claim.JobID).Scan(&dbExpiresAt)
	if err != nil {
		t.Fatalf("query job: %v", err)
	}
	if !dbExpiresAt.Equal(claim.LeaseExpiresAt) {
		t.Errorf("db lease_expires_at mutated on validation failure: got %v, want %v", dbExpiresAt, claim.LeaseExpiresAt)
	}
}
