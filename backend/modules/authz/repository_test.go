package authz

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestEffectiveAccessLive(t *testing.T) {
	dsn := os.Getenv("AUTHZ_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("AUTHZ_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal("connect failed")
	}
	defer pool.Close()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := tx.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("fixture: %v", err)
		}
	}
	suffix := uuid.NewString()[:8]
	userID, groupA, groupB := uuid.New(), uuid.New(), uuid.New()
	exec(`INSERT INTO app.users (id, username, password_hash, is_admin, is_active) VALUES ($1, $2, 'x', false, true)`,
		userID, "authz_"+suffix)
	exec(`INSERT INTO app.groups (id, name) VALUES ($1, $2), ($3, $4)`,
		groupA, "ga_"+suffix, groupB, "gb_"+suffix)
	exec(`INSERT INTO app.group_memberships (group_id, user_id) VALUES ($1, $3), ($2, $3)`, groupA, groupB, userID)

	newConn := func(label string, deleted bool) uuid.UUID {
		id := uuid.New()
		var deletedAt any
		if deleted {
			deletedAt = time.Now()
		}
		exec(`INSERT INTO app.database_connections
			(id, name, host, port, database_name, username, password_ciphertext, credential_key_id, ssl_mode, target_key, deleted_at)
			VALUES ($1, $2, 'localhost', 5432, 'db', 'u', '\x01', 'k1', 'disable', $3, $4)`,
			id, label+"_"+suffix, id.String(), deletedAt)
		return id
	}
	grant := func(group, db uuid.UUID, access string) {
		exec(`INSERT INTO app.group_database_grants (group_id, database_id, access) VALUES ($1, $2, $3)`, group, db, access)
	}

	viewDB := newConn("view", false)
	grant(groupA, viewDB, "view")
	noGrantDB := newConn("nogrant", false)
	deletedDB := newConn("deleted", true)
	grant(groupA, deletedDB, "edit")
	editDB := newConn("edit", false)
	grant(groupA, editDB, "edit")
	bothDB := newConn("both", false)
	grant(groupA, bothDB, "view")
	grant(groupB, bothDB, "edit")

	repo := &Repository{db: tx}
	tests := []struct {
		name    string
		db      uuid.UUID
		want    Access
		wantErr error
	}{
		{"nonexistent", uuid.New(), AccessNone, ErrNotFound},
		{"soft-deleted", deletedDB, AccessNone, ErrNotFound},
		{"no grant", noGrantDB, AccessNone, nil},
		{"view grant", viewDB, AccessView, nil},
		{"edit grant", editDB, AccessEdit, nil},
		{"highest grant wins", bothDB, AccessEdit, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := repo.EffectiveAccess(ctx, userID, tt.db)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("access = %v, want %v", got, tt.want)
			}
		})
	}
}

var _ queryRower = pgx.Tx(nil)
