package auth

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const SessionCookieName = "app_session"
const CSRFCookieName = "csrf_token"

var (
	ErrUnauthenticated = errors.New("unauthenticated")
	ErrSessionNotFound = errors.New("session not found")
	ErrSessionExpired  = errors.New("session expired")
)

type SessionConfig struct {
	IdleTimeout      time.Duration
	AbsoluteLifetime time.Duration
	RotationInterval time.Duration
	GracePeriod      time.Duration
	CookieSecure     bool
	CookieDomain     string
	EncryptionKey    []byte
}

func LoadSessionConfig(jwtSecret string) SessionConfig {
	return SessionConfig{
		IdleTimeout:      LoadDuration("SESSION_IDLE_TIMEOUT", 20*time.Minute),
		AbsoluteLifetime: LoadDuration("SESSION_ABSOLUTE_LIFETIME", 8*time.Hour),
		RotationInterval: LoadDuration("SESSION_ROTATION_INTERVAL", 10*time.Minute),
		GracePeriod:      LoadDuration("SESSION_GRACE_PERIOD", 30*time.Second),
		CookieSecure:     os.Getenv("SESSION_COOKIE_SECURE") == "true",
		CookieDomain:     os.Getenv("SESSION_COOKIE_DOMAIN"),
		EncryptionKey:    deriveKey(jwtSecret),
	}
}

func LoadDuration(envVar string, defaultValue time.Duration) time.Duration {
	value := os.Getenv(envVar)
	if value == "" {
		return defaultValue
	}

	duration, err := time.ParseDuration(value)
	if err != nil {
		return defaultValue
	}
	return duration
}

func deriveKey(secret string) []byte {
	sum := sha256.Sum256([]byte(secret))
	return sum[:]
}

type Session struct {
	FamilyID           uuid.UUID
	UserID             uuid.UUID
	AuthVersion        int64
	CurrentAuthVersion int64
	CreatedAt          time.Time
	LastSeenAt         time.Time
	AbsoluteExpiresAt  time.Time
	RotatedAt          time.Time
}

type SessionRepository struct {
	db *pgxpool.Pool
}

func NewSessionRepository(db *pgxpool.Pool) *SessionRepository {
	return &SessionRepository{db: db}
}

// Create a new session for a user
func (r *SessionRepository) CreateSession(ctx context.Context, user *User, tokenHash, tokenCipherText []byte, now, expires time.Time) (uuid.UUID, error) {
	familyID := uuid.New()
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	defer tx.Rollback(ctx)

	_, err = tx.Exec(ctx,
		`INSERT INTO app.session_families
			(id, user_id, auth_version, created_at, last_seen_at, absolute_expires_at, rotated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		familyID, user.ID, user.AuthVersion, now, now, expires, now,
	)
	if err != nil {
		return uuid.Nil, err
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO app.session_tokens
			(token_hash, family_id, state, created_at, valid_until, token_ciphertext, token_key_id)
		VALUES ($1, $2, 'current', $3, $4, $5, 'env-derived')
	`, tokenHash, familyID, now, expires, tokenCipherText)

	if err != nil {
		return uuid.Nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, err
	}

	return familyID, nil
}


// Find user session by token hash
func (r *SessionRepository) Find(ctx context.Context, tokenHash []byte, now time.Time, idle time.Duration) (*Session, string, error) {
	var session Session
	var state string
	var validUntil time.Time
	err := r.db.QueryRow(ctx, `
		SELECT f.id, f.user_id, f.auth_version, u.auth_version, f.created_at, f.last_seen_at,
		       f.absolute_expires_at, f.rotated_at, t.state, t.valid_until
		FROM app.session_tokens t
		JOIN app.session_families f ON f.id = t.family_id
		JOIN app.users u ON u.id = f.user_id
		WHERE t.token_hash = $1
		  AND t.state IN ('current', 'grace')
		  AND u.is_active = true
		  AND f.revoked_at IS NULL
	`, tokenHash).Scan(
		&session.FamilyID, &session.UserID, &session.AuthVersion, &session.CurrentAuthVersion,
		&session.CreatedAt, &session.LastSeenAt, &session.AbsoluteExpiresAt,
		&session.RotatedAt, &state, &validUntil,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, "", ErrUnauthenticated
		}
		return nil, "", err
	}
	if !now.Before(session.AbsoluteExpiresAt) || !now.Before(validUntil) ||
		!now.Before(session.LastSeenAt.Add(idle)) ||
		session.AuthVersion != session.CurrentAuthVersion {
		return nil, "", ErrSessionExpired
	}
	return &session, state, nil
}

