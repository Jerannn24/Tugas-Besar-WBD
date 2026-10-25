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
