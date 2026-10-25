package server

import (
	"os"

	"github.com/Jerannn24/Tugas-Besar-WBD.git/routes"
	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5/pgxpool"
)

var okBytes = []byte(`{"status":"ok"}`)

func New(db *pgxpool.Pool, jwtSecret string) *fiber.App {
	app := fiber.New(fiber.Config{
		ErrorHandler: ErrorHandler,
	})

	app.Get("/health", func(c *fiber.Ctx) error {
		c.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSONCharsetUTF8)
		return c.Send(okBytes)
	})

	app.Use(Logging())

	routes.Register(app, db, jwtSecret)
	Frontend(app, os.Getenv("FRONTEND_DIST"))

	return app
}

// Frontend menyajikan build multi-page FE (mis. dist/login/index.html) dari dir.
// Dipanggil setelah route API agar /api tidak tertelan.
func Frontend(app *fiber.App, dir string) {
	if dir == "" {
		dir = "web"
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return
	}
	app.Get("/", func(c *fiber.Ctx) error {
		if c.Cookies("app_session") != "" {
			return c.Redirect("/databases/", fiber.StatusFound)
		}
		return c.Redirect("/login/", fiber.StatusFound)
	})
	app.Static("/", dir, fiber.Static{Index: "index.html"})
}
