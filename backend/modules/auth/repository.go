package auth

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

type User struct {
	ID           uuid.UUID `json:"-"`
	Username     string    `json:"username"`
	HashPassword string    `json:"-"`
	AuthVersion  int64     `json:"-"`
	IsAdmin      bool      `json:"-"`
}

type Repository interface {
	Create(ctx context.Context, user *User) error
	FindByUsername(ctx context.Context, username string) (*User, error)
}

type AdminBootstrapper interface {
	BootstrapAdmin(ctx context.Context, username, password string) error
}

type postgresRepository struct {
	db *pgxpool.Pool
}

func NewRepository(db *pgxpool.Pool) Repository {
	return &postgresRepository{db: db}
}

func NewAdminRepository(db *pgxpool.Pool) AdminBootstrapper {
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
		INSERT INTO app.users (id, username, password_hash, auth_version)
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
		`SELECT id, username, password_hash, auth_version, is_admin
		 FROM app.users
		 WHERE username = $1 AND is_active = true`,
		username).Scan(&user.ID, &user.Username, &user.HashPassword, &user.AuthVersion, &user.IsAdmin)

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrUserNotFound
		}
		return nil, err
	}

	return &user, nil
}

func (r *postgresRepository) BootstrapAdmin(ctx context.Context, username, password string) error {
	if username == "" || len(ValidatePassword(password)) > 0 {
		return ErrInvalidInput
	}

	passwordHash, err := bcrypt.GenerateFromPassword(
		[]byte(password),
		bcrypt.DefaultCost,
	)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	_, err = r.db.Exec(ctx, `
		INSERT INTO app.users (id, username, password_hash, is_admin, is_active, auth_version)
		VALUES ($1, $2, $3, true, true, 1)
		ON CONFLICT (username) DO NOTHING
	`, uuid.New(), username, string(passwordHash))
	if err != nil {
		return err
	}

	var isAdmin bool
	if err := r.db.QueryRow(ctx,
		`SELECT is_admin FROM app.users WHERE username = $1`,
		username,
	).Scan(&isAdmin); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrUserNotFound
		}
		return err
	}
	if !isAdmin {
		return errors.New("bootstrap username belongs to a non-admin user")
	}
	return nil
}
