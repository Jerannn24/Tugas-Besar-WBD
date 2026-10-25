package auth

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

func openAuthTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()

	databaseURL := os.Getenv("AUTH_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("AUTH_TEST_DATABASE_URL is not set; integration test skipped")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("create test pool: %v", err)
	}
	if err := db.Ping(ctx); err != nil {
		db.Close()
		t.Fatalf("ping test database: %v", err)
	}
	return db
}

func createAuthTestUser(t *testing.T, db *pgxpool.Pool) *User {
	t.Helper()

	user := &User{
		Username:    "session-test-" + uuid.NewString(),
		AuthVersion: 1,
	}
	passwordHash, err := bcrypt.GenerateFromPassword([]byte("TestPassword1!"), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("hash test password: %v", err)
	}
	user.HashPassword = string(passwordHash)

	repository := NewRepository(db)
	if err := repository.Create(context.Background(), user); err != nil {
		t.Fatalf("create test user: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cleanupURL := os.Getenv("AUTH_TEST_CLEANUP_DATABASE_URL")
		if cleanupURL == "" {
			_, err1 := db.Exec(ctx, `UPDATE app.session_families SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL`, user.ID)
			_, err2 := db.Exec(ctx, `UPDATE app.users SET is_active = false WHERE id = $1`, user.ID)
			if err1 != nil || err2 != nil {
				t.Errorf("deactivate test user: %v %v", err1, err2)
			}
			t.Log("AUTH_TEST_CLEANUP_DATABASE_URL not set; test user deactivated, not deleted")
			return
		}
		cleanup, err := pgxpool.New(ctx, cleanupURL)
		if err != nil {
			t.Errorf("open cleanup pool: %v", err)
			return
		}
		defer cleanup.Close()
		if _, err := cleanup.Exec(ctx, `DELETE FROM app.session_families WHERE user_id = $1`, user.ID); err != nil {
			t.Errorf("delete test sessions: %v", err)
			return
		}
		if _, err := cleanup.Exec(ctx, `DELETE FROM app.users WHERE id = $1`, user.ID); err != nil {
			t.Errorf("delete test user: %v", err)
		}
	})
	return user
}

func newTestService(db *pgxpool.Pool, config SessionConfig) *service {
	return NewService(
		NewRepository(db),
		NewSessionRepository(db),
		config,
	)
}

func testSessionConfig() SessionConfig {
	return SessionConfig{
		IdleTimeout:      time.Hour,
		AbsoluteLifetime: time.Hour,
		RotationInterval: time.Hour,
		GracePeriod:      100 * time.Millisecond,
		EncryptionKey:    deriveKey("integration-test-secret"),
	}
}

func TestBootstrapAdminIsIdempotent(t *testing.T) {
	db := openAuthTestDB(t)
	defer db.Close()

	username := "bootstrap-" + uuid.NewString()
	repository := NewAdminRepository(db)
	password := "AdminPassword1!"

	if err := repository.BootstrapAdmin(context.Background(), username, password); err != nil {
		t.Fatalf("bootstrap admin: %v", err)
	}
	if err := repository.BootstrapAdmin(context.Background(), username, password); err != nil {
		t.Fatalf("repeat bootstrap admin: %v", err)
	}

	var count int
	if err := db.QueryRow(context.Background(),
		`SELECT count(*) FROM app.users WHERE username = $1 AND is_admin = true`,
		username,
	).Scan(&count); err != nil {
		t.Fatalf("count bootstrap admins: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected one bootstrap admin, got %d", count)
	}

	t.Cleanup(func() {
		_, _ = db.Exec(context.Background(), `DELETE FROM app.users WHERE username = $1`, username)
	})
}

func familyOf(t *testing.T, db *pgxpool.Pool, user *User) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := db.QueryRow(context.Background(),
		`SELECT id FROM app.session_families WHERE user_id = $1`, user.ID).Scan(&id); err != nil {
		t.Fatalf("find family: %v", err)
	}
	return id
}

func countTokens(t *testing.T, db *pgxpool.Pool, familyID uuid.UUID, state string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(context.Background(),
		`SELECT count(*) FROM app.session_tokens WHERE family_id = $1 AND state = $2`, familyID, state).Scan(&n); err != nil {
		t.Fatalf("count tokens: %v", err)
	}
	return n
}

func mustHash(t *testing.T, id string) []byte {
	t.Helper()
	h, err := hashSessionID(id)
	if err != nil {
		t.Fatalf("hash session id: %v", err)
	}
	return h
}

func loginUser(t *testing.T, service *service, user *User) string {
	t.Helper()
	login, err := service.createSession(context.Background(), user)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	return login.SessionID
}

