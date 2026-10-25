package migration

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Jerannn24/Tugas-Besar-WBD.git/drafts"
	"github.com/Jerannn24/Tugas-Besar-WBD.git/history"
	"github.com/Jerannn24/Tugas-Besar-WBD.git/viewer"
	"github.com/google/uuid"
)

type liveTransitionFailure struct{ platformStore }

func (s liveTransitionFailure) transition(context.Context, attempt, string, *history.ExecutionError) (attempt, error) {
	return attempt{}, errors.New("injected platform transition outage")
}

// This fixture uses owner only for owned platform setup/cleanup. Every handler
// query uses platform runtime, and target work uses its provisioned role.
func TestLiveMigrationLifecycle(t *testing.T) {
	if os.Getenv("MIGRATION_TEST_PLATFORM_URL") == "" {
		if os.Getenv("TEST_INTEGRATION_REQUIRED") == "1" {
			t.Fatal("missing live migration configuration")
		}
		t.Skip("MIGRATION_TEST_PLATFORM_URL not set")
	}
	ctx, fixtureCancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer fixtureCancel()
	open := func(key string) *sql.DB {
		t.Helper()
		dsn := os.Getenv(key)
		if dsn == "" {
			t.Fatalf("missing %s", key)
		}
		db, err := sql.Open("pgx", dsn)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		if err = db.PingContext(ctx); err != nil {
			t.Fatalf("%s ping: %v", key, err)
		}
		return db
	}
	platform := open("MIGRATION_TEST_PLATFORM_URL")
	owner := open("MIGRATION_TEST_OWNER_URL")
	executor := open("MIGRATION_TEST_TARGET_EXECUTOR_URL")
	preparer := open("MIGRATION_TEST_TARGET_PREPARER_URL")
	vdb := open("MIGRATION_TEST_TARGET_VIEWER_URL")
	targetCleanup := open("MIGRATION_TEST_TARGET_CLEANUP_URL")
	for role, db := range map[string]*sql.DB{"wbd_platform_runtime": platform, "wbd_target_a_executor": executor, "wbd_target_a_preparer": preparer, "wbd_target_a_viewer": vdb} {
		var actual string
		var unsafe bool
		if err := db.QueryRow(`SELECT current_user,rolsuper OR rolbypassrls FROM pg_roles WHERE rolname=current_user`).Scan(&actual, &unsafe); err != nil || actual != role || unsafe {
			t.Fatalf("role %s: actual=%s unsafe=%v err=%v", role, actual, unsafe, err)
		}
	}
	user, database := uuid.NewString(), uuid.NewString()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	table := "live_" + suffix
	renamed := table + "_renamed"
	var target string
	if err := preparer.QueryRow(`SELECT physical_target_id::text FROM wbd_auth.target_marker WHERE singleton`).Scan(&target); err != nil {
		t.Fatal(err)
	}
	must := func(db *sql.DB, q string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	createdTarget := false
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, q := range []string{`DELETE FROM wbd_auth.execution_receipts WHERE database_id=$1`, `DELETE FROM wbd_auth.execution_tickets WHERE database_id=$1`} {
			if _, err := targetCleanup.ExecContext(cleanupCtx, q, database); err != nil {
				t.Errorf("target evidence cleanup: %v", err)
			}
		}
		for _, n := range []string{table, renamed, table + "_rollback", table + "_unknown", table + "_recovery"} {
			if _, err := executor.ExecContext(cleanupCtx, `DROP TABLE IF EXISTS managed.`+n); err != nil {
				t.Errorf("target cleanup: %v", err)
			}
		}
		// Immutable history guards require owner cleanup in one transaction.
		tx, err := owner.BeginTx(cleanupCtx, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer tx.Rollback()
		for _, q := range []string{`WITH deleted_executions AS (DELETE FROM app.migration_executions WHERE database_id=$1 RETURNING id) DELETE FROM app.migration_drafts WHERE database_id=$1`, `DELETE FROM app.database_connections WHERE id=$1`} {
			if _, err = tx.Exec(q, database); err != nil {
				t.Errorf("platform cleanup: %v", err)
				return
			}
		}
		if _, err = tx.Exec(`DELETE FROM app.migration_target_state WHERE id=$1 AND $2 AND NOT EXISTS(SELECT 1 FROM app.database_connections WHERE execution_target_id=$1) AND NOT EXISTS(SELECT 1 FROM app.migration_executions WHERE execution_target_id=$1)`, target, createdTarget); err != nil {
			t.Error(err)
			return
		}
		if _, err = tx.Exec(`DELETE FROM app.users WHERE id=$1`, user); err != nil {
			t.Error(err)
			return
		}

		if err = tx.Commit(); err != nil {
			t.Error(err)
		}
	})
	must(owner, `INSERT INTO app.users(id,username,password_hash) VALUES($1,$2,'fixture')`, user, "live_"+suffix)
	result, err := owner.ExecContext(ctx, `INSERT INTO app.migration_target_state(id) VALUES($1) ON CONFLICT DO NOTHING`, target)
	if err != nil {
		t.Fatal(err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		t.Fatal(err)
	}
	createdTarget = inserted == 1
	must(owner, `INSERT INTO app.database_connections(id,name,host,port,database_name,username,password_ciphertext,credential_key_id,ssl_mode,target_key,execution_target_id) VALUES($1,$2,'postgres-test',5432,'test_target_a','wbd_target_a_viewer','x','fixture','disable',$2,$3)`, database, "live_"+suffix, target)
	allowed := true
	authorize := func(context.Context, Actor, string) error {
		if !allowed {
			return fault(403, "FORBIDDEN", "denied")
		}
		return nil
	}
	h, err := New(Dependencies{Platform: platform, Actor: func(*http.Request) (Actor, error) { return Actor{user}, nil }, Authorize: authorize, CheckCSRF: func(*http.Request) error { return nil }, Acquire: func(ctx context.Context, _ string) (*TargetLease, error) {
		conn, err := executor.Conn(ctx)
		return &TargetLease{Executor: conn, Preparer: preparer, Release: func() {}}, err
	}, Preflight: NewPreflight(testPolicy(t)), Timeout: 3 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	dh, err := drafts.New(drafts.Dependencies{Platform: platform, Actor: func(*http.Request) (drafts.Actor, error) { return drafts.Actor{ID: user}, nil }, Authorize: func(ctx context.Context, a drafts.Actor, id string) error { return authorize(ctx, Actor{a.ID}, id) }, CheckCSRF: func(*http.Request) error { return nil }, Timeout: 3 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	if err = dh.Register(mux); err != nil {
		t.Fatal(err)
	}
	create := func(text string) string {
		t.Helper()
		raw, _ := json.Marshal(map[string]string{"title": "Live migration", "sql": text})
		r := httptest.NewRequest("POST", "/api/databases/"+database+"/drafts", strings.NewReader(string(raw)))
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code != 200 && w.Code != 201 {
			t.Fatalf("create draft: %d %s", w.Code, w.Body.String())
		}
		var body struct {
			Data drafts.Draft `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Data.ID == "" {
			t.Fatalf("draft decode: %v %s", err, w.Body.String())
		}
		return body.Data.ID
	}
	assertCode := func(err error, want string) {
		t.Helper()
		var api *APIError
		if !errors.As(err, &api) || api.Code != want {
			t.Fatalf("error=%v want %s", err, want)
		}
	}
	draft := create(fmt.Sprintf(`CREATE TABLE managed.%s(id int PRIMARY KEY); ALTER TABLE managed.%s RENAME TO %s; INSERT INTO managed.%s(id) VALUES(1);`, table, table, renamed, renamed))
	request := ExecuteRequest{Revision: 1, RequestID: uuid.NewString()}
	got, err := h.Execute(ctx, Actor{user}, database, draft, request)
	if err != nil || got.Status != "succeeded" {
		t.Fatalf("execute rename batch: status=%s error=%v failure=%+v", got.Status, err, got.Error)
	}
	t.Log("committed CREATE -> RENAME -> INSERT batch through executor")
	var receiptCount int
	if err = preparer.QueryRow(`SELECT count(*) FROM wbd_auth.execution_receipts WHERE execution_id=$1`, got.ID).Scan(&receiptCount); err != nil || receiptCount != 1 {
		t.Fatalf("receipt=%d %v", receiptCount, err)
	}
	var applied string
	if err = platform.QueryRow(`SELECT applied_execution_id::text FROM app.migration_drafts WHERE id=$1`, draft).Scan(&applied); err != nil || applied != got.ID {
		t.Fatalf("applied link=%s %v", applied, err)
	}
	replay, err := h.Execute(ctx, Actor{user}, database, draft, request)
	if err != nil || replay.ID != got.ID {
		t.Fatalf("replay: %+v %v", replay, err)
	}
	var count int
	if err = vdb.QueryRow(`SELECT count(*) FROM managed.` + renamed).Scan(&count); err != nil || count != 1 {
		t.Fatalf("replay target row count=%d %v", count, err)
	}
	vh, err := viewer.New(viewer.Dependencies{Platform: platform, Actor: func(*http.Request) (viewer.Actor, error) { return viewer.Actor{ID: user}, nil }, Authorize: func(context.Context, viewer.Actor, string, viewer.Access) error { return nil }, OpenTarget: func(context.Context, string) (*sql.DB, error) { return vdb, nil }, Timeout: 3 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	vm := http.NewServeMux()
	vh.Register(vm)
	w := httptest.NewRecorder()
	vm.ServeHTTP(w, httptest.NewRequest("GET", "/api/databases/"+database+"/rows?schema=managed&table="+renamed, nil))
	if w.Code != 200 {
		t.Fatalf("viewer rows: %d %s", w.Code, w.Body.String())
	}
	t.Log("request replay returned same execution and one target row")
	var rowBody struct {
		Data viewer.RowsResult `json:"data"`
	}
	if err = json.Unmarshal(w.Body.Bytes(), &rowBody); err != nil || len(rowBody.Data.Rows) != 1 || len(rowBody.Data.Rows[0]) != 1 || rowBody.Data.Rows[0][0] == nil || *rowBody.Data.Rows[0][0] != "1" {
		t.Fatalf("viewer row values: %s %v", w.Body.String(), err)
	}
	w = httptest.NewRecorder()
	vm.ServeHTTP(w, httptest.NewRequest("GET", "/api/databases/"+database+"/table?schema=managed&table="+renamed, nil))
	var meta struct {
		Data viewer.Metadata `json:"data"`
	}
	if err = json.Unmarshal(w.Body.Bytes(), &meta); w.Code != 200 || err != nil || meta.Data.Schema != "managed" || meta.Data.Table != renamed || len(meta.Data.Columns) != 1 || meta.Data.Columns[0].Name != "id" || len(meta.Data.Constraints) != 1 || meta.Data.Constraints[0].Kind != "primary" || len(meta.Data.Indexes) != 1 || !meta.Data.Indexes[0].Primary {
		t.Fatalf("viewer metadata: %d %s %v", w.Code, w.Body.String(), err)
	}
	recoveryDraft := create(fmt.Sprintf(`CREATE TABLE managed.%s_recovery(id int); INSERT INTO managed.%s_recovery(id) VALUES(7);`, table, table))
	realStore := h.store
	h.store = liveTransitionFailure{realStore}
	committed, outage := h.Execute(ctx, Actor{user}, database, recoveryDraft, ExecuteRequest{1, uuid.NewString()})
	h.store = realStore
	if outage == nil || committed.ID == "" || committed.Status != "running" {
		t.Fatalf("injected transition outage: %+v %v", committed, outage)
	}
	finalized, err := h.Reconcile(ctx, database, committed.ID)
	if err != nil || finalized.Status != "succeeded" {
		t.Fatalf("present receipt recovery: %+v %v", finalized, err)
	}
	if err = vdb.QueryRow(`SELECT count(*) FROM managed.` + table + `_recovery`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("recovery replay count=%d %v", count, err)
	}
	t.Log("matching receipt reconciled committed target after platform transition outage")
	failedDraft := create(fmt.Sprintf(`CREATE TABLE managed.%s_rollback(id int PRIMARY KEY); INSERT INTO managed.%s_rollback(id) VALUES(1),(1);`, table, table))
	failed, err := h.Execute(ctx, Actor{user}, database, failedDraft, ExecuteRequest{1, uuid.NewString()})
	if err != nil || failed.Status != "failed" || failed.Error == nil || failed.Error.Code != "SQL_FAILED" {
		t.Fatalf("rollback: %+v %v", failed, err)
	}
	var absent bool
	if err = vdb.QueryRow(`SELECT to_regclass($1) IS NULL`, "managed."+table+"_rollback").Scan(&absent); err != nil || !absent {
		t.Fatalf("rollback table absent=%v %v", absent, err)
	}
	if err = preparer.QueryRow(`SELECT count(*) FROM wbd_auth.execution_receipts WHERE execution_id=$1`, failed.ID).Scan(&receiptCount); err != nil || receiptCount != 0 {
		t.Fatalf("rollback receipt count=%d %v", receiptCount, err)
	}
	t.Log("second-statement SQL failure rolled back CREATE and receipt")
	pending := create(fmt.Sprintf(`CREATE TABLE managed.%s_unknown(id int);`, table))
	patch := func(rev int) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(map[string]any{"title": "Updated live draft", "sql": fmt.Sprintf(`CREATE TABLE managed.%s_unknown(id int);`, table), "revision": rev})
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("PATCH", "/api/databases/"+database+"/drafts/"+pending, strings.NewReader(string(raw))))
		return w
	}
	pw := patch(1)
	var updated struct {
		Data drafts.Draft `json:"data"`
	}
	if err = json.Unmarshal(pw.Body.Bytes(), &updated); pw.Code != 200 || err != nil || updated.Data.Revision != 2 {
		t.Fatalf("draft patch: %d %s %v", pw.Code, pw.Body.String(), err)
	}
	pw = patch(1)
	if pw.Code != 409 || !strings.Contains(pw.Body.String(), "VERSION_CONFLICT") {
		t.Fatalf("stale patch: %d %s", pw.Code, pw.Body.String())
	}
	_, err = h.Execute(ctx, Actor{user}, database, pending, ExecuteRequest{1, uuid.NewString()})
	assertCode(err, "VERSION_CONFLICT")
	allowed = false
	_, err = h.Execute(ctx, Actor{user}, database, pending, ExecuteRequest{1, uuid.NewString()})
	assertCode(err, "FORBIDDEN")
	allowed = true
	// Reserve an unresolved real platform attempt without executing target SQL.
	a, created, err := h.store.reserve(ctx, Actor{user}, database, pending, target, ExecuteRequest{2, uuid.NewString()}, func(string) error { return nil })
	if err != nil || !created {
		t.Fatalf("reserve unknown: %v", err)
	}
	a, err = h.store.transition(ctx, a, "unknown", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.Execute(ctx, Actor{user}, database, failedDraft, ExecuteRequest{1, uuid.NewString()})
	assertCode(err, "DATABASE_BUSY")
	recovered, err := h.Reconcile(ctx, database, a.ID)
	if err != nil || recovered.Status != "unknown" {
		t.Fatalf("reconcile missing receipt: %+v %v", recovered, err)
	}
	if err = vdb.QueryRow(`SELECT to_regclass($1) IS NULL`, "managed."+table+"_unknown").Scan(&absent); err != nil || !absent {
		t.Fatalf("reconcile replayed SQL: absent=%v %v", absent, err)
	}
	t.Log("draft PATCH revision, stale requests, authorization denial, busy attempt, missing receipt recovery verified")
	// Existing advisory lock makes another executor wait, then fail closed.
	session, err := h.openTarget(ctx, database, target)
	if err != nil {
		t.Fatal(err)
	}
	defer session.close(time.Second)
	lockCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	_, err = h.openTarget(lockCtx, database, target)
	if err == nil {
		t.Fatal("parallel target session unexpectedly acquired lock")
	}
	assertCode(err, "DATABASE_BUSY")
}
