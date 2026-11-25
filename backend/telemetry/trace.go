package telemetry

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/Jerannn24/Tugas-Besar-WBD.git/executioncontract"
)

type traceKey struct{}
type Trace struct {
	TraceID       string
	SpanID        string
	ParentSpanID  string
	TraceFlags    string
	TraceState    string
	CorrelationID string
}

func TraceFromContext(ctx context.Context) (Trace, bool) {
	v, ok := ctx.Value(traceKey{}).(Trace)
	return v, ok
}

func WithTrace(ctx context.Context, trace Trace) context.Context {
	return context.WithValue(ctx, traceKey{}, trace)
}

func NewTrace(parentHeader, traceState, correlationID string) (Trace, error) {
	trace := Trace{TraceFlags: "01", TraceState: traceState, CorrelationID: correlationID}
	var err error
	if traceState != "" && len(traceState) > 512 {
		return Trace{}, fmt.Errorf("tracestate exceeds 512 bytes")
	}
	parts := strings.Split(parentHeader, "-")
	if parentHeader != "" {
		if len(parts) != 4 || parts[0] != "00" || len(parts[1]) != 32 || len(parts[2]) != 16 || len(parts[3]) != 2 {
			return Trace{}, fmt.Errorf("invalid traceparent")
		}
		if _, err := hex.DecodeString(parts[1]); err != nil {
			return Trace{}, fmt.Errorf("invalid trace id")
		}
		if _, err := hex.DecodeString(parts[2]); err != nil {
			return Trace{}, fmt.Errorf("invalid parent span id")
		}
		if _, err := hex.DecodeString(parts[3]); err != nil {
			return Trace{}, fmt.Errorf("invalid trace flags")
		}
		if strings.Trim(parts[1], "0") == "" || strings.Trim(parts[2], "0") == "" {
			return Trace{}, fmt.Errorf("zero trace/span id")
		}
		trace.TraceID, trace.ParentSpanID, trace.TraceFlags = parts[1], parts[2], parts[3]
	} else {
		trace.TraceID, err = randomHex(16)
		if err != nil {
			return Trace{}, err
		}
	}
	trace.SpanID, err = randomHex(8)
	if err != nil {
		return Trace{}, err
	}
	if correlationID == "" {
		trace.CorrelationID = trace.TraceID
	}
	return trace, nil
}

func (t Trace) Traceparent() string {
	flags := t.TraceFlags
	if flags == "" {
		flags = "01"
	}
	return "00-" + t.TraceID + "-" + t.SpanID + "-" + flags
}

func randomHex(bytes int) (string, error) {
	b := make([]byte, bytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// ChildTrace creates a child span while retaining the current trace identity.
func ChildTrace(ctx context.Context) (context.Context, Trace) {
	parent, ok := TraceFromContext(ctx)
	if !ok {
		parent, _ = NewTrace("", "", "")
	}
	child := parent
	child.ParentSpanID = parent.SpanID
	if spanID, err := randomHex(8); err == nil {
		child.SpanID = spanID
	}
	return WithTrace(ctx, child), child
}

// ContextFromTraceContext restores the trace context persisted with a queued job.
func ContextFromTraceContext(ctx context.Context, persisted executioncontract.TraceContext, correlationID string) (context.Context, error) {
	trace, err := NewTrace(persisted.Traceparent, persisted.Tracestate, correlationID)
	if err != nil {
		return ctx, err
	}
	return WithTrace(ctx, trace), nil
}
