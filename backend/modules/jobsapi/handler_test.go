package jobsapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/Jerannn24/Tugas-Besar-WBD.git/executioncontract"
	"github.com/Jerannn24/Tugas-Besar-WBD.git/modules/auth"
	"github.com/Jerannn24/Tugas-Besar-WBD.git/modules/authz"
	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
)

const (
	dbID    = "11111111-1111-4111-8111-111111111111"
	jobID   = "50000000-0000-4000-8000-000000000001"
	execID  = "60000000-0000-4000-8000-000000000001"
	execID2 = "60000000-0000-4000-8000-000000000002"
	leak    = "dial tcp secret-host:5432 password=hunter2"
)

var actorID = uuid.MustParse("22222222-2222-4222-8222-222222222222")

type jobReaderDouble struct {
	job      executioncontract.JobView
	jobs     executioncontract.JobPage
	attempts executioncontract.AttemptPage
	err      error

	calls      []string
	databaseID string
	jobID      string
	page       executioncontract.Page
}

func (d *jobReaderDouble) GetJob(_ context.Context, databaseID, jobID string) (executioncontract.JobView, error) {
	d.calls = append(d.calls, "GetJob")
	d.databaseID, d.jobID = databaseID, jobID
	return d.job, d.err
}

func (d *jobReaderDouble) ListJobs(_ context.Context, databaseID string, page executioncontract.Page) (executioncontract.JobPage, error) {
	d.calls = append(d.calls, "ListJobs")
	d.databaseID, d.page = databaseID, page
	return d.jobs, d.err
}

func (d *jobReaderDouble) ListAttempts(_ context.Context, databaseID string, page executioncontract.Page) (executioncontract.AttemptPage, error) {
	d.calls = append(d.calls, "ListAttempts")
	d.databaseID, d.page = databaseID, page
	return d.attempts, d.err
}

type actionReaderDouble struct {
	actions map[string][]executioncontract.AllowedAction
	err     error
	calls   []string
	actorID string
}

func (d *actionReaderDouble) AllowedActions(_ context.Context, actorID, _, executionID string) ([]executioncontract.AllowedAction, error) {
	d.calls = append(d.calls, executionID)
	d.actorID = actorID
	if d.err != nil {
		return nil, d.err
	}
	return d.actions[executionID], nil
}

type authorizerDouble struct {
	accessErr error
	calls     int
	actorID   uuid.UUID
	database  uuid.UUID
	required  authz.Access
}

func (d *authorizerDouble) ResolveActor(_ context.Context, userID uuid.UUID) (authz.Actor, error) {
	return authz.Actor{ID: userID}, nil
}

func (d *authorizerDouble) EffectiveAccess(context.Context, uuid.UUID, uuid.UUID) (authz.Access, error) {
	return authz.AccessView, nil
}

func (d *authorizerDouble) RequireAccess(_ context.Context, actorID, databaseID uuid.UUID, required authz.Access) error {
	d.calls++
	d.actorID, d.database, d.required = actorID, databaseID, required
	return d.accessErr
}

func (d *authorizerDouble) RequireAdmin(authz.Actor) error { return authz.ErrForbidden }

type fixture struct {
	jobs    *jobReaderDouble
	actions *actionReaderDouble
	authz   *authorizerDouble
}

func newFixture() *fixture {
	return &fixture{
		jobs: &jobReaderDouble{
			job:  jobView(jobID, execID),
			jobs: executioncontract.JobPage{Items: []executioncontract.JobView{jobView(jobID, execID)}},
		},
		actions: &actionReaderDouble{actions: map[string][]executioncontract.AllowedAction{
			execID: {"propose_rollback"},
		}},
		authz: &authorizerDouble{},
	}
}

func jobView(id, executionID string) executioncontract.JobView {
	return executioncontract.JobView{
		JobAccepted: executioncontract.JobAccepted{JobID: id, ExecutionID: executionID, DatabaseID: dbID, Status: executioncontract.Succeeded},
		Mode:        executioncontract.Standard,
	}
}

// sessionAs mimics auth.SessionMiddleware so the real authz.ActorMiddleware resolves the actor.
func sessionAs(userID uuid.UUID) fiber.Handler {
	return func(c *fiber.Ctx) error {
		c.Locals("auth.session", &auth.AuthenticatedSession{Session: &auth.Session{UserID: userID}})
		return c.Next()
	}
}

