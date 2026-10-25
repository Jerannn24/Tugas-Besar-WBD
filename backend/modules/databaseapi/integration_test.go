package databaseapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Jerannn24/Tugas-Besar-WBD.git/modules/auth"
	"github.com/Jerannn24/Tugas-Besar-WBD.git/modules/authz"
	"github.com/Jerannn24/Tugas-Besar-WBD.git/modules/connections"
	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
)

func TestRegisterRejectsMissingDependencies(t *testing.T) {
	err := Register(fiber.New(), nil, nil, nil)
	if err == nil {
		t.Fatal("Register() error = nil, want dependency error")
	}
}

func TestActorFromRequestMissing(t *testing.T) {
	req := httptest.NewRequest("GET", "/api/databases", nil)
	_, ok := authz.ActorFromRequest(req)
	if ok {
		t.Fatal("ActorFromRequest() ok = true, want false")
	}
}

func TestJSONFallbackUnknownPath(t *testing.T) {
	assertFallback(t, "GET", "/api/databases/nope", http.StatusNotFound, "NOT_FOUND")
}

func TestJSONFallbackWrongMethod(t *testing.T) {
	assertFallback(t, "DELETE", "/api/databases", http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED")
}

func TestJSONFallbackPassesThrough(t *testing.T) {
	rec := httptest.NewRecorder()
	fallbackMux().ServeHTTP(rec, httptest.NewRequest("GET", "/api/databases", nil))
	if rec.Code != http.StatusTeapot {
		t.Fatalf("status = %d, want passthrough %d", rec.Code, http.StatusTeapot)
	}
}

func TestMapAuthzError(t *testing.T) {
	if mapAuthzError(nil) != nil {
		t.Fatal("nil must map to nil")
	}
	tests := []struct {
		err    error
		status int
	}{
		{authz.ErrNotFound, 404},
		{authz.ErrForbidden, 403},
		{authz.ErrUnauthenticated, 401},
	}
	for _, tt := range tests {
		var he httpError
		if err := mapAuthzError(tt.err); !errors.As(err, &he) || he.status != tt.status {
			t.Errorf("mapAuthzError(%v) = %v, want status %d", tt.err, err, tt.status)
		}
	}
	other := errors.New("boom")
	if err := mapAuthzError(other); err != other {
		t.Errorf("unknown error must pass through, got %v", err)
	}
}

func fallbackMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/databases", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	return jsonFallback(mux)
}

func assertFallback(t *testing.T, method, path string, wantStatus int, wantCode string) {
	t.Helper()
	rec := httptest.NewRecorder()
	fallbackMux().ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	if rec.Code != wantStatus {
		t.Fatalf("status = %d, want %d", rec.Code, wantStatus)
	}
	var body struct {
		Error struct {
			Code        string            `json:"code"`
			FieldErrors map[string]string `json:"field_errors"`
			Retryable   *bool             `json:"retryable"`
		} `json:"error"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code != wantCode {
		t.Fatalf("code = %q, want %q", body.Error.Code, wantCode)
	}
	if body.Error.FieldErrors == nil || body.Error.Retryable == nil || *body.Error.Retryable {
		t.Fatalf("field_errors/retryable missing: %+v", body.Error)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Fatalf("content-type = %q", got)
	}
	if wantStatus == http.StatusMethodNotAllowed && rec.Header().Get("Allow") == "" {
		t.Fatal("Allow header missing on 405")
	}
}

const (
	testDB    = "a0000000-0000-4000-8000-0000000000a1"
	testDraft = "d0000000-0000-4000-8000-000000000001"
	testExec  = "e0000000-0000-4000-8000-000000000001"
)

type fakeAuthorizer struct {
	err      error
	required []authz.Access
}

func (f *fakeAuthorizer) ResolveActor(context.Context, uuid.UUID) (authz.Actor, error) {
	return authz.Actor{}, nil
}
func (f *fakeAuthorizer) EffectiveAccess(context.Context, uuid.UUID, uuid.UUID) (authz.Access, error) {
	return "", f.err
}
func (f *fakeAuthorizer) RequireAccess(_ context.Context, _, _ uuid.UUID, required authz.Access) error {
	f.required = append(f.required, required)
	return f.err
}
func (f *fakeAuthorizer) RequireAdmin(authz.Actor) error { return nil }

func routedApp(t *testing.T, authorizer authz.Authorizer) *fiber.App {
	t.Helper()
	db, err := sql.Open("pgx", "postgres://x:y@127.0.0.1:1/none?connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	app := fiber.New()
	api := app.Group("/api", func(c *fiber.Ctx) error {
		// same key authz.ActorMiddleware stores under; its session prerequisite is unexported
		c.Locals("authz.actor", authz.Actor{ID: uuid.MustParse("c0000000-0000-4000-8000-0000000000c1")})
		return c.Next()
	})
	if err := Register(api, db, authorizer, connections.NewPoolManager(nil)); err != nil {
		t.Fatal(err)
	}
	return app
}

type routed struct {
	status int
	code   string
	body   string
}

func send(t *testing.T, app *fiber.App, method, path string) routed {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: auth.CSRFCookieName, Value: "tok"})
	req.Header.Set("X-CSRF-Token", "tok")
	resp, err := app.Test(req, 5000)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var body struct {
		Error struct {
			Code        string            `json:"code"`
			FieldErrors map[string]string `json:"field_errors"`
			Retryable   *bool             `json:"retryable"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("%s %s: non-JSON body %q", method, path, raw)
	}
	if body.Error.FieldErrors == nil || body.Error.Retryable == nil {
		t.Fatalf("%s %s: error body lacks field_errors/retryable: %s", method, path, raw)
	}
	return routed{resp.StatusCode, body.Error.Code, string(raw)}
}

