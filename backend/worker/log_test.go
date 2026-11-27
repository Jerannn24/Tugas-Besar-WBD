package worker

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/Jerannn24/Tugas-Besar-WBD.git/jobs"
	"github.com/Jerannn24/Tugas-Besar-WBD.git/telemetry"
)

func TestSlogLoggerWritesFlatLeveledJSON(t *testing.T) {
	claim := &jobs.Claim{JobID: "job-1", ActorID: "actor-1", MigrationID: "mig-1"}
	trace := telemetry.Trace{TraceID: "0af7651916cd43dd8448eb211c80319c", SpanID: "b7ad6b7169203331"}
	cases := []struct {
		event  string
		fields map[string]any
		level  string
	}{
		{"job_claimed", map[string]any{"job_id": "job-1", "attempt_no": 1}, "INFO"},
		{"job_claimed", map[string]any{"job_id": "job-1", "attempt_no": 2}, "WARN"},
		{"job_finished_success", map[string]any{"job_id": "job-1", "status": "succeeded"}, "INFO"},
		{"job_finished_partial", map[string]any{"job_id": "job-1", "status": "partial"}, "WARN"},
		{"job_execution_partial", map[string]any{"job_id": "job-1", "status": "partial"}, "WARN"},
		{"job_finished_failed", map[string]any{"job_id": "job-1", "status": "failed"}, "ERROR"},
		{"job_execution_failed", map[string]any{"job_id": "job-1", "error_code": "X"}, "ERROR"},
		{"job_finished_partially_reverted", map[string]any{"job_id": "job-1"}, "ERROR"},
		{"job_finished_unknown", map[string]any{"job_id": "job-1"}, "ERROR"},
		{"lease_renewal_failed", map[string]any{"job_id": "job-1"}, "WARN"},
		{"reconcile_batch_job_unresolved", map[string]any{"job_id": "job-1"}, "WARN"},
		{"reconcile_batch_job_failed", map[string]any{"job_id": "job-1"}, "ERROR"},
		{"step_error", map[string]any{"job_id": "job-1", "error_code": "X"}, "ERROR"},
	}
	for _, tc := range cases {
		var buf bytes.Buffer
		addJobTraceFields(tc.fields, claim, trace, false)
		slogLogger(telemetry.NewLogger(&buf, slog.LevelDebug))(tc.event, tc.fields)
		var entry map[string]any
		if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
			t.Fatalf("%s: not JSON: %s", tc.event, buf.String())
		}
		if _, nested := entry["fields"]; nested {
			t.Fatalf("%s: attributes still nested: %s", tc.event, buf.String())
		}
		want := map[string]any{"level": tc.level, "event": tc.event, "msg": tc.event, "job_id": "job-1",
			"actor_id": "actor-1", "migration_id": "mig-1", "trace_id": trace.TraceID, "span_id": trace.SpanID}
		for k, v := range want {
			if entry[k] != v {
				t.Fatalf("%s: %s = %v, want %v (%s)", tc.event, k, entry[k], v, strings.TrimSpace(buf.String()))
			}
		}
	}
}
