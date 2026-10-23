package drafts

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

type Actor struct{ ID string }
type Dependencies struct {
	Platform *sql.DB
	Actor    func(*http.Request) (Actor, error)
	// Authorize wajib memeriksa akses edit yang berlaku saat ini.
	Authorize func(context.Context, Actor, string) error
	// CheckCSRF memvalidasi token pada setiap mutasi.
	CheckCSRF func(*http.Request) error
	Timeout   time.Duration
}
type Handler struct{ d Dependencies }

type Draft struct {
	ID                 string    `json:"id"`
	DatabaseID         string    `json:"database_id"`
	Title              string    `json:"title"`
	SQL                string    `json:"sql"`
	Revision           int       `json:"revision"`
	CreatedBy          string    `json:"created_by"`
	CreatedAt          time.Time `json:"-"`
	UpdatedAt          time.Time `json:"updated_at"`
	AppliedExecutionID *string   `json:"applied_execution_id"`
}
type input struct {
	Title    string `json:"title"`
	SQL      string `json:"sql"`
	Revision *int   `json:"revision,omitempty"`
}

func New(d Dependencies) (*Handler, error) {
	if d.Platform == nil || d.Actor == nil || d.Authorize == nil || d.CheckCSRF == nil || d.Timeout <= 0 {
		return nil, errors.New("draft dependencies are required")
	}
	return &Handler{d}, nil
}
func (h *Handler) Register(m *http.ServeMux) error {
	if m == nil {
		return errors.New("mux is required")
	}
	m.HandleFunc("GET /api/databases/{database_id}/drafts", h.list)
	m.HandleFunc("POST /api/databases/{database_id}/drafts", h.create)
	m.HandleFunc("GET /api/databases/{database_id}/drafts/{draft_id}", h.detail)
	m.HandleFunc("PATCH /api/databases/{database_id}/drafts/{draft_id}", h.patch)
	return nil
}
func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), h.d.Timeout)
	defer cancel()
	_, id, ok := h.authorize(ctx, w, r)
	if !ok {
		return
	}
	rows, err := h.d.Platform.QueryContext(ctx, draftSelect+` WHERE database_id=$1 ORDER BY updated_at DESC,id`, id)
	if err != nil {
		internal(w)
		return
	}
	defer rows.Close()
	items := make([]Draft, 0)
	for rows.Next() {
		d, e := scan(rows)
		if e != nil {
			internal(w)
			return
		}
		items = append(items, d)
	}
	if rows.Err() != nil {
		internal(w)
		return
	}
	success(w, map[string]any{"items": items})
}

func (h *Handler) detail(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), h.d.Timeout)
	defer cancel()
	_, id, ok := h.authorize(ctx, w, r)
	if !ok {
		return
	}
	did := r.PathValue("draft_id")
	if !validUUID(did) {
		apiError(w, 400, "INVALID_REQUEST", "Invalid draft id")
		return
	}
	d, err := scan(h.d.Platform.QueryRowContext(ctx, draftSelect+` WHERE database_id=$1 AND id=$2`, id, did))
	if errors.Is(err, sql.ErrNoRows) {
		notFound(w)
		return
	}
	if err != nil {
		internal(w)
		return
	}
	success(w, d)
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), h.d.Timeout)
	defer cancel()
	a, id, ok := h.mutateAuth(ctx, w, r)
	if !ok {
		return
	}
	in, ok := decode(w, r, false)
	if !ok {
		return
	}
	if validate(w, &in) != nil {
		return
	}
	draftID, err := newUUID()
	if err != nil {
		internal(w)
		return
	}
	tx, err := h.d.Platform.BeginTx(ctx, nil)
	if err != nil {
		internal(w)
		return
	}
	defer tx.Rollback()
	target, err := lockConnection(ctx, tx, id)
	if errors.Is(err, sql.ErrNoRows) {
		notFound(w)
		return
	}
	if err != nil {
		internal(w)
		return
	}
	if err = lockTarget(ctx, tx, target); err != nil {
		internal(w)
		return
	}
	if err = h.d.Authorize(ctx, a, id); err != nil {
		accessError(w, err)
		return
	}
	now := time.Now().UTC()
	d, err := scan(tx.QueryRowContext(ctx, `INSERT INTO app.migration_drafts(id,database_id,title,sql,revision,created_by,created_at,updated_at) VALUES($1,$2,$3,$4,1,$5,$6,$6) RETURNING id::text,database_id::text,title,sql,revision,created_by::text,created_at,updated_at,applied_execution_id::text`, draftID, id, in.Title, in.SQL, a.ID, now))
	if err != nil {
		internal(w)
		return
	}
	if tx.Commit() != nil {
		internal(w)
		return
	}
	write(w, 201, map[string]any{"data": d})
}

