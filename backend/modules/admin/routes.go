package admin

import "github.com/gofiber/fiber/v2"

func RegisterRoutes(r fiber.Router, handler *Handler, guard fiber.Handler, csrf fiber.Handler) {
	admin := r.Group("/admin", guard)
	admin.Get("/users", handler.listUsers)
	admin.Post("/users", csrf, handler.createUser)
	admin.Patch("/users/:user_id", csrf, handler.updateUser)
	admin.Get("/groups", handler.listGroups)
	admin.Post("/groups", csrf, handler.createGroup)
	admin.Patch("/groups/:group_id", csrf, handler.updateGroup)
	admin.Delete("/groups/:group_id", csrf, handler.deleteGroup)
	admin.Get("/groups/:group_id/members", handler.members)
	admin.Put("/groups/:group_id/members", csrf, handler.replaceMembers)
	admin.Get("/groups/:group_id/grants", handler.grants)
	admin.Put("/groups/:group_id/grants", csrf, handler.replaceGrants)
}
