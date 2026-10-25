package auth_test

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/Jerannn24/Tugas-Besar-WBD.git/modules/auth"
	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
)

func contractApp(service *fakeService) *fiber.App {
	app := fiber.New()
	auth.RegisterRoutes(app.Group("/api"), auth.NewHandler(service), auth.SessionMiddleware(service))
	return app
}

func decodeData(t *testing.T, app *fiber.App, method, path string, cookies map[string]string, out any) int {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	for name, value := range cookies {
		req.Header.Add("Cookie", name+"="+value)
	}
	if v, ok := cookies[auth.CSRFCookieName]; ok {
		req.Header.Set("X-CSRF-Token", v)
	}
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := struct {
		Data json.RawMessage `json:"data"`
	}{}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if out != nil {
		if err := json.Unmarshal(body.Data, out); err != nil {
			t.Fatal(err)
		}
	}
	return resp.StatusCode
}

func TestCSRFReturnsTokenMatchingCookie(t *testing.T) {
	app := contractApp(&fakeService{})
	req := httptest.NewRequest("GET", "/api/csrf", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Data struct {
			CSRFToken string `json:"csrf_token"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	var cookie string
	for _, c := range resp.Cookies() {
		if c.Name == auth.CSRFCookieName {
			cookie = c.Value
		}
	}
	if body.Data.CSRFToken == "" || body.Data.CSRFToken != cookie {
		t.Fatalf("csrf_token %q does not match cookie %q", body.Data.CSRFToken, cookie)
	}
}

func TestSessionReturnsUser(t *testing.T) {
	userID := uuid.New()
	app := contractApp(&fakeService{userID: userID})
	var view struct {
		User struct {
			ID       string `json:"id"`
			Username string `json:"username"`
			IsAdmin  bool   `json:"is_admin"`
			IsActive bool   `json:"is_active"`
		} `json:"user"`
		AbsoluteExpiresAt string `json:"absolute_expires_at"`
	}
	status := decodeData(t, app, "GET", "/api/session", map[string]string{auth.SessionCookieName: "s"}, &view)
	if status != 200 {
		t.Fatalf("status = %d", status)
	}
	if view.User.ID != userID.String() || view.User.Username != "alice" || !view.User.IsAdmin || !view.User.IsActive || view.AbsoluteExpiresAt == "" {
		t.Fatalf("unexpected session view: %+v", view)
	}
}

func TestLogoutReturnsLoggedOut(t *testing.T) {
	app := contractApp(&fakeService{userID: uuid.New()})
	var view struct {
		LoggedOut bool `json:"logged_out"`
	}
	status := decodeData(t, app, "POST", "/api/logout", map[string]string{auth.SessionCookieName: "s", auth.CSRFCookieName: "t"}, &view)
	if status != 200 || !view.LoggedOut {
		t.Fatalf("status = %d, logged_out = %v", status, view.LoggedOut)
	}
}
