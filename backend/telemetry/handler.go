package telemetry

import (
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/adaptor"
)

func RegisterRoutes(router fiber.Router, registry *Registry) {
	router.Get("/metrics", adaptor.HTTPHandler(registry))
}
