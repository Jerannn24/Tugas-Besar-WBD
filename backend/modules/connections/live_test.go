package connections

import (
	"context"
	"errors"
	"net/url"
	"os"
	"testing"
	"time"

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

func TestLivePendingJobsPreventUnbindingAndDeletion(t *testing.T) {
	requireURL := func(key string) string {
		value := os.Getenv(key)
		if value == "" {
			if os.Getenv("TEST_INTEGRATION_REQUIRED") == "1" {
				t.Fatalf("%s not set", key)
			}
			t.Skipf("%s not set", key)
		}
		return value
	}
	prefix := "MIGRATION_TEST_TARGET_"
	if os.Getenv(prefix+"B_VIEWER_URL") != "" && os.Getenv(prefix+"B_EXECUTOR_URL") != "" && os.Getenv(prefix+"B_PREPARER_URL") != "" {
		prefix += "B_"
	}
	targetConfig := func(role string) *pgx.ConnConfig {
		key := prefix + role + "_URL"
		cfg, err := pgx.ParseConfig(requireURL(key))
		if err != nil {
			t.Fatalf("invalid %s", key)
		}
		return cfg
	}
	viewer, executor, preparer := targetConfig("VIEWER"), targetConfig("EXECUTOR"), targetConfig("PREPARER")
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, requireURL("MIGRATION_TEST_PLATFORM_URL"))
	if err != nil {
		t.Fatal("invalid MIGRATION_TEST_PLATFORM_URL")
	}
	t.Cleanup(pool.Close)
	owner, err := pgx.Connect(ctx, requireURL("MIGRATION_TEST_OWNER_URL"))
	if err != nil {
		t.Fatal("connect with MIGRATION_TEST_OWNER_URL")
	}
	t.Cleanup(func() { _ = owner.Close(context.Background()) })
	repo := NewRepository(pool, "queue-guard-test-key")
	unbind := Input{
		Name: "queue-guard-" + uuid.NewString(), Host: viewer.Host, Port: int(viewer.Port),
		DatabaseName: viewer.Database, Username: viewer.User, Password: viewer.Password, SSLMode: "disable",
		Executor: &RoleCredential{Username: executor.User, Password: &executor.Password},
		Preparer: &RoleCredential{Username: preparer.User, Password: &preparer.Password},
	}
	item, err := repo.Create(ctx, unbind)
	if err != nil {
		t.Fatal(err)
	}
	cleanup := func(q string, id uuid.UUID) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := owner.Exec(ctx, q, id); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { cleanup("DELETE FROM app.database_connections WHERE id=$1", item.ID) })
	// Keep the seeded job out of claim and recovery while testing connection administration.
	var jobID, actorID, proposalID, draftID uuid.UUID
	err = owner.QueryRow(ctx, `WITH actor AS (
		INSERT INTO app.users (id, username, password_hash)
		VALUES (gen_random_uuid(), 'queue_guard_' || gen_random_uuid()::text, 'hash') RETURNING id
	), draft AS (
		INSERT INTO app.migration_drafts (id, database_id, revision, title, sql, created_by)
		SELECT gen_random_uuid(), $1, 1, 'Queue guard', 'SELECT 1', id FROM actor RETURNING id, created_by
	), proposal AS (
		INSERT INTO app.migration_proposals (database_id, migration_id, revision, action, status, snapshot, snapshot_hash, created_by)
		SELECT $1, id, 1, 'apply', 'draft', '{}'::jsonb, sha256('snapshot'::bytea), created_by FROM draft
		RETURNING id, migration_id, created_by
	)
	INSERT INTO app.jobs (database_id, target_id, request_id, proposal_id, migration_id, revision, execution_id,
		actor_id, action, direction, snapshot, snapshot_hash, next_retry_at, lease_expires_at)
	SELECT c.id, c.execution_target_id, gen_random_uuid()::text, p.id, p.migration_id, 1, gen_random_uuid(),
		p.created_by, 'apply', 'up', '{}'::jsonb, 'snapshot', 'infinity', 'infinity'
	FROM proposal p JOIN app.database_connections c ON c.id=$1
	RETURNING id, actor_id, proposal_id, migration_id`, item.ID).Scan(&jobID, &actorID, &proposalID, &draftID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup("DELETE FROM app.jobs WHERE id=$1", jobID)
		cleanup("DELETE FROM app.migration_proposals WHERE id=$1", proposalID)
		cleanup("DELETE FROM app.migration_drafts WHERE id=$1", draftID)
		cleanup("DELETE FROM app.users WHERE id=$1", actorID)
	})
	label := unbind
	label.Name += "-renamed"
	label.Password = ""
	label.Executor = &RoleCredential{Username: executor.User}
	label.Preparer = &RoleCredential{Username: preparer.User}
	if updated, err := repo.Update(ctx, item.ID, label, nil); err != nil || updated.Name != label.Name {
		t.Fatalf("label update with pending job: %+v, %v", updated, err)
	}

	unbind.Executor, unbind.Preparer = nil, nil
	for _, state := range []struct {
		status string
		blocks bool
	}{{"queued", false}, {"running", false}, {"retry_wait", false}, {"partial", true}} {
		if _, err := owner.Exec(ctx, `UPDATE app.jobs SET status=$2, execution_status=$2, blocks_database=$3 WHERE id=$1`, jobID, state.status, state.blocks); err != nil {
			t.Fatal(err)
		}
		// No local pool lifecycle: the durable guard protects other processes too.
		if _, err := repo.Update(ctx, item.ID, unbind, nil); !errors.Is(err, ErrBusy) {
			t.Errorf("update while job is %s: %v, want ErrBusy", state.status, err)
		}
		if err := repo.Delete(ctx, item.ID, nil); !errors.Is(err, ErrBusy) {
			t.Errorf("delete while job is %s: %v, want ErrBusy", state.status, err)
		}
	}
	if _, err := owner.Exec(ctx, `UPDATE app.jobs SET status='failed', execution_status='failed', blocks_database=false WHERE id=$1`, jobID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Update(ctx, item.ID, unbind, nil); err != nil {
		t.Fatalf("unbinding after clean failure: %v", err)
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
