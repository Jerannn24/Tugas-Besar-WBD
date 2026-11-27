package history

import (
	"encoding/base64"
	"encoding/json"
	"net/url"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestAttemptPageCursorAndScope(t *testing.T) {
	m, mux := historyMock(t)
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	m.ExpectQuery("WITH candidates AS").WithArgs(dbA, nil, nil, false, 2, userID).WillReturnRows(
		sqlmock.NewRows([]string{"id", "started_at", "head", "row"}).AddRow(execTwo, at, execOne, []byte(`{"attempt_id":"`+execTwo+`"}`)).AddRow(execOne, at, execOne, []byte(`{"attempt_id":"`+execOne+`"}`)))
	var page struct {
		Items []json.RawMessage `json:"items"`
		Next  *string           `json:"next_cursor"`
		Head  *string           `json:"active_head_execution_id"`
	}
	decodeData(t, do(mux, "/api/databases/"+dbA+"/migrations?limit=1"), &page)
	if len(page.Items) != 1 || page.Next == nil || page.Head == nil || *page.Head != execOne {
		t.Fatalf("page: %+v", page)
	}
	raw, err := base64.RawURLEncoding.DecodeString(*page.Next)
	if err != nil {
		t.Fatal(err)
	}
	var c historyCursor
	if err := json.Unmarshal(raw, &c); err != nil || c.Attempt != execTwo || !c.Started.Equal(at) {
		t.Fatalf("cursor: %+v %v", c, err)
	}
	m.ExpectQuery("WITH candidates AS").WithArgs(dbA, at, execTwo, false, 2, userID).WillReturnRows(
		sqlmock.NewRows([]string{"id", "started_at", "head", "row"}).AddRow(execOne, at, execOne, []byte(`{"attempt_id":"`+execOne+`"}`)))
	decodeData(t, do(mux, "/api/databases/"+dbA+"/migrations?limit=1&cursor="+url.QueryEscape(*page.Next)), &page)
	if len(page.Items) != 1 || page.Next != nil {
		t.Fatalf("last page: %+v", page)
	}
	assertError(t, do(mux, "/api/databases/"+dbB+"/migrations?limit=1&cursor="+url.QueryEscape(base64.RawURLEncoding.EncodeToString(raw))), 400, "INVALID_REQUEST")
	assertError(t, do(mux, "/api/databases/"+dbA+"/migrations?limit=1&hide_rollback=true&cursor="+url.QueryEscape(base64.RawURLEncoding.EncodeToString(raw))), 400, "INVALID_REQUEST")
}

func TestAttemptPageValidationAndQueryFailure(t *testing.T) {
	m, mux := historyMock(t)
	for _, query := range []string{"limit=", "cursor=", "hide_rollback=", "hide_rollback=maybe", "limit=0", "limit=101", "limit=1&cursor=bad", "limit=1&hide_rollback=maybe"} {
		assertError(t, do(mux, "/api/databases/"+dbA+"/migrations?"+query), 400, "INVALID_REQUEST")
	}
	m.ExpectQuery("WITH candidates AS").WithArgs(dbA, nil, nil, true, 21, userID).WillReturnError(sqlmock.ErrCancelled)
	assertError(t, do(mux, "/api/databases/"+dbA+"/migrations?limit=20&hide_rollback=true"), 500, "INTERNAL_ERROR")
}

func TestAttemptPageEmptyResultReturnsHead(t *testing.T) {
	m, mux := historyMock(t)
	m.ExpectQuery("WITH candidates AS").WithArgs(dbA, nil, nil, false, 21, userID).WillReturnRows(
		sqlmock.NewRows([]string{"id", "started_at", "head", "row"}).AddRow(nil, nil, execOne, nil))
	var page struct {
		Items []json.RawMessage `json:"items"`
		Head  *string           `json:"active_head_execution_id"`
	}
	decodeData(t, do(mux, "/api/databases/"+dbA+"/migrations?hide_rollback=false"), &page)
	if page.Items == nil || len(page.Items) != 0 || page.Head == nil || *page.Head != execOne {
		t.Fatalf("page: %+v", page)
	}
}
