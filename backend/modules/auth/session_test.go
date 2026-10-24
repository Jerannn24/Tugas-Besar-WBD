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
		_, _ = db.Exec(ctx, `DELETE FROM app.session_families WHERE user_id = $1`, user.ID)
		_, _ = db.Exec(ctx, `DELETE FROM app.users WHERE id = $1`, user.ID)
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

func TestSessionLifecycle(t *testing.T) {
	db := openAuthTestDB(t)
	defer db.Close()

	user := createAuthTestUser(t, db)
	config := testSessionConfig()
	service := newTestService(db, config)

	login, err := service.createSession(context.Background(), user)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if login.SessionID == "" {
		t.Fatal("expected session ID")
	}

	authenticated, err := service.Authenticate(context.Background(), login.SessionID)
	if err != nil {
		t.Fatalf("authenticate session: %v", err)
	}
	if authenticated.SessionID != login.SessionID {
		t.Fatalf("expected unchanged session ID, got %q", authenticated.SessionID)
	}

	if err := service.Logout(context.Background(), login.SessionID); err != nil {
		t.Fatalf("logout session: %v", err)
	}
	if _, err := service.Authenticate(context.Background(), login.SessionID); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("expected revoked session to be unauthenticated, got %v", err)
	}
}

func TestSessionRotationGraceAndAbsoluteExpiry(t *testing.T) {
	db := openAuthTestDB(t)
	defer db.Close()

	user := createAuthTestUser(t, db)
	config := testSessionConfig()
	config.RotationInterval = -time.Second
	config.AbsoluteLifetime = 100 * time.Millisecond
	service := newTestService(db, config)

	login, err := service.createSession(context.Background(), user)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	rotated, err := service.Authenticate(context.Background(), login.SessionID)
	if err != nil {
		t.Fatalf("rotate session: %v", err)
	}
	if rotated.SessionID == login.SessionID {
		t.Fatal("expected session rotation")
	}

	if _, err := service.Authenticate(context.Background(), login.SessionID); err != nil {
		t.Fatalf("old session should be valid during grace period: %v", err)
	}

	time.Sleep(config.GracePeriod + 20*time.Millisecond)
	if _, err := service.Authenticate(context.Background(), login.SessionID); !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("expected old session grace expiry, got %v", err)
	}

	time.Sleep(100 * time.Millisecond)
	if _, err := service.Authenticate(context.Background(), rotated.SessionID); !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("expected absolute expiry after rotation, got %v", err)
	}
}

func TestConcurrentSessionRotationOnlyOneSucceeds(t *testing.T) {
	db := openAuthTestDB(t)
	defer db.Close()

	user := createAuthTestUser(t, db)
	config := testSessionConfig()
	service := newTestService(db, config)
	login, err := service.createSession(context.Background(), user)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	session, _, err := service.sessions.Find(
		context.Background(),
		hashSessionID(login.SessionID),
		time.Now().UTC(),
		config.IdleTimeout,
	)
	if err != nil {
		t.Fatalf("find session: %v", err)
	}

	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			newID, generationErr := newSessionID()
			if generationErr != nil {
				results <- generationErr
				return
			}
			ciphertext, generationErr := encryptSessionID(newID, config.EncryptionKey)
			if generationErr != nil {
				results <- generationErr
				return
			}
			results <- service.sessions.Rotate(
				context.Background(),
				session.FamilyID,
				hashSessionID(login.SessionID),
				hashSessionID(newID),
				ciphertext,
				time.Now().UTC(),
				time.Now().UTC().Add(config.GracePeriod),
				session.AbsoluteExpiresAt,
			)
		}()
	}
	wg.Wait()
	close(results)

	successes := 0
	for rotationErr := range results {
		if rotationErr == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("expected exactly one successful rotation, got %d", successes)
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
