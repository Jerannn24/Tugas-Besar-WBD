package workflow

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Jerannn24/Tugas-Besar-WBD.git/executioncontract"
	"github.com/Jerannn24/Tugas-Besar-WBD.git/modules/authz"
	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
)

const (
	batchUp   = "UPDATE managed.batch_items SET processed = TRUE WHERE id = ANY($1::bigint[]);"
	batchDown = "UPDATE managed.batch_items SET processed = FALSE WHERE id = ANY($1::bigint[]);"
)

func batchSnapshot(up string, down *string) Snapshot {
	return Snapshot{
		Title: "Batch", SQLUp: up, SQLDown: down, Revision: 1, Mode: "batch",
		Batch: map[string]any{"schema": "managed", "table": "batch_items", "cursor_column": "id", "batch_size": float64(2)},
	}
}

func issueCodes(v executioncontract.Validation) string {
	codes := []string{}
	for _, issue := range v.Issues {
		codes = append(codes, issue.Code)
	}
	return strings.Join(codes, ",")
}

func TestValidateSnapshotBatchPattern(t *testing.T) {
	down, badDown := batchDown, "DELETE FROM managed.batch_items"
	wrongTable := batchSnapshot(batchUp, &down)
	wrongTable.Batch["table"] = "items"
	for _, tc := range []struct {
		name     string
		snapshot Snapshot
		status   string
		codes    string
	}{
		{"agreed pattern", batchSnapshot(batchUp, &down), "valid", ""},
		{"irreversible batch", batchSnapshot(batchUp, nil), "valid", ""},
		{"unsupported up", batchSnapshot("UPDATE managed.batch_items SET processed = TRUE", &down), "invalid", "SQL_UNSUPPORTED"},
		{"unsupported down", batchSnapshot(batchUp, &badDown), "invalid", "SQL_UNSUPPORTED"},
		{"unsupported config", wrongTable, "invalid", "INVALID_BATCH_CONFIG"},
		{"standard is free form", Snapshot{Title: "Std", SQLUp: "DELETE FROM managed.items", Mode: "standard"}, "valid", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := validateSnapshot(tc.snapshot)
			if got.Status != tc.status || issueCodes(got) != tc.codes {
				t.Fatalf("got %s %q, want %s %q", got.Status, issueCodes(got), tc.status, tc.codes)
			}
		})
	}
}

type failingSubmitRepo struct {
	scopedRepo
	issues []executioncontract.Issue
}

func (r *failingSubmitRepo) Submit(context.Context, uuid.UUID, uuid.UUID, int) (Proposal, error) {
	r.submitted++
	p := r.proposal
	p.Validation = &executioncontract.Validation{Status: "failed", Issues: r.issues}
	return p, ErrInvalidProposal
}

func TestSubmitReviewRejectsUnsupportedBatchSQL(t *testing.T) {
	db, id, actor := uuid.New(), uuid.New(), uuid.New()
	for _, tc := range []struct {
		name   string
		edit   bool
		issues []executioncontract.Issue
		status int
		code   string
	}{
		{"unsupported sql", true, []executioncontract.Issue{{Code: "SQL_UNSUPPORTED", Message: "sql_up batch"}}, 422, "SQL_UNSUPPORTED"},
		{"other validation", true, []executioncontract.Issue{{Code: "INVALID_BATCH_CONFIG", Message: "batch"}}, 422, "VALIDATION_FAILED"},
		{"no edit access", false, []executioncontract.Issue{{Code: "SQL_UNSUPPORTED", Message: "sql_up batch"}}, 403, "FORBIDDEN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &failingSubmitRepo{
				scopedRepo: scopedRepo{proposal: Proposal{ProposalID: id.String(), DatabaseID: db.String(), Revision: 1, Status: "draft"}},
				issues:     tc.issues,
			}
			service := &Service{repository: repo, authorizer: scopedAuth{edit: tc.edit}}
			app := fiber.New()
			app.Use(func(c *fiber.Ctx) error { c.Locals("authz.actor", authz.Actor{ID: actor}); return c.Next() })
			RegisterRoutes(app, NewHandler(service), func(c *fiber.Ctx) error { return c.Next() })
			req := httptest.NewRequest("POST", "/databases/"+db.String()+"/proposals/"+id.String()+"/submit-review", strings.NewReader(`{"revision":1}`))
			req.Header.Set("Content-Type", "application/json")
			res, err := app.Test(req)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			raw, _ := io.ReadAll(res.Body)
			var body struct {
				Error struct {
					Code   string                    `json:"code"`
					Issues []executioncontract.Issue `json:"issues"`
				} `json:"error"`
			}
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Fatalf("decode %s: %v", raw, err)
			}
			if res.StatusCode != tc.status || body.Error.Code != tc.code {
				t.Fatalf("got %d %s, want %d %s: %s", res.StatusCode, body.Error.Code, tc.status, tc.code, raw)
			}
			if tc.status == 422 && (len(body.Error.Issues) != 1 || body.Error.Issues[0].Code != tc.issues[0].Code) {
				t.Fatalf("issues not forwarded: %s", raw)
			}
			if !tc.edit && repo.submitted != 0 {
				t.Fatal("forbidden request reached the repository")
			}
		})
	}
}