func TestSessionLifecycle(t *testing.T) {
	db := openAuthTestDB(t)
	defer db.Close()

	user := createAuthTestUser(t, db)
	service := newTestService(db, testSessionConfig())
	id := loginUser(t, service, user)

	authenticated, err := service.Authenticate(context.Background(), id)
	if err != nil {
		t.Fatalf("authenticate session: %v", err)
	}
	if authenticated.SessionID != id {
		t.Fatalf("expected unchanged session ID, got %q", authenticated.SessionID)
	}

	if err := service.Logout(context.Background(), id); err != nil {
		t.Fatalf("logout session: %v", err)
	}
	if _, err := service.Authenticate(context.Background(), id); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("expected revoked session to be unauthenticated, got %v", err)
	}
}

func TestSessionForcedRotation(t *testing.T) {
	db := openAuthTestDB(t)
	defer db.Close()

	user := createAuthTestUser(t, db)
	config := testSessionConfig()
	config.GracePeriod = time.Minute
	config.RotationInterval = -time.Second
	service := newTestService(db, config)
	oldID := loginUser(t, service, user)
	familyID := familyOf(t, db, user)

	var absoluteBefore time.Time
	if err := db.QueryRow(context.Background(),
		`SELECT absolute_expires_at FROM app.session_families WHERE id = $1`, familyID).Scan(&absoluteBefore); err != nil {
		t.Fatal(err)
	}

	rotated, err := service.Authenticate(context.Background(), oldID)
	if err != nil {
		t.Fatalf("rotate session: %v", err)
	}
	if rotated.SessionID == oldID {
		t.Fatal("expected session rotation")
	}

	var state string
	var nullCipher, nullKey bool
	if err := db.QueryRow(context.Background(), `
		SELECT state, token_ciphertext IS NULL, token_key_id IS NULL
		FROM app.session_tokens WHERE token_hash = $1`, mustHash(t, oldID)).Scan(&state, &nullCipher, &nullKey); err != nil {
		t.Fatal(err)
	}
	if state != "grace" || !nullCipher || !nullKey {
		t.Fatalf("old token state=%s nullCipher=%v nullKey=%v, want grace/true/true", state, nullCipher, nullKey)
	}
	if n := countTokens(t, db, familyID, "current"); n != 1 {
		t.Fatalf("current rows = %d, want 1", n)
	}

	var absoluteAfter time.Time
	if err := db.QueryRow(context.Background(),
		`SELECT absolute_expires_at FROM app.session_families WHERE id = $1`, familyID).Scan(&absoluteAfter); err != nil {
		t.Fatal(err)
	}
	if !absoluteAfter.Equal(absoluteBefore) {
		t.Fatalf("absolute_expires_at changed: %v -> %v", absoluteBefore, absoluteAfter)
	}

	service.sessionConfig.RotationInterval = time.Hour
	recovered, err := service.Authenticate(context.Background(), oldID)
	if err != nil {
		t.Fatalf("grace request: %v", err)
	}
	if recovered.SessionID != rotated.SessionID {
		t.Fatal("grace request did not recover the current session ID")
	}
	if n := countTokens(t, db, familyID, "current"); n != 1 {
		t.Fatalf("current rows after recovery = %d, want 1", n)
	}
	again, err := service.Authenticate(context.Background(), rotated.SessionID)
	if err != nil || again.SessionID != rotated.SessionID {
		t.Fatalf("current token should authenticate unchanged: %v", err)
	}
}

func TestSessionRotationGraceAndAbsoluteExpiry(t *testing.T) {
	db := openAuthTestDB(t)
	defer db.Close()

	user := createAuthTestUser(t, db)
	config := testSessionConfig()
	config.RotationInterval = -time.Second
	config.AbsoluteLifetime = 2 * time.Second
	service := newTestService(db, config)
	oldID := loginUser(t, service, user)

	rotated, err := service.Authenticate(context.Background(), oldID)
	if err != nil {
		t.Fatalf("rotate session: %v", err)
	}
	service.sessionConfig.RotationInterval = time.Hour

	time.Sleep(config.GracePeriod + 50*time.Millisecond)
	if _, err := service.Authenticate(context.Background(), oldID); !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("expected old session grace expiry, got %v", err)
	}
	if _, err := service.Authenticate(context.Background(), rotated.SessionID); err != nil {
		t.Fatalf("current token should remain valid after grace expiry: %v", err)
	}

	time.Sleep(2 * time.Second)
	if _, err := service.Authenticate(context.Background(), rotated.SessionID); !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("expected absolute expiry after rotation, got %v", err)
	}
}

