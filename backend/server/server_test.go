package server

import (
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
)

func TestFrontendServesPagesWithoutSwallowingAPI(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "login"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "login", "index.html"), []byte("<html>login</html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	app := fiber.New(fiber.Config{ErrorHandler: ErrorHandler})
	app.Get("/api/ping", func(c *fiber.Ctx) error { return c.SendString("pong") })
	Frontend(app, dir)

	get := func(path string, cookie string) (int, string, string) {
		req := httptest.NewRequest("GET", path, nil)
		if cookie != "" {
			req.Header.Set("Cookie", cookie)
		}
		resp, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header.Get("Location"), string(body)
	}
	if status, _, body := get("/login/", ""); status != 200 || !strings.Contains(body, "login") {
		t.Fatalf("/login/ = %d %q", status, body)
	}
	if status, loc, _ := get("/", ""); status != 302 || loc != "/login/" {
		t.Fatalf("/ = %d %q", status, loc)
	}
	if status, loc, _ := get("/", "app_session=x"); status != 302 || loc != "/databases/" {
		t.Fatalf("/ with session = %d %q", status, loc)
	}
	if status, _, body := get("/api/ping", ""); status != 200 || body != "pong" {
		t.Fatalf("/api/ping = %d %q", status, body)
	}
	if status, _, body := get("/api/nope", ""); status != 404 || !strings.Contains(body, `"NOT_FOUND"`) {
		t.Fatalf("/api/nope = %d %q", status, body)
	}
}

func TestFrontendSkipsMissingDir(t *testing.T) {
	app := fiber.New()
	Frontend(app, filepath.Join(t.TempDir(), "missing"))
	resp, err := app.Test(httptest.NewRequest("GET", "/login/", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 404 {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}
