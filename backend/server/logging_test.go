package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Jerannn24/Tugas-Besar-WBD.git/modules/auth"
	"github.com/Jerannn24/Tugas-Besar-WBD.git/telemetry"
	"github.com/gofiber/fiber/v2"
)

type loggedApp struct {
	app      *fiber.App
	logs     *bytes.Buffer
	handled  *int
	indexDir string
}

func newLoggedApp(t *testing.T) loggedApp {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "dist")
	writeFile(t, filepath.Join(dir, "index.html"), indexBody)
	handled := 0
	app := fiber.New(fiber.Config{ErrorHandler: func(c *fiber.Ctx, err error) error {
		handled++
		return ErrorHandler(c, err)
	}})
	logs := &bytes.Buffer{}
	app.Use(telemetry.Middleware(telemetry.NewLogger(logs, slog.LevelDebug)))
	api := app.Group("/api")
	api.Post("/mutate", auth.CSRFMiddleware(), func(c *fiber.Ctx) error { return c.SendStatus(fiber.StatusNoContent) })
	api.Get("/boom", func(c *fiber.Ctx) error { return errors.New("boom password=rahasia") })
	Frontend(app, dir)
	return loggedApp{app: app, logs: logs, handled: &handled, indexDir: dir}
}

func requestLog(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("log line is not JSON: %q", line)
		}
		if entry["event"] == "http.request.completed" {
			return entry
		}
	}
	t.Fatalf("no http.request.completed in %s", buf.String())
	return nil
}

func TestRequestLogRecordsSentStatus(t *testing.T) {
	cases := []struct {
		name, method, path, code, level string
		status, handled                 int
	}{
		{"unknown api", "GET", "/api/tidak-ada", "NOT_FOUND", "INFO", 404, 1},
		{"csrf rejected", "POST", "/api/mutate", "CSRF_INVALID", "INFO", 403, 0},
		{"handler error", "GET", "/api/boom", "INTERNAL_ERROR", "ERROR", 500, 1},
		{"spa deep link", "GET", "/databases/abc", "", "INFO", 200, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			la := newLoggedApp(t)
			resp, err := la.app.Test(httptest.NewRequest(tc.method, tc.path, nil))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			var body bytes.Buffer
			_, _ = body.ReadFrom(resp.Body)
			if resp.StatusCode != tc.status {
				t.Fatalf("status = %d, want %d (%s)", resp.StatusCode, tc.status, body.String())
			}
			if tc.code != "" && !strings.Contains(body.String(), `"code":"`+tc.code+`"`) {
				t.Fatalf("body missing code %s: %s", tc.code, body.String())
			}
			if *la.handled != tc.handled {
				t.Fatalf("ErrorHandler called %d times, want %d", *la.handled, tc.handled)
			}
			entry := requestLog(t, la.logs)
			if entry["status"] != float64(tc.status) || entry["level"] != tc.level {
				t.Fatalf("log status=%v level=%v, want %d %s", entry["status"], entry["level"], tc.status, tc.level)
			}
			if strings.Contains(la.logs.String(), "rahasia") {
				t.Fatalf("secret leaked into log: %s", la.logs.String())
			}
		})
	}
}
