package admin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
)

type groupRepository struct {
	fakeAdminRepository
	err          error
	group        Group
	users        []uuid.UUID
	grants       []Grant
	called       string
	receivedID   uuid.UUID
	receivedName string
}

func (r *groupRepository) ListGroups(context.Context) ([]Group, error) {
	r.called = "list groups"
	return []Group{r.group}, r.err
}
func (r *groupRepository) CreateGroup(_ context.Context, name string) (Group, error) {
	r.called = "create group"
	r.receivedName = name
	return r.group, r.err
}
func (r *groupRepository) UpdateGroup(_ context.Context, id uuid.UUID, name string) (Group, error) {
	r.called = "update group"
	r.receivedID = id
	r.receivedName = name
	return r.group, r.err
}
func (r *groupRepository) DeleteGroup(_ context.Context, id uuid.UUID) error {
	r.called = "delete group"
	r.receivedID = id
	return r.err
}
func (r *groupRepository) Members(_ context.Context, id uuid.UUID) ([]uuid.UUID, error) {
	r.called = "members"
	r.receivedID = id
	return r.users, r.err
}
func (r *groupRepository) ReplaceMembers(_ context.Context, id uuid.UUID, users []uuid.UUID) ([]uuid.UUID, error) {
	r.called = "replace members"
	r.receivedID = id
	r.users = users
	return users, r.err
}
func (r *groupRepository) Grants(_ context.Context, id uuid.UUID) ([]Grant, error) {
	r.called = "grants"
	r.receivedID = id
	return r.grants, r.err
}
func (r *groupRepository) ReplaceGrants(_ context.Context, id uuid.UUID, grants []Grant) ([]Grant, error) {
	r.called = "replace grants"
	r.receivedID = id
	r.grants = grants
	return grants, r.err
}

func groupApp(repo serviceRepository) *fiber.App {
	app := newAdminTestApp(repo)
	h := NewHandler(&Service{repository: repo})
	app.Get("/groups", h.listGroups)
	app.Post("/groups", h.createGroup)
	app.Patch("/groups/:group_id", h.updateGroup)
	app.Delete("/groups/:group_id", h.deleteGroup)
	app.Get("/groups/:group_id/members", h.members)
	app.Put("/groups/:group_id/members", h.replaceMembers)
	app.Get("/groups/:group_id/grants", h.grants)
	app.Put("/groups/:group_id/grants", h.replaceGrants)
	return app
}
func adminRequest(t *testing.T, app *fiber.App, method, path, body string) (int, map[string]any, string) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	res, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("invalid response %q: %v", raw, err)
	}
	return res.StatusCode, v, string(raw)
}

func TestGroupHandlersSuccess(t *testing.T) {
	id, user, database := uuid.New(), uuid.New(), uuid.New()
	for _, tc := range []struct {
		name, method, suffix, body string
		status                     int
	}{{"list groups", "GET", "", "", 200}, {"create group", "POST", "", `{"name":"Editors"}`, 201}, {"update group", "PATCH", "/" + id.String(), `{"name":"Editors"}`, 200}, {"delete group", "DELETE", "/" + id.String(), "", 200}, {"members", "GET", "/" + id.String() + "/members", "", 200}, {"replace members", "PUT", "/" + id.String() + "/members", `{"user_ids":["` + user.String() + `"]}`, 200}, {"grants", "GET", "/" + id.String() + "/grants", "", 200}, {"replace grants", "PUT", "/" + id.String() + "/grants", `{"grants":[{"database_id":"` + database.String() + `","access":"edit"}]}`, 200}} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &groupRepository{group: Group{ID: id, Name: "Editors"}, users: []uuid.UUID{user}, grants: []Grant{{DatabaseID: database, Access: "edit"}}}
			status, v, raw := adminRequest(t, groupApp(repo), tc.method, "/groups"+tc.suffix, tc.body)
			if status != tc.status || repo.called != tc.name {
				t.Fatalf("status=%d call=%q body=%s", status, repo.called, raw)
			}
			data, ok := v["data"].(map[string]any)
			if !ok {
				t.Fatalf("missing data: %s", raw)
			}
			switch tc.name {
			case "list groups":
				items := data["items"].([]any)
				if len(items) != 1 || items[0].(map[string]any)["name"] != "Editors" {
					t.Fatal(raw)
				}
			case "create group", "update group":
				if data["id"] != id.String() || data["name"] != "Editors" || repo.receivedName != "Editors" {
					t.Fatal(raw)
				}
			case "delete group":
				if data["deleted"] != true {
					t.Fatal(raw)
				}
			case "members", "replace members":
				items := data["user_ids"].([]any)
				if data["group_id"] != id.String() || len(items) != 1 || items[0] != user.String() {
					t.Fatal(raw)
				}
			case "grants", "replace grants":
				items := data["grants"].([]any)
				if data["group_id"] != id.String() || len(items) != 1 || items[0].(map[string]any)["database_id"] != database.String() || items[0].(map[string]any)["access"] != "edit" {
					t.Fatal(raw)
				}
			}
			if tc.suffix != "" && repo.receivedID != id {
				t.Fatal("wrong group passed to repository")
			}
		})
	}
}

