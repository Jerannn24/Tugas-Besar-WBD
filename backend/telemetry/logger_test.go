package telemetry

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
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