func (f *fixture) app(t *testing.T, withSession bool) *fiber.App {
	t.Helper()
	app := fiber.New()
	api := app.Group("/api")
	if withSession {
		api.Use(sessionAs(actorID))
	}
	protected := api.Group("", authz.ActorMiddleware(f.authz))
	if err := Register(protected, Dependencies{Jobs: f.jobs, Actions: f.actions, Authorizer: f.authz}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	return app
}

type envelope struct {
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data"`
	Error   *struct {
		Code        string            `json:"code"`
		Message     string            `json:"message"`
		FieldErrors map[string]string `json:"field_errors"`
	} `json:"error"`
}

func do(t *testing.T, app *fiber.App, target string) (int, envelope, string) {
	t.Helper()
	req := httptest.NewRequest("GET", target, nil)
	req.Header.Set("X-Actor-Id", uuid.NewString())
	res, err := app.Test(req, -1)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	raw, _ := io.ReadAll(res.Body)
	var body envelope
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return res.StatusCode, body, string(raw)
}

func errorCode(body envelope) string {
	if body.Error == nil {
		return ""
	}
	return body.Error.Code
}

const base = "/api/databases/" + dbID

func TestStatusMapping(t *testing.T) {
	notFound := executioncontract.ContractError{Code: "NOT_FOUND", Message: "job " + leak}
	tests := []struct {
		name       string
		target     string
		session    bool
		accessErr  error
		modelErr   error
		wantStatus int
		wantCode   string
		wantGuard  bool
		wantModel  bool
	}{
		{"no session", base + "/jobs", false, nil, nil, 401, "UNAUTHENTICATED", false, false},
		{"view only list jobs", base + "/jobs", true, nil, nil, 200, "", true, true},
		{"view only get job", base + "/jobs/" + jobID, true, nil, nil, 200, "", true, true},
		{"view only attempts", base + "/attempts", true, nil, nil, 200, "", true, true},
		{"unauthenticated from guard", base + "/jobs", true, authz.ErrUnauthenticated, nil, 401, "UNAUTHENTICATED", true, false},
		{"forbidden", base + "/jobs", true, authz.ErrForbidden, nil, 403, "FORBIDDEN", true, false},
		{"forbidden detail", base + "/jobs/" + jobID, true, authz.ErrForbidden, nil, 403, "FORBIDDEN", true, false},
		{"database not found", base + "/attempts", true, authz.ErrNotFound, nil, 404, "NOT_FOUND", true, false},
		{"guard internal error", base + "/jobs", true, errors.New(leak), nil, 500, "INTERNAL_ERROR", true, false},
		{"job in other database", base + "/jobs/" + jobID, true, nil, notFound, 404, "NOT_FOUND", true, true},
		{"job not found pointer error", base + "/jobs/" + jobID, true, nil, &notFound, 404, "NOT_FOUND", true, true},
		{"invalid database uuid", "/api/databases/not-a-uuid/jobs", true, nil, nil, 400, "INVALID_REQUEST", false, false},
		{"invalid database uuid detail", "/api/databases/not-a-uuid/jobs/" + jobID, true, nil, nil, 400, "INVALID_REQUEST", false, false},
		{"invalid job uuid", base + "/jobs/not-a-uuid", true, nil, nil, 400, "INVALID_REQUEST", false, false},
		{"cursor rejected by model", base + "/jobs?cursor=foreign", true, nil, executioncontract.ContractError{Code: "INVALID_REQUEST", Message: leak}, 400, "INVALID_REQUEST", true, true},
		{"model raw error", base + "/jobs", true, nil, errors.New(leak), 500, "INTERNAL_ERROR", true, true},
		{"model not configured", base + "/attempts", true, nil, executioncontract.ErrNotConfigured, 500, "INTERNAL_ERROR", true, true},
		{"model other contract code", base + "/jobs/" + jobID, true, nil, executioncontract.ContractError{Code: "TARGET_UNAVAILABLE", Message: leak}, 500, "INTERNAL_ERROR", true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture()
			f.authz.accessErr = tt.accessErr
			f.jobs.err = tt.modelErr
			status, body, raw := do(t, f.app(t, tt.session), tt.target)
			if status != tt.wantStatus || errorCode(body) != tt.wantCode {
				t.Fatalf("status=%d code=%q, want %d %q; body %s", status, errorCode(body), tt.wantStatus, tt.wantCode, raw)
			}
			if body.Success != (tt.wantStatus == 200) {
				t.Fatalf("success=%v for status %d", body.Success, status)
			}
			if (f.authz.calls > 0) != tt.wantGuard {
				t.Fatalf("guard calls=%d, want called=%v", f.authz.calls, tt.wantGuard)
			}
			if (len(f.jobs.calls) > 0) != tt.wantModel {
				t.Fatalf("model calls=%v, want called=%v", f.jobs.calls, tt.wantModel)
			}
			if strings.Contains(raw, "hunter2") || strings.Contains(raw, "secret-host") {
				t.Fatalf("response leaks internal error: %s", raw)
			}
		})
	}
}

