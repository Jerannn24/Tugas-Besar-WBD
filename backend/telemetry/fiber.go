package telemetry

import (
	"context"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/gofiber/fiber/v2"
)

var correlationPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

func Middleware(logger *Logger) fiber.Handler {
	return func(c *fiber.Ctx) error {
		started := time.Now()
		correlation := c.Get("X-Correlation-ID")
		if !correlationPattern.MatchString(correlation) {
			correlation = ""
		}
		trace, err := NewTrace(c.Get("traceparent"), c.Get("tracestate"), correlation)
		if err != nil {
			trace, _ = NewTrace("", "", correlation)
		}
		ctx := WithTrace(c.UserContext(), trace)
		c.SetUserContext(ctx)
		c.Set("X-Correlation-ID", trace.CorrelationID)
		c.Set("traceparent", trace.Traceparent())
		if trace.TraceState != "" {
			c.Set("tracestate", trace.TraceState)
		}
		requestErr := c.Next()
		if requestErr != nil {
			if handlerErr := c.App().ErrorHandler(c, requestErr); handlerErr != nil {
				_ = c.SendStatus(fiber.StatusInternalServerError)
			}
		}
		attrs := []slog.Attr{
			slog.String("method", c.Method()),
			slog.String("path", c.Path()),
			slog.Int("status", c.Response().StatusCode()),
			slog.Float64("duration_ms", float64(time.Since(started))/float64(time.Millisecond)),
			slog.String("actor_id", safeLocal(c, "actor_id")),
			slog.String("database_id", safeLocal(c, "database_id")),
		}
		if requestErr != nil {
			attrs = append(attrs, slog.String("error", MaskSecrets(MaskSQL(requestErr.Error()))))
		}
		failed := c.Response().StatusCode() >= 500
		if logger != nil {
			if failed {
				logger.Error(ctx, "http.request.completed", attrs...)
			} else {
				logger.Info(ctx, "http.request.completed", attrs...)
			}
		}
		routePath := ""
		if route := c.Route(); route != nil {
			routePath = route.Path
		}
		ExportSpan(context.Background(), Span{TraceID: trace.TraceID, SpanID: trace.SpanID, ParentSpanID: trace.ParentSpanID, Name: "http.request", Kind: 2, StartedAt: started, EndedAt: time.Now(), Failed: failed, Attributes: map[string]string{"http.method": c.Method(), "http.route": routePath, "http.status_code": strconv.Itoa(c.Response().StatusCode())}})
		return nil
	}
}

func safeLocal(c *fiber.Ctx, key string) string {
	v := c.Locals(key)
	if v == nil && key == "database_id" {
		v = c.Params("database_id")
	}
	if v == nil {
		return ""
	}
	s, ok := v.(string)
	if !ok || len(s) > 128 {
		return ""
	}
	if !correlationPattern.MatchString(s) {
		return ""
	}
	return s
}

func MetricsHandler(registry *Registry) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if registry == nil {
			return c.SendStatus(http.StatusServiceUnavailable)
		}
		c.Set(fiber.HeaderContentType, "text/plain; version=0.0.4; charset=utf-8")
		return registry.ServeFiber(c)
	}
}

func (r *Registry) ServeFiber(c *fiber.Ctx) error {
	// Fiber's response writer is used so the Prometheus endpoint does not
	// allocate a second HTTP server or bypass the application's listener.
	body := r.render()
	c.Set(fiber.HeaderContentType, "text/plain; version=0.0.4; charset=utf-8")
	return c.Send(body)
}
