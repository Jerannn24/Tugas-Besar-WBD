package auth

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type User struct {
	ID           uuid.UUID `json:"-"`
	Username     string    `json:"username"`
	HashPassword string    `json:"-"`
	AuthVersion  int64     `json:"-"`
}

type Repository interface {
	Create(ctx context.Context, user *User) error
	FindByUsername(ctx context.Context, username string) (*User, error)
}

type postgresRepository struct {
	db *pgxpool.Pool
}

func NewRepository(db *pgxpool.Pool) Repository {
	return &postgresRepository{db: db}
}

func (r *postgresRepository) Create(ctx context.Context, user *User) error {
	if user == nil {
		return ErrInvalidInput
	}

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	user.ID = uuid.New()
	user.AuthVersion = 1
	_, err := r.db.Exec(ctx, `
		INSERT INTO users (id, username, password, auth_version) 
		VALUES ($1, $2, $3, $4)`,
		user.ID, user.Username, user.HashPassword, user.AuthVersion)

	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return ErrEmailExists
		}
		return err
	}

	return nil
}

func (r *postgresRepository) FindByUsername(ctx context.Context, username string) (*User, error) {
	if username == "" {
		return nil, ErrInvalidInput
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	var user User
	err := r.db.QueryRow(ctx,
		`SELECT id, username, password, auth_version FROM users WHERE username = $1 AND is_active = true`,
		username).Scan(&user.ID, &user.Username, &user.HashPassword, &user.AuthVersion)

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrUserNotFound
		}
		return nil, err
	}

	return &user, nil
}
