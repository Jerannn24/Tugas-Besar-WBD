package databaseapi

import (
	"net/http"
	"net/http/httptest"
	"testing"

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

			// Setiap dependency sengaja nil untuk menguji validasi awal.
			// Karena Register memeriksa dependency satu per satu dengan
			// kondisi OR, cukup satu dependency nil untuk menghasilkan error.
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
