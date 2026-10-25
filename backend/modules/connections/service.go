package connections

import (
	"context"
	"database/sql"

	"github.com/google/uuid"
)

type Service struct {
	repository *Repository
	pools      *PoolManager
}

func NewService(repository *Repository, pools *PoolManager) *Service {
	return &Service{repository: repository, pools: pools}
}

func (s *Service) List(ctx context.Context) ([]Summary, error) {
	return s.repository.List(ctx)
}

func (s *Service) Create(ctx context.Context, input Input) (Summary, error) {
	return s.repository.Create(ctx, input)
}

// Update and Delete invalidate pools only after the platform transaction has
// committed (the lifecycle hook does it), never before.
func (s *Service) Update(ctx context.Context, id uuid.UUID, input Input) (Summary, error) {
	item, err := s.repository.Update(ctx, id, input, s.pools)
	if err == nil {
		// Non-identity edits (e.g. rename) skip quiesce; viewer pool is still refreshed.
		s.pools.Invalidate(id)
	}
	return item, err
}

func (s *Service) Delete(ctx context.Context, id uuid.UUID) error {
	return s.repository.Delete(ctx, id, s.pools)
}

func (s *Service) Test(ctx context.Context, id uuid.UUID) error {
	return s.repository.Test(ctx, id)
}

func (s *Service) OpenTarget(ctx context.Context, id uuid.UUID) (*sql.DB, error) {
	return s.pools.Open(ctx, id)
}
