package server

import (
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
)

const (
	indexBody = "<!doctype html><html><body>spa-index</body></html>"
	assetBody = "console.log('chunk')"
	iconBody  = "<svg>icon</svg>"
)

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func newSPAApp(t *testing.T) *fiber.App {
	t.Helper()
	parent := t.TempDir()
	dir := filepath.Join(parent, "dist")
	writeFile(t, filepath.Join(dir, "index.html"), indexBody)
	writeFile(t, filepath.Join(dir, "assets", "app-abc123.js"), assetBody)
	writeFile(t, filepath.Join(dir, "favicon.svg"), iconBody)
	writeFile(t, filepath.Join(parent, "secret.txt"), "top-secret")

	app := fiber.New(fiber.Config{ErrorHandler: ErrorHandler})
	app.Get("/api/ping", func(c *fiber.Ctx) error { return c.SendString("pong") })
	Frontend(app, dir)
	return app
}

type result struct {
	status int
	header func(string) string
	body   string
}

func do(t *testing.T, app *fiber.App, method, path string) result {
	t.Helper()
	resp, err := app.Test(httptest.NewRequest(method, path, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return result{status: resp.StatusCode, header: resp.Header.Get, body: string(body)}
}

func TestFrontendDeepLinksServeIndex(t *testing.T) {
	app := newSPAApp(t)
	paths := []string{
		"/",
		"/login",
		"/databases",
		"/databases/30000000-0000-4000-8000-000000000001?schema=public&table=customers",
		"/databases/30000000-0000-4000-8000-000000000001/migrations",
		"/databases/30000000-0000-4000-8000-000000000001/drafts/new",
		"/admin/users",

		"/login/",
		"/database/?id=30000000-0000-4000-8000-000000000001",
		"/migrations/?database=30000000-0000-4000-8000-000000000001",
		"/migration/?database=x&draft=y",
	}
	for _, p := range paths {
		r := do(t, app, "GET", p)
		if r.status != 200 || r.body != indexBody {
			t.Errorf("GET %s = %d %q, want index.html", p, r.status, r.body)
		}
		if got := r.header("Cache-Control"); got != "no-cache" {
			t.Errorf("GET %s Cache-Control = %q, want no-cache", p, got)
		}
		if got := r.header("Content-Type"); !strings.HasPrefix(got, "text/html") {
			t.Errorf("GET %s Content-Type = %q, want text/html", p, got)
		}
		if got := r.header("Location"); got != "" {
			t.Errorf("GET %s Location = %q, want no redirect", p, got)
		}
	}

	if r := do(t, app, "HEAD", "/databases/x/migrations"); r.status != 200 || r.header("Cache-Control") != "no-cache" {
		t.Errorf("HEAD deep link = %d cache=%q", r.status, r.header("Cache-Control"))
	}
}

func TestFrontendAssetsAreImmutable(t *testing.T) {
	app := newSPAApp(t)

	r := do(t, app, "GET", "/assets/app-abc123.js")
	if r.status != 200 || r.body != assetBody {
		t.Fatalf("asset = %d %q", r.status, r.body)
	}
	cc := r.header("Cache-Control")
	if !strings.Contains(cc, "max-age=31536000") || !strings.Contains(cc, "immutable") {
		t.Errorf("asset Cache-Control = %q, want long immutable", cc)
	}

	r = do(t, app, "GET", "/assets/tidak-ada.js")
	if r.status != 404 || strings.Contains(r.body, "spa-index") {
		t.Errorf("missing asset = %d %q, want 404 non-HTML", r.status, r.body)
	}
	if strings.Contains(r.header("Cache-Control"), "immutable") {
		t.Errorf("missing asset must not be cached immutable: %q", r.header("Cache-Control"))
	}
}

func TestFrontendRootFilesAndMissingExtensions(t *testing.T) {
	app := newSPAApp(t)

	r := do(t, app, "GET", "/favicon.svg")
	if r.status != 200 || r.body != iconBody || r.header("Cache-Control") != "no-cache" {
		t.Errorf("favicon = %d %q cache=%q", r.status, r.body, r.header("Cache-Control"))
	}

	for _, p := range []string{"/robots.txt", "/foo.png", "/databases/x/logo.svg"} {
		r := do(t, app, "GET", p)
		if r.status != 404 || strings.Contains(r.body, "spa-index") {
			t.Errorf("GET %s = %d %q, want 404 not index.html", p, r.status, r.body)
		}
	}
}

func TestFrontendDoesNotSwallowAPI(t *testing.T) {
	app := newSPAApp(t)

	if r := do(t, app, "GET", "/api/ping"); r.status != 200 || r.body != "pong" {
		t.Fatalf("/api/ping = %d %q", r.status, r.body)
	}
	
	cases := []struct {
		method, path, code string
		status             int
	}{
		{"GET", "/api/tidak-ada", "NOT_FOUND", 404},
		{"GET", "/api/databases/x/tidak-ada", "NOT_FOUND", 404},
		{"GET", "/api", "NOT_FOUND", 404},
		{"HEAD", "/api/tidak-ada", "", 404},
		{"POST", "/api/tidak-ada", "METHOD_NOT_ALLOWED", 405},
	}
	for _, tc := range cases {
		r := do(t, app, tc.method, tc.path)
		if r.status != tc.status {
			t.Errorf("%s %s status = %d, want %d", tc.method, tc.path, r.status, tc.status)
		}
		if ct := r.header("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("%s %s Content-Type = %q, want JSON", tc.method, tc.path, ct)
		}
		if strings.Contains(strings.ToLower(r.body), "<html") {
			t.Errorf("%s %s returned HTML: %q", tc.method, tc.path, r.body)
		}
		if tc.code != "" && !strings.Contains(r.body, `"`+tc.code+`"`) {
			t.Errorf("%s %s body = %q, want %s envelope", tc.method, tc.path, r.body, tc.code)
		}
	}
}

func TestFrontendBlocksTraversal(t *testing.T) {
	app := newSPAApp(t)
	for _, p := range []string{
		"/..%2fsecret.txt",
		"/%2e%2e/secret.txt",
		"/assets/..%2f..%2fsecret.txt",
		"/..%2f..%2f..%2fetc%2fpasswd",
	} {
		r := do(t, app, "GET", p)
		if strings.Contains(r.body, "top-secret") || strings.Contains(r.body, "root:") {
			t.Errorf("GET %s leaked file outside dist: %d %q", p, r.status, r.body)
		}
	}
}

func TestFrontendSkipsMissingDir(t *testing.T) {
	app := fiber.New()
	Frontend(app, filepath.Join(t.TempDir(), "missing"))
	resp, err := app.Test(httptest.NewRequest("GET", "/databases", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 404 {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}
