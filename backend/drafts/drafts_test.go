package drafts

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

const databaseID = "11111111-1111-4111-8111-111111111111"
const draftID = "22222222-2222-4222-8222-222222222222"
const targetID = "33333333-3333-4333-8333-333333333333"

var draftColumns = []string{"id", "database_id", "title", "sql", "revision", "created_by", "created_at", "updated_at", "applied_execution_id"}
var timestamp = time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)

type statusError int

func (e statusError) Error() string   { return "private authorization details" }
func (e statusError) StatusCode() int { return int(e) }

type uuidArgument struct{}

func (uuidArgument) Match(v driver.Value) bool {
	s, ok := v.(string)
	return ok && validUUID(s) && s[14] == '4' && strings.ContainsRune("89ab", rune(s[19]))
}
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
	h, err := New(Dependencies{Platform: db, Actor: func(*http.Request) (Actor, error) { return Actor{ID: "actor"}, nil }, Authorize: func(_ context.Context, a Actor, id string) error {
		if a.ID != "actor" || id != databaseID {
			t.Errorf("wrong authorization: %+v %s", a, id)
		}
		return nil
	}, CheckCSRF: func(*http.Request) error { return nil }, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	if err := h.Register(mux); err != nil {
		t.Fatal(err)
	}
	return h, m, mux
}
func request(mux *http.ServeMux, method, path, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
	return w
}
func path(detail bool) string {
	s := "/api/databases/" + databaseID + "/drafts"
	if detail {
		s += "/" + draftID
	}
	return s
}
func assertError(t *testing.T, w *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	var v struct {
		Error struct {
			Code string `json:"code"`
		}
	}
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if w.Code != status || v.Error.Code != code {
		t.Fatalf("got %d %s, want %d %s", w.Code, w.Body, status, code)
	}
	if strings.Contains(w.Body.String(), "private") {
		t.Fatal("leaked private details")
	}
}
func draftRows(title, sql string, revision int, applied any) *sqlmock.Rows {
	return sqlmock.NewRows(draftColumns).AddRow(draftID, databaseID, title, sql, revision, "actor", timestamp, timestamp, applied)
}
func expectLocks(m sqlmock.Sqlmock) {
	m.ExpectBegin()
	m.ExpectQuery("SELECT execution_target_id::text").WithArgs(databaseID).WillReturnRows(sqlmock.NewRows([]string{"target"}).AddRow(targetID))
	m.ExpectQuery("SELECT id::text FROM app.migration_target_state").WithArgs(targetID).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(targetID))
}
func assertDraft(t *testing.T, w *httptest.ResponseRecorder, status, revision int, title, sql string) {
	t.Helper()
	var v struct{ Data Draft }
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	d := v.Data
	if w.Code != status || d.ID != draftID || d.DatabaseID != databaseID || d.Title != title || d.SQL != sql || d.Revision != revision || d.CreatedBy != "actor" || d.AppliedExecutionID != nil || !d.UpdatedAt.Equal(timestamp) {
		t.Fatalf("%d %+v %s", w.Code, d, w.Body)
	}
	if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Content-Type") != "application/json" {
		t.Fatal("missing response privacy/content headers")
	}
}
func TestConstructorAndRegistration(t *testing.T) {
	h, _, _ := fixture(t)
	for _, name := range []string{"platform", "actor", "authorize", "csrf", "timeout"} {
		t.Run(name, func(t *testing.T) {
			d := h.d
			switch name {
			case "platform":
				d.Platform = nil
			case "actor":
				d.Actor = nil
			case "authorize":
				d.Authorize = nil
			case "csrf":
				d.CheckCSRF = nil
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
func TestCreateNormalizesTitleAndStartsAtRevisionOne(t *testing.T) {
	h, m, mux := fixture(t)
	authCalls := 0
	h.d.Authorize = func(context.Context, Actor, string) error { authCalls++; return nil }
	expectLocks(m)
	m.ExpectQuery("INSERT INTO app.migration_drafts").WithArgs(uuidArgument{}, databaseID, "Migration title", " SELECT 1; ", "actor", sqlmock.AnyArg()).WillReturnRows(draftRows("Migration title", " SELECT 1; ", 1, nil))
	m.ExpectCommit()
	w := request(mux, "POST", path(false), `{"title":"  Migration title  ","sql":" SELECT 1; "}`)
	assertDraft(t, w, 201, 1, "Migration title", " SELECT 1; ")
	if authCalls != 2 {
		t.Fatalf("expected authorization before and after locks, got %d", authCalls)
	}
}
func TestPatchIncrementsRevision(t *testing.T) {
	_, m, mux := fixture(t)
	expectLocks(m)
	m.ExpectQuery("SELECT id::text,database_id::text,title,sql,revision").WithArgs(databaseID, draftID).WillReturnRows(draftRows("Old", "SELECT 1", 2, nil))
	m.ExpectQuery("SELECT EXISTS").WithArgs(databaseID, draftID).WillReturnRows(sqlmock.NewRows([]string{"busy"}).AddRow(false))
	m.ExpectQuery(`UPDATE app.migration_drafts SET .*revision=revision\+1,.* WHERE database_id=\$1 AND id=\$2 AND revision=\$5 RETURNING`).WithArgs(databaseID, draftID, "New", "SELECT 2", 2).WillReturnRows(draftRows("New", "SELECT 2", 3, nil))
	m.ExpectCommit()
	assertDraft(t, request(mux, "PATCH", path(true), `{"title":" New ","sql":"SELECT 2","revision":2}`), 200, 3, "New", "SELECT 2")
}
func TestPatchConflictsRollbackWithoutUpdate(t *testing.T) {
	for _, tc := range []struct {
		name, code       string
		stored, expected int
		applied          any
		busy             bool
	}{{"stale", "VERSION_CONFLICT", 3, 2, nil, false}, {"applied", "ALREADY_APPLIED", 2, 2, targetID, false}, {"running or unknown execution", "DATABASE_BUSY", 2, 2, nil, true}, {"revision exhausted", "VERSION_CONFLICT", 2147483647, 2147483647, nil, false}} {
		t.Run(tc.name, func(t *testing.T) {
			_, m, mux := fixture(t)
			expectLocks(m)
			m.ExpectQuery("SELECT id::text,database_id::text,title,sql,revision").WithArgs(databaseID, draftID).WillReturnRows(draftRows("Old", "SELECT 1", tc.stored, tc.applied))
			if tc.busy {
				m.ExpectQuery("SELECT EXISTS").WithArgs(databaseID, draftID).WillReturnRows(sqlmock.NewRows([]string{"busy"}).AddRow(true))
			}
			m.ExpectRollback()
			body, _ := json.Marshal(map[string]any{"title": "New", "sql": "SELECT 2", "revision": tc.expected})
			assertError(t, request(mux, "PATCH", path(true), string(body)), 409, tc.code)
		})
	}
}
func TestDenialsDoNotAccessDatabase(t *testing.T) {
	for _, method := range []string{"GET", "POST", "PATCH"} {
		for _, denial := range []string{"actor", "forbidden", "missing", "csrf", "internal"} {
			if method == "GET" && denial == "csrf" {
				continue
			}
			t.Run(method+" "+denial, func(t *testing.T) {
				h, _, mux := fixture(t)
				status, code := 403, "FORBIDDEN"
				switch denial {
				case "actor":
					h.d.Actor = func(*http.Request) (Actor, error) { return Actor{}, errors.New("private") }
					status, code = 401, "UNAUTHENTICATED"
				case "forbidden":
					h.d.Authorize = func(context.Context, Actor, string) error { return statusError(403) }
				case "missing":
					h.d.Authorize = func(context.Context, Actor, string) error { return statusError(404) }
					status, code = 404, "NOT_FOUND"
				case "csrf":
					h.d.CheckCSRF = func(*http.Request) error { return errors.New("private") }
					code = "CSRF_INVALID"
				case "internal":
					h.d.Authorize = func(context.Context, Actor, string) error { return errors.New("private") }
					status, code = 500, "INTERNAL_ERROR"
				}
				assertError(t, request(mux, method, path(method == "PATCH"), `{"title":"T","sql":"SELECT 1","revision":1}`), status, code)
			})
		}
	}
}
func TestInputRejectionBeforeDatabaseIO(t *testing.T) {
	for _, tc := range []struct {
		name, method, body string
		status             int
		code               string
	}{
		{"malformed", "POST", `{`, 400, "INVALID_REQUEST"}, {"null", "POST", `null`, 400, "INVALID_REQUEST"}, {"array", "POST", `[]`, 400, "INVALID_REQUEST"}, {"unknown field", "POST", `{"title":"T","sql":"SELECT 1","admin":true}`, 400, "INVALID_REQUEST"}, {"create revision", "POST", `{"title":"T","sql":"SELECT 1","revision":1}`, 400, "INVALID_REQUEST"}, {"missing revision", "PATCH", `{"title":"T","sql":"SELECT 1"}`, 400, "INVALID_REQUEST"}, {"null revision", "PATCH", `{"title":"T","sql":"SELECT 1","revision":null}`, 400, "INVALID_REQUEST"}, {"zero revision", "PATCH", `{"title":"T","sql":"SELECT 1","revision":0}`, 422, "VALIDATION_FAILED"}, {"large revision", "PATCH", `{"title":"T","sql":"SELECT 1","revision":2147483648}`, 422, "VALIDATION_FAILED"}, {"fractional revision", "PATCH", `{"title":"T","sql":"SELECT 1","revision":1.5}`, 400, "INVALID_REQUEST"}, {"blank title", "POST", `{"title":"  ","sql":"SELECT 1"}`, 422, "VALIDATION_FAILED"}, {"blank sql", "POST", `{"title":"T","sql":" \n "}`, 422, "VALIDATION_FAILED"}, {"nul sql", "POST", `{"title":"T","sql":"SELECT\u0000"}`, 422, "VALIDATION_FAILED"}, {"nul title", "POST", `{"title":"T\u0000","sql":"SELECT 1"}`, 422, "VALIDATION_FAILED"}, {"trailing json", "POST", `{"title":"T","sql":"SELECT 1"} {}`, 400, "INVALID_REQUEST"}, {"invalid utf8", "POST", string([]byte{'{', '"', 'x', '"', ':', '"', 0xff, '"', '}'}), 400, "INVALID_REQUEST"}, {"oversized body", "POST", strings.Repeat(" ", (2<<20)+1), 400, "INVALID_REQUEST"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, mux := fixture(t)
			assertError(t, request(mux, tc.method, path(tc.method == "PATCH"), tc.body), tc.status, tc.code)
		})
	}
	for _, tc := range []struct{ name, title, sql string }{{"title over 100 runes", strings.Repeat("界", 101), "SELECT 1"}, {"sql over byte limit", "T", strings.Repeat("x", 262145)}} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, mux := fixture(t)
			body, _ := json.Marshal(map[string]string{"title": tc.title, "sql": tc.sql})
			w := request(mux, "POST", path(false), string(body))
			assertError(t, w, 422, "VALIDATION_FAILED")
			var v struct {
				Error struct {
					Fields map[string]string `json:"field_errors"`
				}
			}
			json.Unmarshal(w.Body.Bytes(), &v)
			if len(v.Error.Fields) != 1 {
				t.Fatal("missing field validation detail")
			}
		})
	}
}
func TestAcceptedTitleAndSQLBoundaries(t *testing.T) {
	for _, tc := range []struct{ name, title, sql string }{{"one character", "界", "x"}, {"maximum title runes", strings.Repeat("界", 100), "SELECT 1"}, {"maximum sql bytes", "T", strings.Repeat("x", 262144)}} {
		t.Run(tc.name, func(t *testing.T) {
			_, m, mux := fixture(t)
			expectLocks(m)
			m.ExpectQuery("INSERT INTO app.migration_drafts").WithArgs(uuidArgument{}, databaseID, tc.title, tc.sql, "actor", sqlmock.AnyArg()).WillReturnRows(draftRows(tc.title, tc.sql, 1, nil))
			m.ExpectCommit()
			body, _ := json.Marshal(map[string]string{"title": tc.title, "sql": tc.sql})
			assertDraft(t, request(mux, "POST", path(false), string(body)), 201, 1, tc.title, tc.sql)
		})
	}
}
func TestAuthorizationRecheckedAfterLock(t *testing.T) {
	h, m, mux := fixture(t)
	calls := 0
	h.d.Authorize = func(context.Context, Actor, string) error {
		calls++
		if calls == 2 {
			return statusError(403)
		}
		return nil
	}
	expectLocks(m)
	m.ExpectRollback()
	assertError(t, request(mux, "POST", path(false), `{"title":"T","sql":"SELECT 1"}`), 403, "FORBIDDEN")
	if calls != 2 {
		t.Fatalf("authorization calls=%d", calls)
	}
}
func TestListAndDetail(t *testing.T) {
	t.Run("empty list", func(t *testing.T) {
		_, m, mux := fixture(t)
		m.ExpectQuery("SELECT id::text,database_id::text,title,sql,revision").WithArgs(databaseID).WillReturnRows(sqlmock.NewRows(draftColumns))
		w := request(mux, "GET", path(false), "")
		if w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"data":{"items":[]}}` {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
	})
	t.Run("detail", func(t *testing.T) {
		_, m, mux := fixture(t)
		m.ExpectQuery("SELECT id::text,database_id::text,title,sql,revision").WithArgs(databaseID, draftID).WillReturnRows(draftRows("Title", "SELECT 1", 4, nil))
		assertDraft(t, request(mux, "GET", path(true), ""), 200, 4, "Title", "SELECT 1")
	})
	t.Run("not found", func(t *testing.T) {
		_, m, mux := fixture(t)
		m.ExpectQuery("SELECT id::text,database_id::text,title,sql,revision").WithArgs(databaseID, draftID).WillReturnRows(sqlmock.NewRows(draftColumns))
		assertError(t, request(mux, "GET", path(true), ""), 404, "NOT_FOUND")
	})
}
func TestDatabaseErrorsAreSanitizedAndRollback(t *testing.T) {
	for _, kind := range []string{"begin", "connection", "insert", "commit", "list"} {
		t.Run(kind, func(t *testing.T) {
			_, m, mux := fixture(t)
			method, p, body := "POST", path(false), `{"title":"T","sql":"SELECT 1"}`
			switch kind {
			case "begin":
				m.ExpectBegin().WillReturnError(errors.New("private password"))
			case "connection":
				m.ExpectBegin()
				m.ExpectQuery("SELECT execution_target_id::text").WithArgs(databaseID).WillReturnError(errors.New("private password"))
				m.ExpectRollback()
			case "insert":
				expectLocks(m)
				m.ExpectQuery("INSERT INTO app.migration_drafts").WillReturnError(errors.New("private password"))
				m.ExpectRollback()
			case "commit":
				expectLocks(m)
				m.ExpectQuery("INSERT INTO app.migration_drafts").WillReturnRows(draftRows("T", "SELECT 1", 1, nil))
				m.ExpectCommit().WillReturnError(errors.New("private password"))
			case "list":
				method, body = "GET", ""
				m.ExpectQuery("SELECT id::text,database_id::text,title,sql,revision").WithArgs(databaseID).WillReturnError(errors.New("private password"))
			}
			assertError(t, request(mux, method, p, body), 500, "INTERNAL_ERROR")
		})
	}
}

func TestPatchUpdateFailureRollsBack(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		status int
		code   string
	}{{"concurrent revision change", sql.ErrNoRows, 409, "VERSION_CONFLICT"}, {"database failure", errors.New("private update failure"), 500, "INTERNAL_ERROR"}} {
		t.Run(tc.name, func(t *testing.T) {
			_, m, mux := fixture(t)
			expectLocks(m)
			m.ExpectQuery("SELECT id::text,database_id::text,title,sql,revision").WithArgs(databaseID, draftID).WillReturnRows(draftRows("Old", "SELECT 1", 2, nil))
			m.ExpectQuery("SELECT EXISTS").WithArgs(databaseID, draftID).WillReturnRows(sqlmock.NewRows([]string{"busy"}).AddRow(false))
			m.ExpectQuery("UPDATE app.migration_drafts SET").WithArgs(databaseID, draftID, "New", "SELECT 2", 2).WillReturnError(tc.err)
			m.ExpectRollback()
			assertError(t, request(mux, "PATCH", path(true), `{"title":"New","sql":"SELECT 2","revision":2}`), tc.status, tc.code)
		})
	}
}

func TestInvalidIdentifiersHaveNoDatabaseIO(t *testing.T) {
	for _, tc := range []struct{ name, method, path string }{{"database", "GET", "/api/databases/invalid/drafts"}, {"draft detail", "GET", "/api/databases/" + databaseID + "/drafts/invalid"}, {"draft patch", "PATCH", "/api/databases/" + databaseID + "/drafts/invalid"}} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, mux := fixture(t)
			assertError(t, request(mux, tc.method, tc.path, `{"title":"T","sql":"SELECT 1","revision":1}`), 400, "INVALID_REQUEST")
		})
	}
}

func TestCreateWithoutExecutionTarget(t *testing.T) {
	_, m, mux := fixture(t)
	m.ExpectBegin()
	m.ExpectQuery("SELECT execution_target_id::text").WithArgs(databaseID).WillReturnRows(sqlmock.NewRows([]string{"target"}).AddRow(nil))
	m.ExpectQuery("INSERT INTO app.migration_drafts").WithArgs(uuidArgument{}, databaseID, "Title", "SELECT 1", "actor", sqlmock.AnyArg()).WillReturnRows(draftRows("Title", "SELECT 1", 1, nil))
	m.ExpectCommit()
	assertDraft(t, request(mux, "POST", path(false), `{"title":"Title","sql":"SELECT 1"}`), 201, 1, "Title", "SELECT 1")
}

func TestListReadErrorsDoNotReturnPartialSuccess(t *testing.T) {
	for _, stage := range []string{"scan", "iteration"} {
		t.Run(stage, func(t *testing.T) {
			_, m, mux := fixture(t)
			rows := draftRows("Title", "SELECT 1", 1, nil)
			if stage == "scan" {
				rows = sqlmock.NewRows(draftColumns).AddRow(draftID, databaseID, "Title", "SELECT 1", "invalid integer", "actor", timestamp, timestamp, nil)
			} else {
				rows.AddRow(draftID, databaseID, "Title", "SELECT 1", 1, "actor", timestamp, timestamp, nil).RowError(1, errors.New("private stream error"))
			}
			m.ExpectQuery("SELECT id::text,database_id::text,title,sql,revision").WithArgs(databaseID).WillReturnRows(rows)
			assertError(t, request(mux, "GET", path(false), ""), 500, "INTERNAL_ERROR")
		})
	}
}

func TestDetailPreservesAppliedExecutionAndNormalizesTimestamp(t *testing.T) {
	_, m, mux := fixture(t)
	local := time.Date(2026, 10, 25, 19, 0, 0, 0, time.FixedZone("WIB", 7*3600))
	m.ExpectQuery("SELECT id::text,database_id::text,title,sql,revision").WithArgs(databaseID, draftID).WillReturnRows(sqlmock.NewRows(draftColumns).AddRow(draftID, databaseID, "Title", "SELECT 1", 1, "actor", local, local, targetID))
	w := request(mux, "GET", path(true), "")
	var v struct{ Data Draft }
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || v.Data.AppliedExecutionID == nil || *v.Data.AppliedExecutionID != targetID || !v.Data.UpdatedAt.Equal(timestamp) || !strings.Contains(w.Body.String(), `"updated_at":"2026-10-25T12:00:00Z"`) {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), "created_at") {
		t.Fatal("internal created_at field exposed")
	}
}
