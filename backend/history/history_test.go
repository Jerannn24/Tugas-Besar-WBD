package history

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

const (
	dbA       = "a0000000-0000-4000-8000-0000000000a1"
	dbB       = "b0000000-0000-4000-8000-0000000000b1"
	targetA   = "a0000000-1111-4000-8000-0000000000a1"
	targetB   = "b0000000-1111-4000-8000-0000000000b1"
	userID    = "c0000000-0000-4000-8000-0000000000c1"
	execOne   = "e0000000-0000-4000-8000-000000000001"
	execTwo   = "e0000000-0000-4000-8000-000000000002"
	execThree = "e0000000-0000-4000-8000-000000000003"
	execOther = "e0000000-0000-4000-8000-0000000000ff"
	dbEmpty   = "f0000000-0000-4000-8000-0000000000f1"
	bigVer    = "9007199254740993"
	nameTag   = "wbd-history-test"
)

type guardErr int

func (g guardErr) Error() string   { return "guard" }
func (g guardErr) StatusCode() int { return int(g) }

func lazyDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", "postgres://x:y@127.0.0.1:1/none?connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func newMux(t *testing.T, db *sql.DB, actor func(*http.Request) (Actor, error), authorize func(context.Context, Actor, string) error) *http.ServeMux {
	t.Helper()
	if actor == nil {
		actor = func(*http.Request) (Actor, error) { return Actor{ID: userID}, nil }
	}
	if authorize == nil {
		authorize = func(context.Context, Actor, string) error { return nil }
	}
	h, err := New(Dependencies{Platform: db, Actor: actor, Authorize: authorize, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	if err := h.Register(mux); err != nil {
		t.Fatal(err)
	}
	return mux
}

func do(mux *http.ServeMux, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	return rec
}

func assertError(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d (body %s)", rec.Code, status, rec.Body)
	}
	var body struct {
		Error struct {
			Code        string            `json:"code"`
			FieldErrors map[string]string `json:"field_errors"`
			Retryable   *bool             `json:"retryable"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code != code || body.Error.FieldErrors == nil || body.Error.Retryable == nil || *body.Error.Retryable {
		t.Fatalf("error body = %+v, want code %s with field_errors/retryable", body.Error, code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("content-type = %q", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("cache-control = %q", got)
	}
}

func TestNewRejectsMissingDependencies(t *testing.T) {
	db := lazyDB(t)
	actor := func(*http.Request) (Actor, error) { return Actor{}, nil }
	authorize := func(context.Context, Actor, string) error { return nil }
	tests := map[string]Dependencies{
		"platform":  {Actor: actor, Authorize: authorize, Timeout: time.Second},
		"actor":     {Platform: db, Authorize: authorize, Timeout: time.Second},
		"authorize": {Platform: db, Actor: actor, Timeout: time.Second},
		"timeout":   {Platform: db, Actor: actor, Authorize: authorize},
	}
	for name, d := range tests {
		if _, err := New(d); err == nil {
			t.Errorf("New without %s: want error", name)
		}
	}
	h, err := New(Dependencies{Platform: db, Actor: actor, Authorize: authorize, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if h.Register(nil) == nil {
		t.Error("Register(nil) want error")
	}
}

func TestGuardErrorsMapToContract(t *testing.T) {
	tests := []struct {
		err    error
		status int
		code   string
	}{
		{guardErr(401), 401, "UNAUTHENTICATED"},
		{guardErr(403), 403, "FORBIDDEN"},
		{guardErr(404), 404, "NOT_FOUND"},
		{guardErr(418), 500, "INTERNAL_ERROR"},
		{errors.New("plain"), 500, "INTERNAL_ERROR"},
	}
	paths := []string{
		"/api/databases/" + dbA + "/migrations",
		"/api/databases/" + dbA + "/migrations/" + execOne,
	}
	for _, tt := range tests {
		mux := newMux(t, lazyDB(t), nil, func(context.Context, Actor, string) error { return tt.err })
		for _, p := range paths {
			assertError(t, do(mux, p), tt.status, tt.code)
		}
	}
}

func TestActorErrorIs401(t *testing.T) {
	actors := map[string]func(*http.Request) (Actor, error){
		"error": func(*http.Request) (Actor, error) { return Actor{}, errors.New("none") },
		"empty": func(*http.Request) (Actor, error) { return Actor{}, nil },
	}
	for name, actor := range actors {
		called := false
		mux := newMux(t, lazyDB(t), actor, func(context.Context, Actor, string) error { called = true; return nil })
		assertError(t, do(mux, "/api/databases/"+dbA+"/migrations"), 401, "UNAUTHENTICATED")
		if called {
			t.Errorf("%s: authorize must not run without an actor", name)
		}
	}
}

func TestInvalidIDs(t *testing.T) {
	mux := newMux(t, lazyDB(t), nil, nil)
	assertError(t, do(mux, "/api/databases/not-a-uuid/migrations"), 400, "INVALID_REQUEST")
	assertError(t, do(mux, "/api/databases/"+dbA+"/migrations/not-a-uuid"), 400, "INVALID_REQUEST")
	assertError(t, do(mux, "/api/databases/"+dbA+"/migrations/"+strings.Repeat("g", 36)), 400, "INVALID_REQUEST")
}

// pinnedConn hides the driver's session reset, which would discard a
// connection that has an open transaction and lose the fixture transaction.
type pinnedConn struct {
	conn driver.Conn
}

func (p *pinnedConn) Prepare(q string) (driver.Stmt, error) { return p.conn.Prepare(q) }
func (p *pinnedConn) Close() error                          { return p.conn.Close() }
func (p *pinnedConn) Begin() (driver.Tx, error)             { return p.conn.Begin() }
func (p *pinnedConn) PrepareContext(ctx context.Context, q string) (driver.Stmt, error) {
	return p.conn.(driver.ConnPrepareContext).PrepareContext(ctx, q)
}
func (p *pinnedConn) ExecContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	return p.conn.(driver.ExecerContext).ExecContext(ctx, q, args)
}
func (p *pinnedConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	return p.conn.(driver.QueryerContext).QueryContext(ctx, q, args)
}
func (p *pinnedConn) CheckNamedValue(v *driver.NamedValue) error {
	return p.conn.(driver.NamedValueChecker).CheckNamedValue(v)
}

// pinnedConnector hands out one connection only, so a reconnect fails loudly
// instead of silently leaving the fixture transaction.
type pinnedConnector struct {
	inner driver.Connector
	used  bool
}

func (c *pinnedConnector) Connect(ctx context.Context) (driver.Conn, error) {
	if c.used {
		return nil, errors.New("pinned connection already used")
	}
	c.used = true
	conn, err := c.inner.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &pinnedConn{conn}, nil
}
func (c *pinnedConnector) Driver() driver.Driver { return c.inner.Driver() }

func liveDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("HISTORY_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("HISTORY_TEST_DATABASE_URL not set")
	}
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(&pinnedConnector{inner: stdlib.GetConnector(*config)})
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec("BEGIN"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Exec("ROLLBACK") })
	return db
}

func mustExec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("fixture %q: %v", q, err)
	}
}

func addConnection(t *testing.T, db *sql.DB, id, name, targetID string) {
	t.Helper()
	mustExec(t, db, `INSERT INTO app.database_connections
		(id,name,host,port,database_name,username,password_ciphertext,credential_key_id,ssl_mode,target_key)
		VALUES ($1,$2,'h',5432,'d','u','\x01','k','disable',$3)`, id, name+"-"+nameTag, name+"-key-"+nameTag)
	if targetID == "" {
		return
	}
	mustExec(t, db, `INSERT INTO app.migration_target_state (id) VALUES ($1)`, targetID)
	mustExec(t, db, `UPDATE app.database_connections SET execution_target_id=$2 WHERE id=$1`, id, targetID)
}

func seed(t *testing.T, db *sql.DB) {
	t.Helper()
	mustExec(t, db, `INSERT INTO app.users (id,username,password_hash) VALUES ($1,'history_tester','x')`, userID)
	addConnection(t, db, dbA, "history-a", targetA)
	addConnection(t, db, dbB, "history-b", targetB)

	draft := func(id, dbID, title, body string) {
		mustExec(t, db, `INSERT INTO app.migration_drafts (id,database_id,title,sql,created_by) VALUES ($1,$2,$3,$4,$5)`,
			id, dbID, title, body, userID)
	}
	reserve := func(execID, dbID, target, draftID, title, body, version, startedAt string) {
		mustExec(t, db, `UPDATE app.migration_target_state SET next_version=$2::bigint+1 WHERE id=$1`, target, version)
		mustExec(t, db, `INSERT INTO app.migration_executions
			(id,database_id,execution_target_id,draft_id,request_id,draft_revision,version,title,sql,sql_sha256,
			 executed_by,executed_username,started_at,status)
			VALUES ($1,$2,$3,$4,gen_random_uuid(),1,$5::bigint,$6,$7,sha256(convert_to($7,'UTF8')),$8,'history_tester',$9::timestamptz,'running')`,
			execID, dbID, target, draftID, version, title, body, userID, startedAt)
	}

	d1 := "d0000000-0000-4000-8000-000000000001"
	d2 := "d0000000-0000-4000-8000-000000000002"
	d3 := "d0000000-0000-4000-8000-000000000003"
	d4 := "d0000000-0000-4000-8000-000000000004"
	draft(d1, dbA, "create users", "CREATE TABLE u(id int);")
	draft(d2, dbA, "bad change", "ALTER TABLE nope ADD c int;")
	draft(d3, dbA, "in flight", "CREATE INDEX i ON u(id);")
	draft(d4, dbB, "other db", "SELECT 1;")

	reserve(execOne, dbA, targetA, d1, "create users", "CREATE TABLE u(id int);", "1", "2026-01-02 03:04:05+07")
	mustExec(t, db, `UPDATE app.migration_executions SET status='succeeded',finished_at='2026-01-02 03:04:09+07' WHERE id=$1`, execOne)
	reserve(execTwo, dbA, targetA, d2, "bad change", "ALTER TABLE nope ADD c int;", "2", "2026-01-03 00:00:00+00")
	mustExec(t, db, `UPDATE app.migration_executions SET status='failed',finished_at='2026-01-03 00:00:01+00',
		error_code='SQL_ERROR',error_message='relation "nope" does not exist' WHERE id=$1`, execTwo)
	reserve(execThree, dbA, targetA, d3, "in flight", "CREATE INDEX i ON u(id);", bigVer, "2026-01-04 00:00:00+00")
	reserve(execOther, dbB, targetB, d4, "other db", "SELECT 1;", "1", "2026-01-05 00:00:00+00")
}

func decodeData(t *testing.T, rec *httptest.ResponseRecorder, out any) {
	t.Helper()
	if rec.Code != 200 {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body)
	}
	var body struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(body.Data, out); err != nil {
		t.Fatal(err)
	}
}

func TestLiveHistory(t *testing.T) {
	db := liveDB(t)
	seed(t, db)
	mux := newMux(t, db, nil, nil)

	t.Run("list ordered by version desc with string versions", func(t *testing.T) {
		rec := do(mux, "/api/databases/"+dbA+"/migrations")
		if rec.Code != 200 || rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("status=%d cache=%q body=%s", rec.Code, rec.Header().Get("Cache-Control"), rec.Body)
		}
		var body struct {
			Data struct {
				Items []map[string]any `json:"items"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		var versions []string
		for _, it := range body.Data.Items {
			v, ok := it["version"].(string)
			if !ok {
				t.Fatalf("version is %T, want string", it["version"])
			}
			versions = append(versions, v)
			if it["database_id"] != dbA {
				t.Fatalf("leaked execution from another database: %v", it["database_id"])
			}
		}
		if strings.Join(versions, ",") != bigVer+",2,1" {
			t.Fatalf("versions = %v", versions)
		}
		if !strings.Contains(rec.Body.String(), `"version":"`+bigVer+`"`) {
			t.Fatalf("large version not preserved exactly: %s", rec.Body)
		}
	})

	t.Run("detail succeeded", func(t *testing.T) {
		var got Execution
		decodeData(t, do(mux, "/api/databases/"+dbA+"/migrations/"+execOne), &got)
		if got.ID != execOne || got.DatabaseID != dbA || got.Version != "1" || got.DraftRevision != 1 ||
			got.Title != "create users" || got.SQL != "CREATE TABLE u(id int);" || got.Status != "succeeded" ||
			got.ExecutedBy.ID != userID || got.ExecutedBy.Username != "history_tester" || got.Error != nil || got.FinishedAt == nil {
			t.Fatalf("detail = %+v", got)
		}
		if got.StartedAt.Location() != time.UTC || !got.StartedAt.Equal(time.Date(2026, 1, 1, 20, 4, 5, 0, time.UTC)) {
			t.Fatalf("started_at = %v", got.StartedAt)
		}
		if got.FinishedAt.Location() != time.UTC || !got.FinishedAt.Equal(time.Date(2026, 1, 1, 20, 4, 9, 0, time.UTC)) {
			t.Fatalf("finished_at = %v", got.FinishedAt)
		}
	})

	t.Run("detail failed carries error", func(t *testing.T) {
		var got Execution
		decodeData(t, do(mux, "/api/databases/"+dbA+"/migrations/"+execTwo), &got)
		if got.Status != "failed" || got.Error == nil || got.Error.Code != "SQL_ERROR" ||
			got.Error.Message != `relation "nope" does not exist` || got.FinishedAt == nil {
			t.Fatalf("detail = %+v", got)
		}
	})

	t.Run("detail running has null finished_at and error", func(t *testing.T) {
		rec := do(mux, "/api/databases/"+dbA+"/migrations/"+execThree)
		var raw struct {
			Data map[string]json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
			t.Fatal(err)
		}
		if string(raw.Data["finished_at"]) != "null" || string(raw.Data["error"]) != "null" ||
			string(raw.Data["status"]) != `"running"` || string(raw.Data["version"]) != `"`+bigVer+`"` {
			t.Fatalf("data = %s", rec.Body)
		}
	})

	t.Run("other database execution is 404", func(t *testing.T) {
		assertError(t, do(mux, "/api/databases/"+dbA+"/migrations/"+execOther), 404, "NOT_FOUND")
		var got Execution
		decodeData(t, do(mux, "/api/databases/"+dbB+"/migrations/"+execOther), &got)
		if got.ID != execOther || got.DatabaseID != dbB {
			t.Fatalf("detail = %+v", got)
		}
	})

	t.Run("nonexistent execution is 404", func(t *testing.T) {
		assertError(t, do(mux, "/api/databases/"+dbA+"/migrations/e0000000-0000-4000-8000-00000000dead"), 404, "NOT_FOUND")
	})

	t.Run("empty list", func(t *testing.T) {
		addConnection(t, db, dbEmpty, "history-empty", "")
		rec := do(mux, "/api/databases/"+dbEmpty+"/migrations")
		if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"data":{"items":[]}}` {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
		}
	})

	t.Run("guard 403 short-circuits before any query", func(t *testing.T) {
		deny := newMux(t, lazyDB(t), nil, func(context.Context, Actor, string) error { return guardErr(403) })
		assertError(t, do(deny, "/api/databases/"+dbA+"/migrations"), 403, "FORBIDDEN")
		assertError(t, do(deny, "/api/databases/"+dbA+"/migrations/"+execOne), 403, "FORBIDDEN")
	})
}
