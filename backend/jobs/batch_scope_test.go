package jobs

import (
	"context"
	"database/sql"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
)

func setupBatchItemsFixture(t *testing.T, e checkpointEnv) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if os.Getenv("BATCH_SCOPE_ISOLATED_TEST_TARGET") != "1" {
		t.Fatal("BATCH_SCOPE_ISOLATED_TEST_TARGET=1 is required for the isolated test database")
	}

	_, err := e.exec.ExecContext(ctx, "CREATE TABLE managed.batch_items (id bigint PRIMARY KEY, processed boolean NOT NULL DEFAULT false)")
	if err != nil {
		t.Fatalf("create managed.batch_items: %v", err)
	}

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := e.exec.ExecContext(ctx, "DROP TABLE managed.batch_items"); err != nil {
			t.Errorf("drop test fixture: %v", err)
		}
	})
	if _, err := e.exec.ExecContext(ctx, "GRANT SELECT (id, processed) ON managed.batch_items TO wbd_target_a_receipt_owner"); err != nil {
		t.Fatalf("grant fixture access: %v", err)
	}
}

func cleanupBatchScope(t *testing.T, e checkpointEnv, executionID string) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := e.cleanup.ExecContext(ctx, "DELETE FROM wbd_auth.batch_scopes WHERE execution_id = $1::uuid", executionID); err != nil {
			t.Errorf("cleanup scope: %v", err)
		}
	})
}

func TestBatchScope_CommittedCapture(t *testing.T) {
	e := checkpointSetup(t)
	setupBatchItemsFixture(t, e)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	_, err := e.exec.ExecContext(ctx, "INSERT INTO managed.batch_items (id, processed) VALUES (1, false), (2, false), (3, false)")
	if err != nil {
		t.Fatalf("insert fixture: %v", err)
	}

	execID := uuid.NewString()
	cleanupBatchScope(t, e, execID)

	ident := BatchScopeIdentity{
		ExecutionID: execID,
		TargetID:    e.target,
		DatabaseID:  uuid.NewString(),
		ActorID:     uuid.NewString(),
	}

	svc := New(nil)
	scope, err := svc.FreezeBatchScope(ctx, e.prep, ident)
	if err != nil {
		t.Fatalf("freeze batch scope: %v", err)
	}
	if scope == nil || scope.TotalCount != 3 || !scope.Ready {
		t.Fatalf("unexpected scope: %+v", scope)
	}

	items, err := svc.ReadBatchScopeChunk(ctx, e.prep, ident, nil, 10)
	if err != nil {
		t.Fatalf("read batch scope chunk: %v", err)
	}
	expected := []int64{1, 2, 3}
	if !reflect.DeepEqual(items, expected) {
		t.Fatalf("expected %v, got %v", expected, items)
	}
}

func TestBatchScope_ReReadAfterReconnectAndNewService(t *testing.T) {
	e := checkpointSetup(t)
	setupBatchItemsFixture(t, e)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	_, err := e.exec.ExecContext(ctx, "INSERT INTO managed.batch_items (id, processed) VALUES (10, false), (20, false), (30, false)")
	if err != nil {
		t.Fatalf("insert fixture: %v", err)
	}

	execID := uuid.NewString()
	cleanupBatchScope(t, e, execID)

	ident := BatchScopeIdentity{
		ExecutionID: execID,
		TargetID:    e.target,
		DatabaseID:  uuid.NewString(),
		ActorID:     uuid.NewString(),
	}

	svc1 := New(nil)
	scope, err := svc1.FreezeBatchScope(ctx, e.prep, ident)
	if err != nil {
		t.Fatalf("freeze batch scope: %v", err)
	}
	if scope.TotalCount != 3 {
		t.Fatalf("expected count 3, got %d", scope.TotalCount)
	}

	// Simulate complete reconnect with a new preparer DB handle and new service instance
	newPrepURL := os.Getenv("MIGRATION_TEST_TARGET_PREPARER_URL")
	newPrepDB, err := sql.Open("pgx", newPrepURL)
	if err != nil {
		t.Fatalf("reconnect preparer: %v", err)
	}
	defer newPrepDB.Close()

	svc2 := New(nil)
	items, err := svc2.ReadBatchScopeChunk(ctx, newPrepDB, ident, nil, 10)
	if err != nil {
		t.Fatalf("read after reconnect: %v", err)
	}
	expected := []int64{10, 20, 30}
	if !reflect.DeepEqual(items, expected) {
		t.Fatalf("expected %v after reconnect, got %v", expected, items)
	}

	readScope, err := svc2.GetBatchScope(ctx, newPrepDB, ident)
	if err != nil {
		t.Fatalf("get batch scope after reconnect: %v", err)
	}
	if readScope.TotalCount != 3 || !readScope.Ready {
		t.Fatalf("unexpected read scope: %+v", readScope)
	}
}

