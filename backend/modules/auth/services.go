package auth

import (
	"context"
	"errors"
	"time"
	"unicode"

	"golang.org/x/crypto/bcrypt"
)

var ErrInvalidInput = errors.New("invalid input")
var ErrEmailExists = errors.New("email already exists")
var ErrUserNotFound = errors.New("user not found")

type Service interface {
	Register(ctx context.Context, username, password string) (*User, error)
	Login(ctx context.Context, username, password string) (*LoginResult, error)
	Logout(ctx context.Context, sessionID string) error
	Authenticate(ctx context.Context, sessionID string) (*AuthenticatedSession, error)
	SessionConfig() SessionConfig
}

type service struct {
	repo          Repository
	sessions      *SessionRepository
	sessionConfig SessionConfig
}

func NewService(repo Repository, sessions *SessionRepository, sessionConfig SessionConfig) *service {
	return &service{
		repo:          repo,
		sessions:      sessions,
		sessionConfig: sessionConfig,
	}
}

func (s *service) Register(ctx context.Context, username, password string) (*User, error) {
	if username == "" || len(ValidatePassword(password)) > 0 {
		return nil, ErrInvalidInput
	}

	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}

	user := &User{
		Username:     username,
		HashPassword: string(hashedPassword),
	}

	if err = s.repo.Create(ctx, user); err != nil {
		return nil, err
	}

	return user, nil
}

func (s *service) Login(ctx context.Context, username, password string) (*LoginResult, error) {
	user, err := s.repo.FindByUsername(ctx, username)
	if err != nil {
		if errors.Is(err, ErrUserNotFound) {
			return nil, ErrUserNotFound
		}
		return nil, err
	}

	if err = bcrypt.CompareHashAndPassword([]byte(user.HashPassword), []byte(password)); err != nil {
		return nil, ErrUserNotFound
	}

	return s.createSession(ctx, user)
}

func (s *service) Logout(ctx context.Context, sessionID string) error {
	if sessionID == "" {
		return nil
	}

	hash, err := hashSessionID(sessionID)
	if err != nil {
		return nil
	}

	session, _, err := s.sessions.Find(ctx, hash, time.Now().UTC(), s.sessionConfig.IdleTimeout)
	if err != nil {
		return err
	}

	return s.sessions.Revoke(ctx, session.FamilyID, time.Now().UTC())
}

func (s *service) Authenticate(ctx context.Context, sessionID string) (*AuthenticatedSession, error) {
	return s.authenticate(ctx, sessionID)
}

func (s *service) SessionConfig() SessionConfig {
	return s.sessionConfig
}

func ValidatePassword(password string) []error {

	var (
		hasMinLen  = len(password) >= 12
		hasUpper   bool
		hasLower   bool
		hasNumber  bool
		hasSpecial bool
	)

	for _, char := range password {
		switch {
		case unicode.IsUpper(char):
			hasUpper = true
		case unicode.IsLower(char):
			hasLower = true
		case unicode.IsNumber(char):
			hasNumber = true
		case unicode.IsPunct(char) || unicode.IsSymbol(char):
			hasSpecial = true
		}
	}
	var Errors []error

	if !hasMinLen {
		Errors = append(Errors, errors.New("password must be at least 12 characters long"))
	}
	if !hasUpper {
		Errors = append(Errors, errors.New("password must contain at least one uppercase letter"))
	}
	if !hasLower {
		Errors = append(Errors, errors.New("password must contain at least one lowercase letter"))
	}
	if !hasNumber {
		Errors = append(Errors, errors.New("password must contain at least one number"))
	}
	if !hasSpecial {
		Errors = append(Errors, errors.New("password must contain at least one special character"))
	}

	return Errors
}
