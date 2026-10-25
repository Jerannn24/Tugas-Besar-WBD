package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"
)

func csrfApp() *fiber.App {
	app := fiber.New()
	app.Use(CSRFMiddleware())
	app.All("/x", func(c *fiber.Ctx) error { return c.SendStatus(200) })
	return app
}

func TestCSRFMiddlewareRejects(t *testing.T) {
	cases := []struct {
		name           string
		cookie, header string
	}{
		{"no cookie no header", "", ""},
		{"cookie only", "abc", ""},
		{"header only", "", "abc"},
		{"mismatch", "abc", "xyz"},
	}
	for _, method := range []string{"POST", "PATCH", "PUT", "DELETE"} {
		for _, tc := range cases {
			t.Run(method+"/"+tc.name, func(t *testing.T) {
				req := httptest.NewRequest(method, "/x", nil)
				if tc.cookie != "" {
					req.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: tc.cookie})
				}
				if tc.header != "" {
					req.Header.Set("X-CSRF-Token", tc.header)
				}
				resp, err := csrfApp().Test(req)
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				if resp.StatusCode != 403 {
					t.Fatalf("status = %d, want 403", resp.StatusCode)
				}
				var body struct {
					Error struct {
						Code        string            `json:"code"`
						FieldErrors map[string]string `json:"field_errors"`
						Retryable   *bool             `json:"retryable"`
					} `json:"error"`
				}
				if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				if body.Error.Code != "CSRF_INVALID" {
					t.Fatalf("code = %q, want CSRF_INVALID", body.Error.Code)
				}
				if body.Error.FieldErrors == nil || body.Error.Retryable == nil || *body.Error.Retryable {
					t.Fatalf("field_errors/retryable missing: %+v", body.Error)
				}
			})
		}
	}
}

func TestCSRFMiddlewareAllows(t *testing.T) {
	for _, method := range []string{"POST", "PATCH", "PUT", "DELETE"} {
		req := httptest.NewRequest(method, "/x", nil)
		req.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: "tok"})
		req.Header.Set("X-CSRF-Token", "tok")
		resp, err := csrfApp().Test(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("%s status = %d, want 200", method, resp.StatusCode)
		}
	}
}

func TestCSRFMiddlewareSkipsSafeMethods(t *testing.T) {
	for _, method := range []string{"GET", "HEAD", "OPTIONS"} {
		resp, err := csrfApp().Test(httptest.NewRequest(method, "/x", nil))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("%s status = %d, want 200", method, resp.StatusCode)
		}
	}
}
