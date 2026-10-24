package connections

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"database/sql"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
)

var (
	ErrInvalidInput = errors.New("invalid input")
	ErrNotFound     = errors.New("not found")
	ErrConflict     = errors.New("conflict")
)

type Summary struct {
	ID           uuid.UUID `json:"id"`
	Name         string    `json:"name"`
	Host         string    `json:"host"`
	Port         int       `json:"port"`
	DatabaseName string    `json:"database_name"`
	Username     string    `json:"username"`
	SSLMode      string    `json:"ssl_mode"`
}

type Input struct {
	Name         string `json:"name"`
	Host         string `json:"host"`
	Port         int    `json:"port"`
	DatabaseName string `json:"database_name"`
	Username     string `json:"username"`
	Password     string `json:"password"`
	SSLMode      string `json:"ssl_mode"`
}

type Repository struct {
	db  *pgxpool.Pool
	key []byte
}

func NewRepository(db *pgxpool.Pool, secret string) *Repository {
	sum := sha256.Sum256([]byte(secret))
	return &Repository{db: db, key: sum[:]}
}

func (r *Repository) List(ctx context.Context) ([]Summary, error) {
	rows, err := r.db.Query(ctx, `SELECT id,name,host,port,database_name,username,ssl_mode
		FROM app.database_connections WHERE deleted_at IS NULL ORDER BY name,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]Summary, 0)
	for rows.Next() {
		var item Summary
		if err := rows.Scan(&item.ID, &item.Name, &item.Host, &item.Port, &item.DatabaseName, &item.Username, &item.SSLMode); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func validate(input Input, requirePassword bool) error {
	if strings.TrimSpace(input.Name) == "" || input.Name != strings.TrimSpace(input.Name) ||
		strings.TrimSpace(input.Host) == "" || input.Host != strings.TrimSpace(input.Host) ||
		strings.TrimSpace(input.DatabaseName) == "" || input.DatabaseName != strings.TrimSpace(input.DatabaseName) ||
		strings.TrimSpace(input.Username) == "" || input.Username != strings.TrimSpace(input.Username) ||
		input.Port < 1 || input.Port > 65535 {
		return ErrInvalidInput
	}
	if input.SSLMode != "disable" && input.SSLMode != "require" && input.SSLMode != "verify-full" {
		return ErrInvalidInput
	}
	if requirePassword && input.Password == "" {
		return ErrInvalidInput
	}
	if len(input.Password) > 1024 {
		return ErrInvalidInput
	}
	return nil
}

func targetKey(input Input) string {
	return strings.ToLower(net.JoinHostPort(input.Host, strconv.Itoa(input.Port))) + "/" + input.DatabaseName
}

func (r *Repository) Create(ctx context.Context, input Input) (Summary, error) {
	if err := validate(input, true); err != nil {
		return Summary{}, err
	}
	ciphertext, err := r.encrypt(input.Password)
	if err != nil {
		return Summary{}, err
	}
	var item Summary
	err = r.db.QueryRow(ctx, `INSERT INTO app.database_connections
		(id,name,host,port,database_name,username,password_ciphertext,credential_key_id,ssl_mode,target_key)
		VALUES($1,$2,$3,$4,$5,$6,$7,'env-derived-v1',$8,$9)
		RETURNING id,name,host,port,database_name,username,ssl_mode`,
		uuid.New(), input.Name, input.Host, input.Port, input.DatabaseName, input.Username, ciphertext, input.SSLMode, targetKey(input)).
		Scan(&item.ID, &item.Name, &item.Host, &item.Port, &item.DatabaseName, &item.Username, &item.SSLMode)
	if err != nil {
		if strings.Contains(err.Error(), "duplicate key") {
			return Summary{}, ErrConflict
		}
		return Summary{}, err
	}
	return item, nil
}

func (r *Repository) Update(ctx context.Context, id uuid.UUID, input Input) (Summary, error) {
	if id == uuid.Nil || input.Name == "" {
		return Summary{}, ErrInvalidInput
	}
	var item Summary
	if err := validate(input, false); err != nil {
		return Summary{}, err
	}
	var query string
	args := []any{id, input.Name, input.Host, input.Port, input.DatabaseName, input.Username, input.SSLMode, targetKey(input)}
	if input.Password == "" {
		query = `UPDATE app.database_connections SET name=$2,host=$3,port=$4,database_name=$5,username=$6,ssl_mode=$7,target_key=$8,updated_at=clock_timestamp()
			WHERE id=$1 AND deleted_at IS NULL RETURNING id,name,host,port,database_name,username,ssl_mode`
	} else {
		ciphertext, err := r.encrypt(input.Password)
		if err != nil {
			return Summary{}, err
		}
		query = `UPDATE app.database_connections SET name=$2,host=$3,port=$4,database_name=$5,username=$6,ssl_mode=$7,target_key=$8,password_ciphertext=$9,updated_at=clock_timestamp()
			WHERE id=$1 AND deleted_at IS NULL RETURNING id,name,host,port,database_name,username,ssl_mode`
		args = append(args, ciphertext)
	}
	err := r.db.QueryRow(ctx, query, args...).Scan(&item.ID, &item.Name, &item.Host, &item.Port, &item.DatabaseName, &item.Username, &item.SSLMode)
	if errors.Is(err, pgx.ErrNoRows) {
		return Summary{}, ErrNotFound
	}
	if err != nil {
		if strings.Contains(err.Error(), "duplicate key") {
			return Summary{}, ErrConflict
		}
		return Summary{}, err
	}
	return item, nil
}

func (r *Repository) Delete(ctx context.Context, id uuid.UUID) error {
	if id == uuid.Nil {
		return ErrInvalidInput
	}
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	result, err := tx.Exec(ctx, `UPDATE app.database_connections SET deleted_at=clock_timestamp(),updated_at=clock_timestamp() WHERE id=$1 AND deleted_at IS NULL`, id)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrNotFound
	}
	if _, err := tx.Exec(ctx, `DELETE FROM app.group_database_grants WHERE database_id=$1`, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *Repository) OpenTarget(ctx context.Context, id uuid.UUID) (*sql.DB, error) {
	var input Input
	var ciphertext []byte
	err := r.db.QueryRow(ctx, `SELECT host,port,database_name,username,password_ciphertext,ssl_mode
		FROM app.database_connections WHERE id=$1 AND deleted_at IS NULL`, id).
		Scan(&input.Host, &input.Port, &input.DatabaseName, &input.Username, &ciphertext, &input.SSLMode)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	input.Password, err = r.decrypt(ciphertext)
	if err != nil {
		return nil, err
	}
	dsn := (&url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(input.Username, input.Password),
		Host:   net.JoinHostPort(input.Host, strconv.Itoa(input.Port)),
		Path:   "/" + input.DatabaseName,
	}).String() + "?sslmode=" + url.QueryEscape(input.SSLMode)
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	return stdlib.OpenDB(*config.ConnConfig), nil
}

func (r *Repository) Test(ctx context.Context, id uuid.UUID) error {
	db, err := r.OpenTarget(ctx, id)
	if err != nil {
		return err
	}
	defer db.Close()
	return db.PingContext(ctx)
}

func (r *Repository) encrypt(value string) ([]byte, error) {
	block, err := aes.NewCipher(r.key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return aead.Seal(nonce, nonce, []byte(value), nil), nil
}

func (r *Repository) decrypt(value []byte) (string, error) {
	block, err := aes.NewCipher(r.key)
	if err != nil {
		return "", err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	size := aead.NonceSize()
	if len(value) < size {
		return "", ErrInvalidInput
	}
	plain, err := aead.Open(nil, value[:size], value[size:], nil)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

type PoolManager struct {
	repository *Repository
	mu         sync.Mutex
	pools      map[uuid.UUID]*sql.DB
}

func NewPoolManager(repository *Repository) *PoolManager {
	return &PoolManager{repository: repository, pools: make(map[uuid.UUID]*sql.DB)}
}

func (m *PoolManager) Open(ctx context.Context, id uuid.UUID) (*sql.DB, error) {
	m.mu.Lock()
	if db := m.pools[id]; db != nil {
		m.mu.Unlock()
		return db, nil
	}
	m.mu.Unlock()
	db, err := m.repository.OpenTarget(ctx, id)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	if existing := m.pools[id]; existing != nil {
		m.mu.Unlock()
		db.Close()
		return existing, nil
	}
	m.pools[id] = db
	m.mu.Unlock()
	return db, nil
}

func (m *PoolManager) Invalidate(id uuid.UUID) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if db := m.pools[id]; db != nil {
		_ = db.Close()
		delete(m.pools, id)
	}
}

func (m *PoolManager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, db := range m.pools {
		_ = db.Close()
		delete(m.pools, id)
	}
}

func (m *PoolManager) IdleTimeout() time.Duration { return time.Minute }
