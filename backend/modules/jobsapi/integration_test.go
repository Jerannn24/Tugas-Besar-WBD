package jobsapi

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Jerannn24/Tugas-Besar-WBD.git/executioncontract"
	"github.com/Jerannn24/Tugas-Besar-WBD.git/modules/authz"
	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestLiveAccessWithRealAuthorizer(t *testing.T) {
	required := os.Getenv("TEST_INTEGRATION_REQUIRED") == "1"
	refuse := func(msg string) {
		t.Helper()
		if required {
			t.Fatal(msg)
		}
		t.Skip(msg)
	}
	dsn := os.Getenv("AUTHZ_TEST_DATABASE_URL")
	if dsn == "" {
		refuse("AUTHZ_TEST_DATABASE_URL is not set")
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal("AUTHZ_TEST_DATABASE_URL is not a valid connection string")
	}
	if name := config.ConnConfig.Database; !strings.Contains(name, "test") {
		refuse("refusing to write fixtures: database name " + name + " does not contain \"test\"")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal("connect failed")
	}
	defer pool.Close()

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("fixture: %v", err)
		}
	}
	suffix := uuid.NewString()[:8]
	viewer, outsider, inactive := uuid.New(), uuid.New(), uuid.New()
	group, database := uuid.New(), uuid.New()
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM app.groups WHERE id = $1`, group)
		_, _ = pool.Exec(cleanupCtx, `UPDATE app.database_connections SET deleted_at = now() WHERE id = $1`, database)
		_, _ = pool.Exec(cleanupCtx, `UPDATE app.users SET is_active = false WHERE id = ANY($1)`, []uuid.UUID{viewer, outsider, inactive})
	})
	exec(`INSERT INTO app.users (id, username, password_hash, is_admin, is_active) VALUES
		($1, $2, 'x', false, true), ($3, $4, 'x', false, true), ($5, $6, 'x', false, false)`,
		viewer, "jobs_view_"+suffix, outsider, "jobs_none_"+suffix, inactive, "jobs_off_"+suffix)
	exec(`INSERT INTO app.groups (id, name) VALUES ($1, $2)`, group, "jobs_"+suffix)
	exec(`INSERT INTO app.group_memberships (group_id, user_id) VALUES ($1, $2), ($1, $3)`, group, viewer, inactive)
	exec(`INSERT INTO app.database_connections
		(id, name, host, port, database_name, username, password_ciphertext, credential_key_id, ssl_mode, target_key)
		VALUES ($1, $2, 'localhost', 5432, 'db', 'u', '\x01', 'k1', 'disable', $3)`,
		database, "jobs_"+suffix, database.String())
	exec(`INSERT INTO app.group_database_grants (group_id, database_id, access) VALUES ($1, $2, 'view')`, group, database)

	authorizer := authz.NewService(authz.NewRepository(pool))
	tests := []struct {
		name       string
		user       uuid.UUID
		database   uuid.UUID
		path       string
		wantStatus int
		wantCode   string
	}{
		{"view grant list jobs", viewer, database, "/jobs", 200, ""},
		{"view grant get job", viewer, database, "/jobs/" + jobID, 200, ""},
		{"view grant attempts", viewer, database, "/attempts", 200, ""},
		{"no grant", outsider, database, "/jobs", 403, "FORBIDDEN"},
		{"no grant detail", outsider, database, "/jobs/" + jobID, 403, "FORBIDDEN"},
		{"inactive user", inactive, database, "/jobs", 401, "UNAUTHENTICATED"},
		{"database not found", viewer, uuid.New(), "/attempts", 404, "NOT_FOUND"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			jobs := &jobReaderDouble{
				job:  jobView(jobID, execID),
				jobs: executioncontract.JobPage{Items: []executioncontract.JobView{jobView(jobID, execID)}},
			}
			app := fiber.New()
			protected := app.Group("/api", sessionAs(tt.user), authz.ActorMiddleware(authorizer))
			if err := Register(protected, Dependencies{Jobs: jobs, Actions: &actionReaderDouble{}, Authorizer: authorizer}); err != nil {
				t.Fatal(err)
			}
			status, body, raw := do(t, app, "/api/databases/"+tt.database.String()+tt.path)
			if status != tt.wantStatus || errorCode(body) != tt.wantCode {
				t.Fatalf("status=%d code=%q, want %d %q; body %s", status, errorCode(body), tt.wantStatus, tt.wantCode, raw)
			}
			if (len(jobs.calls) > 0) != (tt.wantStatus == 200) {
				t.Fatalf("model calls=%v for status %d", jobs.calls, status)
			}
		})
	}
}
