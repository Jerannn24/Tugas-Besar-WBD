package telemetry

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"
)

type Logger struct {
	logger *slog.Logger
}

func NewLogger(output io.Writer, level slog.Level) *Logger {
	if output == nil {
		output = os.Stdout
	}
	return &Logger{logger: slog.New(slog.NewJSONHandler(output, &slog.HandlerOptions{Level: level}))}
}

func (l *Logger) emit(ctx context.Context, level slog.Level, event string, attrs ...slog.Attr) {
	if l == nil || l.logger == nil {
		return
	}
	fields := []slog.Attr{
		slog.String("event", event),
		slog.Time("timestamp", time.Now().UTC()),
	}
	if trace, ok := TraceFromContext(ctx); ok {
		fields = append(fields,
			slog.String("trace_id", trace.TraceID),
			slog.String("span_id", trace.SpanID),
			slog.String("correlation_id", trace.CorrelationID),
		)
	}
	fields = append(fields, LogAttrsFromContext(ctx)...)
	fields = append(fields, attrs...)
	fields = sanitizeLogAttrs(fields)
	l.logger.LogAttrs(ctx, level, event, fields...)
	ExportLogRecord(ctx, level, event, fields)
}

func sanitizeLogAttrs(attrs []slog.Attr) []slog.Attr {
	safe := make([]slog.Attr, 0, len(attrs))
	for _, attr := range attrs {
		key := strings.ToLower(attr.Key)
		if strings.Contains(key, "password") || strings.Contains(key, "passwd") ||
			strings.Contains(key, "token") || strings.Contains(key, "secret") ||
			strings.Contains(key, "session") || strings.Contains(key, "cookie") ||
			strings.Contains(key, "authorization") || strings.Contains(key, "credential") ||
			strings.Contains(key, "api_key") || strings.Contains(key, "private_key") {
			safe = append(safe, slog.String(attr.Key, "[REDACTED]"))
			continue
		}
		value := attr.Value.Resolve()
		if value.Kind() == slog.KindString {
			safe = append(safe, slog.String(attr.Key, MaskSecrets(value.String())))
		} else {
			safe = append(safe, attr)
		}
	}
	return safe
}

type logAttrsKey struct{}

func WithLogAttrs(ctx context.Context, attrs ...slog.Attr) context.Context {
	merged := append([]slog.Attr(nil), LogAttrsFromContext(ctx)...)
	for _, a := range attrs {
		replaced := false
		for i := range merged {
			if merged[i].Key == a.Key {
				merged[i] = a
				replaced = true
				break
			}
		}
		if !replaced {
			merged = append(merged, a)
		}
	}
	return context.WithValue(ctx, logAttrsKey{}, merged)
}

func LogAttrsFromContext(ctx context.Context) []slog.Attr {
	if ctx == nil {
		return nil
	}
	attrs, _ := ctx.Value(logAttrsKey{}).([]slog.Attr)
	return attrs
}

func (l *Logger) Log(ctx context.Context, level slog.Level, event string, attrs ...slog.Attr) {
	l.emit(ctx, level, event, attrs...)
}

func (l *Logger) Debug(ctx context.Context, event string, attrs ...slog.Attr) {
	l.emit(ctx, slog.LevelDebug, event, attrs...)
}
func (l *Logger) Info(ctx context.Context, event string, attrs ...slog.Attr) {
	l.emit(ctx, slog.LevelInfo, event, attrs...)
}
func (l *Logger) Warn(ctx context.Context, event string, attrs ...slog.Attr) {
	l.emit(ctx, slog.LevelWarn, event, attrs...)
}
func (l *Logger) Error(ctx context.Context, event string, attrs ...slog.Attr) {
	l.emit(ctx, slog.LevelError, event, attrs...)
}

func SQLAttributes(query string, args []any) []slog.Attr {
	return []slog.Attr{
		slog.String("query", MaskSecrets(MaskSQL(query))),
		slog.Int("parameter_count", len(args)),
		slog.String("parameters", "[REDACTED]"),
	}
}

func SafeError(err error) (message, sqlState string) {
	if err == nil {
		return "", ""
	}
	message = strings.TrimSpace(err.Error())
	message = MaskSecrets(MaskSQL(message))
	return message, ""
}
