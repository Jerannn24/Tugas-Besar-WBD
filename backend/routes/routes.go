package routes

import (
	"github.com/Jerannn24/Tugas-Besar-WBD.git/modules/auth"
	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5/pgxpool"
)

func Register(app *fiber.App, db *pgxpool.Pool, jwtSecret string) {
	api := app.Group("/api")

	api.Get("/health", func(c *fiber.Ctx) error {
		return c.JSON(fiber.Map{
			"success": true,
			"data": fiber.Map{
				"message": "API is running",
			},
			"error": nil,
		})
	})

	authRepo := auth.NewRepository(db)
	authSessions := auth.NewSessionRepository(db)
	authService := auth.NewService(authRepo, authSessions, auth.LoadSessionConfig(jwtSecret))
	authHandler := auth.NewHandler(authService)
	auth.RegisterRoutes(api, authHandler, auth.SessionMiddleware(authService))
}