func TestBatchScope_ExclusionOfLaterInserts(t *testing.T) {
	e := checkpointSetup(t)
	setupBatchItemsFixture(t, e)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// Initial membership: 10, 20, 30
	_, err := e.exec.ExecContext(ctx, "INSERT INTO managed.batch_items (id, processed) VALUES (10, false), (20, false), (30, false)")
	if err != nil {
		t.Fatalf("insert initial fixture: %v", err)
	}

	execID := uuid.NewString()
	cleanupBatchScope(t, e, execID)

	ident := BatchScopeIdentity{
		ExecutionID: execID,
		TargetID:    e.target,
		DatabaseID:  uuid.NewString(),
		ActorID:     uuid.NewString(),
	}

	svc := New(nil)
	scope, err := svc.FreezeBatchScope(ctx, e.prep, ident)
	if err != nil {
		t.Fatalf("freeze batch scope: %v", err)
	}
	if scope.TotalCount != 3 {
		t.Fatalf("expected count 3, got %d", scope.TotalCount)
	}

	// Insert later rows: lower ID (5), middle ID (15), higher ID (40)
	_, err = e.exec.ExecContext(ctx, "INSERT INTO managed.batch_items (id, processed) VALUES (5, false), (15, false), (40, false)")
	if err != nil {
		t.Fatalf("insert later rows: %v", err)
	}

	// Verify frozen scope still sees exactly the original IDs in bounded chunks
	items, err := svc.ReadBatchScopeChunk(ctx, e.prep, ident, nil, 100)
	if err != nil {
		t.Fatalf("read chunk: %v", err)
	}
	expected := []int64{10, 20, 30}
	if !reflect.DeepEqual(items, expected) {
		t.Fatalf("scope must exclude later inserts: expected %v, got %v", expected, items)
	}
}

func TestBatchScope_BoundedStableBatches(t *testing.T) {
	e := checkpointSetup(t)
	setupBatchItemsFixture(t, e)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	_, err := e.exec.ExecContext(ctx, "INSERT INTO managed.batch_items (id, processed) VALUES (1, false), (2, false), (3, false), (4, false), (5, false)")
	if err != nil {
		t.Fatalf("insert fixture: %v", err)
	}

	execID := uuid.NewString()
	cleanupBatchScope(t, e, execID)

	ident := BatchScopeIdentity{
		ExecutionID: execID,
		TargetID:    e.target,
		DatabaseID:  uuid.NewString(),
		ActorID:     uuid.NewString(),
	}

	svc := New(nil)
	_, err = svc.FreezeBatchScope(ctx, e.prep, ident)
	if err != nil {
		t.Fatalf("freeze: %v", err)
	}

	// Chunk size = 2
	chunk1, err := svc.ReadBatchScopeChunk(ctx, e.prep, ident, nil, 2)
	if err != nil || !reflect.DeepEqual(chunk1, []int64{1, 2}) {
		t.Fatalf("chunk1 mismatch: %v, err=%v", chunk1, err)
	}

	chunk2, err := svc.ReadBatchScopeChunk(ctx, e.prep, ident, &chunk1[len(chunk1)-1], 2)
	if err != nil || !reflect.DeepEqual(chunk2, []int64{3, 4}) {
		t.Fatalf("chunk2 mismatch: %v, err=%v", chunk2, err)
	}

	chunk3, err := svc.ReadBatchScopeChunk(ctx, e.prep, ident, &chunk2[len(chunk2)-1], 2)
	if err != nil || !reflect.DeepEqual(chunk3, []int64{5}) {
		t.Fatalf("chunk3 mismatch: %v, err=%v", chunk3, err)
	}

	chunk4, err := svc.ReadBatchScopeChunk(ctx, e.prep, ident, &chunk3[len(chunk3)-1], 2)
	if err != nil || len(chunk4) != 0 {
		t.Fatalf("chunk4 expected empty, got %v, err=%v", chunk4, err)
	}

	// Re-reading chunk 1 produces identical stable bounded results
	chunk1Retry, err := svc.ReadBatchScopeChunk(ctx, e.prep, ident, nil, 2)
	if err != nil || !reflect.DeepEqual(chunk1Retry, []int64{1, 2}) {
		t.Fatalf("chunk1 retry not stable: %v, err=%v", chunk1Retry, err)
	}
}

