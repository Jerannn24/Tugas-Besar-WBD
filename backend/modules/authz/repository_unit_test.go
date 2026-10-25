package authz

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type queryRowFunc func(context.Context, string, ...any) pgx.Row

func (f queryRowFunc) QueryRow(ctx context.Context, q string, args ...any) pgx.Row {
	return f(ctx, q, args...)
}

type scanFunc func(...any) error

func (f scanFunc) Scan(dest ...any) error { return f(dest...) }

func TestEffectiveAccessRepositoryRankAndErrors(t *testing.T) {
	actorID, databaseID := uuid.New(), uuid.New()
	queryErr := errors.New("database unavailable")
	for _, tc := range []struct {
		name     string
		exists   bool
		rank     int
		queryErr error
		want     Access
		wantErr  error
	}{
		{"missing database", false, 2, nil, AccessNone, ErrNotFound}, {"no grant", true, 0, nil, AccessNone, nil}, {"view", true, 1, nil, AccessView, nil}, {"edit", true, 2, nil, AccessEdit, nil}, {"unknown rank fails closed", true, 3, nil, AccessNone, nil}, {"query failure", false, 0, queryErr, AccessNone, queryErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &Repository{db: queryRowFunc(func(ctx context.Context, _ string, args ...any) pgx.Row {
				if len(args) != 2 || args[0] != actorID || args[1] != databaseID {
					t.Fatalf("wrong access lookup arguments: %v", args)
				}
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) > 10*time.Second {
					t.Fatal("lookup has no bounded context")
				}
				return scanFunc(func(dest ...any) error {
					if tc.queryErr != nil {
						return tc.queryErr
					}
					*dest[0].(*bool) = tc.exists
					*dest[1].(*int) = tc.rank
					return nil
				})
			})}
			got, err := repo.EffectiveAccess(context.Background(), actorID, databaseID)
			if got != tc.want || !errors.Is(err, tc.wantErr) {
				t.Fatalf("got %s/%v, want %s/%v", got, err, tc.want, tc.wantErr)
			}
		})
	}
}

func TestResolveActorRepositoryActiveMissingAndFailure(t *testing.T) {
	id := uuid.New()
	queryErr := errors.New("query failure")
	for _, tc := range []struct {
		name              string
		admin             bool
		queryErr, wantErr error
	}{{"regular active", false, nil, nil}, {"active admin", true, nil, nil}, {"missing or inactive", false, pgx.ErrNoRows, ErrUnauthenticated}, {"query failure", false, queryErr, queryErr}} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &Repository{db: queryRowFunc(func(_ context.Context, _ string, args ...any) pgx.Row {
				if len(args) != 1 || args[0] != id {
					t.Fatal("wrong actor lookup")
				}
				return scanFunc(func(dest ...any) error {
					if tc.queryErr != nil {
						return tc.queryErr
					}
					*dest[0].(*uuid.UUID) = id
					*dest[1].(*bool) = tc.admin
					return nil
				})
			})}
			actor, err := repo.ResolveActor(context.Background(), id)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("got %v, want %v", err, tc.wantErr)
			}
			if tc.wantErr == nil {
				if actor.ID != id || actor.IsAdmin != tc.admin {
					t.Fatalf("wrong actor: %+v", actor)
				}
			} else if actor != (Actor{}) {
				t.Fatalf("returned actor on error: %+v", actor)
			}
		})
	}
}
