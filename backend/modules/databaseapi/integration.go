package databaseapi

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/Jerannn24/Tugas-Besar-WBD.git/drafts"
	"github.com/Jerannn24/Tugas-Besar-WBD.git/modules/auth"
	"github.com/Jerannn24/Tugas-Besar-WBD.git/modules/authz"
	"github.com/Jerannn24/Tugas-Besar-WBD.git/modules/connections"
	"github.com/Jerannn24/Tugas-Besar-WBD.git/viewer"
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/adaptor"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
)

func Register(app *fiber.App, platform *sql.DB, sessionService auth.Service, authorizer authz.Authorizer, pools *connections.PoolManager) error {
	if platform == nil || sessionService == nil || authorizer == nil || pools == nil {
		return errors.New("database API dependencies are required")
	}
	actor := func(r *http.Request) (viewer.Actor, error) {
		session, err := sessionService.Authenticate(r.Context(), cookieValue(r, auth.SessionCookieName))
		if err != nil {
			return viewer.Actor{}, err
		}
		resolved, err := authorizer.ResolveActor(r.Context(), session.Session.UserID)
		if err != nil {
			return viewer.Actor{}, err
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
		if err := authorizer.RequireAccess(ctx, actorID, targetID, access); err != nil {
			switch {
			case errors.Is(err, authz.ErrUnauthenticated):
				return httpError{status: http.StatusUnauthorized, err: err}
			case errors.Is(err, authz.ErrForbidden):
				return httpError{status: http.StatusForbidden, err: err}
			default:
				return err
			}
		}
		return nil
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
	app.All("/api/databases", adaptor.HTTPHandler(viewerMux))
	app.All("/api/databases/*", adaptor.HTTPHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/drafts") {
			draftMux.ServeHTTP(w, r)
			return
		}
		viewerMux.ServeHTTP(w, r)
	})))
	return nil
}

func cookieValue(r *http.Request, name string) string {
	cookie, err := r.Cookie(name)
	if err != nil {
		return ""
	}
	return cookie.Value
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
