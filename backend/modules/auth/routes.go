package auth

import "github.com/gofiber/fiber/v2"

func RegisterRoutes(r fiber.Router, h *Handler) {
	auth := r.Group("/auth")

	auth.Post("/login", h.Login)
	auth.Post("/register", h.Register)
}
