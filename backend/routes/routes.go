package routes

import (
	"github.com/Jerannn24/Tugas-Besar-WBD.git/modules/admin"
	"github.com/Jerannn24/Tugas-Besar-WBD.git/modules/auth"
	"github.com/Jerannn24/Tugas-Besar-WBD.git/modules/authz"
	"github.com/Jerannn24/Tugas-Besar-WBD.git/modules/connections"
	"github.com/Jerannn24/Tugas-Besar-WBD.git/modules/databaseapi"
	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5/pgxpool"
	"log"
)

func Register(app *fiber.App, db *pgxpool.Pool, jwtSecret string) {
	api := app.Group("/api")
	api.Use(func(c *fiber.Ctx) error {
		c.Set("Cache-Control", "no-store")
		return c.Next()
	})

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
	sessionMiddleware := auth.SessionMiddleware(authService)
	auth.RegisterRoutes(api, authHandler, sessionMiddleware)

	authorizer := authz.NewService(authz.NewRepository(db))
	adminHandler := admin.NewHandler(admin.NewService(admin.NewRepository(db)))
	protected := api.Group("", sessionMiddleware, authz.ActorMiddleware(authorizer))
	admin.RegisterRoutes(protected, adminHandler, authz.AdminMiddleware(authorizer), auth.CSRFMiddleware())
	connectionRepository := connections.NewRepository(db, jwtSecret)
	connectionPools := connections.NewPoolManager(connectionRepository)
	connectionService := connections.NewService(connectionRepository, connectionPools)
	connectionHandler := connections.NewHandler(connectionService)
	connections.RegisterRoutes(protected, connectionHandler, authz.AdminMiddleware(authorizer), auth.CSRFMiddleware())
	
	if platform, err := databaseapi.OpenPlatform(); err != nil {
		log.Printf("database API disabled: %v", err)
	} else if stopReconciler, err := databaseapi.Register(protected, platform, authorizer, connectionPools); err != nil {
		_ = platform.Close()
		log.Printf("database API disabled: %v", err)
	} else {
		app.Hooks().OnShutdown(func() error {
			stopReconciler()
			connectionPools.Close()
			return platform.Close()
		})
	}
}
