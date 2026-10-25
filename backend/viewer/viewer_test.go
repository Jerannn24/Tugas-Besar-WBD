package viewer

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

const databaseID = "11111111-1111-4111-8111-111111111111"

// This driver records the transaction options at the database/sql boundary.
// Query results are tested separately with sqlmock.
type transactionRecorder struct{ options driver.TxOptions }

func (r *transactionRecorder) Connect(context.Context) (driver.Conn, error) { return r, nil }
func (r *transactionRecorder) Driver() driver.Driver                        { return r }
func (r *transactionRecorder) Open(string) (driver.Conn, error)             { return r, nil }
func (r *transactionRecorder) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected prepare")
}
func (r *transactionRecorder) Close() error              { return nil }
func (r *transactionRecorder) Begin() (driver.Tx, error) { return nil, errors.New("expected BeginTx") }
func (r *transactionRecorder) BeginTx(_ context.Context, options driver.TxOptions) (driver.Tx, error) {
	r.options = options
	return recordedTransaction{}, nil
}

type recordedTransaction struct{}

func (recordedTransaction) Commit() error   { return nil }
func (recordedTransaction) Rollback() error { return nil }

func TestViewerOpensReadOnlyTransaction(t *testing.T) {
	recorder := &transactionRecorder{}
	db := sql.OpenDB(recorder)
	defer db.Close()
	h, _, _ := fixture(t)
	h.d.OpenTarget = func(context.Context, string) (*sql.DB, error) { return db, nil }
	req := httptest.NewRequest("GET", "/", nil)
	req.SetPathValue("database_id", databaseID)
	tx, _, cancel, ok := h.open(httptest.NewRecorder(), req)
	if !ok {
		t.Fatal("could not open viewer transaction")
	}
	defer cancel()
	defer tx.Rollback()
	if !recorder.options.ReadOnly {
		t.Fatal("viewer opened a writable transaction")
	}
}

type statusError int

func (e statusError) Error() string   { return "private authorization details" }
func (e statusError) StatusCode() int { return int(e) }

