package databaseapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/Jerannn24/Tugas-Besar-WBD.git/drafts"
	"github.com/Jerannn24/Tugas-Besar-WBD.git/executioncontract"
	"github.com/Jerannn24/Tugas-Besar-WBD.git/history"
	"github.com/Jerannn24/Tugas-Besar-WBD.git/migration"
	"github.com/Jerannn24/Tugas-Besar-WBD.git/modules/authz"
	"github.com/Jerannn24/Tugas-Besar-WBD.git/modules/connections"
	"github.com/Jerannn24/Tugas-Besar-WBD.git/sqlpolicy"
	"github.com/Jerannn24/Tugas-Besar-WBD.git/telemetry"
	"github.com/Jerannn24/Tugas-Besar-WBD.git/viewer"
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/adaptor"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
)

// Register memasang database API dan menjalankan reconciler execution.
// Fungsi stop yang dikembalikan wajib dipanggil saat shutdown sebelum pool ditutup.
func Register(router fiber.Router, platform *sql.DB, authorizer authz.Authorizer, pools *connections.PoolManager) (func(), error) {
	return register(router, platform, authorizer, pools, telemetry.DefaultLogger())
}

func register(router fiber.Router, platform *sql.DB, authorizer authz.Authorizer, pools *connections.PoolManager, logger *telemetry.Logger) (func(), error) {
	if platform == nil || authorizer == nil || pools == nil {
		return nil, errors.New("database API dependencies are required")
	}
	actor := func(r *http.Request) (viewer.Actor, error) {
		resolved, ok := authz.ActorFromRequest(r)
		if !ok {
			return viewer.Actor{}, httpError{status: http.StatusUnauthorized, err: authz.ErrUnauthenticated}
		}
		return viewer.Actor{ID: resolved.ID.String()}, nil
	}
	authorizeView := func(ctx context.Context, actor viewer.Actor, databaseID string, required viewer.Access) error {
		actorID, err := uuid.Parse(actor.ID)
		if err != nil {
			return httpError{status: http.StatusUnauthorized, err: authz.ErrUnauthenticated}
		}
		targetID, err := uuid.Parse(databaseID)
		if err != nil {
			return httpError{status: http.StatusNotFound, err: authz.ErrInvalidID}
		}
		access := authz.AccessView
		if required == viewer.Edit {
			access = authz.AccessEdit
		}
		return mapAuthzError(authorizer.RequireAccess(ctx, actorID, targetID, access))
	}
	viewerHandler, err := viewer.New(viewer.Dependencies{
		Platform:  platform,
		Actor:     actor,
		Authorize: authorizeView,
		OpenTarget: func(ctx context.Context, id string) (*sql.DB, error) {
			targetID, err := uuid.Parse(id)
			if err != nil {
				return nil, err
			}
			return pools.Open(ctx, targetID)
		},
		Recorder: viewerRecorder{logger},
		Timeout:  10 * time.Second,
	})
	if err != nil {
		return nil, err
	}
	draftHandler, err := drafts.New(drafts.Dependencies{
		Platform: platform,
		Actor: func(r *http.Request) (drafts.Actor, error) {
			a, err := actor(r)
			return drafts.Actor{ID: a.ID}, err
		},
		Authorize: func(ctx context.Context, a drafts.Actor, id string) error {
			return authorizeView(ctx, viewer.Actor{ID: a.ID}, id, viewer.Edit)
		},
		CheckCSRF: authz.CheckCSRF,
		Timeout:   10 * time.Second,
	})
	if err != nil {
		return nil, err
	}
	historyHandler, err := history.New(history.Dependencies{
		Platform: platform,
		Actor: func(r *http.Request) (history.Actor, error) {
			a, err := actor(r)
			return history.Actor{ID: a.ID}, err
		},
		Authorize: func(ctx context.Context, a history.Actor, id string) error {
			return authorizeView(ctx, viewer.Actor{ID: a.ID}, id, viewer.View)
		},
		Timeout: 10 * time.Second,
	})
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	if err := viewerHandler.Register(mux); err != nil {
		return nil, err
	}
	if err := draftHandler.Register(mux); err != nil {
		return nil, err
	}
	if err := historyHandler.Register(mux); err != nil {
		return nil, err
	}
	policy, err := sqlpolicy.New("managed")
	if err != nil {
		return nil, err
	}
	migrationHandler, err := migration.New(migration.Dependencies{
		Platform: platform,
		Actor: func(r *http.Request) (migration.Actor, error) {
			a, err := actor(r)
			return migration.Actor{ID: a.ID}, err
		},
		Authorize: func(ctx context.Context, a migration.Actor, id string) error {
			return authorizeView(ctx, viewer.Actor{ID: a.ID}, id, viewer.Edit)
		},
		CheckCSRF: authz.CheckCSRF,
		Acquire:   AcquireLease(pools),
		Preflight: migration.NewPreflight(policy),
		Timeout:   30 * time.Second,
	})
	if err != nil {
		return nil, err
	}
	handler := jsonFallback(mux)
	api := func(c *fiber.Ctx) error {
		trace, ok := telemetry.TraceFromContext(c.UserContext())
		return adaptor.HTTPHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if ok {
				r = r.WithContext(telemetry.WithTrace(r.Context(), trace))
			}
			handler.ServeHTTP(w, r)
		}))(c)
	}
	router.All("/databases", api)
	router.All("/databases/*", api)
	stop := startReconciler(scanUnresolved(platform, 50), migrationHandler.Reconcile, 30*time.Second)
	return stop, nil
}

