package history

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

var executionColumns = []string{"id", "database_id", "draft_id", "draft_revision", "version", "title", "sql", "executed_by", "executed_username", "started_at", "finished_at", "status", "error_code", "error_message"}

func historyMock(t *testing.T) (sqlmock.Sqlmock, *http.ServeMux) {
	t.Helper()
	db, m, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := m.ExpectationsWereMet(); err != nil {
			t.Error(err)
		}
		db.Close()
	})
	return m, newMux(t, db, nil, nil)
}

func TestHistoryQueryAndScanFailuresAreSanitized(t *testing.T) {
	for _, detail := range []bool{false, true} {
		for _, stage := range []string{"query", "scan", "iteration"} {
			if detail && stage == "iteration" {
				continue
			}
			name := stage
			if detail {
				name += " detail"
			} else {
				name += " list"
			}
			t.Run(name, func(t *testing.T) {
				m, mux := historyMock(t)
				query := m.ExpectQuery("SELECT e.id::text,e.database_id::text,e.draft_id::text")
				path := "/api/databases/" + dbA + "/migrations"
				if detail {
					query.WithArgs(dbA, execOne)
					path += "/" + execOne
				} else {
					query.WithArgs(dbA)
				}
				if stage == "query" {
					query.WillReturnError(errors.New("private credentials: password"))
				} else {
					rows := sqlmock.NewRows(executionColumns)
					if stage == "scan" {
						rows.AddRow(execOne, dbA, "draft", "invalid revision", bigVer, "Title", "SELECT 1", userID, "user", time.Now(), nil, "running", nil, nil)
					} else {
						rows.AddRow(execOne, dbA, "draft", 1, bigVer, "Title", "SELECT 1", userID, "user", time.Now(), nil, "running", nil, nil).RowError(0, errors.New("private stream error"))
					}
					query.WillReturnRows(rows)
				}
				rec := do(mux, path)
				assertError(t, rec, 500, "INTERNAL_ERROR")
				if strings.Contains(rec.Body.String(), "private") || strings.Contains(rec.Body.String(), "password") {
					t.Fatal("database error leaked")
				}
			})
		}
	}
}

func TestHistoryUnitSuccessAndMissingExecution(t *testing.T) {
	t.Run("list preserves bigint and nullable fields", func(t *testing.T) {
		m, mux := historyMock(t)
		local := time.Date(2026, 10, 25, 19, 0, 0, 0, time.FixedZone("WIB", 7*3600))
		m.ExpectQuery("SELECT e.id::text,e.database_id::text,e.draft_id::text").WithArgs(dbA).WillReturnRows(sqlmock.NewRows(executionColumns).AddRow(execOne, dbA, "draft", 3, bigVer, "Title", "SELECT 1", userID, "tester", local, nil, "running", nil, nil))
		var v struct {
			Items []Execution `json:"items"`
		}
		decodeData(t, do(mux, "/api/databases/"+dbA+"/migrations"), &v)
		if len(v.Items) != 1 {
			t.Fatalf("items: %+v", v.Items)
		}
		e := v.Items[0]
		if e.ID != execOne || e.Version != bigVer || e.DraftRevision != 3 || e.SQL != "SELECT 1" || e.ExecutedBy.Username != "tester" || e.FinishedAt != nil || e.Error != nil || e.StartedAt.Location() != time.UTC || e.StartedAt.Hour() != 12 {
			t.Fatalf("execution: %+v", e)
		}
	})
	t.Run("failed detail", func(t *testing.T) {
		m, mux := historyMock(t)
		now := time.Now().UTC()
		m.ExpectQuery("SELECT e.id::text,e.database_id::text,e.draft_id::text").WithArgs(dbA, execTwo).WillReturnRows(sqlmock.NewRows(executionColumns).AddRow(execTwo, dbA, "draft", 2, "2", "Failed", "bad sql", userID, "tester", now, now, "failed", "SQL_ERROR", "safe detail"))
		var e Execution
		decodeData(t, do(mux, "/api/databases/"+dbA+"/migrations/"+execTwo), &e)
		if e.Error == nil || e.Error.Code != "SQL_ERROR" || e.Error.Message != "safe detail" || e.Status != "failed" || e.FinishedAt == nil {
			t.Fatalf("execution: %+v", e)
		}
	})
	t.Run("missing", func(t *testing.T) {
		m, mux := historyMock(t)
		m.ExpectQuery("SELECT e.id::text,e.database_id::text,e.draft_id::text").WithArgs(dbA, execOne).WillReturnRows(sqlmock.NewRows(executionColumns))
		assertError(t, do(mux, "/api/databases/"+dbA+"/migrations/"+execOne), 404, "NOT_FOUND")
	})
}
