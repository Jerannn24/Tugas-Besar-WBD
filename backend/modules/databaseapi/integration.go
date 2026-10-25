package databaseapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"time"

	"github.com/Jerannn24/Tugas-Besar-WBD.git/drafts"
	"github.com/Jerannn24/Tugas-Besar-WBD.git/history"
	"github.com/Jerannn24/Tugas-Besar-WBD.git/migration"
	"github.com/Jerannn24/Tugas-Besar-WBD.git/modules/authz"
	"github.com/Jerannn24/Tugas-Besar-WBD.git/modules/connections"
	"github.com/Jerannn24/Tugas-Besar-WBD.git/sqlpolicy"
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
		Timeout: 10 * time.Second,
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
		Acquire:   acquireLease(pools),
		Preflight: migration.NewPreflight(policy),
		Timeout:   30 * time.Second,
	})
	if err != nil {
		return nil, err
	}
	if err := migrationHandler.Register(mux); err != nil {
		return nil, err
	}
	api := adaptor.HTTPHandler(jsonFallback(mux))
	router.All("/databases", api)
	router.All("/databases/*", api)
	stop := startReconciler(scanUnresolved(platform, 50), migrationHandler.Reconcile, 30*time.Second)
	return stop, nil
}

func acquireLease(pools *connections.PoolManager) func(context.Context, string) (*migration.TargetLease, error) {
	return func(ctx context.Context, id string) (*migration.TargetLease, error) {
		databaseID, err := uuid.Parse(id)
		if err != nil {
			return nil, err
		}
		lease, err := pools.Acquire(ctx, databaseID)
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

func OpenPlatform() (*sql.DB, error) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return nil, errors.New("DATABASE_URL is required")
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	db := stdlib.OpenDB(*config.ConnConfig)
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	return db, nil
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