type queryRower interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func recoverCurrentID(ctx context.Context, q queryRower, familyID uuid.UUID, key []byte) (string, error) {
	var hash, ciphertext []byte
	err := q.QueryRow(ctx, `
		SELECT token_hash, token_ciphertext
		FROM app.session_tokens
		WHERE family_id = $1 AND state = 'current'
	`, familyID).Scan(&hash, &ciphertext)
	if err != nil {
		return "", fmt.Errorf("load current session token: %w", err)
	}
	id, err := decryptSessionID(ciphertext, key)
	if err != nil {
		return "", fmt.Errorf("recover current session token: %w", err)
	}
	expected, err := hashSessionID(id)
	if err != nil {
		return "", errors.New("recover current session token: malformed")
	}
	if subtle.ConstantTimeCompare(expected, hash) != 1 {
		return "", errors.New("recover current session token: hash mismatch")
	}
	return id, nil
}

// RecoverCurrent returns the family's current session ID from its stored ciphertext.
func (r *SessionRepository) RecoverCurrent(ctx context.Context, familyID uuid.UUID, key []byte) (string, error) {
	return recoverCurrentID(ctx, r.db, familyID, key)
}

// update the last seen timestamp
func (r *SessionRepository) Touch(ctx context.Context, familyID uuid.UUID, now time.Time) error {
	_, err := r.db.Exec(ctx, `
		UPDATE app.session_families
		SET last_seen_at = GREATEST(last_seen_at, LEAST($2, absolute_expires_at))
		WHERE id = $1 AND revoked_at IS NULL
	`, familyID, now)
	return err
}

// Rotate replaces the current token with newID and returns the family's resulting current session ID.
func (r *SessionRepository) Rotate(ctx context.Context, familyID uuid.UUID, oldHash []byte, newID string, key []byte, now, graceUntil time.Time) (string, error) {
	newHash, err := hashSessionID(newID)
	if err != nil {
		return "", err
	}
	ciphertext, err := encryptSessionID(newID, key)
	if err != nil {
		return "", err
	}

	tx, err := r.db.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)

	var absolute time.Time
	err = tx.QueryRow(ctx, `
		SELECT absolute_expires_at FROM app.session_families
		WHERE id = $1 AND revoked_at IS NULL
		FOR UPDATE
	`, familyID).Scan(&absolute)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrUnauthenticated
		}
		return "", err
	}

	var state string
	err = tx.QueryRow(ctx, `
		SELECT state FROM app.session_tokens WHERE token_hash = $1 AND family_id = $2
	`, oldHash, familyID).Scan(&state)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrUnauthenticated
		}
		return "", err
	}
	if state != "current" {
		id, err := recoverCurrentID(ctx, tx, familyID, key)
		if err != nil {
			return "", err
		}
		return id, tx.Commit(ctx)
	}

	if graceUntil.After(absolute) {
		graceUntil = absolute
	}
	if _, err := tx.Exec(ctx, `
		UPDATE app.session_tokens SET state = 'retired' WHERE family_id = $1 AND state = 'grace'
	`, familyID); err != nil {
		return "", err
	}
	result, err := tx.Exec(ctx, `
		UPDATE app.session_tokens
		SET state = 'grace', valid_until = LEAST($3, valid_until),
		    token_ciphertext = NULL, token_key_id = NULL
		WHERE token_hash = $1 AND family_id = $2 AND state = 'current'
	`, oldHash, familyID, graceUntil)
	if err != nil {
		return "", err
	}
	if result.RowsAffected() != 1 {
		return "", ErrUnauthenticated
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO app.session_tokens
			(token_hash, family_id, state, created_at, valid_until, token_ciphertext, token_key_id)
		VALUES ($1, $2, 'current', $3, $4, $5, 'env-derived')
	`, newHash, familyID, now, absolute, ciphertext); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE app.session_families
		SET rotated_at = LEAST($2, absolute_expires_at),
		    last_seen_at = GREATEST(last_seen_at, LEAST($2, absolute_expires_at))
		WHERE id = $1
	`, familyID, now); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return newID, nil
}

