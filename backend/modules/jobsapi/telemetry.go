package jobsapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"time"

	"github.com/Jerannn24/Tugas-Besar-WBD.git/modules/authz"
	"github.com/Jerannn24/Tugas-Besar-WBD.git/telemetry"
	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
)

func NewJSONLogger(w io.Writer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
		if a.Key == slog.TimeKey && a.Value.Kind() == slog.KindTime {
			a.Value = slog.TimeValue(a.Value.Time().UTC())
		}
		return a
	}}))
}

type eventLogger interface {
	Info(ctx context.Context, event string, attrs ...slog.Attr)
	Error(ctx context.Context, event string, attrs ...slog.Attr)
}

type slogEvents struct{ logger *slog.Logger }

func (s slogEvents) Info(ctx context.Context, event string, attrs ...slog.Attr) {
	s.emit(ctx, slog.LevelInfo, event, attrs)
}

func (s slogEvents) Error(ctx context.Context, event string, attrs ...slog.Attr) {
	s.emit(ctx, slog.LevelError, event, attrs)
}

func (s slogEvents) emit(ctx context.Context, level slog.Level, event string, attrs []slog.Attr) {
	s.logger.LogAttrs(ctx, level, event, append([]slog.Attr{slog.String("event", event)}, attrs...)...)
}

// Tanpa Logger eksplisit, log ikut telemetry B agar trace_id dan correlation_id request terbawa.
func eventsFor(logger *slog.Logger) eventLogger {
	if logger == nil {
		return telemetry.DefaultLogger()
	}
	return slogEvents{logger}
}

func (h *Handler) observe(operation string, next fiber.Handler) fiber.Handler {
	return func(c *fiber.Ctx) error {
		attrs := requestAttrs(c, operation)
		h.log.Info(c.UserContext(), "http.request.start", append(attrs, slog.String("status", "started"))...)
		start := time.Now()
		err := next(c)
		code := c.Response().StatusCode()
		var fe *fiber.Error
		if errors.As(err, &fe) {
			code = fe.Code
		} else if err != nil {
			code = fiber.StatusInternalServerError
		}
		status := "ok"
		if code >= 400 {
			status = "error"
		}
		attrs = append(attrs,
			slog.String("status", status),
			slog.Int("http_status", code),
			slog.Float64("duration_ms", float64(time.Since(start).Microseconds())/1000),
		)
		if code >= 400 {
			if errCode := responseCode(c.Response().Body()); errCode != "" {
				attrs = append(attrs, slog.String("error_code", errCode))
			}
		}
		if code >= 500 {
			h.log.Error(c.UserContext(), "http.request.finish", attrs...)
		} else {
			h.log.Info(c.UserContext(), "http.request.finish", attrs...)
		}
		return err
	}
}

func requestAttrs(c *fiber.Ctx, operation string) []slog.Attr {
	attrs := []slog.Attr{slog.String("operation", operation)}
	// Telemetry B sudah menulis correlation_id dari trace; hanya isi sendiri jika trace tidak ada.
	if _, ok := telemetry.TraceFromContext(c.UserContext()); !ok {
		attrs = append(attrs, slog.String("correlation_id", requestID(c)))
	}
	if actor, ok := authz.ActorFromContext(c); ok && actor.ID != uuid.Nil {
		attrs = append(attrs, slog.String("actor_id", actor.ID.String()))
	}
	if id, err := uuid.Parse(c.Params("database_id")); err == nil {
		attrs = append(attrs, slog.String("database_id", id.String()))
	}
	if id, err := uuid.Parse(c.Params("job_id")); err == nil {
		attrs = append(attrs, slog.String("job_id", id.String()))
	}
	args := c.Context().QueryArgs()
	if n, err := strconv.Atoi(string(args.Peek("limit"))); err == nil {
		attrs = append(attrs, slog.Int("limit", n))
	}
	if args.Has("cursor") {
		attrs = append(attrs, slog.Bool("has_cursor", true))
	}
	return attrs
}

func requestID(c *fiber.Ctx) string {
	if id, err := uuid.Parse(c.Get("X-Request-ID")); err == nil {
		return id.String()
	}
	return uuid.NewString()
}

func responseCode(body []byte) string {
	var v struct {
		Error *struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &v) != nil || v.Error == nil {
		return ""
	}
	return v.Error.Code
}
