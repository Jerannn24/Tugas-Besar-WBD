package migration

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Jerannn24/Tugas-Besar-WBD.git/history"
)

type memoryStore struct {
	a                                                     attempt
	lookupCalls, loadCalls, reserveCalls, transitionCalls int
	lookupFn                                              func(int) (attempt, error)
	reserveFn                                             func(func(string) error) (attempt, bool, error)
	loadFn                                                func(int) (attempt, error)
	transitionErr                                         error
}

func (m *memoryStore) lookup(context.Context, string, string) (attempt, error) {
	m.lookupCalls++
	if m.lookupFn != nil {
		return m.lookupFn(m.lookupCalls)
	}
	return attempt{}, sql.ErrNoRows
}
func (m *memoryStore) binding(context.Context, string) (string, error) { return testTargetID, nil }
func (m *memoryStore) reserve(_ context.Context, _ Actor, _, _, _ string, _ ExecuteRequest, prepare func(string) error) (attempt, bool, error) {
	m.reserveCalls++
	if m.reserveFn != nil {
		return m.reserveFn(prepare)
	}
	if err := prepare(m.a.SQL); err != nil {
		return attempt{}, false, err
	}
	return m.a, true, nil
}
func (m *memoryStore) load(context.Context, string, string) (attempt, error) {
	m.loadCalls++
	if m.loadFn != nil {
		return m.loadFn(m.loadCalls)
	}
	return m.a, nil
}
func (m *memoryStore) transition(_ context.Context, a attempt, status string, failure *history.ExecutionError) (attempt, error) {
	m.transitionCalls++
	if m.transitionErr != nil {
		return attempt{}, m.transitionErr
	}
	a.Status = status
	a.Error = failure
	m.a = a
	return a, nil
}

