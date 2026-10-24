package admin

import (
	"context"

	"github.com/google/uuid"
)

type serviceRepository interface {
	ListUsers(context.Context) ([]User, error)
	CreateUser(context.Context, string, string, bool) (User, error)
	UpdateUser(context.Context, uuid.UUID, *string, *string, *bool, *bool) (User, error)
	ListGroups(context.Context) ([]Group, error)
	CreateGroup(context.Context, string) (Group, error)
	UpdateGroup(context.Context, uuid.UUID, string) (Group, error)
	DeleteGroup(context.Context, uuid.UUID) error
	Members(context.Context, uuid.UUID) ([]uuid.UUID, error)
	ReplaceMembers(context.Context, uuid.UUID, []uuid.UUID) ([]uuid.UUID, error)
	Grants(context.Context, uuid.UUID) ([]Grant, error)
	ReplaceGrants(context.Context, uuid.UUID, []Grant) ([]Grant, error)
}

type Service struct {
	repository serviceRepository
}

func NewService(repository *Repository) *Service {
	return &Service{repository: repository}
}

func (s *Service) ListUsers(ctx context.Context) ([]User, error) {
	return s.repository.ListUsers(ctx)
}

func (s *Service) CreateUser(ctx context.Context, username, password string, isAdmin bool) (User, error) {
	return s.repository.CreateUser(ctx, username, password, isAdmin)
}

func (s *Service) UpdateUser(ctx context.Context, id uuid.UUID, username, password *string, isAdmin, isActive *bool) (User, error) {
	return s.repository.UpdateUser(ctx, id, username, password, isAdmin, isActive)
}

func (s *Service) ListGroups(ctx context.Context) ([]Group, error) {
	return s.repository.ListGroups(ctx)
}

func (s *Service) CreateGroup(ctx context.Context, name string) (Group, error) {
	return s.repository.CreateGroup(ctx, name)
}

func (s *Service) UpdateGroup(ctx context.Context, id uuid.UUID, name string) (Group, error) {
	return s.repository.UpdateGroup(ctx, id, name)
}

func (s *Service) DeleteGroup(ctx context.Context, id uuid.UUID) error {
	return s.repository.DeleteGroup(ctx, id)
}

func (s *Service) Members(ctx context.Context, id uuid.UUID) ([]uuid.UUID, error) {
	return s.repository.Members(ctx, id)
}

func (s *Service) ReplaceMembers(ctx context.Context, id uuid.UUID, userIDs []uuid.UUID) ([]uuid.UUID, error) {
	return s.repository.ReplaceMembers(ctx, id, userIDs)
}

func (s *Service) Grants(ctx context.Context, id uuid.UUID) ([]Grant, error) {
	return s.repository.Grants(ctx, id)
}

func (s *Service) ReplaceGrants(ctx context.Context, id uuid.UUID, grants []Grant) ([]Grant, error) {
	return s.repository.ReplaceGrants(ctx, id, grants)
}
