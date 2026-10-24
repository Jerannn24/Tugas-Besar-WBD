package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
)

type fakeAdminRepository struct {
	createUserFn func(context.Context, string, string, bool) (User, error)
	listUsersFn  func(context.Context) ([]User, error)
}

func (f *fakeAdminRepository) ListUsers(ctx context.Context) ([]User, error) {
	if f.listUsersFn != nil {
		return f.listUsersFn(ctx)
	}
	return []User{}, nil
}

func (f *fakeAdminRepository) CreateUser(
	ctx context.Context,
	username, password string,
	isAdmin bool,
) (User, error) {
	if f.createUserFn != nil {
		return f.createUserFn(ctx, username, password, isAdmin)
	}
	return User{}, nil
}

func (f *fakeAdminRepository) UpdateUser(
	context.Context, uuid.UUID, *string, *string, *bool, *bool,
) (User, error) {
	return User{}, nil
}

func (f *fakeAdminRepository) ListGroups(context.Context) ([]Group, error) {
	return []Group{}, nil
}

func (f *fakeAdminRepository) CreateGroup(context.Context, string) (Group, error) {
	return Group{}, nil
}

func (f *fakeAdminRepository) UpdateGroup(
	context.Context, uuid.UUID, string,
) (Group, error) {
	return Group{}, nil
}

func (f *fakeAdminRepository) DeleteGroup(context.Context, uuid.UUID) error {
	return nil
}

func (f *fakeAdminRepository) Members(context.Context, uuid.UUID) ([]uuid.UUID, error) {
	return []uuid.UUID{}, nil
}

func (f *fakeAdminRepository) ReplaceMembers(
	context.Context, uuid.UUID, []uuid.UUID,
) ([]uuid.UUID, error) {
	return []uuid.UUID{}, nil
}

func (f *fakeAdminRepository) Grants(context.Context, uuid.UUID) ([]Grant, error) {
	return []Grant{}, nil
}

func (f *fakeAdminRepository) ReplaceGrants(
	context.Context, uuid.UUID, []Grant,
) ([]Grant, error) {
	return []Grant{}, nil
}

func newAdminTestApp(repo serviceRepository) *fiber.App {
	app := fiber.New()
	service := &Service{repository: repo}
	handler := NewHandler(service)

	app.Post("/users", handler.createUser)
	app.Get("/users", handler.listUsers)
	app.Patch("/users/:user_id", handler.updateUser)

	return app
}

func TestCreateUserSuccess(t *testing.T) {
	expectedID := uuid.New()
	repo := &fakeAdminRepository{
		createUserFn: func(
			_ context.Context,
			username, password string,
			isAdmin bool,
		) (User, error) {
			if username != "testuser" {
				t.Errorf("username = %q", username)
			}
			if password != "StrongPassword123!" {
				t.Error("password tidak diteruskan dengan benar")
			}
			return User{
				ID:       expectedID,
				Username: username,
				IsAdmin:  isAdmin,
				IsActive: true,
			}, nil
		},
	}

	app := newAdminTestApp(repo)
	req := httptest.NewRequest(
		"POST",
		"/users",
		strings.NewReader(`{
			"username":"testuser",
			"password":"StrongPassword123!",
			"is_admin":false
		}`),
	)
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 201 {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}

	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}

	data, ok := body["data"].(map[string]any)
	if !ok {
		t.Fatalf("response tidak memiliki data object: %#v", body)
	}
	if data["username"] != "testuser" {
		t.Fatalf("username response = %#v", data["username"])
	}
}

func TestCreateUserInvalidJSON(t *testing.T) {
	app := newAdminTestApp(&fakeAdminRepository{})

	req := httptest.NewRequest(
		"POST",
		"/users",
		strings.NewReader(`{"username":`),
	)
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 400 {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestCreateUserRepositoryError(t *testing.T) {
	repo := &fakeAdminRepository{
		createUserFn: func(
			context.Context, string, string, bool,
		) (User, error) {
			return User{}, ErrConflict
		},
	}
	app := newAdminTestApp(repo)

	req := httptest.NewRequest(
		"POST",
		"/users",
		strings.NewReader(`{
			"username":"testuser",
			"password":"StrongPassword123!"
		}`),
	)
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 409 {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
}

func TestUpdateUserInvalidID(t *testing.T) {
	app := newAdminTestApp(&fakeAdminRepository{})

	req := httptest.NewRequest(
		"PATCH",
		"/users/not-a-uuid",
		strings.NewReader(`{}`),
	)
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 400 {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestAdminMapError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"invalid input", ErrInvalidInput, 400},
		{"not found", ErrNotFound, 404},
		{"conflict", ErrConflict, 409},
		{"last admin", ErrLastAdmin, 409},
		{"unknown error", errors.New("database unavailable"), 500},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := fiber.New()
			app.Get("/", func(c *fiber.Ctx) error {
				return mapError(c, tt.err)
			})

			resp, err := app.Test(httptest.NewRequest("GET", "/", nil))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tt.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tt.want)
			}
		})
	}
}