func TestBatchScope_EmptyScope(t *testing.T) {
	e := checkpointSetup(t)
	setupBatchItemsFixture(t, e)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	execID := uuid.NewString()
	cleanupBatchScope(t, e, execID)

	ident := BatchScopeIdentity{
		ExecutionID: execID,
		TargetID:    e.target,
		DatabaseID:  uuid.NewString(),
		ActorID:     uuid.NewString(),
	}

	svc := New(nil)
	scope, err := svc.FreezeBatchScope(ctx, e.prep, ident)
	if err != nil {
		t.Fatalf("freeze empty scope: %v", err)
	}
	if scope.TotalCount != 0 || !scope.Ready {
		t.Fatalf("expected empty ready scope, got: %+v", scope)
	}

	items, err := svc.ReadBatchScopeChunk(ctx, e.prep, ident, nil, 10)
	if err != nil {
		t.Fatalf("read chunk on empty scope: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("expected empty chunk, got: %v", items)
	}
}

func TestBatchScope_IdempotentReopenDoesNotResnapshot(t *testing.T) {
	e := checkpointSetup(t)
	setupBatchItemsFixture(t, e)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	_, err := e.exec.ExecContext(ctx, "INSERT INTO managed.batch_items (id, processed) VALUES (100, false), (200, false), (250, true)")
	if err != nil {
		t.Fatalf("insert fixture: %v", err)
	}

	execID := uuid.NewString()
	cleanupBatchScope(t, e, execID)

	ident := BatchScopeIdentity{
		ExecutionID: execID,
		TargetID:    e.target,
		DatabaseID:  uuid.NewString(),
		ActorID:     uuid.NewString(),
	}

	svc := New(nil)
	scope1, err := svc.FreezeBatchScope(ctx, e.prep, ident)
	if err != nil {
		t.Fatalf("first freeze: %v", err)
	}
	if scope1.TotalCount != 2 {
		t.Fatalf("expected 2, got %d", scope1.TotalCount)
	}

	_, err = e.exec.ExecContext(ctx, "UPDATE managed.batch_items SET processed = (id = 100) WHERE id IN (100, 250)")
	if err != nil {
		t.Fatalf("change eligibility: %v", err)
	}
	if _, err = e.exec.ExecContext(ctx, "INSERT INTO managed.batch_items (id, processed) VALUES (300, false)"); err != nil {
		t.Fatalf("insert row 300: %v", err)
	}
	sameScope, err := svc.FreezeBatchScope(ctx, e.prep, ident)
	if err != nil || sameScope.TotalCount != 2 || sameScope.ActorID != ident.ActorID {
		t.Fatalf("same identity reopen: scope=%+v err=%v", sameScope, err)
	}
	sameItems, err := svc.ReadBatchScopeChunk(ctx, e.prep, ident, nil, 100)
	if err != nil || !reflect.DeepEqual(sameItems, []int64{100, 200}) {
		t.Fatalf("same identity membership: items=%v err=%v", sameItems, err)
	}

	otherActor := ident
	otherActor.ActorID = uuid.NewString()
	scope2, err := svc.FreezeBatchScope(ctx, e.prep, otherActor)
	if err != nil {
		t.Fatalf("idempotent freeze: %v", err)
	}
	if scope2.ActorID != ident.ActorID {
		t.Fatalf("creator changed on reopen: %s", scope2.ActorID)
	}
	if scope2.TotalCount != 2 {
		t.Fatalf("idempotent freeze resnapshotted: expected 2, got %d", scope2.TotalCount)
	}

	items, err := svc.ReadBatchScopeChunk(ctx, e.prep, otherActor, nil, 100)
	if err != nil {
		t.Fatalf("read chunk: %v", err)
	}
	expected := []int64{100, 200}
	if !reflect.DeepEqual(items, expected) {
		t.Fatalf("expected %v, got %v", expected, items)
	}
}

func TestBatchScope_FailClosedOnMismatchedIdentity(t *testing.T) {
	e := checkpointSetup(t)
	setupBatchItemsFixture(t, e)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	_, err := e.exec.ExecContext(ctx, "INSERT INTO managed.batch_items (id, processed) VALUES (1, false)")
	if err != nil {
		t.Fatalf("insert fixture: %v", err)
	}

	execID := uuid.NewString()
	cleanupBatchScope(t, e, execID)

	ident := BatchScopeIdentity{
		ExecutionID: execID,
		TargetID:    e.target,
		DatabaseID:  uuid.NewString(),
		ActorID:     uuid.NewString(),
	}

	svc := New(nil)
	_, err = svc.FreezeBatchScope(ctx, e.prep, ident)
	if err != nil {
		t.Fatalf("initial freeze: %v", err)
	}

	// Mismatched DatabaseID
	mismatchedDB := ident
	mismatchedDB.DatabaseID = uuid.NewString()
	_, err = svc.FreezeBatchScope(ctx, e.prep, mismatchedDB)
	checkpointState(t, err, "42501")

	mismatchedTarget := ident
	mismatchedTarget.TargetID = uuid.NewString()
	_, err = svc.FreezeBatchScope(ctx, e.prep, mismatchedTarget)
	checkpointState(t, err, "42501")

	// Read with mismatched identity fails closed
	_, err = svc.ReadBatchScopeChunk(ctx, e.prep, mismatchedDB, nil, 10)
	if err == nil {
		t.Fatal("expected read failure on mismatched database identity")
	}

	_, err = svc.ReadBatchScopeChunk(ctx, e.prep, mismatchedTarget, nil, 10)
	if err == nil {
		t.Fatal("expected read failure on mismatched target identity")
	}
}

func TestBatchScope_FailClosedOnIncompleteScope(t *testing.T) {
	e := checkpointSetup(t)
	setupBatchItemsFixture(t, e)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	execID := uuid.NewString()
	cleanupBatchScope(t, e, execID)

	ident := BatchScopeIdentity{
		ExecutionID: execID,
		TargetID:    e.target,
		DatabaseID:  uuid.NewString(),
		ActorID:     uuid.NewString(),
	}

	// Simulate incomplete scope (ready = false) inserted via cleanup/owner
	_, err := e.cleanup.ExecContext(ctx, `
		INSERT INTO wbd_auth.batch_scopes
			(execution_id, physical_target_id, database_id, actor_id, total_count, ready)
		VALUES ($1::uuid, $2::uuid, $3::uuid, $4::uuid, 0, false)
	`, ident.ExecutionID, ident.TargetID, ident.DatabaseID, ident.ActorID)
	if err != nil {
		t.Fatalf("insert incomplete scope: %v", err)
	}

	svc := New(nil)
	_, err = svc.FreezeBatchScope(ctx, e.prep, ident)
	checkpointState(t, err, "55000")

	_, err = svc.ReadBatchScopeChunk(ctx, e.prep, ident, nil, 10)
	if err == nil {
		t.Fatal("expected read failure on incomplete scope")
	}

	_, err = svc.GetBatchScope(ctx, e.prep, ident)
	if err == nil {
		t.Fatal("expected get failure on incomplete scope")
	}
}

func TestBatchScope_AbortedCaptureLeavesNoVisibleReadyScope(t *testing.T) {
	e := checkpointSetup(t)
	setupBatchItemsFixture(t, e)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	_, err := e.exec.ExecContext(ctx, "INSERT INTO managed.batch_items (id, processed) VALUES (10, false), (20, false)")
	if err != nil {
		t.Fatalf("insert fixture: %v", err)
	}

	execID := uuid.NewString()
	cleanupBatchScope(t, e, execID)

	ident := BatchScopeIdentity{
		ExecutionID: execID,
		TargetID:    e.target,
		DatabaseID:  uuid.NewString(),
		ActorID:     uuid.NewString(),
	}

	tx, err := e.prep.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}

	svc := New(nil)
	scope, err := svc.FreezeBatchScope(ctx, tx, ident)
	if err != nil {
		t.Fatalf("freeze in tx: %v", err)
	}
	if scope.TotalCount != 2 {
		t.Fatalf("expected count 2 in tx, got %d", scope.TotalCount)
	}

	// Abort transaction
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	// After rollback: verify 0 rows exist and read fails closed
	var scopeCount, itemCount int
	if err := e.prep.QueryRowContext(ctx, "SELECT count(*) FROM wbd_auth.batch_scopes WHERE execution_id = $1::uuid", execID).Scan(&scopeCount); err != nil {
		t.Fatal(err)
	}
	if err := e.prep.QueryRowContext(ctx, "SELECT count(*) FROM wbd_auth.batch_scope_items WHERE execution_id = $1::uuid", execID).Scan(&itemCount); err != nil {
		t.Fatal(err)
	}
	if scopeCount != 0 || itemCount != 0 {
		t.Fatalf("aborted capture published data: scopes=%d items=%d", scopeCount, itemCount)
	}

	_, err = svc.ReadBatchScopeChunk(ctx, e.prep, ident, nil, 10)
	if err == nil {
		t.Fatal("expected read to fail closed after aborted capture")
	}
	scope, err = svc.FreezeBatchScope(ctx, e.prep, ident)
	if err != nil || scope.TotalCount != 2 {
		t.Fatalf("capture after rollback: scope=%v err=%v", scope, err)
	}
}

