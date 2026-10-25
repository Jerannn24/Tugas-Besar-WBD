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
	"time"

	"database/sql"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
)

var (
	ErrInvalidInput      = errors.New("invalid input")
	ErrNotFound          = errors.New("not found")
	ErrConflict          = errors.New("conflict")
	ErrBusy              = errors.New("database busy")
	ErrNotReady          = errors.New("execution target not ready")
	ErrTargetUnavailable = errors.New("target unavailable")
	ErrClosed            = errors.New("connection manager closed")
)

const maxPasswordBytes = 1024

// Summary never contains any password. executor_username/preparer_username
// are null for viewer-only connections.
type Summary struct {
	ID               uuid.UUID `json:"id"`
	Name             string    `json:"name"`
	Host             string    `json:"host"`
	Port             int       `json:"port"`
	DatabaseName     string    `json:"database_name"`
	Username         string    `json:"username"`
	SSLMode          string    `json:"ssl_mode"`
	ExecutorUsername *string   `json:"executor_username"`
	PreparerUsername *string   `json:"preparer_username"`
	ExecutionReady   bool      `json:"execution_ready"`
}

// RoleCredential is a write-only credential pair. A nil Password means
// "keep the stored password" and is only meaningful on update.
type RoleCredential struct {
	Username string  `json:"username"`
	Password *string `json:"password"`
}

type Input struct {
	Name         string          `json:"name"`
	Host         string          `json:"host"`
	Port         int             `json:"port"`
	DatabaseName string          `json:"database_name"`
	Username     string          `json:"username"`
	Password     string          `json:"password"`
	SSLMode      string          `json:"ssl_mode"`
	Executor     *RoleCredential `json:"executor,omitempty"`
	Preparer     *RoleCredential `json:"preparer,omitempty"`
}

type credential struct{ User, Password string }

// targetConfig is the decrypted, in-memory connection description. It must
// never be logged or serialised.
type targetConfig struct {
	Host     string
	Port     int
	Database string
	SSLMode  string
	Viewer   credential
	Executor credential
	Preparer credential
}

func (c targetConfig) hasRoles() bool { return c.Executor.User != "" }

// lifecycle lets the repository ask the pool manager for exclusive
// maintenance of one connection while the platform transaction is open.
type lifecycle interface {
	// Quiesce fails with ErrBusy while leases are active. The returned func
	// is called once the transaction has ended.
	Quiesce(id uuid.UUID) (func(committed bool), error)
}

type Repository struct {
	db     *pgxpool.Pool
	key    []byte
	verify func(context.Context, targetConfig) (uuid.UUID, error)
}

func NewRepository(db *pgxpool.Pool, secret string) *Repository {
	sum := sha256.Sum256([]byte(secret))
	return &Repository{db: db, key: sum[:], verify: verifyTarget}
}

const summaryColumns = `id,name,host,port,database_name,username,ssl_mode,executor_username,preparer_username,execution_target_id IS NOT NULL`

func scanSummary(row pgx.Row) (Summary, error) {
	var item Summary
	err := row.Scan(&item.ID, &item.Name, &item.Host, &item.Port, &item.DatabaseName, &item.Username, &item.SSLMode,
		&item.ExecutorUsername, &item.PreparerUsername, &item.ExecutionReady)
	return item, err
}

