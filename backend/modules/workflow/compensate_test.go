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

type compensateRepo struct {
	scopedRepo
	sourceErr error
	created   int
}

func (r *compensateRepo) LoadDraft(context.Context, uuid.UUID, uuid.UUID) (Snapshot, error) {
	return Snapshot{Title: "Batch", SQLUp: batchUp, Revision: 1, Mode: "batch"}, nil
}
func (r *compensateRepo) sourceSnapshot(context.Context, uuid.UUID, uuid.UUID, int, string, *uuid.UUID, string) (Snapshot, error) {
	return Snapshot{}, r.sourceErr
}
func (r *compensateRepo) CreateProposal(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, int, string, *uuid.UUID, Snapshot, [32]byte, [32]byte, []byte) (uuid.UUID, error) {
	r.created++
	return uuid.Nil, nil
}

func TestCreateCompensateRejectsIrreversibleSource(t *testing.T) {
	db, actor, migration, source := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	irreversible := executioncontract.ContractError{Code: "MIGRATION_IRREVERSIBLE", Message: "no sql down"}
	for _, tc := range []struct {
		name   string
		edit   bool
		err    error
		status int
		code   string
	}{
		{"irreversible source", true, irreversible, 422, "MIGRATION_IRREVERSIBLE"},
		{"invalid source", true, ErrInvalidProposal, 400, "INVALID_REQUEST"},
		{"no edit access", false, irreversible, 403, "FORBIDDEN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &compensateRepo{sourceErr: tc.err}
			service := &Service{repository: repo, authorizer: scopedAuth{edit: tc.edit}}
			app := fiber.New()
			app.Use(func(c *fiber.Ctx) error { c.Locals("authz.actor", authz.Actor{ID: actor}); return c.Next() })
			RegisterRoutes(app, NewHandler(service), func(c *fiber.Ctx) error { return c.Next() })
			body := `{"migration_id":"` + migration.String() + `","revision":1,"action":"compensate","source_execution_id":"` + source.String() + `"}`
			req := httptest.NewRequest("POST", "/databases/"+db.String()+"/proposals", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			res, err := app.Test(req)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			raw, _ := io.ReadAll(res.Body)
			var out struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			_ = json.Unmarshal(raw, &out)
			if res.StatusCode != tc.status || out.Error.Code != tc.code || repo.created != 0 {
				t.Fatalf("got %d %s created=%d, want %d %s: %s", res.StatusCode, out.Error.Code, repo.created, tc.status, tc.code, raw)
			}
		})
	}
}
