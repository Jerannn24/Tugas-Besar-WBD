package jobs

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

type checkpointEnv struct {
	exec, prep, viewer, cleanup *sql.DB
	target string
}

func checkpointSetup(t *testing.T) checkpointEnv {
	t.Helper()
	urls := []string{
		os.Getenv("MIGRATION_TEST_TARGET_EXECUTOR_URL"),
		os.Getenv("MIGRATION_TEST_TARGET_PREPARER_URL"),
		os.Getenv("MIGRATION_TEST_TARGET_VIEWER_URL"),
		os.Getenv("MIGRATION_TEST_TARGET_CLEANUP_URL"),
	}
	missing := 0
	for _, url := range urls {
		if url == "" { missing++ }
	}
	if missing != 0 {
		if missing == len(urls) && os.Getenv("TEST_INTEGRATION_REQUIRED") != "1" {
			t.Skip("target database configuration not set")
		}
		t.Fatal("executor, preparer, viewer and cleanup target database URLs are required")
	}
	dbs := make([]*sql.DB, len(urls))
	for i, url := range urls {
		db, err := sql.Open("pgx", url)
		if err != nil { t.Fatal(err) }
		dbs[i] = db
		t.Cleanup(func() {
			if err := db.Close(); err != nil { t.Errorf("close target database: %v", err) }
		})
	}
	var target string
	if err := dbs[1].QueryRow("SELECT physical_target_id::text FROM wbd_auth.target_marker WHERE singleton").Scan(&target); err != nil { t.Fatal(err) }
	return checkpointEnv{dbs[0], dbs[1], dbs[2], dbs[3], target}
}

func checkpointTicket(t *testing.T, ctx context.Context, e checkpointEnv, tx *sql.Tx, id uuid.UUID, index int, direction string) ([]byte, string) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, table := range []string{"batch_checkpoints", "batch_tickets"} {
			if _, err := e.cleanup.ExecContext(ctx, "DELETE FROM wbd_auth."+table+" WHERE execution_id = $1", id); err != nil {
				t.Errorf("cleanup %s for %s: %v", table, id, err)
			}
		}
	})
	var xid string
	if err := tx.QueryRowContext(ctx, "SELECT pg_current_xact_id()::text").Scan(&xid); err != nil { t.Fatal(err) }
	capability := make([]byte, 32)
	if _, err := rand.Read(capability); err != nil { t.Fatal(err) }
	hash := sha256.Sum256(capability)
	_, err := e.prep.ExecContext(ctx, `INSERT INTO wbd_auth.batch_tickets
		(execution_id, physical_target_id, target_xid, batch_index, direction, capability_sha256)
		VALUES ($1, $2, $3::xid8, $4, $5, $6)`, id, e.target, xid, index, direction, hash[:])
	if err != nil { t.Fatalf("issue ticket: %v", err) }
	return capability, xid
}

func checkpointState(t *testing.T, err error, state string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != state {
		t.Fatalf("expected SQLSTATE %s, got %v", state, err)
	}
}

func TestBatchCheckpoint_CommitAndRollbackAtomicity(t *testing.T) {
	e := checkpointSetup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	table := "batch_items_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	_, err := e.exec.ExecContext(ctx, fmt.Sprintf("CREATE TABLE managed.%s (id bigint PRIMARY KEY, status text NOT NULL)", table))
	if err != nil { t.Fatal(err) }
	t.Cleanup(func() {
		if _, err := e.exec.ExecContext(context.Background(), fmt.Sprintf("DROP TABLE IF EXISTS managed.%s", table)); err != nil {
			t.Errorf("drop managed table %s: %v", table, err)
		}
	})
	if _, err := e.exec.ExecContext(ctx, fmt.Sprintf("INSERT INTO managed.%s VALUES (1, 'pending'), (2, 'pending')", table)); err != nil { t.Fatal(err) }
	for _, commit := range []bool{true, false} {
		tx, err := e.exec.BeginTx(ctx, nil)
		if err != nil { t.Fatal(err) }
		id := uuid.New()
		capability, xid := checkpointTicket(t, ctx, e, tx, id, 0, "up")
		row := 1
		if !commit { row = 2 }
		res, err := tx.ExecContext(ctx, fmt.Sprintf("UPDATE managed.%s SET status = 'processed' WHERE id = $1", table), row)
		if err != nil { t.Fatal(err) }
		count, err := res.RowsAffected()
		if err != nil || count != 1 { t.Fatalf("update count %d: %v", count, err) }
		_, err = tx.ExecContext(ctx, "SELECT wbd_auth.record_checkpoint($1, 0, 'up', $2, $3)", id, count, capability)
		if err != nil { t.Fatalf("record checkpoint: %v", err) }
		if commit { err = tx.Commit() } else { err = tx.Rollback() }
		if err != nil { t.Fatal(err) }
		var status string
		if err := e.exec.QueryRowContext(ctx, fmt.Sprintf("SELECT status FROM managed.%s WHERE id = $1", table), row).Scan(&status); err != nil { t.Fatal(err) }
		var storedXID, target, direction string
		var storedIndex int
		var storedRows int64
		err = e.prep.QueryRowContext(ctx, `SELECT target_xid::text, physical_target_id::text, direction, batch_index, rows_affected
			FROM wbd_auth.batch_checkpoints WHERE execution_id = $1`, id).Scan(&storedXID, &target, &direction, &storedIndex, &storedRows)
		if commit {
			if err != nil || status != "processed" || storedXID != xid || target != e.target || direction != "up" || storedIndex != 0 || storedRows != 1 {
				t.Fatalf("committed checkpoint mismatch: status=%s xid=%s expected=%s target=%s direction=%s index=%d rows=%d err=%v", status, storedXID, xid, target, direction, storedIndex, storedRows, err)
			}
		} else if !errors.Is(err, sql.ErrNoRows) || status != "pending" {
			t.Fatalf("rollback mismatch: status=%s checkpoint error=%v", status, err)
		}
	}
}

