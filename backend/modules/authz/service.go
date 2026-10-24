package authz

import (
	"context"

	"github.com/google/uuid"
)

type service struct {
	repository *Repository
}

func NewService(repository *Repository) Authorizer {
	return &service{repository: repository}
}

func (s *service) ResolveActor(ctx context.Context, userID uuid.UUID) (Actor, error) {
	if userID == uuid.Nil {
		return Actor{}, ErrUnauthenticated
	}
	return s.repository.ResolveActor(ctx, userID)
}

func (s *service) EffectiveAccess(ctx context.Context, actorID, databaseID uuid.UUID) (Access, error) {
	if actorID == uuid.Nil {
		return AccessNone, ErrUnauthenticated
	}
	if databaseID == uuid.Nil {
		return AccessNone, ErrInvalidID
	}
	return s.repository.EffectiveAccess(ctx, actorID, databaseID)
}

func (s *service) RequireAccess(ctx context.Context, actorID, databaseID uuid.UUID, required Access) error {
	if required != AccessView && required != AccessEdit {
		return ErrInvalidID
	}
	actual, err := s.EffectiveAccess(ctx, actorID, databaseID)
	if err != nil {
		return err
	}
	return requireEffectiveAccess(actual, required)
}

func (s *service) RequireAdmin(actor Actor) error {
	if actor.ID == uuid.Nil {
		return ErrUnauthenticated
	}
	if !actor.IsAdmin {
		return ErrForbidden
	}
	return nil
}

func requireEffectiveAccess(actual, required Access) error {
	if required == AccessEdit && actual != AccessEdit {
		return ErrForbidden
	}
	if required == AccessView && actual == AccessNone {
		return ErrForbidden
	}
	return nil
}
