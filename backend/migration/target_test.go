package migration

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

type targetFixture struct {
	executor, preparer sqlmock.Sqlmock
	lease              *TargetLease
	released           int
}

func newTargetFixture(t *testing.T) *targetFixture {
	t.Helper()
	edb, em := mockDB(t)
	pdb, pm := mockDB(t)
	conn, err := edb.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f := &targetFixture{executor: em, preparer: pm}
	f.lease = &TargetLease{Executor: conn, Preparer: pdb, Release: func() { f.released++ }}
	t.Cleanup(func() { _ = conn.Close() })
	return f
}

func (f *targetFixture) expectOpen() {
	f.preparer.ExpectQuery("SELECT physical_target_id").WillReturnRows(sqlmock.NewRows([]string{"marker", "role", "db", "recovery"}).AddRow(testTargetID, "slot_executor", "target", false))
	f.executor.ExpectQuery("SELECT current_database").WillReturnRows(sqlmock.NewRows([]string{"db", "session", "current", "version", "encoding", "client_encoding", "recovery", "unsafe", "dirty"}).AddRow("target", "slot_executor", "slot_executor", 160004, "UTF8", "UTF8", false, false, false))
	f.preparer.ExpectQuery("SELECT session_user").WillReturnRows(sqlmock.NewRows([]string{"role"}).AddRow("slot_preparer"))
	f.executor.ExpectExec("SELECT pg_catalog.pg_advisory_lock").WithArgs(sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
}

func (f *targetFixture) expectClose() {
	f.executor.ExpectQuery("SELECT pg_catalog.pg_advisory_unlock").WithArgs(sqlmock.AnyArg()).WillReturnRows(sqlmock.NewRows([]string{"unlocked"}).AddRow(true))
}

func (f *targetFixture) expectBegin() {
	f.executor.ExpectBegin()
	f.executor.ExpectExec("SELECT pg_catalog.set_config").WithArgs(sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
}

func testReceipt(a attempt) receipt {
	return receipt{ID: a.ID, TargetID: a.TargetID, DatabaseID: a.DatabaseID, Version: a.Version, ActorID: a.ExecutedBy.ID, Hash: append([]byte{}, a.Hash...), XID: "101"}
}

func receiptRows(r receipt) *sqlmock.Rows {
	return sqlmock.NewRows([]string{"id", "target", "db", "version", "hash", "actor", "xid"}).AddRow(r.ID, r.TargetID, r.DatabaseID, r.Version, r.Hash, r.ActorID, r.XID)
}

func (f *targetFixture) expectTicket(a attempt) {
	f.executor.ExpectQuery("SELECT pg_catalog.pg_current_xact_id").WillReturnRows(sqlmock.NewRows([]string{"xid"}).AddRow("101"))
	f.preparer.ExpectExec("INSERT INTO wbd_auth.execution_tickets").WithArgs(a.ID, a.TargetID, a.DatabaseID, "101", int64(1), a.Hash, a.ExecutedBy.ID, sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
}

func TestTargetRejectsIncompleteLease(t *testing.T) {
	for _, lease := range []*TargetLease{nil, {Release: func() {}}} {
		h := &Handler{d: Dependencies{Timeout: time.Second, Acquire: func(context.Context, string) (*TargetLease, error) { return lease, nil }}}
		_, err := h.openTarget(context.Background(), testDBID, testTargetID)
		assertCode(t, err, "TARGET_UNAVAILABLE")
	}
}

func TestTargetIdentityAndRoleValidation(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		marker                  string
		version                 int
		unsafe, dirty, recovery bool
		role                    string
	}{
		{name: "wrong marker", marker: testDBID, version: 160004, role: "slot_executor"},
		{name: "unsupported version", marker: testTargetID, version: 170000, role: "slot_executor"},
		{name: "superuser", marker: testTargetID, version: 160004, unsafe: true, role: "slot_executor"},
		{name: "dirty session", marker: testTargetID, version: 160004, dirty: true, role: "slot_executor"},
		{name: "standby", marker: testTargetID, version: 160004, recovery: true, role: "slot_executor"},
		{name: "wrong role", marker: testTargetID, version: 160004, role: "other_executor"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newTargetFixture(t)
			f.preparer.ExpectQuery("SELECT physical_target_id").WillReturnRows(sqlmock.NewRows([]string{"marker", "role", "db", "recovery"}).AddRow(tc.marker, "slot_executor", "target", false))
			if tc.marker == testTargetID {
				f.executor.ExpectQuery("SELECT current_database").WillReturnRows(sqlmock.NewRows([]string{"db", "session", "current", "version", "encoding", "client_encoding", "recovery", "unsafe", "dirty"}).AddRow("target", tc.role, tc.role, tc.version, "UTF8", "UTF8", tc.recovery, tc.unsafe, tc.dirty))
			}
			h := &Handler{d: Dependencies{Timeout: time.Second, Acquire: func(context.Context, string) (*TargetLease, error) { return f.lease, nil }}}
			_, err := h.openTarget(context.Background(), testDBID, testTargetID)
			assertCode(t, err, "TARGET_UNAVAILABLE")
			if f.released != 1 {
				t.Fatal("rejected lease was not released")
			}
		})
	}
}

func TestTargetRunCommitReceiptAndRollback(t *testing.T) {
	for _, tc := range []string{"success", "sql failure", "receipt failure", "commit uncertain", "receipt missing", "rollback uncertain"} {
		t.Run(tc, func(t *testing.T) {
			f := newTargetFixture(t)
			f.expectOpen()
			h := &Handler{d: Dependencies{Timeout: time.Second, Acquire: func(context.Context, string) (*TargetLease, error) { return f.lease, nil }}}
			s, err := h.openTarget(context.Background(), testDBID, testTargetID)
			if err != nil {
				t.Fatal(err)
			}
			f.expectBegin()
			if err := s.begin(context.Background(), time.Second); err != nil {
				t.Fatal(err)
			}
			a := sampleAttempt()
			f.expectTicket(a)
			first := "CREATE TABLE managed.example (id int)"
			second := "INSERT INTO managed.example VALUES (1)"
			f.executor.ExpectExec(regexp.QuoteMeta(first)).WillReturnResult(sqlmock.NewResult(0, 1))
			if tc == "sql failure" || tc == "rollback uncertain" {
				f.executor.ExpectExec(regexp.QuoteMeta(second)).WillReturnError(errors.New("private driver error"))
				roll := f.executor.ExpectRollback()
				if tc == "rollback uncertain" {
					roll.WillReturnError(errors.New("disconnect"))
				}
			} else {
				f.executor.ExpectExec(regexp.QuoteMeta(second)).WillReturnResult(sqlmock.NewResult(0, 1))
				receiptExec := f.executor.ExpectExec("SELECT wbd_auth.record_receipt").WithArgs(a.ID, sqlmock.AnyArg())
				if tc == "receipt failure" {
					receiptExec.WillReturnError(errors.New("receipt denied"))
					f.executor.ExpectRollback()
				} else {
					receiptExec.WillReturnResult(sqlmock.NewResult(0, 1))
					commit := f.executor.ExpectCommit()
					if tc == "commit uncertain" {
						commit.WillReturnError(errors.New("commit response lost"))
					} else {
						rows := receiptRows(testReceipt(a))
						if tc == "receipt missing" {
							rows = sqlmock.NewRows([]string{"id"})
						}
						f.preparer.ExpectQuery("SELECT execution_id").WithArgs(a.ID).WillReturnRows(rows)
					}
				}
			}
			result := s.run(context.Background(), a, []string{first, second})
			want := "succeeded"
			if tc == "sql failure" || tc == "receipt failure" {
				want = "failed"
			}
			if tc == "commit uncertain" || tc == "receipt missing" || tc == "rollback uncertain" {
				want = "unknown"
			}
			if result.status != want {
				t.Fatalf("outcome=%+v want %s", result, want)
			}
			if tc == "success" && (result.receipt == nil || !matchingReceipt(a, *result.receipt)) {
				t.Fatal("success lacks matching receipt")
			}
			if !s.tainted {
				f.expectClose()
			}
			s.close(time.Second)
			if f.released != 1 {
				t.Fatal("lease not released")
			}
		})
	}
}

func TestMatchingReceiptChecksEveryIdentityField(t *testing.T) {
	a := sampleAttempt()
	if !matchingReceipt(a, testReceipt(a)) {
		t.Fatal("valid receipt rejected")
	}
	mutations := []func(*receipt){func(r *receipt) { r.ID = testDBID }, func(r *receipt) { r.TargetID = testDBID }, func(r *receipt) { r.DatabaseID = testDraftID }, func(r *receipt) { r.Version = "2" }, func(r *receipt) { r.ActorID = testDBID }, func(r *receipt) { r.XID = "" }, func(r *receipt) { r.Hash[0] ^= 1 }, func(r *receipt) { r.Hash = nil }}
	for i, mutate := range mutations {
		r := testReceipt(a)
		mutate(&r)
		if matchingReceipt(a, r) {
			t.Errorf("mutation %d accepted", i)
		}
	}
}

func TestTargetExecStatementsStopsAtFirstError(t *testing.T) {
	db, m := mockDB(t)
	m.ExpectBegin()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("statement failure")
	m.ExpectExec("first").WillReturnError(sentinel)
	m.ExpectRollback()
	if err := execStatements(context.Background(), tx, []string{"first", "must not run"}); !errors.Is(err, sentinel) {
		t.Fatalf("error=%v", err)
	}
	if err := execStatements(context.Background(), tx, nil); err == nil {
		t.Fatal("empty batch accepted")
	}
	_ = tx.Rollback()
}

func TestStatementSlices(t *testing.T) {
	if !slicesOf("one; two", []string{"one", "two"}) {
		t.Fatal("valid slices rejected")
	}
	for _, s := range [][]string{nil, {""}, {"two", "one"}, {"missing"}} {
		if slicesOf("one; two", s) {
			t.Errorf("accepted %q", s)
		}
	}
}