func TestSessionRetiredTokenRejected(t *testing.T) {
	db := openAuthTestDB(t)
	defer db.Close()

	user := createAuthTestUser(t, db)
	service := newTestService(db, testSessionConfig())
	id := loginUser(t, service, user)

	if _, err := db.Exec(context.Background(), `
		UPDATE app.session_tokens
		SET state = 'retired', token_ciphertext = NULL, token_key_id = NULL
		WHERE token_hash = $1`, mustHash(t, id)); err != nil {
		t.Fatalf("retire token: %v", err)
	}
	if _, err := service.Authenticate(context.Background(), id); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("expected retired token rejection, got %v", err)
	}
}

func TestSessionRejectedAfterInvalidation(t *testing.T) {
	cases := map[string]struct {
		query string
		want  error
	}{
		"revoked family":    {`UPDATE app.session_families SET revoked_at = now() WHERE user_id = $1`, ErrUnauthenticated},
		"inactive user":     {`UPDATE app.users SET is_active = false WHERE id = $1`, ErrUnauthenticated},
		"auth version bump": {`UPDATE app.users SET auth_version = auth_version + 1 WHERE id = $1`, ErrSessionExpired},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			db := openAuthTestDB(t)
			defer db.Close()

			user := createAuthTestUser(t, db)
			config := testSessionConfig()
			config.GracePeriod = time.Minute
			config.RotationInterval = -time.Second
			service := newTestService(db, config)
			oldID := loginUser(t, service, user)
			rotated, err := service.Authenticate(context.Background(), oldID)
			if err != nil {
				t.Fatalf("rotate session: %v", err)
			}
			service.sessionConfig.RotationInterval = time.Hour

			if _, err := db.Exec(context.Background(), tc.query, user.ID); err != nil {
				t.Fatalf("invalidate: %v", err)
			}
			for label, id := range map[string]string{"grace": oldID, "current": rotated.SessionID} {
				if _, err := service.Authenticate(context.Background(), id); !errors.Is(err, tc.want) {
					t.Fatalf("%s token: err = %v, want %v", label, err, tc.want)
				}
			}
		})
	}
}

func TestSessionIdleTimeout(t *testing.T) {
	db := openAuthTestDB(t)
	defer db.Close()

	user := createAuthTestUser(t, db)
	config := testSessionConfig()
	config.IdleTimeout = 100 * time.Millisecond
	service := newTestService(db, config)
	id := loginUser(t, service, user)

	time.Sleep(200 * time.Millisecond)
	if _, err := service.Authenticate(context.Background(), id); !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("expected idle timeout, got %v", err)
	}
}

func TestConcurrentAuthenticateRotatesOnce(t *testing.T) {
	db := openAuthTestDB(t)
	defer db.Close()

	user := createAuthTestUser(t, db)
	config := testSessionConfig()
	config.GracePeriod = time.Minute
	config.RotationInterval = -time.Second
	service := newTestService(db, config)
	oldID := loginUser(t, service, user)
	familyID := familyOf(t, db, user)

	const workers = 4
	var wg sync.WaitGroup
	start := make(chan struct{})
	ids := make([]string, workers)
	errs := make([]error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			result, err := service.Authenticate(context.Background(), oldID)
			errs[i] = err
			if err == nil {
				ids[i] = result.SessionID
			}
		}(i)
	}
	close(start)
	wg.Wait()

	for i := 0; i < workers; i++ {
		if errs[i] != nil {
			t.Fatalf("worker %d: %v", i, errs[i])
		}
		if ids[i] == oldID || ids[i] != ids[0] {
			t.Fatalf("worker %d returned a different or stale ID", i)
		}
	}
	if n := countTokens(t, db, familyID, "current"); n != 1 {
		t.Fatalf("current rows = %d, want 1", n)
	}
}

func TestLoadDuration(t *testing.T) {
	t.Setenv("TEST_SESSION_DURATION", "5m")

	got := LoadDuration("TEST_SESSION_DURATION", time.Minute)
	if got != 5*time.Minute {
		t.Fatalf("LoadDuration() = %v, want 5m", got)
	}
}

func TestLoadDurationUsesDefaultWhenUnset(t *testing.T) {
	t.Setenv("TEST_SESSION_DURATION", "")

	want := 10 * time.Minute
	got := LoadDuration("TEST_SESSION_DURATION", want)

	if got != want {
		t.Fatalf("LoadDuration() = %v, want %v", got, want)
	}
}

func TestValidatePassword(t *testing.T) {
	tests := []struct {
		name     string
		password string
		wantErr  bool
	}{
		{
			name:     "strong password",
			password: "StrongPassword123!",
			wantErr:  false,
		},
		{
			name:     "empty password",
			password: "",
			wantErr:  true,
		},
		{
			name:     "short password",
			password: "short",
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errs := ValidatePassword(tt.password)
			if gotErr := len(errs) > 0; gotErr != tt.wantErr {
				t.Fatalf("ValidatePassword() errors = %v, wantErr %v", errs, tt.wantErr)
			}
		})
	}
}