func TestGroupHandlerErrorsAndMalformedRequests(t *testing.T) {
	id := uuid.NewString()
	for _, tc := range []struct {
		method, path, body string
		err                error
		status             int
		code               string
	}{
		{"GET", "/groups", "", errors.New("private password"), 500, "INTERNAL_ERROR"}, {"POST", "/groups", `{"name":"Editors"}`, ErrConflict, 409, "CONFLICT"}, {"PATCH", "/groups/" + id, `{"name":"Editors"}`, ErrNotFound, 404, "NOT_FOUND"}, {"DELETE", "/groups/" + id, "", ErrNotFound, 404, "NOT_FOUND"}, {"GET", "/groups/" + id + "/members", "", errors.New("private password"), 500, "INTERNAL_ERROR"}, {"GET", "/groups/" + id + "/grants", "", errors.New("private password"), 500, "INTERNAL_ERROR"}, {"PUT", "/groups/" + id + "/members", `{"user_ids":[]}`, ErrInvalidInput, 400, "INVALID_INPUT"}, {"PUT", "/groups/" + id + "/grants", `{"grants":[]}`, ErrNotFound, 404, "NOT_FOUND"},
		{"POST", "/groups", `{`, nil, 400, "INVALID_INPUT"}, {"PATCH", "/groups/invalid", `{"name":"Editors"}`, nil, 400, "INVALID_INPUT"}, {"DELETE", "/groups/00000000-0000-0000-0000-000000000000", "", nil, 400, "INVALID_INPUT"}, {"GET", "/groups/invalid/members", "", nil, 400, "INVALID_INPUT"}, {"GET", "/groups/invalid/grants", "", nil, 400, "INVALID_INPUT"}, {"PUT", "/groups/" + id + "/members", `{"user_ids":["invalid"]}`, nil, 400, "INVALID_INPUT"}, {"PUT", "/groups/" + id + "/grants", `{"grants":[{"database_id":"invalid"}]}`, nil, 400, "INVALID_INPUT"},
	} {
		t.Run(tc.method+tc.path+tc.code, func(t *testing.T) {
			repo := &groupRepository{err: tc.err}
			status, v, raw := adminRequest(t, groupApp(repo), tc.method, tc.path, tc.body)
			if status != tc.status {
				t.Fatalf("%d %s", status, raw)
			}
			errBody, ok := v["error"].(map[string]any)
			if !ok || errBody["code"] != tc.code {
				t.Fatal(raw)
			}
			if strings.Contains(raw, "private") || strings.Contains(raw, "password") {
				t.Fatal("private database error exposed")
			}
			if tc.err == nil && repo.called != "" {
				t.Fatal("repository called for malformed request")
			}
		})
	}
}

func TestListUsersSuccessAndFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		repo := &fakeAdminRepository{listUsersFn: func(context.Context) ([]User, error) {
			if fail {
				return nil, errors.New("private password")
			}
			return []User{{ID: uuid.New(), Username: "admin", IsAdmin: true, IsActive: true}}, nil
		}}
		status, v, raw := adminRequest(t, newAdminTestApp(repo), "GET", "/users", "")
		if fail {
			if status != 500 || strings.Contains(raw, "private") {
				t.Fatalf("%d %s", status, raw)
			}
		} else {
			items := v["data"].(map[string]any)["items"].([]any)
			if status != 200 || len(items) != 1 || items[0].(map[string]any)["username"] != "admin" || strings.Contains(raw, "password") {
				t.Fatalf("%d %s", status, raw)
			}
		}
	}
}

func TestAdminValidationBoundaries(t *testing.T) {
	for _, tc := range []struct {
		value string
		valid bool
	}{{"ab", false}, {"abc", true}, {strings.Repeat("x", 64), true}, {strings.Repeat("x", 65), false}, {"  ", false}} {
		if got := validUsername(tc.value); got != tc.valid {
			t.Errorf("username length %d: %v", len(tc.value), got)
		}
	}
	for _, tc := range []struct {
		value string
		valid bool
	}{{"", false}, {" ", false}, {"x", true}, {strings.Repeat("x", 100), true}, {strings.Repeat("x", 101), false}} {
		if got := validGroupName(tc.value); got != tc.valid {
			t.Errorf("group length %d: %v", len(tc.value), got)
		}
	}
	for _, tc := range []struct {
		length int
		valid  bool
	}{{11, false}, {12, true}, {72, true}, {1025, false}} {
		if got := validPassword(strings.Repeat("x", tc.length)); got != tc.valid {
			t.Errorf("password length %d: %v", tc.length, got)
		}
	}
}
