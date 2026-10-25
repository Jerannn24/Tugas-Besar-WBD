package migration

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Jerannn24/Tugas-Besar-WBD.git/history"
	"github.com/Jerannn24/Tugas-Besar-WBD.git/sqlpolicy"
)

type Actor struct{ ID string }

type Dependencies struct {
	Platform *sql.DB
	Actor    func(*http.Request) (Actor, error)
	Authorize func(context.Context, Actor, string) error
	CheckCSRF func(*http.Request) error
	Acquire func(context.Context, string) (*TargetLease, error)
	Preflight func(context.Context, *sql.Tx, string) error
	Timeout   time.Duration
}

type ExecuteRequest struct {
	Revision  int    `json:"revision"`
	RequestID string `json:"request_id"`
}

type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string   { return e.Message }
func (e *APIError) StatusCode() int { return e.Status }
func fault(status int, code, message string) error {
	return &APIError{Status: status, Code: code, Message: message}
}

type Handler struct {
	d      Dependencies
	policy *sqlpolicy.Policy
	store  platformStore
}

func New(d Dependencies) (*Handler, error) {
	if d.Platform == nil || d.Actor == nil || d.Authorize == nil || d.CheckCSRF == nil || d.Acquire == nil || d.Preflight == nil || d.Timeout <= 0 {
		return nil, errors.New("migration dependencies are required")
	}
	policy, err := sqlpolicy.New("managed")
	if err != nil {
		return nil, err
	}
	return &Handler{d: d, policy: policy, store: sqlStore{db: d.Platform}}, nil
}

func (h *Handler) Register(m *http.ServeMux) error {
	if m == nil {
		return errors.New("mux is required")
	}
	m.HandleFunc("POST /api/databases/{database_id}/drafts/{draft_id}/execute", h.executeHTTP)
	return nil
}

type attempt struct {
	history.Execution
	TargetID  string
	RequestID string
	Hash      []byte
}

func terminal(status string) bool { return status == "succeeded" || status == "failed" }

func sameIntent(a attempt, actor Actor, draftID string, in ExecuteRequest) error {
	if a.ExecutedBy.ID != actor.ID || a.DraftID != draftID || a.DraftRevision != in.Revision {
		return fault(409, "REQUEST_CONFLICT", "Request id belongs to a different execution intent")
	}
	return nil
}

func (h *Handler) Execute(ctx context.Context, actor Actor, databaseID, draftID string, in ExecuteRequest) (history.Execution, error) {
	ctx, cancel := context.WithTimeout(ctx, h.d.Timeout)
	defer cancel()
	if !validUUID(actor.ID) || !validUUID(databaseID) || !validUUID(draftID) || !validUUID(in.RequestID) || in.Revision < 1 || int64(in.Revision) > 2147483647 {
		return history.Execution{}, fault(400, "INVALID_REQUEST", "Invalid execution request")
	}
	actor.ID, databaseID, draftID, in.RequestID = strings.ToLower(actor.ID), strings.ToLower(databaseID), strings.ToLower(draftID), strings.ToLower(in.RequestID)
	if err := h.d.Authorize(ctx, actor, databaseID); err != nil {
		return history.Execution{}, guardError(err)
	}
	existing, err := h.store.lookup(ctx, databaseID, in.RequestID)
	if err == nil {
		if err := sameIntent(existing, actor, draftID, in); err != nil {
			return history.Execution{}, err
		}
		return existing.Execution, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return history.Execution{}, err
	}
	targetID, err := h.store.binding(ctx, databaseID)
	if err != nil {
		return history.Execution{}, err
	}
	session, err := h.openTarget(ctx, databaseID, targetID)
	if err != nil {
		return history.Execution{}, err
	}
	defer session.close(h.d.Timeout)
	if err := h.d.Authorize(ctx, actor, databaseID); err != nil {
		return history.Execution{}, guardError(err)
	}
	existing, err = h.store.lookup(ctx, databaseID, in.RequestID)
	if err == nil {
		if err := sameIntent(existing, actor, draftID, in); err != nil {
			return history.Execution{}, err
		}
		return existing.Execution, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return history.Execution{}, err
	}
	if err := session.begin(ctx, h.d.Timeout); err != nil {
		return history.Execution{}, err
	}
	a, created, err := h.store.reserve(ctx, actor, databaseID, draftID, targetID, in, func(sqlText string) error {
		if err := h.d.Authorize(ctx, actor, databaseID); err != nil {
			return guardError(err)
		}
		if err := h.policy.Validate(sqlText); err != nil {
			return fault(422, "SQL_UNSUPPORTED", "SQL is outside the supported subset")
		}
		if err := h.d.Preflight(ctx, session.tx, sqlText); err != nil {
			var api *APIError
			if errors.As(err, &api) {
				return api
			}
			return fault(502, "TARGET_UNAVAILABLE", "Target safety check could not be completed")
		}
		return nil
	})
	if err != nil {
		return history.Execution{}, err
	}
	if !created {
		if err := h.d.Authorize(ctx, actor, databaseID); err != nil {
			return history.Execution{}, guardError(err)
		}
		return a.Execution, nil
	}
	var result outcome
	if err := h.d.Authorize(ctx, actor, databaseID); err != nil {
		result = session.abort("ACCESS_REVOKED", "Execution permission is no longer available")
	} else {
		result = session.run(ctx, a)
	}
	finishCtx, finishCancel := context.WithTimeout(context.WithoutCancel(ctx), h.d.Timeout)
	defer finishCancel()
	finished, err := h.finish(finishCtx, a, result)
	if err != nil {
		if finished.ID != "" {
			return finished.Execution, err
		}
		return a.Execution, err
	}
	return finished.Execution, nil
}