func TestGuardUsesContextActorAndViewAccess(t *testing.T) {
	f := newFixture()
	status, _, raw := do(t, f.app(t, true), base+"/jobs/"+jobID+"?actor_id="+uuid.NewString())
	if status != 200 {
		t.Fatalf("status=%d body %s", status, raw)
	}
	if f.authz.actorID != actorID || f.authz.database.String() != dbID || f.authz.required != authz.AccessView {
		t.Fatalf("RequireAccess(%v, %v, %v)", f.authz.actorID, f.authz.database, f.authz.required)
	}
	if f.jobs.databaseID != dbID || f.jobs.jobID != jobID {
		t.Fatalf("GetJob(%q, %q)", f.jobs.databaseID, f.jobs.jobID)
	}
	if f.actions.actorID != actorID.String() {
		t.Fatalf("AllowedActions actor=%q", f.actions.actorID)
	}
}

func TestHandlerWithoutActorInContext(t *testing.T) {
	f := newFixture()
	h, err := New(Dependencies{Jobs: f.jobs, Actions: f.actions, Authorizer: f.authz})
	if err != nil {
		t.Fatal(err)
	}
	app := fiber.New()
	app.Get("/api/databases/:database_id/jobs", h.ListJobs)
	status, body, _ := do(t, app, base+"/jobs")
	if status != 401 || errorCode(body) != "UNAUTHENTICATED" || f.authz.calls != 0 {
		t.Fatalf("status=%d code=%q guard=%d", status, errorCode(body), f.authz.calls)
	}
}

func TestPageValidation(t *testing.T) {
	tests := []struct {
		name       string
		query      string
		wantStatus int
		wantField  string
		wantLimit  int
	}{
		{"default limit", "", 200, "", 20},
		{"min limit", "?limit=1", 200, "", 1},
		{"max limit", "?limit=100", 200, "", 100},
		{"zero", "?limit=0", 400, "limit", 0},
		{"too large", "?limit=101", 400, "limit", 0},
		{"negative", "?limit=-5", 400, "limit", 0},
		{"not a number", "?limit=abc", 400, "limit", 0},
		{"decimal", "?limit=2.5", 400, "limit", 0},
		{"empty limit", "?limit=", 400, "limit", 0},
		{"empty cursor", "?cursor=", 400, "cursor", 0},
	}
	for _, path := range []string{"/jobs", "/attempts"} {
		for _, tt := range tests {
			t.Run(path+" "+tt.name, func(t *testing.T) {
				f := newFixture()
				status, body, raw := do(t, f.app(t, true), base+path+tt.query)
				if status != tt.wantStatus {
					t.Fatalf("status=%d, want %d; body %s", status, tt.wantStatus, raw)
				}
				if tt.wantStatus == 400 {
					if errorCode(body) != "INVALID_REQUEST" || body.Error.FieldErrors[tt.wantField] == "" {
						t.Fatalf("want INVALID_REQUEST with field_errors.%s; body %s", tt.wantField, raw)
					}
					if len(f.jobs.calls) != 0 {
						t.Fatalf("model called on invalid page: %v", f.jobs.calls)
					}
					return
				}
				if f.jobs.page.Limit != tt.wantLimit || f.jobs.page.Cursor != nil {
					t.Fatalf("page=%+v, want limit %d and nil cursor", f.jobs.page, tt.wantLimit)
				}
			})
		}
	}
}

func TestCursorPassedThrough(t *testing.T) {
	cursor := "opaque.Token_-~with=equals"
	next := "next-opaque=="
	for _, path := range []string{"/jobs", "/attempts"} {
		t.Run(path, func(t *testing.T) {
			f := newFixture()
			f.jobs.jobs.NextCursor = &next
			f.jobs.attempts = executioncontract.AttemptPage{Items: []executioncontract.Attempt{{AttemptID: "a1"}}, NextCursor: &next}
			status, body, raw := do(t, f.app(t, true), base+path+"?limit=5&cursor=opaque.Token_-~with%3Dequals")
			if status != 200 {
				t.Fatalf("status=%d body %s", status, raw)
			}
			if f.jobs.page.Cursor == nil || *f.jobs.page.Cursor != cursor || f.jobs.page.Limit != 5 {
				t.Fatalf("page=%+v", f.jobs.page)
			}
			var data struct {
				NextCursor *string `json:"next_cursor"`
			}
			if err := json.Unmarshal(body.Data, &data); err != nil || data.NextCursor == nil || *data.NextCursor != next {
				t.Fatalf("next_cursor not passed through: %s", raw)
			}
		})
	}
}

