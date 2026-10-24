package auth

import "github.com/gofiber/fiber/v2"

func RegisterRoutes(r fiber.Router, h *Handler, middleware fiber.Handler) {
	auth := r.Group("/auth")

	auth.Get("/csrf", h.CSRF)
	auth.Post("/login", CSRFMiddleware(), h.Login)
	auth.Post("/register", CSRFMiddleware(), h.Register)
	auth.Post("/logout", middleware, CSRFMiddleware(), h.Logout)
}
