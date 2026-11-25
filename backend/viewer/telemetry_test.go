package viewer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/jackc/pgx/v5/pgconn"
)

type recorded struct {
	start  bool
	event  Event
	result Result
}

type memRecorder struct {
	mu     sync.Mutex
	events []recorded
}

func (m *memRecorder) Start(_ context.Context, e Event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, recorded{start: true, event: e})
}

func (m *memRecorder) Finish(_ context.Context, e Event, r Result) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, recorded{event: e, result: r})
}

const secretCell = "cell-secret-value"
const correlation = "22222222-2222-4222-8222-222222222222"

func recordedFixture(t *testing.T) (*memRecorder, sqlmock.Sqlmock, func(string) *httptest.ResponseRecorder) {
	t.Helper()
	h, m, mux := fixture(t)
	rec := &memRecorder{}
	h.d.Recorder = rec
	return rec, m, func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set("X-Request-ID", correlation)
		mux.ServeHTTP(w, r)
		return w
	}
}

func pairs(t *testing.T, rec *memRecorder, wantDatabase bool) []recorded {
	t.Helper()
	if len(rec.events)%2 != 0 {
		t.Fatalf("unbalanced events: %+v", rec.events)
	}
	var finishes []recorded
	for i := 0; i < len(rec.events); i += 2 {
		s, f := rec.events[i], rec.events[i+1]
		if !s.start || f.start || s.event.Name != f.event.Name || s.event.Operation != f.event.Operation {
			t.Fatalf("event %d is not a start/finish pair: %+v %+v", i, s, f)
		}
		e := f.event
		if e.Operation == "" || e.Actor != "actor" || e.CorrelationID != correlation || e.Params == nil {
			t.Fatalf("missing attributes: %+v", e)
		}
		if wantDatabase && e.DatabaseID != databaseID {
			t.Fatalf("missing database id: %+v", e)
		}
		if e.Name == "sql.query" && e.Query == "" {
			t.Fatalf("query text missing: %+v", e)
		}
		if f.result.Status == "" || f.result.Duration < 0 || f.result.Rows < 0 {
			t.Fatalf("missing result: %+v", f.result)
		}
		finishes = append(finishes, f)
	}
	return finishes
}

func operations(fs []recorded) string {
	ops := make([]string, len(fs))
	for i, f := range fs {
		ops[i] = f.event.Operation + ":" + f.result.Status + ":" + fmt.Sprint(f.result.Rows)
	}
	return strings.Join(ops, ",")
}

func assertNoSecret(t *testing.T, rec *memRecorder, secrets ...string) {
	t.Helper()
	dump := fmt.Sprintf("%+v", rec.events)
	for _, s := range secrets {
		if strings.Contains(dump, s) {
			t.Fatalf("log leaked %q: %s", s, dump)
		}
	}
}

func expectSecretMetadata(m sqlmock.Sqlmock) {
	m.ExpectQuery("SELECT c.oid FROM").WithArgs("public", "items").WillReturnRows(sqlmock.NewRows([]string{"oid"}).AddRow(42))
	m.ExpectQuery("SELECT a.attname,pg_catalog.format_type").WithArgs(42).WillReturnRows(sqlmock.NewRows([]string{"name", "type", "nullable", "default"}).AddRow("id", "bigint", false, nil).AddRow("label", "text", true, "'"+secretCell+"'::text"))
	m.ExpectQuery("SELECT conname,CASE contype").WithArgs(42).WillReturnRows(sqlmock.NewRows([]string{"name", "kind", "definition"}).AddRow("pk", "primary", "PRIMARY KEY (id)"))
	m.ExpectQuery("SELECT ci.relname,pg_catalog.pg_get_indexdef").WithArgs(42).WillReturnRows(sqlmock.NewRows([]string{"name", "definition", "unique", "primary"}).AddRow("pk", "CREATE UNIQUE INDEX pk", true, true))
}