func TestEmptyListsAreArrays(t *testing.T) {
	for _, path := range []string{"/jobs", "/attempts"} {
		t.Run(path, func(t *testing.T) {
			f := newFixture()
			f.jobs.jobs = executioncontract.JobPage{}
			f.jobs.attempts = executioncontract.AttemptPage{}
			status, body, raw := do(t, f.app(t, true), base+path)
			if status != 200 || string(body.Data) != `{"items":[],"next_cursor":null}` {
				t.Fatalf("status=%d data=%s", status, raw)
			}
		})
	}
}

func TestAllowedActions(t *testing.T) {
	t.Run("filled per execution and cached", func(t *testing.T) {
		f := newFixture()
		f.actions.actions[execID2] = nil
		f.jobs.jobs.Items = []executioncontract.JobView{jobView(jobID, execID), jobView(uuid.NewString(), execID), jobView(uuid.NewString(), execID2)}
		_, body, raw := do(t, f.app(t, true), base+"/jobs")
		var page executioncontract.JobPage
		if err := json.Unmarshal(body.Data, &page); err != nil {
			t.Fatal(err)
		}
		got := [][]executioncontract.AllowedAction{page.Items[0].AllowedActions, page.Items[1].AllowedActions, page.Items[2].AllowedActions}
		want := [][]executioncontract.AllowedAction{{"propose_rollback"}, {"propose_rollback"}, {}}
		if !reflect.DeepEqual(got, want) || !strings.Contains(raw, `"allowed_actions":[]`) {
			t.Fatalf("allowed_actions=%v, want %v; body %s", got, want, raw)
		}
		if !reflect.DeepEqual(f.actions.calls, []string{execID, execID2}) {
			t.Fatalf("AllowedActions calls=%v", f.actions.calls)
		}
	})
	for _, path := range []string{"/jobs", "/jobs/" + jobID} {
		for _, tc := range []struct {
			name   string
			err    error
			status int
		}{
			{"internal", errors.New(leak), 500},
			{"forbidden", executioncontract.ContractError{Code: "FORBIDDEN"}, 403},
			{"unauthenticated", executioncontract.ContractError{Code: "UNAUTHENTICATED"}, 401},
		} {
			t.Run(tc.name+" "+path, func(t *testing.T) {
				f := newFixture()
				f.actions.err = tc.err
				status, _, raw := do(t, f.app(t, true), base+path)
				if status != tc.status || strings.Contains(raw, leak) || strings.Contains(raw, "allowed_actions") {
					t.Fatalf("status=%d body %s", status, raw)
				}
			})
		}
	}
	t.Run("model value not trusted", func(t *testing.T) {
		f := newFixture()
		f.jobs.job.AllowedActions = []executioncontract.AllowedAction{"approve"}
		f.actions.actions = nil
		_, _, raw := do(t, f.app(t, true), base+"/jobs/"+jobID)
		if !strings.Contains(raw, `"allowed_actions":[]`) {
			t.Fatalf("body %s", raw)
		}
	})
}

func TestOnlyReadPorts(t *testing.T) {
	methods := func(v any) []string {
		typ := reflect.TypeOf(v).Elem()
		names := make([]string, typ.NumMethod())
		for i := range names {
			names[i] = typ.Method(i).Name
		}
		sort.Strings(names)
		return names
	}
	if got := methods((*JobReader)(nil)); !reflect.DeepEqual(got, []string{"GetJob", "ListAttempts", "ListJobs"}) {
		t.Fatalf("JobReader methods=%v", got)
	}
	if got := methods((*ActionReader)(nil)); !reflect.DeepEqual(got, []string{"AllowedActions"}) {
		t.Fatalf("ActionReader methods=%v", got)
	}
	// The double has no EnqueueTx/FinalizeJobTx, so it cannot satisfy the full model port.
	if _, ok := any(&jobReaderDouble{}).(executioncontract.Jobs); ok {
		t.Fatal("double must not implement executioncontract.Jobs")
	}
}

func TestNewRejectsMissingDependencies(t *testing.T) {
	f := newFixture()
	for name, deps := range map[string]Dependencies{
		"jobs":       {Actions: f.actions, Authorizer: f.authz},
		"actions":    {Jobs: f.jobs, Authorizer: f.authz},
		"authorizer": {Jobs: f.jobs, Actions: f.actions},
	} {
		if _, err := New(deps); err == nil {
			t.Errorf("missing %s: want error", name)
		}
		if err := Register(fiber.New(), deps); err == nil {
			t.Errorf("Register missing %s: want error", name)
		}
	}
}
