// Package history daftar dan detail snapshot eksekusi.
package history

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

type Actor struct{ ID string }

type Dependencies struct {
	Platform *sql.DB
	Actor    func(*http.Request) (Actor, error)
	// Authorize periksa target aktif dan akses view/edit saat ini.
	// Error guard  StatusCode 401, 403, atau 404.
	Authorize func(context.Context, Actor, string) error
	Timeout   time.Duration
}

type Handler struct{ d Dependencies }

type Executor struct {
	ID       string `json:"id"`
	Username string `json:"username"`
}

type ExecutionError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type Execution struct {
	ID            string          `json:"id"`
	DatabaseID    string          `json:"database_id"`
	DraftID       string          `json:"draft_id"`
	DraftRevision int             `json:"draft_revision"`
	Version       string          `json:"version"`
	Title         string          `json:"title"`
	SQL           string          `json:"sql"`
	ExecutedBy    Executor        `json:"executed_by"`
	StartedAt     time.Time       `json:"started_at"`
	FinishedAt    *time.Time      `json:"finished_at"`
	Status        string          `json:"status"`
	Error         *ExecutionError `json:"error"`
}

func New(d Dependencies) (*Handler, error) {
	if d.Platform == nil || d.Actor == nil || d.Authorize == nil || d.Timeout <= 0 {
		return nil, errors.New("history dependencies are required")
	}
	return &Handler{d}, nil
}

func (h *Handler) Register(m *http.ServeMux) error {
	if m == nil {
		return errors.New("mux is required")
	}
	m.HandleFunc("GET /api/databases/{database_id}/migrations", h.list)
	m.HandleFunc("GET /api/databases/{database_id}/migrations/{execution_id}", h.detail)
	return nil
}

const executionSelect = `SELECT e.id::text,e.database_id::text,e.draft_id::text,
e.draft_revision,e.version::text,e.title,e.sql,e.executed_by::text,e.executed_username,
e.started_at,e.finished_at,e.status,e.error_code,e.error_message
FROM app.migration_executions e`

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), h.d.Timeout)
	defer cancel()
	id, ok := h.authorize(ctx, w, r)
	if !ok {
		return
	}
	rows, err := h.d.Platform.QueryContext(ctx, executionSelect+` WHERE e.database_id=$1 ORDER BY e.version DESC`, id)
	if err != nil {
		internal(w)
		return
	}
	defer rows.Close()
	items := make([]Execution, 0)
	for rows.Next() {
		execution, err := scan(rows)
		if err != nil {
			internal(w)
			return
		}
		items = append(items, execution)
	}
	if rows.Err() != nil {
		internal(w)
		return
	}
	write(w, http.StatusOK, map[string]any{"data": map[string]any{"items": items}})
}

func (h *Handler) detail(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), h.d.Timeout)
	defer cancel()
	id, ok := h.authorize(ctx, w, r)
	if !ok {
		return
	}
	executionID := r.PathValue("execution_id")
	if !validUUID(executionID) {
		apiError(w, 400, "INVALID_REQUEST", "Invalid execution id")
		return
	}
	execution, err := scan(h.d.Platform.QueryRowContext(ctx, executionSelect+` WHERE e.database_id=$1 AND e.id=$2`, id, executionID))
	if errors.Is(err, sql.ErrNoRows) {
		apiError(w, 404, "NOT_FOUND", "Execution not found")
		return
	}
	if err != nil {
		internal(w)
		return
	}
	write(w, http.StatusOK, map[string]any{"data": execution})
}

type rowScanner interface{ Scan(...any) error }

func scan(row rowScanner) (Execution, error) {
	var execution Execution
	var finished sql.NullTime
	var code, message sql.NullString
	err := row.Scan(&execution.ID, &execution.DatabaseID, &execution.DraftID,
		&execution.DraftRevision, &execution.Version, &execution.Title, &execution.SQL,
		&execution.ExecutedBy.ID, &execution.ExecutedBy.Username,
		&execution.StartedAt, &finished, &execution.Status, &code, &message)
	if err != nil {
		return Execution{}, err
	}
	execution.StartedAt = execution.StartedAt.UTC()
	if finished.Valid {
		utc := finished.Time.UTC()
		execution.FinishedAt = &utc
	}
	if code.Valid {
		execution.Error = &ExecutionError{Code: code.String, Message: message.String}
	}
	return execution, nil
}

func (h *Handler) authorize(ctx context.Context, w http.ResponseWriter, r *http.Request) (string, bool) {
	actor, err := h.d.Actor(r.WithContext(ctx))
	if err != nil || actor.ID == "" {
		apiError(w, 401, "UNAUTHENTICATED", "Authentication required")
		return "", false
	}
	id := r.PathValue("database_id")
	if !validUUID(id) {
		apiError(w, 400, "INVALID_REQUEST", "Invalid database id")
		return "", false
	}
	if err := h.d.Authorize(ctx, actor, id); err != nil {
		accessError(w, err)
		return "", false
	}
	return id, true
}

func validUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

func accessError(w http.ResponseWriter, err error) {
	var guard interface{ StatusCode() int }
	if !errors.As(err, &guard) {
		internal(w)
		return
	}
	switch guard.StatusCode() {
	case 401:
		apiError(w, 401, "UNAUTHENTICATED", "Authentication required")
	case 403:
		apiError(w, 403, "FORBIDDEN", "Access denied")
	case 404:
		apiError(w, 404, "NOT_FOUND", "Database not found")
	default:
		internal(w)
	}
}

func internal(w http.ResponseWriter) {
	apiError(w, 500, "INTERNAL_ERROR", "Internal error")
}

func apiError(w http.ResponseWriter, status int, code, message string) {
	write(w, status, map[string]any{"error": map[string]any{
		"code": code, "message": message, "field_errors": map[string]string{}, "retryable": false,
	}})
}

func write(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
