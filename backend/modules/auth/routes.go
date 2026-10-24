package auth

import "github.com/gofiber/fiber/v2"

func RegisterRoutes(r fiber.Router, h *Handler, middleware fiber.Handler) {
	auth := r.Group("/auth")

	auth.Get("/csrf", h.CSRF)
	auth.Post("/login", CSRFMiddleware(), h.Login)
	auth.Post("/logout", middleware, CSRFMiddleware(), h.Logout)

	r.Get("/csrf", h.CSRF)
	r.Post("/login", CSRFMiddleware(), h.Login)
	r.Get("/session", middleware, h.Session)
	r.Post("/logout", middleware, CSRFMiddleware(), h.Logout)
}