func TestRoutingReachesEveryHandler(t *testing.T) {
	base := "/api/databases/" + testDB
	routes := []struct {
		method, path string
		access       authz.Access
	}{
		{"GET", base + "/schemas", authz.AccessView},
		{"GET", base + "/table?schema=public&table=t", authz.AccessView},
		{"GET", base + "/rows?schema=public&table=t", authz.AccessView},
		{"GET", base + "/drafts", authz.AccessEdit},
		{"POST", base + "/drafts", authz.AccessEdit},
		{"GET", base + "/drafts/" + testDraft, authz.AccessEdit},
		{"PATCH", base + "/drafts/" + testDraft, authz.AccessEdit},
		{"GET", base + "/migrations", authz.AccessView},
		{"GET", base + "/migrations/" + testExec, authz.AccessView},
	}
	for _, rt := range routes {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			authorizer := &fakeAuthorizer{err: authz.ErrForbidden}
			got := send(t, routedApp(t, authorizer), rt.method, rt.path)
			if got.status != 403 || got.code != "FORBIDDEN" {
				t.Fatalf("got %d %s, want 403 FORBIDDEN: %s", got.status, got.code, got.body)
			}
			if len(authorizer.required) != 1 || authorizer.required[0] != rt.access {
				t.Fatalf("required access = %v, want [%s]", authorizer.required, rt.access)
			}
		})
	}
}

func TestRoutingListReachesViewer(t *testing.T) {
	authorizer := &fakeAuthorizer{err: authz.ErrForbidden}
	got := send(t, routedApp(t, authorizer), "GET", "/api/databases")
	if got.status != 500 || got.code != "INTERNAL_ERROR" {
		t.Fatalf("got %d %s, want 500 INTERNAL_ERROR (list queries the unreachable platform DB): %s", got.status, got.code, got.body)
	}
	if len(authorizer.required) != 0 {
		t.Fatalf("list must not call RequireAccess, got %v", authorizer.required)
	}
}

func TestRoutingFallbacks(t *testing.T) {
	base := "/api/databases/" + testDB
	tests := []struct {
		method, path string
		status       int
		code         string
	}{
		{"GET", base + "/nope", 404, "NOT_FOUND"},
		{"DELETE", base + "/migrations", 405, "METHOD_NOT_ALLOWED"},
		{"POST", base + "/drafts/" + testDraft + "/execute", 404, "NOT_FOUND"},
		{"POST", base + "/migrations", 405, "METHOD_NOT_ALLOWED"},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			authorizer := &fakeAuthorizer{}
			got := send(t, routedApp(t, authorizer), tt.method, tt.path)
			if got.status != tt.status || got.code != tt.code {
				t.Fatalf("got %d %s, want %d %s: %s", got.status, got.code, tt.status, tt.code, got.body)
			}
			if len(authorizer.required) != 0 {
				t.Fatalf("no handler may run, RequireAccess calls = %v", authorizer.required)
			}
		})
	}
}

func TestRoutingHistoryValidationRuns(t *testing.T) {
	authorizer := &fakeAuthorizer{}
	got := send(t, routedApp(t, authorizer), "GET", "/api/databases/"+testDB+"/migrations/not-a-uuid")
	if got.status != 400 || got.code != "INVALID_REQUEST" {
		t.Fatalf("got %d %s, want 400 INVALID_REQUEST: %s", got.status, got.code, got.body)
	}
	if len(authorizer.required) != 1 || authorizer.required[0] != authz.AccessView {
		t.Fatalf("history must authorize with view access, got %v", authorizer.required)
	}
}
