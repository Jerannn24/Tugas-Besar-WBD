package connections

import (
	"context"
	"database/sql"

	"github.com/google/uuid"
)

type serviceRepository interface {
	List(context.Context) ([]Summary, error)
	Create(context.Context, Input) (Summary, error)
	Update(context.Context, uuid.UUID, Input) (Summary, error)
	Delete(context.Context, uuid.UUID) error
	Test(context.Context, uuid.UUID) error
	OpenTarget(context.Context, uuid.UUID) (*sql.DB, error)
}

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

func (s *Service) Update(ctx context.Context, id uuid.UUID, input Input) (Summary, error) {
	item, err := s.repository.Update(ctx, id, input)
	if err == nil {
		s.pools.Invalidate(id)
	}
	return item, err
}

func (s *Service) Delete(ctx context.Context, id uuid.UUID) error {
	err := s.repository.Delete(ctx, id)
	if err == nil {
		s.pools.Invalidate(id)
	}
	return err
}

func (s *Service) Test(ctx context.Context, id uuid.UUID) error {
	return s.repository.Test(ctx, id)
}

func (s *Service) OpenTarget(ctx context.Context, id uuid.UUID) (*sql.DB, error) {
	return s.pools.Open(ctx, id)
}