func TestBatchScope_MetadataProtectionAndRolePrivilegeMatrix(t *testing.T) {
	e := checkpointSetup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Direct mutation attempts by executor must fail with 42501
	for _, query := range []string{
		"SELECT * FROM wbd_auth.batch_scopes",
		"INSERT INTO wbd_auth.batch_scopes (execution_id, physical_target_id, database_id, actor_id, total_count, ready) VALUES ('00000000-0000-0000-0000-000000000001', '00000000-0000-0000-0000-000000000001', '00000000-0000-0000-0000-000000000001', '00000000-0000-0000-0000-000000000001', 0, false)",
		"UPDATE wbd_auth.batch_scopes SET total_count = 0",
		"DELETE FROM wbd_auth.batch_scopes",
		"SELECT * FROM wbd_auth.batch_scope_items",
		"INSERT INTO wbd_auth.batch_scope_items (execution_id, item_id) VALUES ('00000000-0000-0000-0000-000000000001', 1)",
		"UPDATE wbd_auth.batch_scope_items SET item_id = 2",
		"DELETE FROM wbd_auth.batch_scope_items",
		"SELECT wbd_auth.freeze_batch_scope(NULL, NULL, NULL, NULL)",
	} {
		_, err := e.exec.ExecContext(ctx, query)
		checkpointState(t, err, "42501")
	}

	// Direct mutation attempts by preparer must fail with 42501 (only SELECT and EXECUTE are granted)
	for _, query := range []string{
		"INSERT INTO wbd_auth.batch_scopes (execution_id, physical_target_id, database_id, actor_id, total_count, ready) VALUES ('00000000-0000-0000-0000-000000000001', '00000000-0000-0000-0000-000000000001', '00000000-0000-0000-0000-000000000001', '00000000-0000-0000-0000-000000000001', 0, false)",
		"UPDATE wbd_auth.batch_scopes SET total_count = 0",
		"DELETE FROM wbd_auth.batch_scopes",
		"INSERT INTO wbd_auth.batch_scope_items (execution_id, item_id) VALUES ('00000000-0000-0000-0000-000000000001', 1)",
		"UPDATE wbd_auth.batch_scope_items SET item_id = 2",
		"DELETE FROM wbd_auth.batch_scope_items",
	} {
		_, err := e.prep.ExecContext(ctx, query)
		checkpointState(t, err, "42501")
	}

	// Check table and function privilege matrix
	for _, role := range []struct {
		name         string
		db           *sql.DB
		scopes       [4]bool
		scopeItems   [4]bool
		freezeAccess bool
	}{
		{"executor", e.exec, [4]bool{}, [4]bool{}, false},
		{"preparer", e.prep, [4]bool{true, false, false, false}, [4]bool{true, false, false, false}, true},
		{"viewer", e.viewer, [4]bool{}, [4]bool{}, false},
	} {
		t.Run(role.name, func(t *testing.T) {
			var username string
			if err := role.db.QueryRowContext(ctx, "SELECT current_user").Scan(&username); err != nil {
				t.Fatal(err)
			}
			for _, table := range []struct {
				name string
				want [4]bool
			}{
				{"wbd_auth.batch_scopes", role.scopes},
				{"wbd_auth.batch_scope_items", role.scopeItems},
			} {
				for i, privilege := range []string{"SELECT", "INSERT", "UPDATE", "DELETE"} {
					var got bool
					if err := e.cleanup.QueryRowContext(ctx, "SELECT has_table_privilege($1, $2, $3)", username, table.name, privilege).Scan(&got); err != nil {
						t.Fatal(err)
					}
					if got != table.want[i] {
						t.Errorf("%s %s %s privilege = %t, want %t", role.name, table.name, privilege, got, table.want[i])
					}
				}
			}
			var got bool
			if err := e.cleanup.QueryRowContext(ctx, "SELECT has_function_privilege($1, 'wbd_auth.freeze_batch_scope(uuid,uuid,uuid,uuid)', 'EXECUTE')", username).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != role.freezeAccess {
				t.Errorf("%s freeze_batch_scope EXECUTE privilege = %t, want %t", role.name, got, role.freezeAccess)
			}
		})
	}
}