func TestEveryViewerQueryRecordsStartAndFinish(t *testing.T) {
	t.Run("databases", func(t *testing.T) {
		rec, m, get := recordedFixture(t)
		m.ExpectQuery("SELECT c.id::text,c.name").WithArgs("actor").WillReturnRows(sqlmock.NewRows([]string{"id", "name", "access"}).AddRow(databaseID, secretCell, "edit"))
		if w := get("/api/databases"); w.Code != 200 {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
		fs := pairs(t, rec, false)
		if got := operations(fs); got != "databases.list:ok:1" {
			t.Fatal(got)
		}
		if p := fs[0].event.Params; len(p) != 1 || p[0] != "actor" {
			t.Fatalf("params %+v", p)
		}
		assertNoSecret(t, rec, secretCell)
	})
	t.Run("schemas", func(t *testing.T) {
		rec, m, get := recordedFixture(t)
		m.ExpectBegin()
		m.ExpectQuery("SELECT n.nspname,c.relname").WillReturnRows(sqlmock.NewRows([]string{"schema", "table"}).AddRow("public", secretCell).AddRow("empty", nil))
		m.ExpectRollback()
		if w := get("/api/databases/" + databaseID + "/schemas"); w.Code != 200 {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
		if got := operations(pairs(t, rec, true)); got != "tx.begin_read_only:ok:0,schemas.list:ok:2,tx.rollback:ok:0" {
			t.Fatal(got)
		}
		assertNoSecret(t, rec, secretCell)
	})
	t.Run("table", func(t *testing.T) {
		rec, m, get := recordedFixture(t)
		m.ExpectBegin()
		expectSecretMetadata(m)
		m.ExpectRollback()
		if w := get("/api/databases/" + databaseID + "/table?schema=public&table=items"); w.Code != 200 {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
		want := "tx.begin_read_only:ok:0,table.lookup:ok:1,table.columns:ok:2,table.constraints:ok:1,table.indexes:ok:1,tx.rollback:ok:0"
		if got := operations(pairs(t, rec, true)); got != want {
			t.Fatal(got)
		}
		assertNoSecret(t, rec, secretCell)
	})
	t.Run("rows", func(t *testing.T) {
		rec, m, get := recordedFixture(t)
		m.ExpectBegin()
		expectSecretMetadata(m)
		m.ExpectQuery("WITH chosen AS").WithArgs("public", "items").WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("id"))
		m.ExpectQuery(regexp.QuoteMeta(`SELECT t."id"::text,t."label"::text FROM "public"."items" AS t ORDER BY t."id" LIMIT $1 OFFSET $2`)).WithArgs(3, 5).
			WillReturnRows(sqlmock.NewRows([]string{"id", "label"}).AddRow("1", secretCell).AddRow("2", nil).AddRow("3", secretCell))
		m.ExpectRollback()
		w := get("/api/databases/" + databaseID + "/rows?schema=public&table=items&limit=2&offset=5")
		if w.Code != 200 || !strings.Contains(w.Body.String(), secretCell) {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
		fs := pairs(t, rec, true)
		want := "tx.begin_read_only:ok:0,table.lookup:ok:1,table.columns:ok:2,table.constraints:ok:1,table.indexes:ok:1,rows.order:ok:1,rows.select:ok:3,tx.rollback:ok:0"
		if got := operations(fs); got != want {
			t.Fatal(got)
		}
		sel := fs[6].event
		if len(sel.Params) != 2 || sel.Params[0] != 3 || sel.Params[1] != 5 || !strings.Contains(sel.Query, `"public"."items"`) {
			t.Fatalf("select event %+v", sel)
		}
		if lookup := fs[1].event.Params; len(lookup) != 2 || lookup[0] != "public" || lookup[1] != "items" {
			t.Fatalf("lookup params %+v", lookup)
		}
		assertNoSecret(t, rec, secretCell)
	})
}

func TestViewerErrorsRecordSQLStateWithoutMessage(t *testing.T) {
	pgErr := &pgconn.PgError{Code: "42501", Message: "permission denied password=hunter2", Detail: "host=target.internal"}
	t.Run("query", func(t *testing.T) {
		rec, m, get := recordedFixture(t)
		m.ExpectBegin()
		m.ExpectQuery("SELECT n.nspname,c.relname").WillReturnError(fmt.Errorf("wrapped: %w", pgErr))
		m.ExpectRollback()
		assertError(t, get("/api/databases/"+databaseID+"/schemas"), 502, "TARGET_UNAVAILABLE")
		fs := pairs(t, rec, true)
		if got := operations(fs); got != "tx.begin_read_only:ok:0,schemas.list:error:0,tx.rollback:ok:0" {
			t.Fatal(got)
		}
		if r := fs[1].result; r.SQLState != "42501" || r.Error != "query_failed" {
			t.Fatalf("%+v", r)
		}
		assertNoSecret(t, rec, "hunter2", "target.internal", "permission denied")
	})
	t.Run("begin", func(t *testing.T) {
		rec, m, get := recordedFixture(t)
		m.ExpectBegin().WillReturnError(pgErr)
		assertError(t, get("/api/databases/"+databaseID+"/schemas"), 502, "TARGET_UNAVAILABLE")
		fs := pairs(t, rec, true)
		if got := operations(fs); got != "tx.begin_read_only:error:0" || fs[0].result.SQLState != "42501" {
			t.Fatal(got, fs[0].result)
		}
		assertNoSecret(t, rec, "hunter2", "target.internal")
	})
	t.Run("scan", func(t *testing.T) {
		rec, m, get := recordedFixture(t)
		m.ExpectBegin()
		m.ExpectQuery("SELECT n.nspname,c.relname").WillReturnRows(sqlmock.NewRows([]string{"schema", "table"}).AddRow(nil, secretCell))
		m.ExpectRollback()
		assertError(t, get("/api/databases/"+databaseID+"/schemas"), 502, "TARGET_UNAVAILABLE")
		fs := pairs(t, rec, true)
		if fs[1].result.Error != "scan_failed" || fs[1].result.SQLState != "" {
			t.Fatalf("%+v", fs[1].result)
		}
		assertNoSecret(t, rec, secretCell)
	})
	t.Run("table missing", func(t *testing.T) {
		rec, m, get := recordedFixture(t)
		m.ExpectBegin()
		m.ExpectQuery("SELECT c.oid FROM").WithArgs("public", "gone").WillReturnRows(sqlmock.NewRows([]string{"oid"}))
		m.ExpectRollback()
		assertError(t, get("/api/databases/"+databaseID+"/table?schema=public&table=gone"), 404, "NOT_FOUND")
		if got := operations(pairs(t, rec, true)); got != "tx.begin_read_only:ok:0,table.lookup:no_rows:0,tx.rollback:ok:0" {
			t.Fatal(got)
		}
	})
	t.Run("platform", func(t *testing.T) {
		rec, m, get := recordedFixture(t)
		m.ExpectQuery("SELECT c.id::text,c.name").WithArgs("actor").WillReturnError(errors.New("dial tcp target.internal: password authentication failed"))
		assertError(t, get("/api/databases"), 500, "INTERNAL_ERROR")
		fs := pairs(t, rec, false)
		if got := operations(fs); got != "databases.list:error:0" || fs[0].result.SQLState != "" {
			t.Fatal(got)
		}
		assertNoSecret(t, rec, "target.internal", "password authentication")
	})
}

func TestDeniedRequestsRecordNoSQL(t *testing.T) {
	rec, _, get := recordedFixture(t)
	assertError(t, get("/api/databases/not-a-uuid/schemas"), 400, "INVALID_REQUEST")
	if len(rec.events) != 0 {
		t.Fatalf("%+v", rec.events)
	}
}

func TestInvalidRequestIDIsReplaced(t *testing.T) {
	h, m, mux := fixture(t)
	rec := &memRecorder{}
	h.d.Recorder = rec
	m.ExpectQuery("SELECT c.id::text,c.name").WithArgs("actor").WillReturnRows(sqlmock.NewRows([]string{"id", "name", "access"}))
	r := httptest.NewRequest("GET", "/api/databases", nil)
	r.Header.Set("X-Request-ID", "session=abc; injected")
	mux.ServeHTTP(httptest.NewRecorder(), r)
	if id := rec.events[0].event.CorrelationID; !validID(id) || strings.Contains(id, "session") {
		t.Fatalf("correlation id %q", id)
	}
}

func TestSlogOutputIsStructuredAndMasked(t *testing.T) {
	h, m, mux := fixture(t)
	var buf bytes.Buffer
	h.d.Recorder = NewSlogRecorder(NewJSONLogger(&buf))
	m.ExpectBegin()
	expectSecretMetadata(m)
	m.ExpectQuery("WITH chosen AS").WithArgs("public", "items").WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("id"))
	m.ExpectQuery("SELECT t.").WithArgs(51, 0).WillReturnRows(sqlmock.NewRows([]string{"id", "label"}).AddRow("1", secretCell))
	m.ExpectRollback()
	r := httptest.NewRequest("GET", "/api/databases/"+databaseID+"/rows?schema=public&table=items", nil)
	r.Header.Set("X-Request-ID", correlation)
	r.Header.Set("Cookie", "session=private-session-token")
	mux.ServeHTTP(httptest.NewRecorder(), r)

	m.ExpectBegin()
	m.ExpectQuery("SELECT n.nspname,c.relname").WillReturnError(&pgconn.PgError{Code: "57014", Message: "canceling statement password=hunter2"})
	m.ExpectRollback()
	r = httptest.NewRequest("GET", "/api/databases/"+databaseID+"/schemas", nil)
	r.Header.Set("X-Request-ID", correlation)
	mux.ServeHTTP(httptest.NewRecorder(), r)

	out := buf.String()
	t.Log("\n" + out)
	for _, s := range []string{secretCell, "private-session-token", "hunter2", "canceling statement"} {
		if strings.Contains(out, s) {
			t.Fatalf("log leaked %q", s)
		}
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 22 {
		t.Fatalf("got %d lines", len(lines))
	}
	sawFailure := false
	for _, line := range lines {
		var v map[string]any
		if err := json.Unmarshal([]byte(line), &v); err != nil {
			t.Fatalf("invalid json %q: %v", line, err)
		}
		for _, k := range []string{"time", "level", "msg", "event", "operation", "actor_id", "database_id", "correlation_id", "status"} {
			if _, ok := v[k]; !ok {
				t.Fatalf("missing %s in %s", k, line)
			}
		}
		if v["event"] != v["msg"] || !strings.HasSuffix(v["time"].(string), "Z") {
			t.Fatalf("bad event/time in %s", line)
		}
		if strings.HasSuffix(v["event"].(string), ".finish") {
			for _, k := range []string{"duration_ms", "rows"} {
				if _, ok := v[k]; !ok {
					t.Fatalf("missing %s in %s", k, line)
				}
			}
		}
		if v["status"] == "error" {
			sawFailure = v["level"] == "ERROR" && v["sql_state"] == "57014" && v["error"] == "query_failed"
		}
	}
	if !sawFailure {
		t.Fatal("failure line missing sql_state")
	}
}
