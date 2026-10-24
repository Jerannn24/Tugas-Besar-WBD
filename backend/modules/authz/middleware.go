package authz

import (
	"errors"

	"github.com/Jerannn24/Tugas-Besar-WBD.git/modules/auth"
	"github.com/Jerannn24/Tugas-Besar-WBD.git/response"
	"github.com/gofiber/fiber/v2"
)

const actorLocalKey = "authz.actor"

func ActorMiddleware(authorizer Authorizer) fiber.Handler {
	return func(c *fiber.Ctx) error {
		session, ok := auth.SessionFromContext(c)
		if !ok || session == nil || session.Session == nil {
			return response.Fail(c, fiber.StatusUnauthorized, "UNAUTHENTICATED", "Sesi tidak valid")
		}

		actor, err := authorizer.ResolveActor(c.UserContext(), session.Session.UserID)
		if err != nil {
			if errors.Is(err, ErrUnauthenticated) {
				return response.Fail(c, fiber.StatusUnauthorized, "UNAUTHENTICATED", "Sesi tidak valid")
			}
			return response.Fail(c, fiber.StatusInternalServerError, "INTERNAL_ERROR", "Terjadi kesalahan internal")
		}
		c.Locals(actorLocalKey, actor)
		return c.Next()
	}
}

func ActorFromContext(c *fiber.Ctx) (Actor, bool) {
	actor, ok := c.Locals(actorLocalKey).(Actor)
	return actor, ok
}

func AdminMiddleware(authorizer Authorizer) fiber.Handler {
	return func(c *fiber.Ctx) error {
		actor, ok := ActorFromContext(c)
		if !ok {
			return response.Fail(c, fiber.StatusUnauthorized, "UNAUTHENTICATED", "Sesi tidak valid")
		}
		if err := authorizer.RequireAdmin(actor); err != nil {
			if errors.Is(err, ErrUnauthenticated) {
				return response.Fail(c, fiber.StatusUnauthorized, "UNAUTHENTICATED", "Sesi tidak valid")
			}
			return response.Fail(c, fiber.StatusForbidden, "FORBIDDEN", "Akses ditolak")
		}
		return c.Next()
	}
}
