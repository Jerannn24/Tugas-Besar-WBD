package jobsapi

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Jerannn24/Tugas-Besar-WBD.git/executioncontract"
	"github.com/Jerannn24/Tugas-Besar-WBD.git/modules/authz"
	"github.com/Jerannn24/Tugas-Besar-WBD.git/telemetry"
	"github.com/gofiber/fiber/v2"
)

const requestCorrelation = "33333333-3333-4333-8333-333333333333"

func loggedApp(t *testing.T, f *fixture, buf *bytes.Buffer) *fiber.App {
	t.Helper()
	app := fiber.New()
	api := app.Group("/api", sessionAs(actorID))
	protected := api.Group("", authz.ActorMiddleware(f.authz))
	if err := Register(protected, Dependencies{Jobs: f.jobs, Actions: f.actions, Authorizer: f.authz, Logger: NewJSONLogger(buf)}); err != nil {
		t.Fatal(err)
	}
	return app
}

func logLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var v map[string]any
		if err := json.Unmarshal([]byte(line), &v); err != nil {
			t.Fatalf("invalid json %q: %v", line, err)
		}
		out = append(out, v)
	}
	return out
}

func TestRequestsLogStartAndFinish(t *testing.T) {
	cases := []struct {
		name, target, operation string
		access                  error
		status                  float64
		code                    string
	}{
		{"list", base + "/jobs?limit=5&cursor=opaque-secret-cursor", "jobs.list", nil, 200, ""},
		{"get", base + "/jobs/" + jobID, "jobs.get", nil, 200, ""},
		{"attempts", base + "/attempts", "attempts.list", nil, 200, ""},
		{"bad limit", base + "/jobs?limit=0", "jobs.list", nil, 400, "INVALID_REQUEST"},
		{"forbidden", base + "/jobs", "jobs.list", authz.ErrForbidden, 403, "FORBIDDEN"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture()
			f.authz.accessErr = tc.access
			f.jobs.job.Error = &executioncontract.JobError{Code: "INTERNAL_ERROR", Message: "secret_table failed"}
			var buf bytes.Buffer
			req := httptest.NewRequest("GET", tc.target, nil)
			req.Header.Set("X-Request-ID", requestCorrelation)
			req.Header.Set("Cookie", "session=private-session-token")
			if _, err := loggedApp(t, f, &buf).Test(req, -1); err != nil {
				t.Fatal(err)
			}
			t.Log("\n" + buf.String())
			for _, s := range []string{"opaque-secret-cursor", "private-session-token", "secret_table", "propose_rollback"} {
				if strings.Contains(buf.String(), s) {
					t.Fatalf("log leaked %q", s)
				}
			}
			lines := logLines(t, &buf)
			if len(lines) != 2 || lines[0]["event"] != "http.request.start" || lines[1]["event"] != "http.request.finish" {
				t.Fatalf("%+v", lines)
			}
			for _, l := range lines {
				if l["event"] != l["msg"] || l["operation"] != tc.operation || l["correlation_id"] != requestCorrelation || l["actor_id"] != actorID.String() || l["database_id"] != dbID || !strings.HasSuffix(l["time"].(string), "Z") {
					t.Fatalf("missing attributes: %+v", l)
				}
			}
			fin := lines[1]
			if fin["http_status"] != tc.status || fin["duration_ms"] == nil {
				t.Fatalf("%+v", fin)
			}
			if tc.code != "" && (fin["error_code"] != tc.code || fin["status"] != "error") {
				t.Fatalf("%+v", fin)
			}
			if tc.code == "" && fin["status"] != "ok" {
				t.Fatalf("%+v", fin)
			}
			if tc.name == "list" && (fin["has_cursor"] != true || fin["limit"] != float64(5)) {
				t.Fatalf("%+v", fin)
			}
			if tc.name == "get" && fin["job_id"] != jobID {
				t.Fatalf("%+v", fin)
			}
		})
	}
}

func TestDefaultLoggerIsTelemetry(t *testing.T) {
	h, err := New(Dependencies{Jobs: newFixture().jobs, Actions: newFixture().actions, Authorizer: newFixture().authz})
	if err != nil {
		t.Fatal(err)
	}
	if h.log != telemetry.DefaultLogger() {
		t.Fatalf("default logger = %T, want telemetry.DefaultLogger()", h.log)
	}
}

func TestTelemetryLoggerCarriesTrace(t *testing.T) {
	f := newFixture()
	h, err := New(Dependencies{Jobs: f.jobs, Actions: f.actions, Authorizer: f.authz})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	h.log = telemetry.NewLogger(&buf, slog.LevelInfo)
	app := fiber.New()
	api := app.Group("/api", telemetry.Middleware(nil), sessionAs(actorID))
	protected := api.Group("", authz.ActorMiddleware(f.authz))
	protected.Get("/databases/:database_id/jobs", h.observe("jobs.list", h.ListJobs))

	req := httptest.NewRequest("GET", base+"/jobs", nil)
	req.Header.Set("X-Correlation-ID", requestCorrelation)
	req.Header.Set("X-Request-ID", "44444444-4444-4444-8444-444444444444")
	if _, err := app.Test(req, -1); err != nil {
		t.Fatal(err)
	}
	lines := logLines(t, &buf)
	if len(lines) != 2 {
		t.Fatalf("%+v", lines)
	}
	for _, l := range lines {
		if l["trace_id"] == nil || l["span_id"] == nil || l["correlation_id"] != requestCorrelation || l["operation"] != "jobs.list" {
			t.Fatalf("trace not carried: %+v", l)
		}
	}
}
