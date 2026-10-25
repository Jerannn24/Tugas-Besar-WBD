package migration

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Jerannn24/Tugas-Besar-WBD.git/history"
)

const (
	testDBID        = "a0000000-0000-4000-8000-000000000001"
	testDraftID     = "a0000000-0000-4000-8000-000000000002"
	testTargetID    = "a0000000-0000-4000-8000-000000000003"
	testActorID     = "a0000000-0000-4000-8000-000000000004"
	testRequestID   = "a0000000-0000-4000-8000-000000000005"
	testExecutionID = "a0000000-0000-4000-8000-000000000006"
)

func mockDB(t *testing.T) (*sql.DB, sqlmock.Sqlmock) {
	t.Helper()
	db, m, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := m.ExpectationsWereMet(); err != nil {
			t.Error(err)
		}
		_ = db.Close()
	})
	return db, m
}

func sampleAttempt() attempt {
	return attempt{Execution: history.Execution{ID: testExecutionID, DatabaseID: testDBID, DraftID: testDraftID, DraftRevision: 1, Version: "1", Title: "create", SQL: "CREATE TABLE managed.example (id int)", ExecutedBy: history.Executor{ID: testActorID, Username: "editor"}, StartedAt: time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC), Status: "running"}, TargetID: testTargetID, RequestID: testRequestID, Hash: sqlHash("CREATE TABLE managed.example (id int)")}
}

func attemptRows(a attempt) *sqlmock.Rows {
	var finished, code, message driver.Value
	if a.FinishedAt != nil {
		finished = *a.FinishedAt
	}
	if a.Error != nil {
		code, message = a.Error.Code, a.Error.Message
	}
	return sqlmock.NewRows([]string{"id", "database_id", "draft_id", "draft_revision", "version", "title", "sql", "actor_id", "username", "started_at", "finished_at", "status", "code", "message", "target_id", "request_id", "hash"}).AddRow(a.ID, a.DatabaseID, a.DraftID, a.DraftRevision, a.Version, a.Title, a.SQL, a.ExecutedBy.ID, a.ExecutedBy.Username, a.StartedAt, finished, a.Status, code, message, a.TargetID, a.RequestID, a.Hash)
}

func assertCode(t *testing.T, err error, code string) {
	t.Helper()
	var api *APIError
	if !errors.As(err, &api) || api.Code != code {
		t.Fatalf("error = %v, want %s", err, code)
	}
}

func TestStoreBinding(t *testing.T) {
	for _, tc := range []struct {
		name    string
		value   driver.Value
		missing bool
		code    string
	}{
		{name: "bound", value: testTargetID}, {name: "unbound", code: "TARGET_UNAVAILABLE"}, {name: "missing", missing: true, code: "NOT_FOUND"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, m := mockDB(t)
			rows := sqlmock.NewRows([]string{"target"})
			if !tc.missing {
				rows.AddRow(tc.value)
			}
			m.ExpectQuery("SELECT execution_target_id").WithArgs(testDBID).WillReturnRows(rows)
			got, err := (sqlStore{db}).binding(context.Background(), testDBID)
			if tc.code != "" {
				assertCode(t, err, tc.code)
			} else if err != nil || got != testTargetID {
				t.Fatalf("binding = %q, %v", got, err)
			}
		})
	}
}

func TestStoreRejectsCorruptHash(t *testing.T) {
	db, m := mockDB(t)
	a := sampleAttempt()
	a.Hash = []byte("incorrect")
	m.ExpectQuery("SELECT").WillReturnRows(attemptRows(a))
	if _, err := (sqlStore{db}).lookup(context.Background(), testDBID, testRequestID); err == nil {
		t.Fatal("accepted corrupt stored SQL")
	}
}

func reservePrefix(m sqlmock.Sqlmock, next int64) {
	m.ExpectBegin()
	m.ExpectQuery("SELECT execution_target_id.*FOR UPDATE").WithArgs(testDBID).WillReturnRows(sqlmock.NewRows([]string{"target"}).AddRow(testTargetID))
	m.ExpectQuery("SELECT next_version.*FOR UPDATE").WithArgs(testTargetID).WillReturnRows(sqlmock.NewRows([]string{"next"}).AddRow(next))
	m.ExpectQuery("SELECT.*FROM app.migration_executions").WithArgs(testDBID, testRequestID).WillReturnRows(sqlmock.NewRows([]string{"id"}))
}

