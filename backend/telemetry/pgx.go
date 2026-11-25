package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/Jerannn24/Tugas-Besar-WBD.git/executioncontract"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type queryStartKey struct{}
type queryStart struct {
	at    time.Time
	query string
	args  []any
	trace Trace
}
type QueryTracer struct{ Logger *Logger }

func (t *QueryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	ctx, child := ChildTrace(ctx)
	started := queryStart{at: time.Now(), query: data.SQL, args: data.Args, trace: child}
	ctx = context.WithValue(ctx, queryStartKey{}, started)
	if t != nil && t.Logger != nil {
		t.Logger.Info(ctx, sqlEventName(data.SQL, "started"), SQLAttributes(data.SQL, data.Args)...)
	}
	return ctx
}

func (t *QueryTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	started, ok := ctx.Value(queryStartKey{}).(queryStart)
	if !ok {
		return
	}
	attrs := SQLAttributes(started.query, started.args)
	attrs = append(attrs, slogDuration(time.Since(started.at)), slogInt("rows_affected", data.CommandTag.RowsAffected()))
	failed := data.Err != nil
	if data.Err != nil {
		msg, state := SafeError(data.Err)
		var pgErr *pgconn.PgError
		if errors.As(data.Err, &pgErr) {
			state = pgErr.Code
		}
		attrs = append(attrs, slogString("error", msg))
		if state != "" {
			attrs = append(attrs, slogString("sql_state", state))
		}
		if t != nil && t.Logger != nil {
			t.Logger.Error(ctx, sqlEventName(started.query, "failed"), attrs...)
		}
	} else if t != nil && t.Logger != nil {
		t.Logger.Info(ctx, sqlEventName(started.query, "completed"), attrs...)
	}
	ExportSpan(context.Background(), Span{TraceID: started.trace.TraceID, SpanID: started.trace.SpanID, ParentSpanID: started.trace.ParentSpanID, Name: "db.sql.query", Kind: 3, StartedAt: started.at, EndedAt: time.Now(), Failed: failed, Attributes: map[string]string{"db.system": "postgresql", "db.statement": MaskSQL(started.query), "db.operation.parameter_count": fmt.Sprint(len(started.args)), "correlation_id": started.trace.CorrelationID}})
}

func slogDuration(d time.Duration) slog.Attr {
	return slog.Float64("duration_ms", float64(d)/float64(time.Millisecond))
}
func slogInt(k string, v int64) slog.Attr { return slog.Int64(k, v) }
func slogString(k, v string) slog.Attr    { return slog.String(k, v) }

var _ pgx.QueryTracer = (*QueryTracer)(nil)

func sqlEventName(query, phase string) string {
	statement := strings.ToUpper(strings.TrimSpace(query))
	statement = strings.TrimLeft(statement, "(")
	if strings.HasPrefix(statement, "COMMIT") || strings.HasPrefix(statement, "END") {
		return "sql.transaction.commit." + phase
	}
	if strings.HasPrefix(statement, "ROLLBACK") || strings.HasPrefix(statement, "ABORT") {
		return "sql.transaction.rollback." + phase
	}
	return "sql.query." + phase
}

// StartPoolMetrics periodically exports the platform pgx pool's active and
// idle connection counts. The caller owns and must invoke the returned stop.
func StartPoolMetrics(ctx context.Context, pool *pgxpool.Pool, registry *Registry, interval time.Duration) func() {
	if interval <= 0 {
		interval = 15 * time.Second
	}
	if registry == nil {
		registry = DefaultRegistry()
	}
	workerCtx, cancel := context.WithCancel(ctx)
	collect := func() {
		if pool == nil {
			return
		}
		stats := pool.Stat()
		registry.Set(workerCtx, executioncontract.GaugePoolConnections, float64(stats.AcquiredConns()), executioncontract.MetricLabels{executioncontract.LabelState: executioncontract.ValueActive})
		registry.Set(workerCtx, executioncontract.GaugePoolConnections, float64(stats.IdleConns()), executioncontract.MetricLabels{executioncontract.LabelState: executioncontract.ValueIdle})
	}
	collect()
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-workerCtx.Done():
				return
			case <-ticker.C:
				collect()
			}
		}
	}()
	return cancel
}
