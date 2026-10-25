package connections

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestCheckFactsRequiresSupportedSafeIdentity(t *testing.T) {
	good := sessionFacts{Database: "target", SessionUser: "target_executor", Version: 160000, Encoding: "UTF8", SystemID: "123", DatabaseOID: "456"}
	cfg := targetConfig{Database: "target"}
	for _, tc := range []struct {
		name   string
		change func(*sessionFacts)
		valid  bool
	}{{"pg16 minimum", func(*sessionFacts) {}, true}, {"pg16 latest", func(f *sessionFacts) { f.Version = 169999 }, true}, {"wrong role", func(f *sessionFacts) { f.SessionUser = "private_other" }, false}, {"wrong database", func(f *sessionFacts) { f.Database = "private_other" }, false}, {"recovery", func(f *sessionFacts) { f.Recovery = true }, false}, {"wrong encoding", func(f *sessionFacts) { f.Encoding = "LATIN1" }, false}, {"old server", func(f *sessionFacts) { f.Version = 159999 }, false}, {"new server", func(f *sessionFacts) { f.Version = 170000 }, false}, {"unsafe attributes", func(f *sessionFacts) { f.Unsafe = true }, false}} {
		t.Run(tc.name, func(t *testing.T) {
			facts := good
			tc.change(&facts)
			err := checkFacts("executor", facts, cfg, "target_executor")
			if tc.valid {
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, ErrTargetUnavailable) || strings.Contains(err.Error(), "private_other") {
				t.Fatalf("unsafe or unsanitized result: %v", err)
			}
		})
	}
}

func TestCredentialConfigRoundTripAndErrorSecrecy(t *testing.T) {
	cfg := targetConfig{Host: "localhost", Port: 5432, Database: "db name", SSLMode: "require"}
	cred := credential{User: "viewer@name", Password: "secret:@ /?&"}
	cc, err := connConfig(cfg, cred)
	if err != nil {
		t.Fatal(err)
	}
	if cc.User != cred.User || cc.Password != cred.Password || cc.Database != cfg.Database || cc.ConnectTimeout != 5*time.Second {
		t.Fatal("connection parameters corrupted or timeout omitted")
	}
	cfg.SSLMode = "private_invalid_sslmode"
	_, err = connConfig(cfg, cred)
	if !errors.Is(err, ErrInvalidInput) || strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), cred.Password) {
		t.Fatalf("unsafe config error: %v", err)
	}
	exec, prep := "target_executor", "target_preparer"
	raw, err := json.Marshal(Summary{ID: uuid.New(), Username: "viewer", ExecutorUsername: &exec, PreparerUsername: &prep, ExecutionReady: true})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	json.Unmarshal(raw, &fields)
	if fields["executor_username"] != exec || fields["execution_ready"] != true {
		t.Fatal(string(raw))
	}
	for key := range fields {
		if strings.Contains(key, "password") || strings.Contains(key, "ciphertext") || strings.Contains(key, "key_id") {
			t.Fatalf("credential field exposed: %s", raw)
		}
	}
	app := fiber.New()
	app.Get("/", func(c *fiber.Ctx) error { return mapError(c, fmt.Errorf("%w: private password", ErrTargetUnavailable)) })
	res, err := app.Test(httptest.NewRequest("GET", "/", nil))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != 502 || strings.Contains(string(body), "private") || strings.Contains(string(body), "password") {
		t.Fatalf("unsafe response %d %s", res.StatusCode, body)
	}
}

func TestVerifyBoundedSanitizesFailureAndRequiresMarker(t *testing.T) {
	marker := uuid.New()
	for _, tc := range []struct {
		name    string
		marker  uuid.UUID
		err     error
		success bool
	}{{"success", marker, nil, true}, {"empty marker", uuid.Nil, nil, false}, {"driver failure", uuid.Nil, errors.New("private password"), false}, {"cancelled", uuid.Nil, context.Canceled, false}, {"known unavailable", uuid.Nil, unverified("role mismatch"), false}} {
		t.Run(tc.name, func(t *testing.T) {
			r := NewRepository(nil, "secret")
			r.verify = func(ctx context.Context, _ targetConfig) (uuid.UUID, error) {
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) > 20*time.Second {
					t.Fatal("verification not bounded")
				}
				return tc.marker, tc.err
			}
			got, err := r.verifyBounded(context.Background(), targetConfig{})
			if tc.success {
				if err != nil || got != marker {
					t.Fatalf("%s %v", got, err)
				}
			} else if got != uuid.Nil || !errors.Is(err, ErrTargetUnavailable) || strings.Contains(err.Error(), "private") {
				t.Fatalf("%s %v", got, err)
			}
		})
	}
}

