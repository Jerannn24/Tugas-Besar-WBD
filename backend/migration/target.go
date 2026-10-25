package migration

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/binary"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/Jerannn24/Tugas-Besar-WBD.git/history"
)

type TargetLease struct {
	Executor *sql.Conn
	Preparer *sql.DB
	Release func()
}

type targetSession struct {
	lease           *TargetLease
	key             int64
	locked, tainted bool
	tx              *sql.Tx
}

type receipt struct {
	ID, TargetID, DatabaseID, Version, ActorID, XID string
	Hash                                            []byte
}

type outcome struct {
	status  string
	receipt *receipt
	failure *history.ExecutionError
}

func unknown(code, message string) outcome {
	return outcome{status: "unknown", failure: &history.ExecutionError{Code: code, Message: message}}
}

func (h *Handler) openTarget(ctx context.Context, databaseID, targetID string) (*targetSession, error) {
	lease, err := h.d.Acquire(ctx, databaseID)
	if err != nil || lease == nil || lease.Executor == nil || lease.Preparer == nil || lease.Release == nil {
		if lease != nil {
			if lease.Executor != nil {
				_ = lease.Executor.Raw(func(any) error { return driver.ErrBadConn })
				_ = lease.Executor.Close()
			}
			if lease.Release != nil {
				lease.Release()
			}
		}
		return nil, fault(502, "TARGET_UNAVAILABLE", "Target connection is unavailable")
	}
	s := &targetSession{lease: lease}
	ok := false
	defer func() {
		if !ok {
			s.tainted = true
			s.close(h.d.Timeout)
		}
	}()
	var marker, role, preparerDB string
	var recovery bool
	err = lease.Preparer.QueryRowContext(ctx, `SELECT physical_target_id::text,executor_role::text,current_database(),pg_catalog.pg_is_in_recovery() FROM wbd_auth.target_marker WHERE singleton`).Scan(&marker, &role, &preparerDB, &recovery)
	if err != nil || recovery || marker != targetID {
		return nil, fault(502, "TARGET_UNAVAILABLE", "Target identity could not be verified")
	}
	var executorDB, sessionRole, currentRole, encoding, clientEncoding string
	var version int
	var unsafe, dirty bool
	err = lease.Executor.QueryRowContext(ctx, `SELECT current_database(),session_user::text,current_user::text,
current_setting('server_version_num')::int,current_setting('server_encoding'),current_setting('client_encoding'),pg_catalog.pg_is_in_recovery(),
r.rolsuper OR r.rolcreatedb OR r.rolcreaterole OR r.rolreplication OR r.rolbypassrls OR EXISTS(SELECT 1 FROM pg_catalog.pg_auth_members WHERE member=r.oid),
EXISTS(SELECT 1 FROM pg_catalog.pg_locks WHERE pid=pg_catalog.pg_backend_pid() AND locktype='advisory')
FROM pg_catalog.pg_roles r WHERE rolname=session_user`).Scan(&executorDB, &sessionRole, &currentRole, &version, &encoding, &clientEncoding, &recovery, &unsafe, &dirty)
	if err != nil || recovery || unsafe || dirty || executorDB != preparerDB || sessionRole != role || currentRole != role || encoding != "UTF8" || clientEncoding != "UTF8" || version < 160000 || version >= 170000 {
		return nil, fault(502, "TARGET_UNAVAILABLE", "Executor connection is not ready")
	}
	var preparerRole string
	err = lease.Preparer.QueryRowContext(ctx, `SELECT session_user::text`).Scan(&preparerRole)
	if err != nil || !strings.HasSuffix(role, "_executor") || preparerRole != strings.TrimSuffix(role, "_executor")+"_preparer" {
		return nil, fault(502, "TARGET_UNAVAILABLE", "Preparer role could not be verified")
	}
	hash := sha256.Sum256([]byte("migration:" + marker))
	s.key = int64(binary.BigEndian.Uint64(hash[:8]))
	if _, err := lease.Executor.ExecContext(ctx, `SELECT pg_catalog.pg_advisory_lock($1)`, s.key); err != nil {
		return nil, fault(409, "DATABASE_BUSY", "Target execution lock is unavailable")
	}
	s.locked, ok = true, true
	return s, nil
}

func (s *targetSession) begin(ctx context.Context, timeout time.Duration) error {
	tx, err := s.lease.Executor.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		s.tainted = true
		return fault(502, "TARGET_UNAVAILABLE", "Target transaction could not be started")
	}
	s.tx = tx
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < timeout {
		timeout = time.Until(deadline)
	}
	milliseconds := strconv.FormatInt(max(int64(1), timeout.Milliseconds()), 10)
	_, err = tx.ExecContext(ctx, `SELECT pg_catalog.set_config('search_path','pg_catalog,managed,pg_temp',true),
pg_catalog.set_config('standard_conforming_strings','on',true),pg_catalog.set_config('statement_timeout',$1,true)`, milliseconds)
	if err != nil {
		return fault(502, "TARGET_UNAVAILABLE", "Target settings could not be applied")
	}
	return nil
}

