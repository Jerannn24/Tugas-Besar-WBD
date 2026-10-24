package authz

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Repository struct {
	db *pgxpool.Pool
}

func NewRepository(db *pgxpool.Pool) *Repository {
	return &Repository{db: db}
}

func (r *Repository) ResolveActor(ctx context.Context, userID uuid.UUID) (Actor, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	var actor Actor
	err := r.db.QueryRow(ctx, `
		SELECT id, is_admin
		FROM app.users
		WHERE id = $1 AND is_active = true
	`, userID).Scan(&actor.ID, &actor.IsAdmin)
	if errors.Is(err, pgx.ErrNoRows) {
		return Actor{}, ErrUnauthenticated
	}
	if err != nil {
		return Actor{}, err
	}
	return actor, nil
}

func (r *Repository) EffectiveAccess(ctx context.Context, actorID, databaseID uuid.UUID) (Access, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	var rank int
	err := r.db.QueryRow(ctx, `
		SELECT COALESCE(MAX(
			CASE g.access
				WHEN 'edit' THEN 2
				WHEN 'view' THEN 1
				ELSE 0
			END
		), 0)
		FROM app.group_memberships m
		JOIN app.group_database_grants g ON g.group_id = m.group_id
		JOIN app.database_connections d ON d.id = g.database_id
		WHERE m.user_id = $1
		  AND g.database_id = $2
		  AND d.deleted_at IS NULL
	`, actorID, databaseID).Scan(&rank)
	if err != nil {
		return AccessNone, err
	}

	switch rank {
	case 2:
		return AccessEdit, nil
	case 1:
		return AccessView, nil
	default:
		return AccessNone, nil
	}
}