func TestBatchScope_ConcurrentPublicationAndBounds(t *testing.T) {
	e := checkpointSetup(t)
	setupBatchItemsFixture(t, e)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if _, err := e.exec.ExecContext(ctx, "INSERT INTO managed.batch_items VALUES (1, false), (2, true)"); err != nil {
		t.Fatal(err)
	}
	ident := BatchScopeIdentity{uuid.NewString(), e.target, uuid.NewString(), uuid.NewString()}
	cleanupBatchScope(t, e, ident.ExecutionID)
	svc := New(nil)
	tx, err := e.prep.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := svc.FreezeBatchScope(ctx, tx, ident); err != nil {
		t.Fatal(err)
	}
	var capturePID int
	if err := tx.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&capturePID); err != nil {
		t.Fatal(err)
	}
	var fixtureLocked bool
	if err := e.cleanup.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM pg_locks WHERE pid = $1 AND relation = 'managed.batch_items'::regclass
		AND mode = 'AccessShareLock' AND granted
	)`, capturePID).Scan(&fixtureLocked); err != nil || !fixtureLocked {
		t.Fatalf("fixture lock held = %t, err = %v", fixtureLocked, err)
	}
	if _, err := svc.GetBatchScope(ctx, e.prep, ident); err == nil {
		t.Fatal("uncommitted scope visible on another connection")
	}
	if _, err := e.exec.ExecContext(ctx, "INSERT INTO managed.batch_items VALUES (3, false)"); err != nil {
		t.Fatal(err)
	}
	conn, err := e.prep.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	defer cancel()
	var pid int
	if err := conn.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := svc.FreezeBatchScope(ctx, conn, ident)
		result <- err
	}()
	for {
		var blocked bool
		if err := e.cleanup.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE pid = $1 AND wait_event = 'advisory')", pid).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		if ctx.Err() != nil {
			t.Fatal("concurrent capture did not wait")
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	for _, limit := range []int{1, 10000} {
		items, err := svc.ReadBatchScopeChunk(ctx, e.prep, ident, nil, limit)
		if err != nil || !reflect.DeepEqual(items, []int64{1}) {
			t.Fatalf("limit %d: items=%v err=%v", limit, items, err)
		}
	}
	for _, limit := range []int{0, -1, 10001} {
		if _, err := svc.ReadBatchScopeChunk(ctx, e.prep, ident, nil, limit); err == nil {
			t.Fatalf("limit %d unexpectedly accepted", limit)
		}
	}
}

func TestBatchScope_ConcurrentCaptureRollbackAndIdentity(t *testing.T) {
	for _, tc := range []struct {
		name          string
		rollback      bool
		wrongDatabase bool
	}{
		{"rollback permits waiter", true, false},
		{"competing database rejected", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := checkpointSetup(t)
			setupBatchItemsFixture(t, e)
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			if _, err := e.exec.ExecContext(ctx, "INSERT INTO managed.batch_items VALUES (1, false)"); err != nil {
				t.Fatal(err)
			}
			ident := BatchScopeIdentity{uuid.NewString(), e.target, uuid.NewString(), uuid.NewString()}
			cleanupBatchScope(t, e, ident.ExecutionID)
			svc := New(nil)
			tx, err := e.prep.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if _, err := svc.FreezeBatchScope(ctx, tx, ident); err != nil {
				t.Fatal(err)
			}
			if _, err := e.exec.ExecContext(ctx, "INSERT INTO managed.batch_items VALUES (2, false)"); err != nil {
				t.Fatal(err)
			}
			waiter := ident
			if tc.wrongDatabase {
				waiter.DatabaseID = uuid.NewString()
			}
			conn, err := e.prep.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			var pid int
			if err := conn.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
				t.Fatal(err)
			}
			result := make(chan error, 1)
			go func() {
				_, err := svc.FreezeBatchScope(ctx, conn, waiter)
				result <- err
			}()
			defer cancel()
			for {
				var blocked bool
				if err := e.cleanup.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE pid = $1 AND wait_event = 'advisory')", pid).Scan(&blocked); err != nil {
					t.Fatal(err)
				}
				if blocked {
					break
				}
				if ctx.Err() != nil {
					t.Fatal("concurrent capture did not wait")
				}
			}
			if tc.rollback {
				err = tx.Rollback()
			} else {
				err = tx.Commit()
			}
			if err != nil {
				t.Fatal(err)
			}
			err = <-result
			if tc.wrongDatabase {
				checkpointState(t, err, "42501")
			} else if err != nil {
				t.Fatalf("waiter after rollback: %v", err)
			}
			items, err := svc.ReadBatchScopeChunk(ctx, e.prep, ident, nil, 10)
			want := []int64{1}
			if tc.rollback {
				want = []int64{1, 2}
			}
			if err != nil || !reflect.DeepEqual(items, want) {
				t.Fatalf("published membership: got %v, want %v, err=%v", items, want, err)
			}
		})
	}
}

func TestBatchScope_FixturePrivilegesAreNarrow(t *testing.T) {
	e := checkpointSetup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if os.Getenv("BATCH_SCOPE_ISOLATED_TEST_TARGET") != "1" {
		t.Fatal("BATCH_SCOPE_ISOLATED_TEST_TARGET=1 is required for the isolated test database")
	}
	createUnrelated := func(name string) {
		t.Helper()
		if _, err := e.exec.ExecContext(ctx, "CREATE TABLE managed."+name+" (id bigint)"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if _, err := e.exec.ExecContext(ctx, "DROP TABLE managed."+name); err != nil {
				t.Errorf("drop %s: %v", name, err)
			}
		})
	}
	createUnrelated("batch_scope_unrelated_existing")
	setupBatchItemsFixture(t, e)
	createUnrelated("batch_scope_unrelated_new")
	for _, column := range []string{"id", "processed"} {
		var allowed bool
		if err := e.cleanup.QueryRowContext(ctx, "SELECT has_column_privilege('wbd_target_a_receipt_owner', 'managed.batch_items', $1, 'SELECT')", column).Scan(&allowed); err != nil || !allowed {
			t.Fatalf("fixture column %s privilege=%t err=%v", column, allowed, err)
		}
	}
	for _, name := range []string{"managed.batch_scope_unrelated_existing", "managed.batch_scope_unrelated_new"} {
		var allowed bool
		if err := e.cleanup.QueryRowContext(ctx, "SELECT has_table_privilege('wbd_target_a_receipt_owner', $1, 'SELECT')", name).Scan(&allowed); err != nil || allowed {
			t.Fatalf("unrelated table %s privilege=%t err=%v", name, allowed, err)
		}
	}
	for _, role := range []struct {
		name string
		db   *sql.DB
	}{{"executor", e.exec}, {"preparer", e.prep}, {"viewer", e.viewer}} {
		t.Run(role.name, func(t *testing.T) {
			tx, err := role.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			_, err = tx.ExecContext(ctx, "CREATE TEMP TABLE batch_scope_temp_fixture (id bigint)")
			checkpointState(t, err, "42501")
		})
	}
}
