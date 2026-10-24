package authz

import (
	"context"

	"github.com/google/uuid"
)

type Access string

const (
	AccessNone Access = "none"
	AccessView Access = "view"
	AccessEdit Access = "edit"
)

type UserResolver interface {
	ResolveActor(ctx context.Context, userID uuid.UUID) (Actor, error)
}

type Authorizer interface {
	ResolveActor(ctx context.Context, userID uuid.UUID) (Actor, error)
	EffectiveAccess(ctx context.Context, actorID, databaseID uuid.UUID) (Access, error)
	RequireAccess(ctx context.Context, actorID, databaseID uuid.UUID, required Access) error
	RequireAdmin(actor Actor) error
}
