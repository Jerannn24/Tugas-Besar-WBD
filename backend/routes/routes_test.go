package routes

import (
	"encoding/json"
	"net/http/httptest"
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
