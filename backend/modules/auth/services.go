package auth

import (
	"context"
	"errors"
)

var ErrInvalidInput = errors.New("invalid input")
var ErrEmailExists = errors.New("email already exists")
var ErrUserNotFound = errors.New("user not found")

type Service interface {
	Register(ctx context.Context, username, password string) (*User, error)
	Login(ctx context.Context, username, password string) (*User, string, error)
}

type service struct {
	repo      Repository
	jwtSecret []byte
}

func NewService(repo Repository, jwtSecret string) Service {
	return &service{
		repo:      repo,
		jwtSecret: []byte(jwtSecret),
	}
}

func (s *service) Register(ctx context.Context, username, password string) (*User, error) {
	// Implementasi logika untuk mendaftarkan pengguna baru
	return nil, nil
}

func (s *service) Login(ctx context.Context, username, password string) (*User, string, error) {
	// Implementasi logika untuk login pengguna
	return nil, "", nil
}
