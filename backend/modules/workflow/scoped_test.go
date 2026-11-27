package workflow

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Jerannn24/Tugas-Besar-WBD.git/modules/authz"
	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
)

type scopedRepo struct {
	serviceRepository
	proposal   Proposal
	submitted  int
	review     bool
	enqueueErr error
}

func (r *scopedRepo) Get(_ context.Context, id uuid.UUID) (Proposal, error) {
	if id.String() != r.proposal.ProposalID {
		return Proposal{}, ErrNotFound
	}
	return r.proposal, nil
}
func (r *scopedRepo) Submit(_ context.Context, _, _ uuid.UUID, revision int) (Proposal, error) {
	r.submitted++
	if revision != r.proposal.Revision {
		return Proposal{}, ErrProposalStale
	}
	return r.proposal, nil
}
func (r *scopedRepo) MayReview(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (bool, error) {
	return r.review, nil
}
func (r *scopedRepo) mayEnqueue(context.Context, uuid.UUID, uuid.UUID) error { return r.enqueueErr }

type scopedAuth struct {
	authz.Authorizer
	edit bool
}

func (a scopedAuth) RequireAccess(_ context.Context, _, _ uuid.UUID, access authz.Access) error {
	if access == authz.AccessEdit && !a.edit {
		return authz.ErrForbidden
	}
	return nil
}

func TestScopedProposalRoutes(t *testing.T) {
	db, other, id, actor := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	repo := &scopedRepo{proposal: Proposal{ProposalID: id.String(), DatabaseID: db.String(), Revision: 3, Status: "draft"}}
	service := &Service{repository: repo, authorizer: scopedAuth{edit: true}}
	app := fiber.New()
	app.Use(func(c *fiber.Ctx) error { c.Locals("authz.actor", authz.Actor{ID: actor}); return c.Next() })
	RegisterRoutes(app, NewHandler(service), func(c *fiber.Ctx) error { return c.Next() })
	request := func(method, database, suffix, body string) int {
		req := httptest.NewRequest(method, "/databases/"+database+"/proposals/"+id.String()+suffix, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		res, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		return res.StatusCode
	}
	if got := request("POST", other.String(), "/submit-review", `{"revision":3}`); got != 404 {
		t.Fatalf("foreign scope: %d", got)
	}
	if got := request("POST", db.String(), "/submit-review", `{"revision":2}`); got != 409 {
		t.Fatalf("stale revision: %d", got)
	}
	if repo.submitted != 0 {
		t.Fatal("rejected request mutated proposal")
	}
	if got := request("POST", db.String(), "/submit-review", `{"revision":3}`); got != 200 || repo.submitted != 1 {
		t.Fatalf("submit: status %d calls %d", got, repo.submitted)
	}
	for _, tc := range []struct {
		status       string
		edit, review bool
		want         string
	}{{"draft", true, false, "submit_review"}, {"draft", false, false, ""}, {"pending_review", false, true, "approve"}, {"pending_review", true, false, ""}, {"approved", true, false, "enqueue"}, {"queued", true, true, ""}} {
		repo.proposal.Status, repo.review = tc.status, tc.review
		service.authorizer = scopedAuth{edit: tc.edit}
		p, err := service.present(context.Background(), actor, repo.proposal)
		if err != nil {
			t.Fatal(err)
		}
		got := strings.Join(p.AllowedActions, ",")
		if got != tc.want {
			t.Fatalf("%s: %q want %q", tc.status, got, tc.want)
		}
		raw, _ := json.Marshal(p)
		if !strings.Contains(string(raw), `"approvals":[]`) || !strings.Contains(string(raw), `"validation":null`) {
			t.Fatalf("shape: %s", raw)
		}
	}
	repo.proposal.Status, repo.enqueueErr = "approved", ErrPendingApprovals
	service.authorizer = scopedAuth{edit: true}
	p, err := service.present(context.Background(), actor, repo.proposal)
	if err != nil || len(p.AllowedActions) != 0 {
		t.Fatalf("stale approval eligibility: %+v %v", p.AllowedActions, err)
	}
}