func TestBatchCheckpoint_DuplicateRetryRollsBack(t *testing.T) {
	e := checkpointSetup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	table := "batch_items_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	if _, err := e.exec.ExecContext(ctx, fmt.Sprintf("CREATE TABLE managed.%s (id bigint PRIMARY KEY, status text NOT NULL)", table)); err != nil { t.Fatal(err) }
	t.Cleanup(func() {
		if _, err := e.exec.ExecContext(context.Background(), fmt.Sprintf("DROP TABLE IF EXISTS managed.%s", table)); err != nil {
			t.Errorf("drop managed table %s: %v", table, err)
		}
	})
	if _, err := e.exec.ExecContext(ctx, fmt.Sprintf("INSERT INTO managed.%s VALUES (1, 'pending'), (2, 'pending')", table)); err != nil { t.Fatal(err) }
	id := uuid.New()
	first, err := e.exec.BeginTx(ctx, nil)
	if err != nil { t.Fatal(err) }
	defer first.Rollback()
	capability, xid := checkpointTicket(t, ctx, e, first, id, 0, "up")
	if _, err := first.ExecContext(ctx, fmt.Sprintf("UPDATE managed.%s SET status = 'processed' WHERE id = 1", table)); err != nil { t.Fatal(err) }
	if _, err := first.ExecContext(ctx, "SELECT wbd_auth.record_checkpoint($1, 0, 'up', 1, $2)", id, capability); err != nil { t.Fatal(err) }
	if err := first.Commit(); err != nil { t.Fatal(err) }

	// Ticket keys are unique independently of checkpoint keys.
	hash := sha256.Sum256(capability)
	_, err = e.prep.ExecContext(ctx, `INSERT INTO wbd_auth.batch_tickets
		(execution_id, physical_target_id, target_xid, batch_index, direction, capability_sha256)
		VALUES ($1, $2, $3::xid8, 0, 'up', $4)`, id, e.target, xid, hash[:])
	checkpointState(t, err, "23505")

	// A new batch receives its own ticket; the first batch's ticket remains
	// bound to the original transaction and cannot authorize a retry.
	second, err := e.exec.BeginTx(ctx, nil)
	if err != nil { t.Fatal(err) }
	defer second.Rollback()
	secondCapability, _ := checkpointTicket(t, ctx, e, second, id, 1, "up")
	if _, err := second.ExecContext(ctx, fmt.Sprintf("UPDATE managed.%s SET status = 'processed' WHERE id = 2", table)); err != nil { t.Fatal(err) }
	if _, err := second.ExecContext(ctx, "SELECT wbd_auth.record_checkpoint($1, 1, 'up', 1, $2)", id, secondCapability); err != nil { t.Fatalf("record second batch: %v", err) }
	_, err = second.ExecContext(ctx, "SELECT wbd_auth.record_checkpoint($1, 0, 'up', 1, $2)", id, capability)
	checkpointState(t, err, "42501")
	if err := second.Rollback(); err != nil { t.Fatal(err) }

	var firstStatus, secondStatus string
	if err := e.exec.QueryRowContext(ctx, fmt.Sprintf("SELECT (SELECT status FROM managed.%s WHERE id = 1), (SELECT status FROM managed.%s WHERE id = 2)", table, table)).Scan(&firstStatus, &secondStatus); err != nil { t.Fatal(err) }
	if firstStatus != "processed" || secondStatus != "pending" {
		t.Fatalf("duplicate rollback: first status=%s second status=%s", firstStatus, secondStatus)
	}
	var storedXID, target, direction string
	var index int
	var rows, count int64
	if err := e.prep.QueryRowContext(ctx, `SELECT target_xid::text, physical_target_id::text, direction, batch_index, rows_affected,
		(SELECT count(*) FROM wbd_auth.batch_checkpoints WHERE execution_id = $1)
		FROM wbd_auth.batch_checkpoints WHERE execution_id = $1 AND direction = 'up' AND batch_index = 0`, id).
		Scan(&storedXID, &target, &direction, &index, &rows, &count); err != nil { t.Fatal(err) }
	if storedXID != xid || target != e.target || direction != "up" || index != 0 || rows != 1 || count != 1 {
		t.Fatalf("original checkpoint changed: xid=%s expected=%s target=%s direction=%s index=%d rows=%d count=%d", storedXID, xid, target, direction, index, rows, count)
	}
}

