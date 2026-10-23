package server

import (
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

	return app
}
