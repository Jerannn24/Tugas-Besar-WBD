package viewer

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/google/uuid"
)

type Event struct {
	Name          string
	Operation     string
	Actor         string
	DatabaseID    string
	CorrelationID string
	Query         string
	Params        []any
}

type Result struct {
	Status   string
	Duration time.Duration
	Rows     int
	SQLState string
	Error    string
}

type Recorder interface {
	Start(context.Context, Event)
	Finish(context.Context, Event, Result)
}

type slogRecorder struct{ log *slog.Logger }

func NewSlogRecorder(l *slog.Logger) Recorder {
	if l == nil {
		l = NewJSONLogger(os.Stdout)
	}
	return slogRecorder{l}
}

func NewJSONLogger(w io.Writer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
		if a.Key == slog.TimeKey && a.Value.Kind() == slog.KindTime {
			a.Value = slog.TimeValue(a.Value.Time().UTC())
		}
		return a
	}}))
}

func (r slogRecorder) Start(ctx context.Context, e Event) {
	name := e.Name + ".start"
	r.log.LogAttrs(ctx, slog.LevelInfo, name, append(eventAttrs(name, e), slog.String("status", "started"))...)
}

func (r slogRecorder) Finish(ctx context.Context, e Event, res Result) {
	name := e.Name + ".finish"
	level := slog.LevelInfo
	if res.Status == "error" {
		level = slog.LevelError
	}
	attrs := append(eventAttrs(name, e),
		slog.String("status", res.Status),
		slog.Float64("duration_ms", float64(res.Duration.Microseconds())/1000),
		slog.Int("rows", res.Rows),
	)
	if res.SQLState != "" {
		attrs = append(attrs, slog.String("sql_state", res.SQLState))
	}
	if res.Error != "" {
		attrs = append(attrs, slog.String("error", res.Error))
	}
	r.log.LogAttrs(ctx, level, name, attrs...)
}

func eventAttrs(name string, e Event) []slog.Attr {
	attrs := []slog.Attr{
		slog.String("event", name),
		slog.String("operation", e.Operation),
		slog.String("actor_id", e.Actor),
		slog.String("correlation_id", e.CorrelationID),
	}
	if e.DatabaseID != "" {
		attrs = append(attrs, slog.String("database_id", e.DatabaseID))
	}
	if e.Query != "" {
		attrs = append(attrs, slog.String("query", e.Query), slog.Any("params", e.Params))
	}
	return attrs
}

type scope struct{ actor, databaseID, correlationID string }
type scopeKey struct{}

func withScope(ctx context.Context, sc scope) context.Context {
	return context.WithValue(ctx, scopeKey{}, sc)
}

func correlationID(r *http.Request) string {
	if id, err := uuid.Parse(r.Header.Get("X-Request-ID")); err == nil {
		return id.String()
	}
	return uuid.NewString()
}

var errScan = errors.New("scan")

func (h *Handler) track(ctx context.Context, name, op, query string, args ...any) func(rows int, err error) {
	sc, _ := ctx.Value(scopeKey{}).(scope)
	params := args
	if params == nil {
		params = []any{}
	}
	e := Event{Name: name, Operation: op, Actor: sc.actor, DatabaseID: sc.databaseID, CorrelationID: sc.correlationID, Query: query, Params: params}
	h.d.Recorder.Start(ctx, e)
	start := time.Now()
	done := false
	return func(rows int, err error) {
		if done {
			return
		}
		done = true
		res := Result{Status: "ok", Duration: time.Since(start), Rows: rows}
		if err != nil {
			res.Status, res.Error, res.SQLState = classify(err)
		}
		h.d.Recorder.Finish(ctx, e, res)
	}
}

func classify(err error) (status, category, state string) {
	var s interface{ SQLState() string }
	if errors.As(err, &s) {
		state = s.SQLState()
	}
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "no_rows", "", state
	case errors.Is(err, context.DeadlineExceeded):
		return "error", "timeout", state
	case errors.Is(err, context.Canceled):
		return "error", "canceled", state
	case errors.Is(err, errScan):
		return "error", "scan_failed", state
	}
	return "error", "query_failed", state
}

func (h *Handler) rollback(ctx context.Context, tx *sql.Tx) {
	end := h.track(ctx, "sql.rollback", "tx.rollback", "")
	err := tx.Rollback()
	if errors.Is(err, sql.ErrTxDone) {
		err = nil
	}
	end(0, err)
}
