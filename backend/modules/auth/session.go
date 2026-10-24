package auth

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const SessionCookieName = "session_token"
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
		IdleTimeout:      LoadDuration("SESSION_IDLE_TIMEOUT", 30*time.Minute),
		AbsoluteLifetime: LoadDuration("SESSION_ABSOLUTE_LIFETIME", 8*time.Hour),
		RotationInterval: LoadDuration("SESSION_ROTATION_INTERVAL", 15*time.Minute),
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
func (r *SessionRepository) Find(ctx context.Context, tokenHash []byte, now time.Time, idle time.Duration) (*Session, bool, error) {
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
		  AND u.is_active = true
		  AND f.revoked_at IS NULL
	`, tokenHash).Scan(
		&session.FamilyID, &session.UserID, &session.AuthVersion, &session.CurrentAuthVersion,
		&session.CreatedAt, &session.LastSeenAt, &session.AbsoluteExpiresAt,
		&session.RotatedAt, &state, &validUntil,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, false, ErrUnauthenticated
		}
		return nil, false, err
	}
	if !now.Before(session.AbsoluteExpiresAt) || !now.Before(validUntil) ||
		!now.Before(session.LastSeenAt.Add(idle)) ||
		session.AuthVersion != session.CurrentAuthVersion {
		return nil, false, ErrSessionExpired
	}
	return &session, state == "current", nil
}

// update the last seen timestamp
func (r *SessionRepository) Touch(ctx context.Context, familyID uuid.UUID, now time.Time) error {
	_, err := r.db.Exec(ctx, `
		UPDATE app.session_families
		SET last_seen_at = LEAST($2, absolute_expires_at)
		WHERE id = $1 AND revoked_at IS NULL
	`, familyID, now)
	return err
}

// Replace old session with a new one
func (r *SessionRepository) Rotate(ctx context.Context, familyID uuid.UUID, oldHash, newHash, ciphertext []byte, now, graceUntil, absolute time.Time) error {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	result, err := tx.Exec(ctx, `
		UPDATE app.session_tokens
		SET state = 'grace', valid_until = LEAST($3, valid_until)
		WHERE token_hash = $1 AND family_id = $2 AND state = 'current'
	`, oldHash, familyID, graceUntil)
	if err != nil || result.RowsAffected() != 1 {
		if err == nil {
			err = ErrUnauthenticated
		}
		return err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO app.session_tokens
			(token_hash, family_id, state, created_at, valid_until, token_ciphertext, token_key_id)
		VALUES ($1, $2, 'current', $3, $4, $5, 'env-derived')
	`, newHash, familyID, now, absolute, ciphertext)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		UPDATE app.session_families SET rotated_at = $2 WHERE id = $1
	`, familyID, now)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
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

func hashSessionID(sessionID string) []byte {
	hash := sha256.Sum256([]byte(sessionID))
	return hash[:]
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

	if _, err := s.sessions.CreateSession(ctx, user, hashSessionID(sessionID), cipherText, now, expires); err != nil {
		return nil, err
	}

	return &LoginResult{
		SessionID: sessionID,
		ExpiresAt: expires,
	}, nil
}

func (s *service) authenticate(ctx context.Context, sessionID string) (*AuthenticatedSession, error) {
	if sessionID == "" {
		return nil, ErrUnauthenticated
	}

	now := time.Now().UTC()
	session, current, err := s.sessions.Find(ctx, hashSessionID(sessionID), now, s.sessionConfig.IdleTimeout)
	if err != nil {
		return nil, err
	}

	if current && now.Sub(session.RotatedAt) >= s.sessionConfig.RotationInterval {
		newID, err := newSessionID()
		if err != nil {
			return nil, err
		}

		cipherText, err := encryptSessionID(newID, s.sessionConfig.EncryptionKey)
		if err != nil {
			return nil, err
		}

		if err := s.sessions.Rotate(ctx, session.FamilyID, hashSessionID(sessionID), hashSessionID(newID), cipherText, now, now.Add(s.sessionConfig.GracePeriod), session.AbsoluteExpiresAt); err != nil {
			if errors.Is(err, ErrUnauthenticated) {
				return &AuthenticatedSession{
					SessionID: sessionID,
					Session:   session,
				}, nil
			}
			return nil, err
		}

		return &AuthenticatedSession{
			SessionID: newID,
			Session:   session,
		}, nil
	}

	if err := s.sessions.Touch(ctx, session.FamilyID, now); err != nil {
		return nil, err
	}

	return &AuthenticatedSession{
		SessionID: sessionID,
		Session:   session,
	}, nil
}