// Revoke all sessions in a family
func (r *SessionRepository) Revoke(ctx context.Context, familyID uuid.UUID, now time.Time) error {
	_, err := r.db.Exec(ctx, `
		UPDATE app.session_families SET revoked_at = $2 WHERE id = $1 AND revoked_at IS NULL
	`, familyID, now)
	return err
}

type LoginResult struct {
	SessionID string
	ExpiresAt time.Time
}

type AuthenticatedSession struct {
	SessionID string
	Session   *Session
}

func newSessionID() (string, error) {
	raw := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// parseSessionID decodes a session ID into its 32 raw bytes.
func parseSessionID(sessionID string) ([]byte, error) {
	raw, err := base64.RawURLEncoding.DecodeString(sessionID)
	if err != nil || len(raw) != 32 {
		return nil, ErrUnauthenticated
	}
	return raw, nil
}

func hashSessionID(sessionID string) ([]byte, error) {
	raw, err := parseSessionID(sessionID)
	if err != nil {
		return nil, err
	}
	hash := sha256.Sum256(raw)
	return hash[:], nil
}

func encryptSessionID(sessionID string, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
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

	ciphertext := aead.Seal(nonce, nonce, []byte(sessionID), nil)
	return ciphertext, nil
}

func decryptSessionID(ciphertext, key []byte) (string, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}

	aead, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}

	if len(ciphertext) < aead.NonceSize() {
		return "", errors.New("session ciphertext too short")
	}
	nonce, sealed := ciphertext[:aead.NonceSize()], ciphertext[aead.NonceSize():]
	plain, err := aead.Open(nil, nonce, sealed, nil)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

func (s *service) createSession(ctx context.Context, user *User) (*LoginResult, error) {
	now := time.Now().UTC()
	expires := now.Add(s.sessionConfig.AbsoluteLifetime)
	sessionID, err := newSessionID()
	if err != nil {
		return nil, err
	}

	cipherText, err := encryptSessionID(sessionID, s.sessionConfig.EncryptionKey)
	if err != nil {
		return nil, err
	}

	hash, err := hashSessionID(sessionID)
	if err != nil {
		return nil, err
	}
	if _, err := s.sessions.CreateSession(ctx, user, hash, cipherText, now, expires); err != nil {
		return nil, err
	}

	return &LoginResult{
		SessionID: sessionID,
		ExpiresAt: expires,
	}, nil
}

func (s *service) authenticate(ctx context.Context, sessionID string) (*AuthenticatedSession, error) {
	hash, err := hashSessionID(sessionID)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	session, state, err := s.sessions.Find(ctx, hash, now, s.sessionConfig.IdleTimeout)
	if err != nil {
		return nil, err
	}

	resultID := sessionID
	switch {
	case state == "grace":
		resultID, err = s.sessions.RecoverCurrent(ctx, session.FamilyID, s.sessionConfig.EncryptionKey)
		if err != nil {
			return nil, err
		}
	case now.Sub(session.RotatedAt) >= s.sessionConfig.RotationInterval:
		newID, err := newSessionID()
		if err != nil {
			return nil, err
		}
		resultID, err = s.sessions.Rotate(ctx, session.FamilyID, hash, newID, s.sessionConfig.EncryptionKey, now, now.Add(s.sessionConfig.GracePeriod))
		if err != nil {
			return nil, err
		}
	}

	if err := s.sessions.Touch(ctx, session.FamilyID, now); err != nil {
		return nil, err
	}

	return &AuthenticatedSession{
		SessionID: resultID,
		Session:   session,
	}, nil
}
