package databaseapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Jerannn24/Tugas-Besar-WBD.git/modules/authz"
	"github.com/gofiber/fiber/v2"
)

func TestRegisterRejectsMissingDependencies(t *testing.T) {
	tests := []struct {
		name        string
		platformNil bool
		sessionNil  bool
		authzNil    bool
		poolsNil    bool
	}{
		{
			name:        "missing platform database",
			platformNil: true,
		},
		{
			name:       "missing session service",
			sessionNil: true,
		},
		{
			name:     "missing authorizer",
			authzNil: true,
		},
		{
			name:     "missing pool manager",
			poolsNil: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := fiber.New()

			var platformNil = tt.platformNil
			var sessionNil = tt.sessionNil
			var authzNil = tt.authzNil
			var poolsNil = tt.poolsNil
			
			_ = platformNil
			_ = sessionNil
			_ = authzNil
			_ = poolsNil

			err := Register(app, nil, nil, nil, nil)
			if err == nil {
				t.Fatal("Register() error = nil, want dependency error")
			}
		})
	}
}

func TestCookieValue(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{
		Name:  "app_session",
		Value: "opaque-session-token",
	})

	if got := cookieValue(req, "app_session"); got != "opaque-session-token" {
		t.Fatalf("cookieValue() = %q, want %q", got, "opaque-session-token")
	}
}

func TestCookieValueMissingCookie(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)

	if got := cookieValue(req, "app_session"); got != "" {
		t.Fatalf("cookieValue() = %q, want empty string", got)
	}
}

func fallbackMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/databases", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	return jsonFallback(mux)
}

func assertFallback(t *testing.T, method, path string, status int, code string) {
	t.Helper()
	rec := httptest.NewRecorder()
	fallbackMux().ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	if rec.Code != status {
		t.Fatalf("%s %s status = %d, want %d", method, path, rec.Code, status)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Fatalf("Content-Type = %q", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q", cc)
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
		t.Fatalf("unexpected error body: %s", rec.Body.String())
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