func executionHandler(t *testing.T, f *targetFixture, store *memoryStore) *Handler {
	t.Helper()
	platform, _ := mockDB(t)
	h, err := New(Dependencies{Platform: platform, Actor: func(*http.Request) (Actor, error) { return Actor{testActorID}, nil }, Authorize: func(context.Context, Actor, string) error { return nil }, CheckCSRF: func(*http.Request) error { return nil }, Acquire: func(context.Context, string) (*TargetLease, error) {
		if f == nil {
			t.Fatal("target opened unexpectedly")
		}
		return f.lease, nil
	}, Preflight: func(context.Context, *sql.Tx, string) error { return nil }, Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	h.store = store
	return h
}

func TestExecutorReplaysSameIntentWithoutTarget(t *testing.T) {
	a := sampleAttempt()
	a.Status = "succeeded"
	s := &memoryStore{a: a, lookupFn: func(int) (attempt, error) { return a, nil }}
	h := executionHandler(t, nil, s)
	got, err := h.Execute(context.Background(), Actor{testActorID}, testDBID, testDraftID, ExecuteRequest{1, testRequestID})
	if err != nil || got.ID != a.ID || s.reserveCalls != 0 {
		t.Fatalf("replay=%+v error=%v reserve=%d", got, err, s.reserveCalls)
	}
	for _, in := range []ExecuteRequest{{2, testRequestID}, {1, testRequestID}} {
		actor := Actor{testActorID}
		if in.Revision == 1 {
			actor.ID = testDraftID
		}
		_, err := h.Execute(context.Background(), actor, testDBID, testDraftID, in)
		assertCode(t, err, "REQUEST_CONFLICT")
	}
}

func TestExecutorRejectsInvalidInputOrAccessBeforeStore(t *testing.T) {
	s := &memoryStore{}
	h := executionHandler(t, nil, s)
	for _, in := range []ExecuteRequest{{0, testRequestID}, {1, "bad"}, {2147483648, testRequestID}} {
		_, err := h.Execute(context.Background(), Actor{testActorID}, testDBID, testDraftID, in)
		assertCode(t, err, "INVALID_REQUEST")
	}
	h.d.Authorize = func(context.Context, Actor, string) error { return fault(403, "FORBIDDEN", "denied") }
	_, err := h.Execute(context.Background(), Actor{testActorID}, testDBID, testDraftID, ExecuteRequest{1, testRequestID})
	assertCode(t, err, "FORBIDDEN")
	if s.lookupCalls != 0 || s.reserveCalls != 0 {
		t.Fatal("access rejection touched store")
	}
}

func TestExecutorRunsWholeMigration(t *testing.T) {
	f := newTargetFixture(t)
	f.expectOpen()
	f.expectBegin()
	a := sampleAttempt()
	s := &memoryStore{a: a}
	h := executionHandler(t, f, s)
	preflight := 0
	h.d.Preflight = func(_ context.Context, tx *sql.Tx, text string) error {
		preflight++
		if tx == nil || text != a.SQL {
			t.Fatal("wrong preflight transaction or SQL")
		}
		return nil
	}
	f.expectTicket(a)
	f.executor.ExpectExec(regexp.QuoteMeta(a.SQL)).WillReturnResult(sqlmock.NewResult(0, 1))
	f.executor.ExpectExec("SELECT wbd_auth.record_receipt").WillReturnResult(sqlmock.NewResult(0, 1))
	f.executor.ExpectCommit()
	f.preparer.ExpectQuery("SELECT execution_id").WithArgs(a.ID).WillReturnRows(receiptRows(testReceipt(a)))
	f.expectClose()
	got, err := h.Execute(context.Background(), Actor{testActorID}, testDBID, testDraftID, ExecuteRequest{1, testRequestID})
	if err != nil || got.Status != "succeeded" || preflight != 1 || s.reserveCalls != 1 || s.transitionCalls != 1 || f.released != 1 {
		t.Fatalf("result=%+v error=%v preflight=%d reserve=%d transition=%d released=%d", got, err, preflight, s.reserveCalls, s.transitionCalls, f.released)
	}
}

func TestExecutorSafetyAndRevocation(t *testing.T) {
	for _, tc := range []string{"SQL unsupported", "preflight unavailable", "preflight rejected", "access revoked"} {
		t.Run(tc, func(t *testing.T) {
			f := newTargetFixture(t)
			f.expectOpen()
			f.expectBegin()
			a := sampleAttempt()
			if tc == "SQL unsupported" {
				a.SQL = "DROP SCHEMA managed CASCADE"
			}
			s := &memoryStore{a: a}
			h := executionHandler(t, f, s)
			switch tc {
			case "preflight unavailable":
				h.d.Preflight = func(context.Context, *sql.Tx, string) error { return errors.New("private host failure") }
			case "preflight rejected":
				h.d.Preflight = func(context.Context, *sql.Tx, string) error { return unsupported("unsafe trigger") }
			case "access revoked":
				calls := 0
				h.d.Authorize = func(context.Context, Actor, string) error {
					calls++
					if calls >= 4 {
						return fault(403, "FORBIDDEN", "revoked")
					}
					return nil
				}
			}
			f.executor.ExpectRollback()
			f.expectClose()
			got, err := h.Execute(context.Background(), Actor{testActorID}, testDBID, testDraftID, ExecuteRequest{1, testRequestID})
			if tc == "access revoked" {
				if err != nil || got.Status != "failed" || got.Error == nil || got.Error.Code != "ACCESS_REVOKED" {
					t.Fatalf("revocation=%+v %v", got, err)
				}
			} else {
				code := "SQL_UNSUPPORTED"
				if tc == "preflight unavailable" {
					code = "TARGET_UNAVAILABLE"
				}
				assertCode(t, err, code)
				if s.transitionCalls != 0 {
					t.Fatal("pre-reservation rejection finalized an attempt")
				}
			}
		})
	}
}

func TestExecutorHTTPGuardsAndStrictJSON(t *testing.T) {
	for _, tc := range []struct {
		name, body        string
		actorErr, csrfErr bool
		status            int
		code              string
	}{
		{"anonymous", `{}`, true, false, 401, "UNAUTHENTICATED"},
		{"CSRF", `{}`, false, true, 403, "CSRF_INVALID"},
		{"extra", `{"revision":1,"request_id":"` + testRequestID + `","sql":"SELECT 1"}`, false, false, 400, "INVALID_REQUEST"},
		{"null", `null`, false, false, 400, "INVALID_REQUEST"},
		{"missing", `{"revision":1}`, false, false, 400, "INVALID_REQUEST"},
		{"large", strings.Repeat("x", 4097), false, false, 400, "INVALID_REQUEST"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &memoryStore{}
			h := executionHandler(t, nil, s)
			if tc.actorErr {
				h.d.Actor = func(*http.Request) (Actor, error) { return Actor{}, errors.New("no session") }
			}
			if tc.csrfErr {
				h.d.CheckCSRF = func(*http.Request) error { return errors.New("csrf") }
			}
			mux := http.NewServeMux()
			if err := h.Register(mux); err != nil {
				t.Fatal(err)
			}
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest("POST", "/api/databases/"+testDBID+"/drafts/"+testDraftID+"/execute", strings.NewReader(tc.body)))
			if rec.Code != tc.status || !strings.Contains(rec.Body.String(), `"`+tc.code+`"`) || rec.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("HTTP %d %s", rec.Code, rec.Body)
			}
			if s.lookupCalls != 0 {
				t.Fatal("invalid HTTP request touched store")
			}
		})
	}
}

func TestExecutorHTTPReturnsAcceptedForUnresolvedReplay(t *testing.T) {
	for _, status := range []string{"running", "unknown", "succeeded", "failed"} {
		t.Run(status, func(t *testing.T) {
			a := sampleAttempt()
			a.Status = status
			s := &memoryStore{lookupFn: func(int) (attempt, error) { return a, nil }}
			h := executionHandler(t, nil, s)
			rec := httptest.NewRecorder()
			req := httptest.NewRequest("POST", "/", strings.NewReader(`{"revision":1,"request_id":"`+testRequestID+`"}`))
			req.SetPathValue("database_id", testDBID)
			req.SetPathValue("draft_id", testDraftID)
			h.executeHTTP(rec, req)
			want := 200
			if !terminal(status) {
				want = 202
			}
			if rec.Code != want {
				t.Fatalf("status %d want %d", rec.Code, want)
			}
		})
	}
}
