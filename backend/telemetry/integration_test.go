package telemetry

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Jerannn24/Tugas-Besar-WBD.git/executioncontract"
	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5"
)

func TestRequestQueueWorkerSQLTrace(t *testing.T) {
	app := fiber.New()
	app.Use(Middleware(nil))
	var persisted executioncontract.TraceContext
	var requestTrace Trace
	app.Post("/enqueue", func(c *fiber.Ctx) error {
		requestTrace, _ = TraceFromContext(c.UserContext())
		persisted = executioncontract.TraceContext{Traceparent: requestTrace.Traceparent(), Tracestate: requestTrace.TraceState}
		return c.SendStatus(202)
	})
	req := httptest.NewRequest("POST", "/enqueue", nil)
	req.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	req.Header.Set("tracestate", "vendor=value")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 202 || persisted.Tracestate != "vendor=value" || requestTrace.ParentSpanID != "00f067aa0ba902b7" {
		t.Fatalf("request trace not propagated: status=%d trace=%+v", resp.StatusCode, requestTrace)
	}
	ctx, err := ContextFromTraceContext(context.Background(), persisted, "")
	if err != nil {
		t.Fatal(err)
	}
	worker, _ := TraceFromContext(ctx)
	if worker.TraceID != requestTrace.TraceID || worker.ParentSpanID != requestTrace.SpanID || worker.TraceState != persisted.Tracestate {
		t.Fatalf("worker trace not linked: %+v", worker)
	}
	lockCtx, lock := ChildTrace(ctx)
	tracer := &QueryTracer{}
	sqlCtx := tracer.TraceQueryStart(lockCtx, nil, pgx.TraceQueryStartData{SQL: "SELECT 1"})
	sqlTrace, _ := TraceFromContext(sqlCtx)
	if lock.ParentSpanID != worker.SpanID || sqlTrace.ParentSpanID != lock.SpanID || sqlTrace.TraceID != worker.TraceID {
		t.Fatalf("lock/SQL trace not linked: lock=%+v sql=%+v", lock, sqlTrace)
	}
}

func TestQueryTracerRedactsSQLAndErrors(t *testing.T) {
	var output bytes.Buffer
	tracer := &QueryTracer{Logger: NewLogger(&output, slog.LevelInfo)}
	ctx := tracer.TraceQueryStart(context.Background(), nil, pgx.TraceQueryStartData{
		SQL: "SELECT 'private-value', 4321 -- private-comment", Args: []any{"private-parameter"},
	})
	tracer.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{Err: errors.New("private-unquoted-error")})
	for _, secret := range []string{"private-value", "4321", "private-comment", "private-parameter", "private-unquoted-error"} {
		if strings.Contains(output.String(), secret) {
			t.Fatalf("query log leaked %q", secret)
		}
	}
	if !strings.Contains(output.String(), "query failed") {
		t.Fatal("missing failure event")
	}
}