func TestBatchCheckpoint_Authorization(t *testing.T) {
	e := checkpointSetup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, which := range []string{"missing", "execution", "index", "direction", "capability", "transaction"} {
		t.Run(which, func(t *testing.T) {
			tx, err := e.exec.BeginTx(ctx, nil)
			if err != nil { t.Fatal(err) }
			defer tx.Rollback()
			id := uuid.New()
			capability, _ := checkpointTicket(t, ctx, e, tx, id, 0, "up")
			callID, index, direction := id, 0, "up"
			switch which {
			case "missing", "execution": callID = uuid.New()
			case "index": index = 1
			case "direction": direction = "down"
			case "capability": capability = make([]byte, 32)
			case "transaction":
				if err := tx.Rollback(); err != nil { t.Fatal(err) }
				tx, err = e.exec.BeginTx(ctx, nil)
				if err != nil { t.Fatal(err) }
				defer tx.Rollback()
			}
			_, err = tx.ExecContext(ctx, "SELECT wbd_auth.record_checkpoint($1, $2, $3, 1, $4)", callID, index, direction, capability)
			checkpointState(t, err, "42501")
		})
	}
}

func TestBatchCheckpoint_MetadataProtection(t *testing.T) {
	e := checkpointSetup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, query := range []string{
		"SELECT * FROM wbd_auth.batch_tickets",
		"INSERT INTO wbd_auth.batch_tickets DEFAULT VALUES",
		"UPDATE wbd_auth.batch_tickets SET batch_index = 0",
		"DELETE FROM wbd_auth.batch_tickets",
		"INSERT INTO wbd_auth.batch_checkpoints DEFAULT VALUES",
		"UPDATE wbd_auth.batch_checkpoints SET rows_affected = 0",
		"DELETE FROM wbd_auth.batch_checkpoints",
	} {
		_, err := e.exec.ExecContext(ctx, query)
		checkpointState(t, err, "42501")
	}
	for _, query := range []string{
		"SELECT * FROM wbd_auth.batch_tickets",
		"UPDATE wbd_auth.batch_tickets SET batch_index = 0",
		"DELETE FROM wbd_auth.batch_tickets",
		"INSERT INTO wbd_auth.batch_checkpoints DEFAULT VALUES",
		"UPDATE wbd_auth.batch_checkpoints SET rows_affected = 0",
		"DELETE FROM wbd_auth.batch_checkpoints",
		"SELECT wbd_auth.record_checkpoint(NULL, 0, 'up', 1, NULL)",
	} {
		_, err := e.prep.ExecContext(ctx, query)
		checkpointState(t, err, "42501")
	}
}

func TestBatchCheckpoint_PrivilegeMatrix(t *testing.T) {
	e := checkpointSetup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, role := range []struct {
		name string
		db *sql.DB
		tickets, checkpoints [4]bool
		execute bool
	}{
		{"executor", e.exec, [4]bool{}, [4]bool{}, true},
		{"preparer", e.prep, [4]bool{false, true, false, false}, [4]bool{true, false, false, false}, false},
		{"viewer", e.viewer, [4]bool{}, [4]bool{}, false},
	} {
		t.Run(role.name, func(t *testing.T) {
			var username string
			if err := role.db.QueryRowContext(ctx, "SELECT current_user").Scan(&username); err != nil { t.Fatal(err) }
			for _, table := range []struct {
				name string
				want [4]bool
			}{
				{"wbd_auth.batch_tickets", role.tickets},
				{"wbd_auth.batch_checkpoints", role.checkpoints},
			} {
				for i, privilege := range []string{"SELECT", "INSERT", "UPDATE", "DELETE"} {
					var got bool
					if err := e.cleanup.QueryRowContext(ctx, "SELECT has_table_privilege($1, $2, $3)", username, table.name, privilege).Scan(&got); err != nil { t.Fatal(err) }
					if got != table.want[i] { t.Errorf("%s %s privilege = %t, want %t", table.name, privilege, got, table.want[i]) }
				}
			}
			var got bool
			if err := e.cleanup.QueryRowContext(ctx, "SELECT has_function_privilege($1, 'wbd_auth.record_checkpoint(uuid,integer,text,bigint,bytea)', 'EXECUTE')", username).Scan(&got); err != nil { t.Fatal(err) }
			if got != role.execute { t.Errorf("record_checkpoint EXECUTE privilege = %t, want %t", got, role.execute) }
		})
	}
}
