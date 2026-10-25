package databaseapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/Jerannn24/Tugas-Besar-WBD.git/drafts"
	"github.com/Jerannn24/Tugas-Besar-WBD.git/modules/authz"
	"github.com/Jerannn24/Tugas-Besar-WBD.git/modules/connections"
	"github.com/Jerannn24/Tugas-Besar-WBD.git/viewer"
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/adaptor"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
)

func Register(router fiber.Router, platform *sql.DB, authorizer authz.Authorizer, pools *connections.PoolManager) error {
	if platform == nil || authorizer == nil || pools == nil {
		return errors.New("database API dependencies are required")
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
		return err
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
		return err
	}
	viewerMux := http.NewServeMux()
	if err := viewerHandler.Register(viewerMux); err != nil {
		return err
	}
	draftMux := http.NewServeMux()
	if err := draftHandler.Register(draftMux); err != nil {
		return err
	}
	viewerAPI := jsonFallback(viewerMux)
	draftAPI := jsonFallback(draftMux)
	router.All("/databases", adaptor.HTTPHandler(viewerAPI))
	router.All("/databases/*", adaptor.HTTPHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/drafts") {
			draftAPI.ServeHTTP(w, r)
			return
		}
		viewerAPI.ServeHTTP(w, r)
	})))
	return nil
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