type viewerRecorder struct{ logger *telemetry.Logger }

func (r viewerRecorder) attrs(e viewer.Event) []slog.Attr {
	attrs := []slog.Attr{
		slog.String("operation", e.Operation),
		slog.String("actor_id", e.Actor),
	}
	if e.DatabaseID != "" {
		attrs = append(attrs, slog.String("database_id", e.DatabaseID))
	}
	if e.Query != "" {
		attrs = append(attrs, telemetry.SQLAttributes(e.Query, e.Params)...)
		attrs = append(attrs, slog.String("params", "[REDACTED]"))
	}
	return attrs
}

func (r viewerRecorder) Start(ctx context.Context, e viewer.Event) {
	attrs := r.attrs(e)
	if _, ok := telemetry.TraceFromContext(ctx); !ok {
		attrs = append(attrs, slog.String("correlation_id", e.CorrelationID))
	}
	r.logger.Info(ctx, e.Name+".start", append(attrs, slog.String("status", "started"))...)
}

func (r viewerRecorder) Finish(ctx context.Context, e viewer.Event, result viewer.Result) {
	attrs := r.attrs(e)
	if _, ok := telemetry.TraceFromContext(ctx); !ok {
		attrs = append(attrs, slog.String("correlation_id", e.CorrelationID))
	}
	attrs = append(attrs, slog.String("status", result.Status),
		slog.Float64("duration_ms", float64(result.Duration.Microseconds())/1000),
		slog.Int("rows", result.Rows))
	if result.SQLState != "" {
		attrs = append(attrs, slog.String("sql_state", result.SQLState))
	}
	if result.Error != "" {
		attrs = append(attrs, slog.String("error", telemetry.MaskSecrets(telemetry.MaskSQL(result.Error))))
	}
	if result.Status == "error" {
		r.logger.Error(ctx, e.Name+".finish", attrs...)
	} else {
		r.logger.Info(ctx, e.Name+".finish", attrs...)
	}
}

// AcquireLease adapts the shared role-aware pool manager to the migration executor.
func AcquireLease(pools *connections.PoolManager) func(context.Context, string) (*migration.TargetLease, error) {
	return func(ctx context.Context, id string) (*migration.TargetLease, error) {
		databaseID, err := uuid.Parse(id)
		if err != nil {
			return nil, err
		}
		started := time.Now()
		lease, err := pools.Acquire(ctx, databaseID)
		result := executioncontract.ValueSuccess
		if err != nil {
			result = executioncontract.ValueFailure
		}
		telemetry.DefaultRegistry().Observe(ctx, executioncontract.HistogramPoolWait, time.Since(started).Seconds(),
			executioncontract.MetricLabels{executioncontract.LabelResult: result})
		if err != nil {
			return nil, err
		}
		return &migration.TargetLease{Executor: lease.Executor, Preparer: lease.Preparer, Release: lease.Release}, nil
	}
}

type httpError struct {
	status int
	err    error
}

func (e httpError) Error() string   { return e.err.Error() }
func (e httpError) Unwrap() error   { return e.err }
func (e httpError) StatusCode() int { return e.status }

func OpenPlatform(pool *pgxpool.Pool) *sql.DB {
	db := stdlib.OpenDB(*pool.Config().ConnConfig)
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	return db
}

func mapAuthzError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, authz.ErrUnauthenticated):
		return httpError{status: http.StatusUnauthorized, err: err}
	case errors.Is(err, authz.ErrForbidden):
		return httpError{status: http.StatusForbidden, err: err}
	case errors.Is(err, authz.ErrNotFound):
		return httpError{status: http.StatusNotFound, err: err}
	default:
		return err
	}
}

type statusRecorder struct {
	header http.Header
	status int
}

func (s *statusRecorder) Header() http.Header         { return s.header }
func (s *statusRecorder) Write(b []byte) (int, error) { return len(b), nil }
func (s *statusRecorder) WriteHeader(status int)      { s.status = status }

// jsonFallback replaces the mux's plain-text 404/405 with the JSON error contract.
func jsonFallback(mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, pattern := mux.Handler(r); pattern != "" {
			mux.ServeHTTP(w, r)
			return
		}
		rec := &statusRecorder{header: http.Header{}, status: http.StatusOK}
		mux.ServeHTTP(rec, r)
		if rec.status != http.StatusNotFound && rec.status != http.StatusMethodNotAllowed {
			mux.ServeHTTP(w, r)
			return
		}
		code, message := "NOT_FOUND", "Endpoint tidak ditemukan"
		status := http.StatusNotFound
		if rec.status == http.StatusMethodNotAllowed {
			status, code, message = http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "Method tidak diizinkan"
		}
		if allow := rec.header.Get("Allow"); allow != "" {
			w.Header().Set("Allow", allow)
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"code":         code,
				"message":      message,
				"field_errors": map[string]string{},
				"retryable":    false,
			},
		})
	})
}
