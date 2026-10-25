package response

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"
)

func decode(t *testing.T, app *fiber.App, path string) map[string]json.RawMessage {
	t.Helper()
	resp, err := app.Test(httptest.NewRequest("GET", path, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	return body
}

func TestFailBodyFollowsContract(t *testing.T) {
	app := fiber.New()
	app.Get("/x", func(c *fiber.Ctx) error { return Fail(c, 403, "FORBIDDEN", "Akses ditolak") })

	body := decode(t, app, "/x")
	var e map[string]json.RawMessage
	if err := json.Unmarshal(body["error"], &e); err != nil {
		t.Fatal(err)
	}
	if string(e["field_errors"]) != "{}" {
		t.Fatalf("field_errors = %s, want {}", e["field_errors"])
	}
	if string(e["retryable"]) != "false" {
		t.Fatalf("retryable = %s, want false", e["retryable"])
	}
	if string(e["code"]) != `"FORBIDDEN"` || string(e["message"]) != `"Akses ditolak"` {
		t.Fatalf("unexpected code/message: %s %s", e["code"], e["message"])
	}
	if string(body["success"]) != "false" || string(body["data"]) != "null" {
		t.Fatalf("success/data = %s/%s", body["success"], body["data"])
	}
}

func TestJSONSuccessShapeUnchanged(t *testing.T) {
	app := fiber.New()
	app.Get("/x", func(c *fiber.Ctx) error { return JSON(c, 200, fiber.Map{"a": 1}) })

	body := decode(t, app, "/x")
	if len(body) != 3 {
		t.Fatalf("keys = %d, want 3", len(body))
	}
	if string(body["success"]) != "true" || string(body["data"]) != `{"a":1}` || string(body["error"]) != "null" {
		t.Fatalf("unexpected body: %v", body)
	}
}
