package routes

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Jerannn24/Tugas-Besar-WBD.git/jobs"
	"github.com/Jerannn24/Tugas-Besar-WBD.git/modules/admin"
	"github.com/Jerannn24/Tugas-Besar-WBD.git/modules/auth"
	"github.com/Jerannn24/Tugas-Besar-WBD.git/modules/authz"
	"github.com/Jerannn24/Tugas-Besar-WBD.git/modules/connections"
	"github.com/Jerannn24/Tugas-Besar-WBD.git/modules/databaseapi"
	"github.com/Jerannn24/Tugas-Besar-WBD.git/modules/workflow"
	"github.com/Jerannn24/Tugas-Besar-WBD.git/telemetry"
	"github.com/Jerannn24/Tugas-Besar-WBD.git/worker"
	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Register starts the database services and returns their shutdown function.
// Stop serving HTTP before calling shutdown; it drains the worker before closing pools.
func Register(ctx context.Context, app *fiber.App, db *pgxpool.Pool, jwtSecret string) (func() error, error) {
	if db == nil {
		return nil, errors.New("platform connection pool is required")
	}
	platform := databaseapi.OpenPlatform(db)
	startupCtx, cancelStartup := context.WithTimeout(ctx, 10*time.Second)
	defer cancelStartup()
	var migrated bool
	err := platform.QueryRowContext(startupCtx, `SELECT EXISTS (
		SELECT 1 FROM app.schema_migrations WHERE version = '014_linear_job_sources'
	)`).Scan(&migrated)
	if err != nil || !migrated {
		_ = platform.Close()
		if err != nil {
			return nil, fmt.Errorf("check platform migration state: %w", err)
		}
		return nil, errors.New("platform migration 014_linear_job_sources is required")
	}

	protected, authorizer := registerAPI(app, db, jwtSecret)
	connectionRepository := connections.NewRepository(db, jwtSecret)
	connectionPools := connections.NewPoolManager(connectionRepository)
	connectionService := connections.NewService(connectionRepository, connectionPools)
	connectionHandler := connections.NewHandler(connectionService)
	connections.RegisterRoutes(protected, connectionHandler, authz.AdminMiddleware(authorizer), auth.CSRFMiddleware())

	shutdown := func() error {
		connectionPools.Close()
		return platform.Close()
	}
	workflowService, err := workflow.NewService(db, authorizer)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("initialize workflow: %w", err), shutdown())
	}
	jobService := jobs.New(platform)
	var queueWorkers [2]*worker.Worker
	for i := range queueWorkers {
		queueWorkers[i], err = worker.New(jobService, jobs.ExecutionDeps{
			Gate:    workflow.NewGate(db),
			Acquire: databaseapi.AcquireLease(connectionPools),
		}, worker.Config{})
		if err != nil {
			return nil, errors.Join(fmt.Errorf("initialize queue worker: %w", err), shutdown())
		}
	}
	workflowService.SetPlatform(platform)
	workflowService.SetJobs(jobService)
	workflow.RegisterRoutes(protected, workflow.NewHandler(workflowService), auth.CSRFMiddleware())
	stopReconciler, err := databaseapi.Register(protected, platform, authorizer, connectionPools)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("initialize database API: %w", err), shutdown())
	}
	for _, queueWorker := range queueWorkers {
		go queueWorker.Run(context.WithoutCancel(ctx))
	}

	return sync.OnceValue(func() error {
		for _, queueWorker := range queueWorkers {
			queueWorker.Stop()
		}
		timer := time.NewTimer(45 * time.Second)
		defer timer.Stop()
		stopReconciler()
		for _, queueWorker := range queueWorkers {
			select {
			case <-queueWorker.Done():
			case <-timer.C:
				return errors.New("queue worker did not finish cancellation; leaving pools open until process exit")
			}
		}
		return shutdown()
	}), nil
}

func registerAPI(app *fiber.App, db *pgxpool.Pool, jwtSecret string) (fiber.Router, authz.Authorizer) {
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

	metricsRegistry := telemetry.NewRegistry()
	telemetry.RegisterRoutes(api, metricsRegistry)

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
	return protected, authorizer
}