func fixture(t *testing.T) (*Handler, sqlmock.Sqlmock, *http.ServeMux) {
	t.Helper()
	db, m, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := m.ExpectationsWereMet(); err != nil {
			t.Error(err)
		}
		db.Close()
	})
	h, err := New(Dependencies{Platform: db, Actor: func(*http.Request) (Actor, error) { return Actor{ID: "actor"}, nil }, Authorize: func(_ context.Context, a Actor, id string, access Access) error {
		if a.ID != "actor" || id != databaseID || access != View {
			t.Errorf("wrong authorization: %+v %s %s", a, id, access)
		}
		return nil
	}, OpenTarget: func(context.Context, string) (*sql.DB, error) { return db, nil }, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	if err := h.Register(mux); err != nil {
		t.Fatal(err)
	}
	return h, m, mux
}
func request(mux *http.ServeMux, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
	return w
}
func assertError(t *testing.T, w *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	var body struct {
		Error struct {
			Code string `json:"code"`
		}
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if w.Code != status || body.Error.Code != code {
		t.Fatalf("got %d %s, want %d %s", w.Code, w.Body, status, code)
	}
	if strings.Contains(w.Body.String(), "private") {
		t.Fatal("leaked private error")
	}
}
func expectMetadata(m sqlmock.Sqlmock, s, t string) {
	m.ExpectQuery("SELECT c.oid FROM").WithArgs(s, t).WillReturnRows(sqlmock.NewRows([]string{"oid"}).AddRow(42))
	m.ExpectQuery("SELECT a.attname,pg_catalog.format_type").WithArgs(42).WillReturnRows(sqlmock.NewRows([]string{"name", "type", "nullable", "default"}).AddRow("id\"x", "bigint", false, "nextval('seq')").AddRow("label", "text", true, nil))
	m.ExpectQuery("SELECT conname,CASE contype").WithArgs(42).WillReturnRows(sqlmock.NewRows([]string{"name", "kind", "definition"}).AddRow("pk", "primary", "PRIMARY KEY (id)"))
	m.ExpectQuery("SELECT ci.relname,pg_catalog.pg_get_indexdef").WithArgs(42).WillReturnRows(sqlmock.NewRows([]string{"name", "definition", "unique", "primary"}).AddRow("pk", "CREATE UNIQUE INDEX pk", true, true))
}
func TestConstructorAndRegisterValidation(t *testing.T) {
	h, _, _ := fixture(t)
	good := h.d
	for _, field := range []string{"platform", "actor", "authorize", "target", "timeout"} {
		t.Run(field, func(t *testing.T) {
			d := good
			switch field {
			case "platform":
				d.Platform = nil
			case "actor":
				d.Actor = nil
			case "authorize":
				d.Authorize = nil
			case "target":
				d.OpenTarget = nil
			case "timeout":
				d.Timeout = 0
			}
			if _, err := New(d); err == nil {
				t.Fatal("accepted incomplete dependencies")
			}
		})
	}
	if err := h.Register(nil); err == nil {
		t.Fatal("accepted nil mux")
	}
}
func TestSchemasGroupsTablesAndKeepsEmptySchemas(t *testing.T) {
	_, m, mux := fixture(t)
	m.ExpectBegin()
	m.ExpectQuery("SELECT n.nspname,c.relname").WillReturnRows(sqlmock.NewRows([]string{"schema", "table"}).AddRow("z", "one").AddRow("a", nil).AddRow("z", "two"))
	m.ExpectRollback()
	w := request(mux, "/api/databases/"+databaseID+"/schemas")
	want := `{"data":{"items":[{"name":"a","tables":[]},{"name":"z","tables":[{"name":"one"},{"name":"two"}]}]}}`
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != want {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("cacheable response")
	}
}
func TestTableMetadata(t *testing.T) {
	_, m, mux := fixture(t)
	m.ExpectBegin()
	expectMetadata(m, "public", "items")
	m.ExpectRollback()
	w := request(mux, "/api/databases/"+databaseID+"/table?schema=public&table=items")
	var result struct{ Data Metadata }
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	v := result.Data
	if w.Code != 200 || v.Schema != "public" || v.Table != "items" || len(v.Columns) != 2 || v.Columns[0].Default == nil || *v.Columns[0].Default != "nextval('seq')" || v.Columns[0].Nullable || !v.Columns[1].Nullable || v.Columns[1].Default != nil || len(v.Constraints) != 1 || v.Constraints[0].Kind != "primary" || len(v.Indexes) != 1 || !v.Indexes[0].Unique || !v.Indexes[0].Primary {
		t.Fatalf("%d %+v %s", w.Code, v, w.Body)
	}
}
func TestRowsTextNullPaginationAndQuotedIdentifiers(t *testing.T) {
	for _, more := range []bool{false, true} {
		t.Run(map[bool]string{false: "last page", true: "more rows"}[more], func(t *testing.T) {
			_, m, mux := fixture(t)
			s, table := "s\"chema", "t\"able"
			m.ExpectBegin()
			expectMetadata(m, s, table)
			m.ExpectQuery("WITH chosen AS").WithArgs(s, table).WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("id\"x"))
			q := `SELECT t."id""x"::text,t."label"::text FROM "s""chema"."t""able" AS t ORDER BY t."id""x" LIMIT $1 OFFSET $2`
			rows := sqlmock.NewRows([]string{"id", "label"}).AddRow("9007199254740993", nil)
			if more {
				rows.AddRow("9007199254740994", "extra")
			}
			m.ExpectQuery(regexp.QuoteMeta(q)).WithArgs(2, 3).WillReturnRows(rows)
			m.ExpectRollback()
			w := request(mux, "/api/databases/"+databaseID+"/rows?"+url.Values{"schema": {s}, "table": {table}, "limit": {"1"}, "offset": {"3"}}.Encode())
			var result struct{ Data RowsResult }
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			v := result.Data
			if w.Code != 200 || v.Limit != 1 || v.Offset != 3 || v.HasMore != more || len(v.Columns) != 2 || len(v.Rows) != 1 || v.Rows[0][0] == nil || *v.Rows[0][0] != "9007199254740993" || v.Rows[0][1] != nil {
				t.Fatalf("%d %+v %s", w.Code, v, w.Body)
			}
		})
	}
}
func TestDenialsNeverOpenTarget(t *testing.T) {
	for _, tc := range []struct {
		name     string
		actorErr bool
		access   error
		status   int
		code     string
	}{{"unauthenticated", true, nil, 401, "UNAUTHENTICATED"}, {"forbidden", false, statusError(403), 403, "FORBIDDEN"}, {"missing", false, statusError(404), 404, "NOT_FOUND"}, {"auth failure", false, errors.New("private auth error"), 500, "INTERNAL_ERROR"}} {
		t.Run(tc.name, func(t *testing.T) {
			h, _, mux := fixture(t)
			h.d.OpenTarget = func(context.Context, string) (*sql.DB, error) {
				t.Fatal("opened target before authorization")
				return nil, nil
			}
			h.d.Authorize = func(context.Context, Actor, string, Access) error { return tc.access }
			if tc.actorErr {
				h.d.Actor = func(*http.Request) (Actor, error) { return Actor{}, errors.New("private") }
			}
			w := request(mux, "/api/databases/"+databaseID+"/schemas")
			assertError(t, w, tc.status, tc.code)
		})
	}
}
func TestInvalidParametersHaveNoDatabaseIO(t *testing.T) {
	for _, query := range []string{"schema=pg_catalog&table=x", "schema=public&table=", "schema=public&table=x&limit=0", "schema=public&table=x&limit=201", "schema=public&table=x&offset=-1", "schema=public&table=x&offset=999999999999999999999999"} {
		t.Run(query, func(t *testing.T) {
			h, _, mux := fixture(t)
			h.d.OpenTarget = func(context.Context, string) (*sql.DB, error) {
				t.Fatal("opened target for invalid input")
				return nil, nil
			}
			assertError(t, request(mux, "/api/databases/"+databaseID+"/rows?"+query), 400, "INVALID_REQUEST")
		})
	}
}
func TestDatabaseFailuresAreSanitized(t *testing.T) {
	for _, kind := range []string{"platform", "target open", "target begin", "target query", "table missing"} {
		t.Run(kind, func(t *testing.T) {
			h, m, mux := fixture(t)
			path := "/api/databases/" + databaseID + "/schemas"
			status, code := 502, "TARGET_UNAVAILABLE"
			switch kind {
			case "platform":
				path = "/api/databases"
				m.ExpectQuery("SELECT c.id::text,c.name").WithArgs("actor").WillReturnError(errors.New("private password"))
				status, code = 500, "INTERNAL_ERROR"
			case "target open":
				h.d.OpenTarget = func(context.Context, string) (*sql.DB, error) { return nil, errors.New("private dsn") }
			case "target begin":
				m.ExpectBegin().WillReturnError(errors.New("private dsn"))
			case "target query":
				m.ExpectBegin()
				m.ExpectQuery("SELECT n.nspname,c.relname").WillReturnError(errors.New("private sql"))
				m.ExpectRollback()
			case "table missing":
				path = "/api/databases/" + databaseID + "/table?schema=public&table=gone"
				m.ExpectBegin()
				m.ExpectQuery("SELECT c.oid FROM").WithArgs("public", "gone").WillReturnRows(sqlmock.NewRows([]string{"oid"}))
				m.ExpectRollback()
				status, code = 404, "NOT_FOUND"
			}
			assertError(t, request(mux, path), status, code)
		})
	}
}
func TestDatabaseList(t *testing.T) {
	_, m, mux := fixture(t)
	m.ExpectQuery("SELECT c.id::text,c.name").WithArgs("actor").WillReturnRows(sqlmock.NewRows([]string{"id", "name", "access"}).AddRow(databaseID, "Demo", "edit"))
	w := request(mux, "/api/databases")
	var v struct {
		Data struct{ Items []map[string]string }
	}
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || len(v.Data.Items) != 1 || v.Data.Items[0]["access"] != "edit" || v.Data.Items[0]["id"] != databaseID {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}

func TestEmptySchemasAndRowsUseArrays(t *testing.T) {
	t.Run("schemas", func(t *testing.T) {
		_, m, mux := fixture(t)
		m.ExpectBegin()
		m.ExpectQuery("SELECT n.nspname,c.relname").WillReturnRows(sqlmock.NewRows([]string{"schema", "table"}))
		m.ExpectRollback()
		w := request(mux, "/api/databases/"+databaseID+"/schemas")
		if w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"data":{"items":[]}}` {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
	})
	t.Run("rows without unique key use default pagination", func(t *testing.T) {
		_, m, mux := fixture(t)
		m.ExpectBegin()
		expectMetadata(m, "public", "items")
		m.ExpectQuery("WITH chosen AS").WithArgs("public", "items").WillReturnRows(sqlmock.NewRows([]string{"name"}))
		m.ExpectQuery(regexp.QuoteMeta(`SELECT t."id""x"::text,t."label"::text FROM "public"."items" AS t LIMIT $1 OFFSET $2`)).WithArgs(51, 0).WillReturnRows(sqlmock.NewRows([]string{"id", "label"}))
		m.ExpectRollback()
		w := request(mux, "/api/databases/"+databaseID+"/rows?schema=public&table=items")
		var v struct{ Data RowsResult }
		if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
			t.Fatal(err)
		}
		if w.Code != 200 || v.Data.Limit != 50 || v.Data.Offset != 0 || v.Data.HasMore || v.Data.Rows == nil || len(v.Data.Rows) != 0 {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
	})
}

func TestMetadataFailureStagesAreSanitized(t *testing.T) {
	for _, stage := range []string{"oid", "columns", "constraints", "indexes"} {
		t.Run(stage, func(t *testing.T) {
			_, m, mux := fixture(t)
			m.ExpectBegin()
			if stage == "oid" {
				m.ExpectQuery("SELECT c.oid FROM").WithArgs("public", "items").WillReturnError(errors.New("private catalog"))
			} else {
				m.ExpectQuery("SELECT c.oid FROM").WithArgs("public", "items").WillReturnRows(sqlmock.NewRows([]string{"oid"}).AddRow(42))
				if stage == "columns" {
					m.ExpectQuery("SELECT a.attname,pg_catalog.format_type").WithArgs(42).WillReturnError(errors.New("private catalog"))
				} else {
					m.ExpectQuery("SELECT a.attname,pg_catalog.format_type").WithArgs(42).WillReturnRows(sqlmock.NewRows([]string{"name", "type", "nullable", "default"}))
					if stage == "constraints" {
						m.ExpectQuery("SELECT conname,CASE contype").WithArgs(42).WillReturnError(errors.New("private catalog"))
					} else {
						m.ExpectQuery("SELECT conname,CASE contype").WithArgs(42).WillReturnRows(sqlmock.NewRows([]string{"name", "kind", "definition"}))
						m.ExpectQuery("SELECT ci.relname,pg_catalog.pg_get_indexdef").WithArgs(42).WillReturnError(errors.New("private catalog"))
					}
				}
			}
			m.ExpectRollback()
			assertError(t, request(mux, "/api/databases/"+databaseID+"/table?schema=public&table=items"), 502, "TARGET_UNAVAILABLE")
		})
	}
}

func TestSchemaReadErrorsAreNotPartialSuccess(t *testing.T) {
	for _, stage := range []string{"scan", "iteration"} {
		t.Run(stage, func(t *testing.T) {
			_, m, mux := fixture(t)
			m.ExpectBegin()
			rows := sqlmock.NewRows([]string{"schema", "table"})
			if stage == "scan" {
				rows.AddRow(nil, "item")
			} else {
				rows.AddRow("public", "item").AddRow("other", "item").RowError(1, errors.New("private stream failure"))
			}
			m.ExpectQuery("SELECT n.nspname,c.relname").WillReturnRows(rows)
			m.ExpectRollback()
			assertError(t, request(mux, "/api/databases/"+databaseID+"/schemas"), 502, "TARGET_UNAVAILABLE")
		})
	}
}

func TestInvalidDatabaseIDNeverAuthorizesOrOpens(t *testing.T) {
	h, _, mux := fixture(t)
	h.d.Authorize = func(context.Context, Actor, string, Access) error { t.Fatal("authorized invalid id"); return nil }
	h.d.OpenTarget = func(context.Context, string) (*sql.DB, error) { t.Fatal("opened invalid id"); return nil, nil }
	assertError(t, request(mux, "/api/databases/not-a-uuid/schemas"), 400, "INVALID_REQUEST")
}
