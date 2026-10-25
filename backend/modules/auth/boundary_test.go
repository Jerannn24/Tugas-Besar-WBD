package auth_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Jerannn24/Tugas-Besar-WBD.git/modules/auth"
	"github.com/Jerannn24/Tugas-Besar-WBD.git/modules/authz"
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/adaptor"
	"github.com/google/uuid"
)

type fakeService struct {
	newID  string
	userID uuid.UUID
}

func (f *fakeService) Register(context.Context, string, string) (*auth.User, error) {
	return nil, errors.New("unused")
}
func (f *fakeService) Login(context.Context, string, string) (*auth.LoginResult, error) {
	return nil, errors.New("unused")
}
func (f *fakeService) Logout(context.Context, string) error { return nil }
func (f *fakeService) Authenticate(_ context.Context, sessionID string) (*auth.AuthenticatedSession, error) {
	id := sessionID
	if f.newID != "" {
		id = f.newID
	}
	return &auth.AuthenticatedSession{
		SessionID: id,
		Session:   &auth.Session{UserID: f.userID, AbsoluteExpiresAt: time.Now().Add(time.Hour)},
	}, nil
}
func (f *fakeService) CurrentUser(_ context.Context, id uuid.UUID) (*auth.User, error) {
	return &auth.User{ID: id, Username: "alice", IsAdmin: true}, nil
}
func (f *fakeService) SessionConfig() auth.SessionConfig { return auth.SessionConfig{} }

type fakeAuthorizer struct{}

func (fakeAuthorizer) ResolveActor(_ context.Context, id uuid.UUID) (authz.Actor, error) {
	return authz.Actor{ID: id}, nil
}
func (fakeAuthorizer) EffectiveAccess(context.Context, uuid.UUID, uuid.UUID) (authz.Access, error) {
	return authz.AccessNone, nil
}
func (fakeAuthorizer) RequireAccess(context.Context, uuid.UUID, uuid.UUID, authz.Access) error {
	return nil
}
func (fakeAuthorizer) RequireAdmin(authz.Actor) error { return nil }

func sessionCookieFrom(resp *http.Response) *http.Cookie {
	for _, c := range resp.Cookies() {
		if c.Name == auth.SessionCookieName {
			return c
		}
	}
	return nil
}

func doRequest(t *testing.T, app *fiber.App, cookie string) *http.Response {
	t.Helper()
	req := httptest.NewRequest("GET", "/x", nil)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: cookie})
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestSessionMiddlewareSetsCookieOnlyWhenIDChanges(t *testing.T) {
	svc := &fakeService{newID: "new-id", userID: uuid.New()}
	app := fiber.New()
	app.Use(auth.SessionMiddleware(svc))
	app.Get("/x", func(c *fiber.Ctx) error { return c.SendStatus(200) })

	cookie := sessionCookieFrom(doRequest(t, app, "old-id"))
	if cookie == nil || cookie.Value != "new-id" {
		t.Fatalf("cookie = %+v, want app_session=new-id", cookie)
	}
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || cookie.Path != "/" {
		t.Fatalf("cookie attributes wrong: %+v", cookie)
	}

	svc.newID = ""
	if got := sessionCookieFrom(doRequest(t, app, "same-id")); got != nil {
		t.Fatalf("unexpected Set-Cookie for unchanged ID: %+v", got)
	}
}

func TestAdaptorSeesActorAndKeepsRotationCookie(t *testing.T) {
	userID := uuid.New()
	svc := &fakeService{newID: "new-id", userID: userID}
	app := fiber.New()
	protected := app.Group("", auth.SessionMiddleware(svc), authz.ActorMiddleware(fakeAuthorizer{}))
	protected.All("/x", adaptor.HTTPHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actor, ok := authz.ActorFromRequest(r)
		if !ok || actor.ID != userID {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})))

	resp := doRequest(t, app, "old-id")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (actor not visible)", resp.StatusCode)
	}
	cookie := sessionCookieFrom(resp)
	if cookie == nil || cookie.Value != "new-id" {
		t.Fatalf("Set-Cookie lost through adaptor: %+v", cookie)
	}
	if !cookie.HttpOnly || !strings.EqualFold(cookie.Path, "/") {
		t.Fatalf("cookie attributes wrong: %+v", cookie)
	}
}