func reserveDraft(m sqlmock.Sqlmock, revision int, applied driver.Value) {
	a := sampleAttempt()
	m.ExpectQuery("SELECT title,sql,revision").WithArgs(testDBID, testDraftID).WillReturnRows(sqlmock.NewRows([]string{"title", "sql", "revision", "applied"}).AddRow(a.Title, a.SQL, revision, applied))
}

func TestStoreReserveAllocatesVersionAndSnapshot(t *testing.T) {
	db, m := mockDB(t)
	a := sampleAttempt()
	a.Version = "8"
	reservePrefix(m, 4)
	reserveDraft(m, 1, nil)
	m.ExpectQuery("SELECT EXISTS").WithArgs(testTargetID, testDraftID).WillReturnRows(sqlmock.NewRows([]string{"busy"}).AddRow(false))
	m.ExpectQuery("SELECT username").WithArgs(testActorID).WillReturnRows(sqlmock.NewRows([]string{"username"}).AddRow("editor"))
	m.ExpectQuery("SELECT COALESCE").WithArgs(testDBID).WillReturnRows(sqlmock.NewRows([]string{"highest"}).AddRow(7))
	m.ExpectExec("UPDATE app.migration_target_state").WithArgs(testTargetID, 9).WillReturnResult(sqlmock.NewResult(0, 1))
	m.ExpectQuery("INSERT INTO app.migration_executions").WithArgs(sqlmock.AnyArg(), testDBID, testTargetID, testDraftID, testRequestID, 1, int64(8), a.Title, a.SQL, a.Hash, testActorID, "editor").WillReturnRows(attemptRows(a))
	m.ExpectCommit()
	prepared := false
	got, created, err := (sqlStore{db}).reserve(context.Background(), Actor{testActorID}, testDBID, testDraftID, testTargetID, ExecuteRequest{1, testRequestID}, func(s string) error {
		prepared = true
		if s != a.SQL {
			t.Fatal("preflight SQL differs from snapshot")
		}
		return nil
	})
	if err != nil || !created || !prepared || got.Version != "8" || got.Status != "running" {
		t.Fatalf("reserve = %+v, %v, %v", got, created, err)
	}
}

func TestStoreReserveRejectionsDoNotAllocateVersion(t *testing.T) {
	for _, tc := range []struct {
		name     string
		revision int
		applied  driver.Value
		busy     bool
		next     int64
		code     string
	}{
		{name: "revision", revision: 2, next: 1, code: "VERSION_CONFLICT"},
		{name: "applied", revision: 1, applied: testExecutionID, next: 1, code: "ALREADY_APPLIED"},
		{name: "busy", revision: 1, busy: true, next: 1, code: "DATABASE_BUSY"},
		{name: "exhausted", revision: 1, next: math.MaxInt64, code: "VERSION_CONFLICT"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, m := mockDB(t)
			reservePrefix(m, tc.next)
			reserveDraft(m, tc.revision, tc.applied)
			if tc.code != "ALREADY_APPLIED" && tc.name != "revision" {
				m.ExpectQuery("SELECT EXISTS").WillReturnRows(sqlmock.NewRows([]string{"busy"}).AddRow(tc.busy))
				if !tc.busy {
					m.ExpectQuery("SELECT username").WillReturnRows(sqlmock.NewRows([]string{"username"}).AddRow("editor"))
					m.ExpectQuery("SELECT COALESCE").WillReturnRows(sqlmock.NewRows([]string{"max"}).AddRow(0))
				}
			}
			m.ExpectRollback()
			_, created, err := (sqlStore{db}).reserve(context.Background(), Actor{testActorID}, testDBID, testDraftID, testTargetID, ExecuteRequest{1, testRequestID}, func(string) error { return nil })
			if created {
				t.Fatal("reserved rejected attempt")
			}
			assertCode(t, err, tc.code)
		})
	}
}

func TestStoreReservePreflightFailureRollsBack(t *testing.T) {
	db, m := mockDB(t)
	reservePrefix(m, 1)
	reserveDraft(m, 1, nil)
	m.ExpectQuery("SELECT EXISTS").WillReturnRows(sqlmock.NewRows([]string{"busy"}).AddRow(false))
	m.ExpectRollback()
	sentinel := errors.New("preflight rejected")
	_, created, err := (sqlStore{db}).reserve(context.Background(), Actor{testActorID}, testDBID, testDraftID, testTargetID, ExecuteRequest{1, testRequestID}, func(string) error { return sentinel })
	if created || !errors.Is(err, sentinel) {
		t.Fatalf("created=%v error=%v", created, err)
	}
}

