package authz

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Jerannn24/Tugas-Besar-WBD.git/modules/auth"
	"github.com/google/uuid"
)

func TestRequireAccess(t *testing.T) {
	tests := []struct {
		name     string
		actual   Access
		required Access
		wantErr  bool
	}{
		{"view allows view", AccessView, AccessView, false},
		{"edit allows view", AccessEdit, AccessView, false},
		{"view denies edit", AccessView, AccessEdit, true},
		{"none denies view", AccessNone, AccessView, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := requireEffectiveAccess(tt.actual, tt.required)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestRequireAdmin(t *testing.T) {
	s := &service{}
	if err := s.RequireAdmin(Actor{ID: uuid.New(), IsAdmin: false}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("expected forbidden, got %v", err)
	}
	if err := s.RequireAdmin(Actor{ID: uuid.New(), IsAdmin: true}); err != nil {
		t.Fatalf("expected admin to pass, got %v", err)
	}
}

func TestCheckCSRF(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/", nil)
	request.AddCookie(&http.Cookie{Name: auth.CSRFCookieName, Value: "token"})
	request.Header.Set("X-CSRF-Token", "token")
	if err := CheckCSRF(request); err != nil {
		t.Fatalf("expected valid CSRF request, got %v", err)
	}
	request.Header.Set("X-CSRF-Token", "wrong")
	if !errors.Is(CheckCSRF(request), ErrCSRF) {
		t.Fatal("expected invalid CSRF request")
	}
}

func TestSafeMethods(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		if IsCSRFRequired(method) {
			t.Fatalf("%s should not require CSRF", method)
		}
	}
	if !IsCSRFRequired(http.MethodPost) {
		t.Fatal("POST should require CSRF")
	}
}