func TestSQLStateNeverReturnsDriverMessage(t *testing.T) {
	err := fmt.Errorf("wrapped: %w", &pgconn.PgError{Code: "42501", Message: "private password", Detail: "private DSN"})
	if got := sqlState(err); got != "42501" {
		t.Fatal(got)
	}
	if got := sqlState(errors.New("private client error")); got != "client" {
		t.Fatal(got)
	}
}

func TestAcquireFailureReleasesReservation(t *testing.T) {
	for _, stage := range []string{"load", "open roles", "cancelled connection"} {
		t.Run(stage, func(t *testing.T) {
			m, id := fakeManager(true), uuid.New()
			t.Cleanup(m.Close)
			ctx := context.Background()
			want := ErrTargetUnavailable
			switch stage {
			case "load":
				m.load = func(context.Context, uuid.UUID) (targetConfig, *uuid.UUID, error) {
					return targetConfig{}, nil, ErrNotFound
				}
				want = ErrNotFound
			case "open roles":
				m.openRoles = func(targetConfig) (rolePools, error) { return rolePools{}, errors.New("private password") }
			case "cancelled connection":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			lease, err := m.Acquire(ctx, id)
			if lease != nil || !errors.Is(err, want) {
				t.Fatalf("lease=%v err=%v want %v", lease, err, want)
			}
			if m.ActiveLeases(id) != 0 {
				t.Fatal("failed acquisition leaked active reservation")
			}
			resume, err := m.Quiesce(id)
			if err != nil {
				t.Fatalf("failed acquisition left maintenance blocked: %v", err)
			}
			resume(false)
		})
	}
}

func TestViewerPoolCacheInvalidationAndClose(t *testing.T) {
	m, id := fakeManager(true), uuid.New()
	opens := 0
	m.openViewer = func(targetConfig) (*sql.DB, error) { opens++; return sql.OpenDB(fakeConnector{}), nil }
	t.Cleanup(m.Close)
	first, err := m.Open(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	same, err := m.Open(context.Background(), id)
	if err != nil || same != first || opens != 1 {
		t.Fatal("viewer pool not reused")
	}
	m.Invalidate(id)
	if _, err := first.Conn(context.Background()); err == nil {
		t.Fatal("invalidated viewer pool still open")
	}
	next, err := m.Open(context.Background(), id)
	if err != nil || next == first || opens != 2 {
		t.Fatal("viewer pool not refreshed")
	}
	m.Close()
	if _, err := next.Conn(context.Background()); err == nil {
		t.Fatal("manager close left viewer pool open")
	}
	if _, err := m.Open(context.Background(), id); !errors.Is(err, ErrClosed) {
		t.Fatalf("open after close: %v", err)
	}
	if _, err := m.Quiesce(id); !errors.Is(err, ErrClosed) {
		t.Fatalf("quiesce after close: %v", err)
	}
}

func TestViewerPoolLoadAndOpenFailuresAreRetryable(t *testing.T) {
	for _, stage := range []string{"load", "open"} {
		t.Run(stage, func(t *testing.T) {
			m, id := fakeManager(true), uuid.New()
			t.Cleanup(m.Close)
			originalLoad := m.load
			fail := true
			m.load = func(ctx context.Context, id uuid.UUID) (targetConfig, *uuid.UUID, error) {
				if stage == "load" && fail {
					return targetConfig{}, nil, ErrNotFound
				}
				return originalLoad(ctx, id)
			}
			m.openViewer = func(targetConfig) (*sql.DB, error) {
				if stage == "open" && fail {
					return nil, ErrTargetUnavailable
				}
				return sql.OpenDB(fakeConnector{}), nil
			}
			db, err := m.Open(context.Background(), id)
			if db != nil || err == nil {
				t.Fatal("failed open produced pool")
			}
			fail = false
			db, err = m.Open(context.Background(), id)
			if err != nil || db == nil {
				t.Fatalf("failed to recover: %v", err)
			}
		})
	}
}

func TestAbortedMaintenanceKeepsExistingGeneration(t *testing.T) {
	m, id := fakeManager(true), uuid.New()
	t.Cleanup(m.Close)
	lease, err := m.Acquire(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	prep := lease.Preparer
	lease.Release()
	resume, err := m.Quiesce(id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Quiesce(id); !errors.Is(err, ErrBusy) {
		t.Fatalf("concurrent maintenance admitted: %v", err)
	}
	resume(false)
	resume(true)
	next, err := m.Acquire(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Release()
	if next.Preparer != prep {
		t.Fatal("aborted/idempotent maintenance invalidated generation")
	}
}