func (h *Handler) patch(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), h.d.Timeout)
	defer cancel()
	a, id, ok := h.mutateAuth(ctx, w, r)
	if !ok {
		return
	}
	did := r.PathValue("draft_id")
	if !validUUID(did) {
		apiError(w, 400, "INVALID_REQUEST", "Invalid draft id")
		return
	}
	in, ok := decode(w, r, true)
	if !ok {
		return
	}
	if validate(w, &in) != nil {
		return
	}
	if in.Revision == nil || *in.Revision < 1 {
		fieldError(w, "revision", "Must be positive")
		return
	}
	expected := *in.Revision
	if expected > int(^uint32(0)>>1) {
		fieldError(w, "revision", "Must fit a positive 32-bit integer")
		return
	}
	tx, err := h.d.Platform.BeginTx(ctx, nil)
	if err != nil {
		internal(w)
		return
	}
	defer tx.Rollback()
	target, err := lockConnection(ctx, tx, id)
	if errors.Is(err, sql.ErrNoRows) {
		notFound(w)
		return
	}
	if err != nil {
		internal(w)
		return
	}
	if err = lockTarget(ctx, tx, target); err != nil {
		internal(w)
		return
	}
	if err = h.d.Authorize(ctx, a, id); err != nil {
		accessError(w, err)
		return
	}
	var d Draft
	var applied sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT id::text,database_id::text,title,sql,revision,created_by::text,created_at,updated_at,applied_execution_id::text FROM app.migration_drafts WHERE database_id=$1 AND id=$2 FOR UPDATE`, id, did).Scan(&d.ID, &d.DatabaseID, &d.Title, &d.SQL, &d.Revision, &d.CreatedBy, &d.CreatedAt, &d.UpdatedAt, &applied)
	if errors.Is(err, sql.ErrNoRows) {
		notFound(w)
		return
	}
	if err != nil {
		internal(w)
		return
	}
	d.CreatedAt = d.CreatedAt.UTC()
	d.UpdatedAt = d.UpdatedAt.UTC()
	if applied.Valid {
		conflict(w, "ALREADY_APPLIED")
		return
	}
	if d.Revision != expected {
		conflict(w, "VERSION_CONFLICT")
		return
	}
	if d.Revision >= int(^uint32(0)>>1) || expected > int(^uint32(0)>>1) {
		conflict(w, "VERSION_CONFLICT")
		return
	}
	var busy bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM app.migration_executions WHERE database_id=$1 AND draft_id=$2 AND status IN ('running','unknown'))`, id, did).Scan(&busy); err != nil {
		internal(w)
		return
	}
	if busy {
		conflict(w, "DATABASE_BUSY")
		return
	}
	updated, err := scan(tx.QueryRowContext(ctx, `UPDATE app.migration_drafts SET title=$3,sql=$4,revision=revision+1,updated_at=GREATEST(clock_timestamp(),updated_at) WHERE database_id=$1 AND id=$2 AND revision=$5 RETURNING id::text,database_id::text,title,sql,revision,created_by::text,created_at,updated_at,applied_execution_id::text`, id, did, in.Title, in.SQL, expected))
	if errors.Is(err, sql.ErrNoRows) {
		conflict(w, "VERSION_CONFLICT")
		return
	}
	if err != nil {
		internal(w)
		return
	}
	if tx.Commit() != nil {
		internal(w)
		return
	}
	success(w, updated)
}

const draftSelect = `SELECT id::text,database_id::text,title,sql,revision,created_by::text,created_at,updated_at,applied_execution_id::text FROM app.migration_drafts`

type rowScanner interface{ Scan(...any) error }

func scan(row rowScanner) (Draft, error) {
	var d Draft
	var applied sql.NullString
	err := row.Scan(&d.ID, &d.DatabaseID, &d.Title, &d.SQL, &d.Revision, &d.CreatedBy, &d.CreatedAt, &d.UpdatedAt, &applied)
	d.CreatedAt = d.CreatedAt.UTC()
	d.UpdatedAt = d.UpdatedAt.UTC()
	if applied.Valid {
		d.AppliedExecutionID = &applied.String
	}
	return d, err
}
func lockConnection(ctx context.Context, tx *sql.Tx, id string) (sql.NullString, error) {
	var target sql.NullString
	e := tx.QueryRowContext(ctx, `SELECT execution_target_id::text FROM app.database_connections WHERE id=$1 AND deleted_at IS NULL FOR UPDATE`, id).Scan(&target)
	return target, e
}
func lockTarget(ctx context.Context, tx *sql.Tx, target sql.NullString) error {
	if target.Valid {
		var got string
		return tx.QueryRowContext(ctx, `SELECT id::text FROM app.migration_target_state WHERE id=$1 FOR UPDATE`, target.String).Scan(&got)
	}
	return nil
}