func TestStoreReserveReplayDoesNotAllocateOrPrepare(t *testing.T) {
	db, m := mockDB(t)
	a := sampleAttempt()
	m.ExpectBegin()
	m.ExpectQuery("SELECT execution_target_id.*FOR UPDATE").WithArgs(testDBID).WillReturnRows(sqlmock.NewRows([]string{"target"}).AddRow(testTargetID))
	m.ExpectQuery("SELECT next_version.*FOR UPDATE").WithArgs(testTargetID).WillReturnRows(sqlmock.NewRows([]string{"next"}).AddRow(2))
	m.ExpectQuery("SELECT.*FROM app.migration_executions").WithArgs(testDBID, testRequestID).WillReturnRows(attemptRows(a))
	m.ExpectRollback()
	got, created, err := (sqlStore{db}).reserve(context.Background(), Actor{testActorID}, testDBID, testDraftID, testTargetID, ExecuteRequest{1, testRequestID}, func(string) error { t.Fatal("replay called prepare"); return nil })
	if err != nil || created || got.ID != a.ID {
		t.Fatalf("replay=%+v created=%v error=%v", got, created, err)
	}
}

func TestStoreSuccessRollsBackIfAppliedLinkCannotBeWritten(t *testing.T) {
	db, m := mockDB(t)
	a := sampleAttempt()
	transitionPrefix(m, a)
	updated := a
	updated.Status = "succeeded"
	m.ExpectQuery("UPDATE app.migration_executions").WillReturnRows(attemptRows(updated))
	m.ExpectExec("UPDATE app.migration_drafts SET applied_execution_id").WillReturnResult(sqlmock.NewResult(0, 0))
	m.ExpectRollback()
	if _, err := (sqlStore{db}).transition(context.Background(), a, "succeeded", nil); err == nil {
		t.Fatal("success was committed without applied draft link")
	}
}

func transitionPrefix(m sqlmock.Sqlmock, a attempt) {
	m.ExpectBegin()
	m.ExpectQuery("SELECT id::text FROM app.database_connections.*FOR UPDATE").WithArgs(a.DatabaseID).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(a.DatabaseID))
	m.ExpectQuery("SELECT id::text FROM app.migration_target_state.*FOR UPDATE").WithArgs(a.TargetID).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(a.TargetID))
	m.ExpectQuery("SELECT applied_execution_id").WithArgs(a.DatabaseID, a.DraftID).WillReturnRows(sqlmock.NewRows([]string{"applied"}).AddRow(nil))
	m.ExpectQuery("SELECT.*FROM app.migration_executions").WithArgs(a.DatabaseID, a.ID).WillReturnRows(attemptRows(a))
}

func TestStoreTransitionPersistsOutcomeAndAppliedLink(t *testing.T) {
	for _, status := range []string{"succeeded", "failed", "unknown"} {
		t.Run(status, func(t *testing.T) {
			db, m := mockDB(t)
			a := sampleAttempt()
			transitionPrefix(m, a)
			updated := a
			updated.Status = status
			var failure *history.ExecutionError
			var code, message driver.Value
			if status != "succeeded" {
				failure = &history.ExecutionError{Code: "SQL_FAILED", Message: "SQL failed"}
				code, message = failure.Code, failure.Message
				updated.Error = failure
			}
			m.ExpectQuery("UPDATE app.migration_executions").WithArgs(a.DatabaseID, a.ID, status, code, message).WillReturnRows(attemptRows(updated))
			if status == "succeeded" {
				m.ExpectExec("UPDATE app.migration_drafts SET applied_execution_id").WithArgs(a.DatabaseID, a.DraftID, a.ID).WillReturnResult(sqlmock.NewResult(0, 1))
			}
			m.ExpectCommit()
			got, err := (sqlStore{db}).transition(context.Background(), a, status, failure)
			if err != nil || got.Status != status {
				t.Fatalf("transition = %+v, %v", got, err)
			}
		})
	}
}

func TestStoreTransitionRejectsFinalOrInvalidOutcome(t *testing.T) {
	db, m := mockDB(t)
	s := sqlStore{db}
	a := sampleAttempt()
	if _, err := s.transition(context.Background(), a, "running", nil); err == nil {
		t.Fatal("accepted invalid status")
	}
	if _, err := s.transition(context.Background(), a, "failed", nil); err == nil {
		t.Fatal("failed without evidence")
	}
	a.Status = "succeeded"
	transitionPrefix(m, a)
	m.ExpectRollback()
	_, err := s.transition(context.Background(), a, "failed", &history.ExecutionError{Code: "FAIL", Message: "failed"})
	assertCode(t, err, "REQUEST_CONFLICT")
}
