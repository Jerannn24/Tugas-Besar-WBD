package routes

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
)

func TestHealthRoute(t *testing.T) {
	t.Setenv("DATABASE_URL", "")

	app := fiber.New()
	Register(app, nil, "test-secret")

	req := httptest.NewRequest("GET", "/api/health", nil)

	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var body struct {
		Success bool `json:"success"`
		Data    struct {
			Message string `json:"message"`
		} `json:"data"`
		Error any `json:"error"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}

	if !body.Success {
		t.Fatal("success should be true")
	}
	if body.Data.Message != "API is running" {
		t.Fatalf("message = %q, want %q", body.Data.Message, "API is running")
	}
	if body.Error != nil {
		t.Fatalf("error should be null, got %#v", body.Error)
	}
}

func TestAPIResponsesAreNoStore(t *testing.T) {
	t.Setenv("DATABASE_URL", "")

	app := fiber.New()
	Register(app, nil, "test-secret")

	for _, path := range []string{"/api/health", "/api/csrf", "/api/does-not-exist"} {
		resp, err := app.Test(httptest.NewRequest("GET", path, nil))
		if err != nil {
			t.Fatal(err)
		}
		if got := resp.Header.Get("Cache-Control"); got != "no-store" {
			t.Errorf("%s Cache-Control = %q, want no-store", path, got)
		}
		resp.Body.Close()
	}
}

func TestUnknownAPIRouteReturnsJSONError(t *testing.T) {
	t.Setenv("DATABASE_URL", "")

	app := fiber.New()
	Register(app, nil, "test-secret")

	resp, err := app.Test(httptest.NewRequest("GET", "/api/does-not-exist", nil))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	var body struct {
		Error struct {
			Code        string            `json:"code"`
			FieldErrors map[string]string `json:"field_errors"`
			Retryable   *bool             `json:"retryable"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code == "" || body.Error.FieldErrors == nil || body.Error.Retryable == nil {
		t.Fatalf("body not in C01 shape: %+v", body.Error)
	}
}
