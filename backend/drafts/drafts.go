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

	"github.com/Jerannn24/Tugas-Besar-WBD.git/executioncontract"
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
	ID                       string                         `json:"id"`
	DatabaseID               string                         `json:"database_id"`
	Title                    string                         `json:"title"`
	SQL                      string                         `json:"sql"`
	Revision                 int                            `json:"revision"`
	CreatedBy                string                         `json:"created_by"`
	CreatedAt                time.Time                      `json:"-"`
	UpdatedAt                time.Time                      `json:"updated_at"`
	AppliedExecutionID       *string                        `json:"applied_execution_id"`
	IsApplied                bool                           `json:"is_applied"`
	LatestExecutionID        *string                        `json:"latest_execution_id"`
	LatestExecutionDirection *string                        `json:"latest_execution_direction"`
	SQLDown                  *string                        `json:"sql_down,omitempty"`
	IrreversibleReason       *string                        `json:"irreversible_reason,omitempty"`
	Mode                     executioncontract.Mode         `json:"mode"`
	Batch                    *executioncontract.BatchConfig `json:"batch"`
}
type input struct {
	Title                 string                         `json:"title"`
	SQL                   string                         `json:"sql"`
	Revision              *int                           `json:"revision,omitempty"`
	SQLDown               *string                        `json:"sql_down"`
	IrreversibleReason    *string                        `json:"irreversible_reason"`
	Mode                  *executioncontract.Mode        `json:"mode"`
	Batch                 *executioncontract.BatchConfig `json:"batch"`
	hasMode               bool
	hasBatch              bool
	hasSQLDown            bool
	hasIrreversibleReason bool
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
	rows, err := h.d.Platform.QueryContext(ctx, draftSelect+` WHERE d.database_id=$1 ORDER BY d.updated_at DESC,d.id`, id)
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
	d, err := scan(h.d.Platform.QueryRowContext(ctx, draftSelect+` WHERE d.database_id=$1 AND d.id=$2`, id, did))
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
	if !in.hasMode {
		mode := executioncontract.Standard
		in.Mode = &mode
	}
	if !validateBatch(w, *in.Mode, in.Batch) {
		return
	}
	batch, err := json.Marshal(in.Batch)
	if err != nil {
		internal(w)
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
	d, err := scan(tx.QueryRowContext(ctx, `INSERT INTO app.migration_drafts
		(id,database_id,title,sql,revision,created_by,created_at,updated_at,sql_down,irreversible_reason,mode,batch)
		VALUES($1,$2,$3,$4,1,$5,$6,$6,$7,$8,$9,NULLIF($10::jsonb,'null'::jsonb)) RETURNING `+draftProjection,
		draftID, id, in.Title, in.SQL, a.ID, now, in.SQLDown, in.IrreversibleReason, *in.Mode, string(batch)))
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
	// Check while holding the connection lock, before waiting on the draft
	// row: the worker can hold the draft while authorizing against the connection.
	var busy bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM app.migration_executions WHERE database_id=$1 AND draft_id=$2 AND status IN ('running','unknown')
		UNION ALL
		SELECT 1 FROM app.jobs WHERE database_id=$1 AND migration_id=$2
			AND (status IN ('queued','running','retry_wait') OR blocks_database)
	)`, id, did).Scan(&busy)
	if err != nil {
		internal(w)
		return
	}
	if busy {
		conflict(w, "DATABASE_BUSY")
		return
	}
	var revision int
	var locked bool
	var oldSQLDown, oldReason *string
	var oldMode executioncontract.Mode
	var oldBatch []byte
	err = tx.QueryRowContext(ctx, `SELECT revision, applied_execution_id IS NOT NULL OR revision_locked_at IS NOT NULL OR EXISTS (
		SELECT FROM app.migration_history_entries h
		WHERE h.database_id=d.database_id AND h.migration_id=d.id AND h.revision=d.revision
		  AND h.job_id IS NOT NULL AND h.action='apply' AND h.direction='up'
		), sql_down, irreversible_reason, mode, batch FROM app.migration_drafts d WHERE database_id=$1 AND id=$2 FOR UPDATE`,
		id, did).Scan(&revision, &locked, &oldSQLDown, &oldReason, &oldMode, &oldBatch)
	if errors.Is(err, sql.ErrNoRows) {
		notFound(w)
		return
	}
	if err != nil {
		internal(w)
		return
	}
	if locked {
		conflict(w, "ALREADY_APPLIED")
		return
	}
	if revision != expected || revision >= int(^uint32(0)>>1) {
		conflict(w, "VERSION_CONFLICT")
		return
	}
	if !in.hasSQLDown {
		in.SQLDown = oldSQLDown
	}
	if !in.hasIrreversibleReason {
		in.IrreversibleReason = oldReason
	}
	if !in.hasMode {
		in.Mode = &oldMode
	}
	if !in.hasBatch && len(oldBatch) != 0 {
		if err := json.Unmarshal(oldBatch, &in.Batch); err != nil {
			internal(w)
			return
		}
	}
	if !validateBatch(w, *in.Mode, in.Batch) {
		return
	}
	batch, err := json.Marshal(in.Batch)
	if err != nil {
		internal(w)
		return
	}
	updated, err := scan(tx.QueryRowContext(ctx, `UPDATE app.migration_drafts SET title=$3,sql=$4,
		revision=revision+1,updated_at=GREATEST(clock_timestamp(),updated_at),sql_down=$6,irreversible_reason=$7,mode=$8,batch=NULLIF($9::jsonb,'null'::jsonb)
		WHERE database_id=$1 AND id=$2 AND revision=$5 RETURNING `+draftProjection,
		id, did, in.Title, in.SQL, expected, in.SQLDown, in.IrreversibleReason, *in.Mode, string(batch)))
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

const draftReturningProjection = `id::text,database_id::text,title,sql,revision,created_by::text,created_at,updated_at,applied_execution_id::text,sql_down,irreversible_reason,mode,batch,
	(applied_execution_id IS NOT NULL),applied_execution_id::text,
	CASE WHEN applied_execution_id IS NOT NULL THEN 'up'::text ELSE NULL::text END`

const draftSelect = `SELECT d.id::text,d.database_id::text,d.title,d.sql,d.revision,d.created_by::text,
	d.created_at,d.updated_at,d.applied_execution_id::text,d.sql_down,d.irreversible_reason,d.mode,d.batch,
	CASE WHEN m2.execution_id IS NOT NULL THEN m2.direction = 'up'
	     ELSE d.applied_execution_id IS NOT NULL END AS is_applied,
	COALESCE(m2.execution_id::text,d.applied_execution_id::text) AS latest_execution_id,
	COALESCE(m2.direction,CASE WHEN d.applied_execution_id IS NOT NULL THEN 'up'::text ELSE NULL::text END) AS latest_execution_direction
	FROM app.migration_drafts d
	LEFT JOIN LATERAL (
		SELECT h.execution_id,h.direction
		FROM app.migration_history_entries h
		WHERE h.database_id=d.database_id AND h.migration_id=d.id
		  AND h.revision=d.revision AND h.job_id IS NOT NULL AND h.is_complete
		ORDER BY h.version DESC,h.committed_at DESC,h.commit_id DESC
		LIMIT 1
	) m2 ON TRUE`

const draftProjection = draftReturningProjection

type rowScanner interface{ Scan(...any) error }

func scan(row rowScanner) (Draft, error) {
	var d Draft
	var applied, latest, direction sql.NullString
	var batch []byte
	err := row.Scan(&d.ID, &d.DatabaseID, &d.Title, &d.SQL, &d.Revision, &d.CreatedBy, &d.CreatedAt, &d.UpdatedAt,
		&applied, &d.SQLDown, &d.IrreversibleReason, &d.Mode, &batch, &d.IsApplied, &latest, &direction)
	if err == nil && len(batch) != 0 {
		err = json.Unmarshal(batch, &d.Batch)
	}
	d.CreatedAt = d.CreatedAt.UTC()
	d.UpdatedAt = d.UpdatedAt.UTC()
	if applied.Valid {
		d.AppliedExecutionID = &applied.String
	}
	if latest.Valid {
		d.LatestExecutionID = &latest.String
	}
	if direction.Valid {
		d.LatestExecutionDirection = &direction.String
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
		if key != "title" && key != "sql" && key != "sql_down" && key != "irreversible_reason" && key != "mode" && key != "batch" && !(patch && key == "revision") {
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
	// A nil value clears a field only when the request actually included it.
	_, in.hasSQLDown = fields["sql_down"]
	_, in.hasIrreversibleReason = fields["irreversible_reason"]
	_, in.hasMode = fields["mode"]
	_, in.hasBatch = fields["batch"]
	if in.hasMode && in.Mode == nil {
		fieldError(w, "mode", "Must be standard or batch")
		return in, false
	}
	return in, true
}
func validate(w http.ResponseWriter, in *input) error {
	if in.hasMode && *in.Mode != executioncontract.Standard && *in.Mode != executioncontract.Batch {
		fieldError(w, "mode", "Must be standard or batch")
		return errors.New("invalid")
	}
	if in.Batch != nil && (in.Batch.BatchSize < 1 || in.Batch.BatchSize > 10000) {
		fieldError(w, "batch", "Batch size must be between 1 and 10000")
		return errors.New("invalid")
	}
	in.Title = strings.TrimSpace(in.Title)
	if strings.ContainsRune(in.Title, 0) || len([]rune(in.Title)) < 1 || len([]rune(in.Title)) > 100 {
		fieldError(w, "title", "Must contain 1 to 100 characters")
		return errors.New("invalid")
	}
	if !validText(in.SQL, 262144) {
		fieldError(w, "sql", "Must contain nonblank SQL up to 262144 bytes without NUL")
		return errors.New("invalid")
	}
	if in.SQLDown != nil && !validText(*in.SQLDown, 262144) {
		fieldError(w, "sql_down", "Must contain nonblank SQL up to 262144 bytes without NUL")
		return errors.New("invalid")
	}
	if in.IrreversibleReason != nil && !validText(*in.IrreversibleReason, 1024) {
		fieldError(w, "irreversible_reason", "Must contain nonblank text up to 1024 bytes without NUL")
		return errors.New("invalid")
	}
	return nil
}
func validateBatch(w http.ResponseWriter, mode executioncontract.Mode, batch *executioncontract.BatchConfig) bool {
	if (mode == executioncontract.Batch) != (batch != nil) {
		fieldError(w, "batch", "Batch configuration must match mode and have a size between 1 and 10000")
		return false
	}
	return true
}
func validText(value string, maxBytes int) bool {
	return len(value) <= maxBytes && strings.IndexByte(value, 0) < 0 && strings.TrimSpace(value) != ""
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