func (h *Handler) finish(ctx context.Context, a attempt, result outcome) (attempt, error) {
	if result.status == "succeeded" && (result.receipt == nil || !matchingReceipt(a, *result.receipt)) {
		updated, err := h.store.transition(ctx, a, "unknown", &history.ExecutionError{Code: "RECEIPT_MISMATCH", Message: "Execution receipt does not match the attempt"})
		if err != nil {
			return attempt{}, err
		}
		return updated, fault(409, "REQUEST_CONFLICT", "Execution receipt does not match the attempt")
	}
	return h.store.transition(ctx, a, result.status, result.failure)
}

func (h *Handler) executeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), h.d.Timeout)
	defer cancel()
	actor, err := h.d.Actor(r.WithContext(ctx))
	if err != nil || actor.ID == "" {
		writeError(w, fault(401, "UNAUTHENTICATED", "Authentication required"))
		return
	}
	if err := h.d.CheckCSRF(r.WithContext(ctx)); err != nil {
		writeError(w, fault(403, "CSRF_INVALID", "Request verification failed"))
		return
	}
	var in ExecuteRequest
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4096))
	if err != nil || !utf8.Valid(raw) {
		writeError(w, fault(400, "INVALID_REQUEST", "Invalid JSON request"))
		return
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || len(fields) != 2 || fields["revision"] == nil || fields["request_id"] == nil || json.Unmarshal(raw, &in) != nil {
		writeError(w, fault(400, "INVALID_REQUEST", "Invalid JSON request"))
		return
	}
	execution, err := h.Execute(ctx, actor, r.PathValue("database_id"), r.PathValue("draft_id"), in)
	if err != nil && execution.ID == "" {
		writeError(w, err)
		return
	}
	status := http.StatusOK
	if err != nil || !terminal(execution.Status) {
		status = http.StatusAccepted
	}
	write(w, status, map[string]any{"data": execution})
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

func newUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6], b[8] = b[6]&0x0f|0x40, b[8]&0x3f|0x80
	x := hex.EncodeToString(b[:])
	return x[:8] + "-" + x[8:12] + "-" + x[12:16] + "-" + x[16:20] + "-" + x[20:], nil
}

func guardError(err error) error {
	var status interface{ StatusCode() int }
	if errors.As(err, &status) {
		switch status.StatusCode() {
		case 401:
			return fault(401, "UNAUTHENTICATED", "Authentication required")
		case 403:
			return fault(403, "FORBIDDEN", "Access denied")
		case 404:
			return fault(404, "NOT_FOUND", "Database not found")
		}
	}
	return fault(500, "INTERNAL_ERROR", "Internal error")
}

func writeError(w http.ResponseWriter, err error) {
	api := &APIError{Status: 500, Code: "INTERNAL_ERROR", Message: "Internal error"}
	var supplied *APIError
	if errors.As(err, &supplied) {
		api = supplied
	}
	write(w, api.Status, map[string]any{"error": map[string]any{
		"code": api.Code, "message": api.Message, "field_errors": map[string]string{}, "retryable": false,
	}})
}

func write(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func sqlHash(sqlText string) []byte { hash := sha256.Sum256([]byte(sqlText)); return hash[:] }