func (s *targetSession) run(ctx context.Context, a attempt, statements []string) outcome {
	var xid string
	if err := s.tx.QueryRowContext(ctx, `SELECT pg_catalog.pg_current_xact_id()::text`).Scan(&xid); err != nil {
		return s.abort("TARGET_PREPARATION_FAILED", "Target transaction could not be prepared")
	}
	version, err := numericVersion(a.Version)
	if err != nil || version < 1 {
		return s.abort("TARGET_PREPARATION_FAILED", "Execution version is invalid")
	}
	capability := make([]byte, 32)
	if _, err := rand.Read(capability); err != nil {
		return s.abort("TARGET_PREPARATION_FAILED", "Execution authorization could not be prepared")
	}
	defer clear(capability)
	digest := sha256.Sum256(capability)
	_, err = s.lease.Preparer.ExecContext(ctx, `INSERT INTO wbd_auth.execution_tickets
(execution_id,physical_target_id,database_id,target_xid,version,sql_sha256,actor_id,capability_sha256)
VALUES($1::uuid,$2::uuid,$3::uuid,$4::xid8,$5,$6,$7::uuid,$8)`, a.ID, a.TargetID, a.DatabaseID, xid, version, a.Hash, a.ExecutedBy.ID, digest[:])
	if err != nil {
		return s.abort("TARGET_PREPARATION_FAILED", "Execution authorization could not be recorded")
	}
	if err := execStatements(ctx, s.tx, statements); err != nil {
		return s.abort("SQL_FAILED", "SQL execution failed")
	}
	if _, err := s.tx.ExecContext(ctx, `SELECT wbd_auth.record_receipt($1::uuid,$2::bytea)`, a.ID, capability); err != nil {
		return s.abort("RECEIPT_FAILED", "Execution receipt could not be recorded")
	}
	err = s.tx.Commit()
	s.tx = nil
	if err != nil {
		s.tainted = true
		return unknown("COMMIT_UNCONFIRMED", "Target commit could not be confirmed")
	}
	r, found, err := s.readReceipt(ctx, a.ID)
	if err != nil || !found {
		return unknown("RECEIPT_UNCONFIRMED", "Execution receipt could not be confirmed")
	}
	if r.XID != xid {
		return unknown("RECEIPT_MISMATCH", "Execution receipt has a different transaction")
	}
	return outcome{status: "succeeded", receipt: &r}
}

func (s *targetSession) abort(code, message string) outcome {
	if s.tx == nil {
		return unknown("ROLLBACK_UNCONFIRMED", "Target rollback could not be confirmed")
	}
	err := s.tx.Rollback()
	s.tx = nil
	if err != nil {
		s.tainted = true
		return unknown("ROLLBACK_UNCONFIRMED", "Target rollback could not be confirmed")
	}
	return outcome{status: "failed", failure: &history.ExecutionError{Code: code, Message: message}}
}

func (s *targetSession) readReceipt(ctx context.Context, executionID string) (receipt, bool, error) {
	var r receipt
	err := s.lease.Preparer.QueryRowContext(ctx, `SELECT execution_id::text,physical_target_id::text,database_id::text,version::text,sql_sha256,actor_id::text,target_xid::text FROM wbd_auth.execution_receipts WHERE execution_id=$1`, executionID).Scan(&r.ID, &r.TargetID, &r.DatabaseID, &r.Version, &r.Hash, &r.ActorID, &r.XID)
	if errors.Is(err, sql.ErrNoRows) {
		return receipt{}, false, nil
	}
	return r, err == nil, err
}

func matchingReceipt(a attempt, r receipt) bool {
	return r.ID == a.ID && r.TargetID == a.TargetID && r.DatabaseID == a.DatabaseID && r.Version == a.Version && r.ActorID == a.ExecutedBy.ID && r.XID != "" && len(r.Hash) == 32 && bytes.Equal(r.Hash, a.Hash)
}

func (s *targetSession) close(timeout time.Duration) {
	if s.tx != nil {
		if err := s.tx.Rollback(); err != nil {
			s.tainted = true
		}
		s.tx = nil
	}
	if s.locked && !s.tainted {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		var unlocked bool
		err := s.lease.Executor.QueryRowContext(ctx, `SELECT pg_catalog.pg_advisory_unlock($1)`, s.key).Scan(&unlocked)
		cancel()
		if err != nil || !unlocked {
			s.tainted = true
		}
	}
	if s.tainted {
		_ = s.lease.Executor.Raw(func(any) error { return driver.ErrBadConn })
	}
	_ = s.lease.Executor.Close()
	s.lease.Release()
}

// execStatements menjalankan setiap statement secara berurutan pada transaksi
// yang sama dan berhenti pada kegagalan pertama. Setiap statement dikirim
// sendiri karena protokol extended pgx menolak banyak perintah dalam satu teks.
func execStatements(ctx context.Context, tx *sql.Tx, statements []string) error {
	if len(statements) == 0 {
		return errors.New("no statements")
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

// slicesOf memastikan statements berurutan, tidak tumpang tindih, dan setiap
// elemen adalah potongan persis dari teks SQL yang di-hash.
func slicesOf(whole string, statements []string) bool {
	pos := 0
	for _, s := range statements {
		i := strings.Index(whole[pos:], s)
		if s == "" || i < 0 {
			return false
		}
		pos += i + len(s)
	}
	return len(statements) > 0
}
