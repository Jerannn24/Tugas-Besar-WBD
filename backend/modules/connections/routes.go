package connections

import "github.com/gofiber/fiber/v2"

func RegisterRoutes(r fiber.Router, handler *Handler, guard fiber.Handler, csrf fiber.Handler) {
	group := r.Group("/admin/connections", guard)
	group.Get("/", handler.list)
	group.Post("/", csrf, handler.create)
	group.Patch("/:database_id", csrf, handler.update)
	group.Post("/:database_id/test", csrf, handler.test)
	group.Delete("/:database_id", csrf, handler.delete)
}
