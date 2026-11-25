package server

import (
	"context"
	"os"
	"time"

	"github.com/Jerannn24/Tugas-Besar-WBD.git/routes"
	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5/pgxpool"
)

var okBytes = []byte(`{"status":"ok"}`)

func New(ctx context.Context, db *pgxpool.Pool, jwtSecret string) (*fiber.App, func() error, error) {
	app := fiber.New(fiber.Config{
		ErrorHandler: ErrorHandler,
		ReadTimeout:  15 * time.Second,
	})

	app.Get("/health", func(c *fiber.Ctx) error {
		c.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSONCharsetUTF8)
		return c.Send(okBytes)
	})

	app.Use(Logging())

	shutdown, err := routes.Register(ctx, app, db, jwtSecret)
	if err != nil {
		return nil, nil, err
	}
	Frontend(app, os.Getenv("FRONTEND_DIST"))

	return app, shutdown, nil
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