func (r *Repository) List(ctx context.Context) ([]Summary, error) {
	rows, err := r.db.Query(ctx, `SELECT `+summaryColumns+`
		FROM app.database_connections WHERE deleted_at IS NULL ORDER BY name,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]Summary, 0)
	for rows.Next() {
		item, err := scanSummary(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func cleanName(value string) bool {
	return strings.TrimSpace(value) != "" && value == strings.TrimSpace(value)
}

func validRolePair(executor, preparer string) bool {
	return strings.HasSuffix(executor, "_executor") && len(executor) > len("_executor") &&
		preparer == strings.TrimSuffix(executor, "_executor")+"_preparer"
}

func validate(input Input, requirePassword bool) error {
	if !cleanName(input.Name) || !cleanName(input.Host) || !cleanName(input.DatabaseName) || !cleanName(input.Username) ||
		input.Port < 1 || input.Port > 65535 {
		return ErrInvalidInput
	}
	if input.SSLMode != "disable" && input.SSLMode != "require" && input.SSLMode != "verify-full" {
		return ErrInvalidInput
	}
	if requirePassword && input.Password == "" {
		return ErrInvalidInput
	}
	if len(input.Password) > maxPasswordBytes {
		return ErrInvalidInput
	}
	if (input.Executor == nil) != (input.Preparer == nil) {
		return ErrInvalidInput
	}
	if input.Executor == nil {
		return nil
	}
	for _, role := range []*RoleCredential{input.Executor, input.Preparer} {
		if !cleanName(role.Username) || role.Username == input.Username {
			return ErrInvalidInput
		}
		if role.Password == nil {
			if requirePassword {
				return ErrInvalidInput
			}
			continue
		}
		if *role.Password == "" || len(*role.Password) > maxPasswordBytes {
			return ErrInvalidInput
		}
	}
	if input.Executor.Username == input.Preparer.Username || !validRolePair(input.Executor.Username, input.Preparer.Username) {
		return ErrInvalidInput
	}
	return nil
}

func targetKey(input Input) string {
	return strings.ToLower(net.JoinHostPort(input.Host, strconv.Itoa(input.Port))) + "/" + input.DatabaseName
}

func isUnique(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505"
}

func (r *Repository) encryptOptional(value *string) ([]byte, error) {
	if value == nil {
		return nil, nil
	}
	return r.encrypt(*value)
}

func (r *Repository) Create(ctx context.Context, input Input) (Summary, error) {
	if err := validate(input, true); err != nil {
		return Summary{}, err
	}
	ciphertext, err := r.encrypt(input.Password)
	if err != nil {
		return Summary{}, err
	}
	var execUser, prepUser *string
	var execCT, prepCT []byte
	var marker uuid.UUID
	if input.Executor != nil {
		cfg := targetConfig{Host: input.Host, Port: input.Port, Database: input.DatabaseName, SSLMode: input.SSLMode,
			Viewer:   credential{input.Username, input.Password},
			Executor: credential{input.Executor.Username, *input.Executor.Password},
			Preparer: credential{input.Preparer.Username, *input.Preparer.Password}}
		marker, err = r.verifyBounded(ctx, cfg)
		if err != nil {
			return Summary{}, err
		}
		if execCT, err = r.encrypt(cfg.Executor.Password); err != nil {
			return Summary{}, err
		}
		if prepCT, err = r.encrypt(cfg.Preparer.Password); err != nil {
			return Summary{}, err
		}
		execUser, prepUser = &input.Executor.Username, &input.Preparer.Username
	}
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return Summary{}, err
	}
	defer tx.Rollback(ctx)
	id := uuid.New()
	if _, err = tx.Exec(ctx, `INSERT INTO app.database_connections
		(id,name,host,port,database_name,username,password_ciphertext,credential_key_id,ssl_mode,target_key,
		 executor_username,executor_password_ciphertext,preparer_username,preparer_password_ciphertext)
		VALUES($1,$2,$3,$4,$5,$6,$7,'env-derived-v1',$8,$9,$10,$11,$12,$13)`,
		id, input.Name, input.Host, input.Port, input.DatabaseName, input.Username, ciphertext, input.SSLMode, targetKey(input),
		execUser, execCT, prepUser, prepCT); err != nil {
		if isUnique(err) {
			return Summary{}, ErrConflict
		}
		return Summary{}, err
	}
	if input.Executor != nil {
		if err := bindTarget(ctx, tx, id, marker); err != nil {
			return Summary{}, err
		}
	}
	item, err := scanSummary(tx.QueryRow(ctx, `SELECT `+summaryColumns+` FROM app.database_connections WHERE id=$1`, id))
	if err != nil {
		return Summary{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Summary{}, err
	}
	return item, nil
}

// bindTarget keeps an existing counter on re-registration, locks the state
// row (the connection row is already locked/new by the caller) and points the
// connection at it. A duplicate active binding surfaces as ErrConflict.
func bindTarget(ctx context.Context, tx pgx.Tx, id, marker uuid.UUID) error {
	if marker == uuid.Nil {
		return ErrTargetUnavailable
	}
	if _, err := tx.Exec(ctx, `INSERT INTO app.migration_target_state(id,next_version) VALUES($1,1) ON CONFLICT (id) DO NOTHING`, marker); err != nil {
		return err
	}
	var locked uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT id FROM app.migration_target_state WHERE id=$1 FOR UPDATE`, marker).Scan(&locked); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE app.database_connections SET execution_target_id=$2 WHERE id=$1`, id, marker); err != nil {
		if isUnique(err) {
			return ErrConflict
		}
		return err
	}
	return nil
}

func (r *Repository) verifyBounded(ctx context.Context, cfg targetConfig) (uuid.UUID, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	marker, err := r.verify(ctx, cfg)
	if err != nil {
		if !errors.Is(err, ErrTargetUnavailable) {
			return uuid.Nil, ErrTargetUnavailable
		}
		return uuid.Nil, err
	}
	if marker == uuid.Nil {
		return uuid.Nil, ErrTargetUnavailable
	}
	return marker, nil
}

type storedRow struct {
	ID                                      uuid.UUID
	Name, Host, Database, Username, SSLMode string
	Port                                    int
	ViewerCT                                []byte
	ExecUser, PrepUser                      *string
	ExecCT, PrepCT                          []byte
	Target                                  *uuid.UUID
	UpdatedAt                               time.Time
}

const storedColumns = `id,name,host,port,database_name,username,password_ciphertext,ssl_mode,
	executor_username,executor_password_ciphertext,preparer_username,preparer_password_ciphertext,execution_target_id,updated_at`

func scanStored(row pgx.Row) (storedRow, error) {
	var s storedRow
	err := row.Scan(&s.ID, &s.Name, &s.Host, &s.Port, &s.Database, &s.Username, &s.ViewerCT, &s.SSLMode,
		&s.ExecUser, &s.ExecCT, &s.PrepUser, &s.PrepCT, &s.Target, &s.UpdatedAt)
	return s, err
}

func (r *Repository) config(s storedRow) (targetConfig, error) {
	cfg := targetConfig{Host: s.Host, Port: s.Port, Database: s.Database, SSLMode: s.SSLMode}
	var err error
	cfg.Viewer.User = s.Username
	if cfg.Viewer.Password, err = r.decrypt(s.ViewerCT); err != nil {
		return targetConfig{}, err
	}
	if s.ExecUser != nil && s.PrepUser != nil {
		cfg.Executor.User, cfg.Preparer.User = *s.ExecUser, *s.PrepUser
		if cfg.Executor.Password, err = r.decrypt(s.ExecCT); err != nil {
			return targetConfig{}, err
		}
		if cfg.Preparer.Password, err = r.decrypt(s.PrepCT); err != nil {
			return targetConfig{}, err
		}
	}
	return cfg, nil
}

func pick(provided *string, stored string) string {
	if provided != nil {
		return *provided
	}
	return stored
}

func (r *Repository) Update(ctx context.Context, id uuid.UUID, input Input, lc lifecycle) (Summary, error) {
	if id == uuid.Nil || input.Name == "" {
		return Summary{}, ErrInvalidInput
	}
	if err := validate(input, false); err != nil {
		return Summary{}, err
	}
	current, err := scanStored(r.db.QueryRow(ctx, `SELECT `+storedColumns+` FROM app.database_connections WHERE id=$1 AND deleted_at IS NULL`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Summary{}, ErrNotFound
	}
	if err != nil {
		return Summary{}, err
	}
	old, err := r.config(current)
	if err != nil {
		return Summary{}, err
	}
	// Desired configuration: omitted passwords keep stored ones; omitted role
	// objects mean viewer-only (unbound).
	want := targetConfig{Host: input.Host, Port: input.Port, Database: input.DatabaseName, SSLMode: input.SSLMode,
		Viewer: credential{input.Username, old.Viewer.Password}}
	viewerCT := current.ViewerCT
	if input.Password != "" {
		want.Viewer.Password = input.Password
		if viewerCT, err = r.encrypt(input.Password); err != nil {
			return Summary{}, err
		}
	}
	var execCT, prepCT []byte
	var execUser, prepUser *string
	if input.Executor != nil {
		if !old.hasRoles() && (input.Executor.Password == nil || input.Preparer.Password == nil) {
			return Summary{}, ErrInvalidInput
		}
		want.Executor = credential{input.Executor.Username, pick(input.Executor.Password, old.Executor.Password)}
		want.Preparer = credential{input.Preparer.Username, pick(input.Preparer.Password, old.Preparer.Password)}
		execCT, prepCT = current.ExecCT, current.PrepCT
		if input.Executor.Password != nil {
			if execCT, err = r.encrypt(*input.Executor.Password); err != nil {
				return Summary{}, err
			}
		}
		if input.Preparer.Password != nil {
			if prepCT, err = r.encrypt(*input.Preparer.Password); err != nil {
				return Summary{}, err
			}
		}
		execUser, prepUser = &input.Executor.Username, &input.Preparer.Username
	}
	changed := want != old
	var marker *uuid.UUID
	if want.hasRoles() {
		if changed || current.Target == nil {
			m, err := r.verifyBounded(ctx, want)
			if err != nil {
				return Summary{}, err
			}
			marker = &m
		} else {
			marker = current.Target
		}
	}
	rebind := (marker == nil) != (current.Target == nil) || (marker != nil && *marker != *current.Target)
	identityChange := changed || rebind

	tx, err := r.db.Begin(ctx)
	if err != nil {
		return Summary{}, err
	}
	defer tx.Rollback(ctx)
	locked, err := scanStored(tx.QueryRow(ctx, `SELECT `+storedColumns+` FROM app.database_connections WHERE id=$1 AND deleted_at IS NULL FOR UPDATE`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Summary{}, ErrNotFound
	}
	if err != nil {
		return Summary{}, err
	}
	// Revalidate: anything verified above must still describe the stored row.
	if !locked.UpdatedAt.Equal(current.UpdatedAt) || (locked.Target == nil) != (current.Target == nil) ||
		(locked.Target != nil && *locked.Target != *current.Target) {
		return Summary{}, ErrConflict
	}
	if err := guardRunning(ctx, tx, id, locked.Target); err != nil {
		return Summary{}, err
	}
	committed := false
	if identityChange && lc != nil {
		resume, err := lc.Quiesce(id)
		if err != nil {
			return Summary{}, err
		}
		defer func() { resume(committed) }()
	}
	if rebind && locked.Target != nil && marker != nil {
		// A different physical target would restart version numbering while
		// the database id already carries history.
		var history bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM app.migration_executions WHERE database_id=$1)`, id).Scan(&history); err != nil {
			return Summary{}, err
		}
		if history {
			return Summary{}, ErrConflict
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE app.database_connections SET name=$2,host=$3,port=$4,database_name=$5,username=$6,ssl_mode=$7,target_key=$8,
		password_ciphertext=$9,executor_username=$10,executor_password_ciphertext=$11,preparer_username=$12,preparer_password_ciphertext=$13,
		execution_target_id=NULL,updated_at=clock_timestamp() WHERE id=$1`,
		id, input.Name, input.Host, input.Port, input.DatabaseName, input.Username, input.SSLMode, targetKey(input),
		viewerCT, execUser, execCT, prepUser, prepCT); err != nil {
		if isUnique(err) {
			return Summary{}, ErrConflict
		}
		return Summary{}, err
	}
	if marker != nil {
		if err := bindTarget(ctx, tx, id, *marker); err != nil {
			return Summary{}, err
		}
	}
	item, err := scanSummary(tx.QueryRow(ctx, `SELECT `+summaryColumns+` FROM app.database_connections WHERE id=$1`, id))
	if err != nil {
		return Summary{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Summary{}, err
	}
	committed = true
	return item, nil
}

// guardRunning locks the bound target state (lock order: connection row,
// then migration_target_state) and refuses while an execution is unresolved.
func guardRunning(ctx context.Context, tx pgx.Tx, id uuid.UUID, target *uuid.UUID) error {
	if target == nil {
		return nil
	}
	var state uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT id FROM app.migration_target_state WHERE id=$1 FOR UPDATE`, *target).Scan(&state); err != nil {
		return err
	}
	var busy bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM app.migration_executions WHERE execution_target_id=$1 AND status IN ('running','unknown'))`, *target).Scan(&busy); err != nil {
		return err
	}
	if busy {
		return ErrBusy
	}
	return nil
}

func (r *Repository) Delete(ctx context.Context, id uuid.UUID, lc lifecycle) error {
	if id == uuid.Nil {
		return ErrInvalidInput
	}
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var target *uuid.UUID
	err = tx.QueryRow(ctx, `SELECT execution_target_id FROM app.database_connections WHERE id=$1 AND deleted_at IS NULL FOR UPDATE`, id).Scan(&target)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if err := guardRunning(ctx, tx, id, target); err != nil {
		return err
	}
	committed := false
	if lc != nil {
		resume, err := lc.Quiesce(id)
		if err != nil {
			return err
		}
		defer func() { resume(committed) }()
	}
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
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	committed = true
	return nil
}

// loadConfig returns the decrypted configuration of an active connection and
// its bound physical target. Unbound connections yield ErrNotReady.
func (r *Repository) loadConfig(ctx context.Context, id uuid.UUID) (targetConfig, *uuid.UUID, error) {
	s, err := scanStored(r.db.QueryRow(ctx, `SELECT `+storedColumns+` FROM app.database_connections WHERE id=$1 AND deleted_at IS NULL`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return targetConfig{}, nil, ErrNotFound
	}
	if err != nil {
		return targetConfig{}, nil, err
	}
	cfg, err := r.config(s)
	if err != nil {
		return targetConfig{}, nil, err
	}
	return cfg, s.Target, nil
}

func dsn(cfg targetConfig, cred credential) string {
	return (&url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(cred.User, cred.Password),
		Host:   net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)),
		Path:   "/" + cfg.Database,
	}).String() + "?sslmode=" + url.QueryEscape(cfg.SSLMode)
}

func connConfig(cfg targetConfig, cred credential) (*pgx.ConnConfig, error) {
	cc, err := pgx.ParseConfig(dsn(cfg, cred))
	if err != nil {
		return nil, ErrInvalidInput // deliberately drops the underlying error text
	}
	cc.ConnectTimeout = 5 * time.Second
	return cc, nil
}

// openViewer opens the read-only viewer pool with bounded size.
func openViewer(cfg targetConfig) (*sql.DB, error) {
	cc, err := connConfig(cfg, cfg.Viewer)
	if err != nil {
		return nil, err
	}
	db := stdlib.OpenDB(*cc)
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(2)
	db.SetConnMaxIdleTime(time.Minute)
	return db, nil
}

func (r *Repository) OpenTarget(ctx context.Context, id uuid.UUID) (*sql.DB, error) {
	cfg, _, err := r.loadConfig(ctx, id)
	if err != nil {
		return nil, err
	}
	return openViewer(cfg)
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