func (h *Handler) authorize(ctx context.Context, w http.ResponseWriter, r *http.Request) (Actor, string, bool) {
	a, err := h.d.Actor(r)
	if err != nil || a.ID == "" {
		apiError(w, 401, "UNAUTHENTICATED", "Authentication required")
		return Actor{}, "", false
	}
	id := r.PathValue("database_id")
	if !validUUID(id) {
		apiError(w, 400, "INVALID_REQUEST", "Invalid database id")
		return Actor{}, "", false
	}
	if err = h.d.Authorize(ctx, a, id); err != nil {
		accessError(w, err)
		return Actor{}, "", false
	}
	return a, id, true
}
func (h *Handler) mutateAuth(ctx context.Context, w http.ResponseWriter, r *http.Request) (Actor, string, bool) {
	a, id, ok := h.authorize(ctx, w, r)
	if !ok {
		return Actor{}, "", false
	}
	if h.d.CheckCSRF(r) != nil {
		apiError(w, 403, "CSRF_INVALID", "Request verification failed")
		return Actor{}, "", false
	}
	return a, id, true
}
func decode(w http.ResponseWriter, r *http.Request, patch bool) (input, bool) {
	var in input
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	raw, err := io.ReadAll(r.Body)
	if err != nil || !utf8.Valid(raw) {
		apiError(w, 400, "INVALID_REQUEST", "Invalid JSON request")
		return in, false
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		apiError(w, 400, "INVALID_REQUEST", "Invalid JSON request")
		return in, false
	}
	for key := range fields {
		if key != "title" && key != "sql" && !(patch && key == "revision") {
			apiError(w, 400, "INVALID_REQUEST", "Invalid JSON request")
			return in, false
		}
	}
	_, hasRevision := fields["revision"]
	if (!patch && hasRevision) || (patch && (!hasRevision || string(fields["revision"]) == "null")) {
		apiError(w, 400, "INVALID_REQUEST", "Invalid JSON request")
		return in, false
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if dec.Decode(&in) != nil {
		apiError(w, 400, "INVALID_REQUEST", "Invalid JSON request")
		return in, false
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		apiError(w, 400, "INVALID_REQUEST", "Invalid JSON request")
		return in, false
	}
	return in, true
}
func validate(w http.ResponseWriter, in *input) error {
	in.Title = strings.TrimSpace(in.Title)
	if strings.ContainsRune(in.Title, 0) || len([]rune(in.Title)) < 1 || len([]rune(in.Title)) > 100 {
		fieldError(w, "title", "Must contain 1 to 100 characters")
		return errors.New("invalid")
	}
	if strings.TrimSpace(in.SQL) == "" || len(in.SQL) > 262144 || strings.IndexByte(in.SQL, 0) >= 0 {
		fieldError(w, "sql", "Must contain nonblank SQL up to 262144 bytes without NUL")
		return errors.New("invalid")
	}
	return nil
}
func newUUID() (string, error) {
	b := make([]byte, 16)
	if _, e := rand.Read(b); e != nil {
		return "", e
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	x := hex.EncodeToString(b)
	return x[:8] + "-" + x[8:12] + "-" + x[12:16] + "-" + x[16:20] + "-" + x[20:], nil
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
func fieldError(w http.ResponseWriter, k, msg string) {
	write(w, 422, map[string]any{"error": map[string]any{"code": "VALIDATION_FAILED", "message": "Validation failed", "field_errors": map[string]string{k: msg}, "retryable": false}})
}
func conflict(w http.ResponseWriter, code string) {
	apiError(w, 409, code, "Draft cannot be changed in its current state")
}
func notFound(w http.ResponseWriter) { apiError(w, 404, "NOT_FOUND", "Draft or database not found") }
func internal(w http.ResponseWriter) { apiError(w, 500, "INTERNAL_ERROR", "Internal error") }

// Error guard menyediakan StatusCode 401, 403, atau 404; error lain menjadi internal.
func accessError(w http.ResponseWriter, e error) {
	var s interface{ StatusCode() int }
	if !errors.As(e, &s) {
		internal(w)
		return
	}
	switch s.StatusCode() {
	case 401:
		apiError(w, 401, "UNAUTHENTICATED", "Authentication required")
	case 403:
		apiError(w, 403, "FORBIDDEN", "Access denied")
	case 404:
		notFound(w)
	default:
		internal(w)
	}
}
func apiError(w http.ResponseWriter, status int, code, msg string) {
	write(w, status, map[string]any{"error": map[string]any{"code": code, "message": msg, "field_errors": map[string]string{}, "retryable": false}})
}
func success(w http.ResponseWriter, v any) { write(w, 200, map[string]any{"data": v}) }
func write(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
