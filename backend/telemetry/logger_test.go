package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestLoggerEmitsTraceAndCorrelationFields(t *testing.T) {
	var output bytes.Buffer
	logger := NewLogger(&output, slog.LevelInfo)
	trace, err := NewTrace("", "", "corr-456")
	if err != nil {
		t.Fatal(err)
	}
	logger.Info(WithTrace(context.Background(), trace), "test.event", slog.String("status", "ok"))
	line := output.String()
	for _, field := range []string{`"event":"test.event"`, `"trace_id":"` + trace.TraceID + `"`, `"span_id":"` + trace.SpanID + `"`, `"correlation_id":"corr-456"`} {
		if !strings.Contains(line, field) {
			t.Fatalf("missing %s in %s", field, line)
		}
	}
}

func TestQueryLogCarriesCorrelationAttrsFlatAndMasked(t *testing.T) {
	var output bytes.Buffer
	tracer := &QueryTracer{Logger: NewLogger(&output, slog.LevelInfo)}
	trace, err := NewTrace("", "", "corr-789")
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithLogAttrs(WithTrace(context.Background(), trace),
		slog.String("job_id", "job-1"), slog.String("execution_id", "exec-1"),
		slog.String("migration_id", "mig-1"), slog.String("attempt_id", "att-1"),
		slog.String("actor_id", "actor-1"), slog.Int("batch_index", 0))
	ctx = WithLogAttrs(ctx, slog.Int("batch_index", 2))
	ctx = tracer.TraceQueryStart(ctx, nil, pgx.TraceQueryStartData{
		SQL: "ALTER ROLE app PASSWORD 'rahasia-sekali'", Args: []any{"parameter-rahasia"},
	})
	tracer.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{})

	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("want start and completed lines, got %d: %s", len(lines), output.String())
	}
	for _, line := range lines {
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("not JSON: %s", line)
		}
		want := map[string]any{"level": "INFO", "job_id": "job-1", "execution_id": "exec-1", "migration_id": "mig-1",
			"attempt_id": "att-1", "actor_id": "actor-1", "batch_index": float64(2), "trace_id": trace.TraceID,
			"parameters": "[REDACTED]"}
		for k, v := range want {
			if entry[k] != v {
				t.Fatalf("%s = %v, want %v in %s", k, entry[k], v, line)
			}
		}
		if entry["span_id"] == "" || entry["span_id"] == nil {
			t.Fatalf("missing span_id: %s", line)
		}
		if strings.Count(line, `"batch_index"`) != 1 {
			t.Fatalf("batch_index duplicated: %s", line)
		}
	}
	for _, secret := range []string{"rahasia-sekali", "parameter-rahasia"} {
		if strings.Contains(output.String(), secret) {
			t.Fatalf("log leaked %q", secret)
		}
	}
}
