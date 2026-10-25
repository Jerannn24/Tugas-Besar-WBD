package connections

import (
	"context"
	"errors"
	"net/url"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const livePrefix = "live-batch4-"

type liveEnv struct {
	ctx   context.Context
	repo  *Repository
	pm    *PoolManager
	owner *pgx.Conn
	input func(name string) Input
}

func liveSetup(t *testing.T) *liveEnv {
	t.Helper()
	platformDSN := os.Getenv("CONNECTIONS_TEST_DATABASE_URL")
	if platformDSN == "" {
		t.Skip("CONNECTIONS_TEST_DATABASE_URL not set")
	}
	get := func(k string) string {
		v := os.Getenv(k)
		if v == "" {
			t.Skipf("%s not set", k)
		}
		return v
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, platformDSN)
	if err != nil {
		t.Fatal("platform pool")
	}
	t.Cleanup(pool.Close)
	ownerDSN := (&url.URL{Scheme: "postgres", User: url.UserPassword(get("DB_TEST_OWNER_USER"), get("DB_TEST_OWNER_PASSWORD")),
		Host: "postgres-test:5432", Path: "/" + get("DB_TEST_PLATFORM_DB")}).String() + "?sslmode=disable"
	owner, err := pgx.Connect(ctx, ownerDSN)
	if err != nil {
		t.Fatal("owner connect")
	}
	clean := func() {
		for _, q := range []string{
			`DELETE FROM app.group_database_grants WHERE database_id IN (SELECT id FROM app.database_connections WHERE name LIKE 'live-batch4-%')`,
			`DELETE FROM app.database_connections WHERE name LIKE 'live-batch4-%'`,
			`DELETE FROM app.migration_target_state s WHERE NOT EXISTS (SELECT 1 FROM app.database_connections c WHERE c.execution_target_id=s.id) AND NOT EXISTS (SELECT 1 FROM app.migration_executions e WHERE e.execution_target_id=s.id)`,
		} {
			if _, err := owner.Exec(ctx, q); err != nil {
				t.Errorf("cleanup failed: %v", err)
			}
		}
	}
	clean()
	t.Cleanup(func() {
		clean()
		var n int
		if err := owner.QueryRow(ctx, `SELECT (SELECT count(*) FROM app.database_connections)+(SELECT count(*) FROM app.migration_target_state)`).Scan(&n); err != nil || n != 0 {
			t.Errorf("leftover platform rows: %d (%v)", n, err)
		}
		owner.Close(ctx)
	})
	repo := NewRepository(pool, "live-secret")
	pm := NewPoolManager(repo)
	t.Cleanup(pm.Close)
	db := get("DB_TEST_TARGET_A")
	viewerPW, execPW, prepPW := get("DB_TEST_TARGET_A_VIEWER_PASSWORD"), get("DB_TEST_TARGET_A_EXECUTOR_PASSWORD"), get("DB_TEST_TARGET_A_PREPARER_PASSWORD")
	return &liveEnv{ctx: ctx, repo: repo, pm: pm, owner: owner, input: func(name string) Input {
		return Input{Name: livePrefix + name, Host: "postgres-test", Port: 5432, DatabaseName: db, SSLMode: "disable",
			Username: "wbd_target_a_viewer", Password: viewerPW,
			Executor: &RoleCredential{Username: "wbd_target_a_executor", Password: pw(execPW)},
			Preparer: &RoleCredential{Username: "wbd_target_a_preparer", Password: pw(prepPW)}}
	}}
}

func mustConfig(in Input) targetConfig {
	return targetConfig{Host: in.Host, Port: in.Port, Database: in.DatabaseName, SSLMode: in.SSLMode,
		Viewer: credential{in.Username, in.Password}, Executor: credential{in.Executor.Username, *in.Executor.Password},
		Preparer: credential{in.Preparer.Username, *in.Preparer.Password}}
}

func TestLiveBindReregisterAndAcquire(t *testing.T) {
	e := liveSetup(t)
	item, err := e.repo.Create(e.ctx, e.input("a"))
	if err != nil || !item.ExecutionReady || item.ExecutorUsername == nil || *item.ExecutorUsername != "wbd_target_a_executor" {
		t.Fatalf("create: %v %+v", err, item)
	}
	var target uuid.UUID
	var next int64
	if err := e.owner.QueryRow(e.ctx, `SELECT c.execution_target_id,s.next_version FROM app.database_connections c JOIN app.migration_target_state s ON s.id=c.execution_target_id WHERE c.id=$1`, item.ID).Scan(&target, &next); err != nil || next != 1 {
		t.Fatalf("binding row: %v next=%d", err, next)
	}
	want, err := verifyTarget(e.ctx, mustConfig(e.input("a")))
	if err != nil || want != target {
		t.Fatalf("execution_target_id %v != marker %v (%v)", target, want, err)
	}
	if _, err := e.repo.Create(e.ctx, e.input("dup")); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate: %v", err)
	}

	lease, err := e.pm.Acquire(e.ctx, item.ID)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	var role string
	if err := lease.Executor.QueryRowContext(e.ctx, `SELECT session_user::text`).Scan(&role); err != nil || role != "wbd_target_a_executor" {
		t.Fatalf("executor session: %v %q", err, role)
	}
	var marker uuid.UUID
	if err := lease.Preparer.QueryRowContext(e.ctx, `SELECT physical_target_id FROM wbd_auth.target_marker`).Scan(&marker); err != nil || marker != target {
		t.Fatalf("preparer marker: %v", err)
	}
	tx, err := lease.Executor.BeginTx(e.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(e.ctx, `INSERT INTO wbd_auth.execution_tickets(execution_id,physical_target_id,database_id,target_xid,version,sql_sha256,actor_id,capability_sha256) VALUES (gen_random_uuid(),$1,gen_random_uuid(),pg_current_xact_id(),1,sha256('x'),gen_random_uuid(),sha256('y'))`, marker); err == nil {
		t.Error("executor inserted into execution_tickets")
	}
	_ = tx.Rollback()
	ptx, err := lease.Preparer.BeginTx(e.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ptx.ExecContext(e.ctx, `CREATE TABLE managed.batch4_probe(i int)`); err == nil {
		t.Error("preparer created a table in managed")
	}
	_ = ptx.Rollback()

	// Active lease blocks credential changes and delete (busy-path via lease counter).
	changed := e.input("a")
	changed.Executor, changed.Preparer = nil, nil // unbinding is an identity change
	if _, err := e.repo.Update(e.ctx, item.ID, changed, e.pm); !errors.Is(err, ErrBusy) {
		t.Errorf("update with active lease: %v", err)
	}
	if err := e.repo.Delete(e.ctx, item.ID, e.pm); !errors.Is(err, ErrBusy) {
		t.Errorf("delete with active lease: %v", err)
	}
	lease.Release()
	lease.Release()

	viewer, err := e.pm.Open(e.ctx, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	vtx, err := viewer.BeginTx(e.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := vtx.ExecContext(e.ctx, `CREATE TABLE managed.batch4_probe(i int)`); err == nil {
		t.Error("viewer created a table in managed")
	}
	_ = vtx.Rollback()

	// Re-registration keeps the version counter.
	if _, err := e.owner.Exec(e.ctx, `UPDATE app.migration_target_state SET next_version=7 WHERE id=$1`, target); err != nil {
		t.Fatal(err)
	}
	if err := e.repo.Delete(e.ctx, item.ID, e.pm); err != nil {
		t.Fatalf("delete: %v", err)
	}
	again, err := e.repo.Create(e.ctx, e.input("b"))
	if err != nil || !again.ExecutionReady {
		t.Fatalf("re-register: %v", err)
	}
	if err := e.owner.QueryRow(e.ctx, `SELECT next_version FROM app.migration_target_state WHERE id=$1`, target).Scan(&next); err != nil || next != 7 {
		t.Fatalf("next_version = %d (%v), want 7", next, err)
	}
	renamed := e.input("c")
	renamed.Executor.Password, renamed.Preparer.Password = nil, nil // keep stored
	if got, err := e.repo.Update(e.ctx, again.ID, renamed, e.pm); err != nil || !got.ExecutionReady {
		t.Fatalf("update: %v", err)
	}
	renamed.Executor, renamed.Preparer = nil, nil
	if got, err := e.repo.Update(e.ctx, again.ID, renamed, e.pm); err != nil || got.ExecutionReady || got.ExecutorUsername != nil {
		t.Fatalf("unbind: %v %+v", err, got)
	}
}

func TestLiveWrongRolesRejectedNothingStored(t *testing.T) {
	e := liveSetup(t)
	in := e.input("wrong")
	in.Executor.Username, in.Preparer.Username = "wbd_target_b_executor", "wbd_target_b_preparer"
	if _, err := e.repo.Create(e.ctx, in); !errors.Is(err, ErrTargetUnavailable) {
		t.Fatalf("wrong roles: %v", err)
	}
	bad := e.input("badpw")
	bad.Preparer.Password = pw("definitely-wrong")
	if _, err := e.repo.Create(e.ctx, bad); !errors.Is(err, ErrTargetUnavailable) {
		t.Fatalf("bad password: %v", err)
	}
	half := e.input("half")
	half.Preparer = nil
	if _, err := e.repo.Create(e.ctx, half); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("half pair: %v", err)
	}
	var n int
	if err := e.owner.QueryRow(e.ctx, `SELECT count(*) FROM app.database_connections`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("stored rows: %d %v", n, err)
	}
	vo := e.input("viewer-only")
	vo.Executor, vo.Preparer = nil, nil
	item, err := e.repo.Create(e.ctx, vo)
	if err != nil || item.ExecutionReady {
		t.Fatalf("viewer-only: %v %+v", err, item)
	}
	if _, err := e.pm.Acquire(e.ctx, item.ID); !errors.Is(err, ErrNotReady) {
		t.Fatalf("acquire unbound: %v", err)
	}
}
