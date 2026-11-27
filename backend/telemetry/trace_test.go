package telemetry

import (
	"context"
	"strings"
	"testing"
)

func TestTracePropagationCreatesChildSpan(t *testing.T) {
	parent := "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	trace, err := NewTrace(parent, "congo=t61rcWkgMzE", "corr-123")
	if err != nil {
		t.Fatal(err)
	}
	if trace.TraceID != "4bf92f3577b34da6a3ce929d0e0e4736" || trace.ParentSpanID != "00f067aa0ba902b7" {
		t.Fatalf("unexpected propagated trace: %+v", trace)
	}
	ctx, child := ChildTrace(WithTrace(context.Background(), trace))
	if child.TraceID != trace.TraceID || child.ParentSpanID != trace.SpanID || child.SpanID == trace.SpanID {
		t.Fatalf("invalid child trace: %+v", child)
	}
	if got, ok := TraceFromContext(ctx); !ok || got.SpanID != child.SpanID {
		t.Fatal("child trace missing from context")
	}
	if !strings.HasPrefix(child.Traceparent(), "00-"+trace.TraceID+"-") {
		t.Fatalf("invalid traceparent: %s", child.Traceparent())
	}
}

func TestInvalidTraceparentIsRejected(t *testing.T) {
	if _, err := NewTrace("not-a-traceparent", "", ""); err == nil {
		t.Fatal("expected invalid traceparent error")
	}
}
