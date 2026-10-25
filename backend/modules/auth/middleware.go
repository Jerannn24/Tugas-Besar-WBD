package auth

import (
	"time"

	"github.com/Jerannn24/Tugas-Besar-WBD.git/response"
	"github.com/gofiber/fiber/v2"
)

const sessionLocalKey = "auth.session"

func SessionMiddleware(service Service) fiber.Handler {
	return func(c *fiber.Ctx) error {
		session, err := service.Authenticate(c.UserContext(), c.Cookies(SessionCookieName))
		if err != nil {
			return response.Fail(c, fiber.StatusUnauthorized, "UNAUTHENTICATED", "Sesi tidak valid atau telah berakhir")
		}

		if session.SessionID != c.Cookies(SessionCookieName) {
			setSessionCookie(c, service.SessionConfig(), session.SessionID, session.Session.AbsoluteExpiresAt)
		}
		c.Locals(sessionLocalKey, session)
		return c.Next()
	}
}

func CSRFMiddleware() fiber.Handler {
	return func(c *fiber.Ctx) error {
		switch c.Method() {
		case fiber.MethodGet, fiber.MethodHead, fiber.MethodOptions:
			return c.Next()
		}
		cookie := c.Cookies(CSRFCookieName)
		header := c.Get("X-CSRF-Token")
		if cookie == "" || header == "" || cookie != header {
			return response.Fail(c, fiber.StatusForbidden, "CSRF_INVALID", "Permintaan tidak valid")
		}
		return c.Next()
	}
}

func setSessionCookie(c *fiber.Ctx, config SessionConfig, sessionID string, expiresAt time.Time) {
	c.Cookie(&fiber.Cookie{
		Name:     SessionCookieName,
		Value:    sessionID,
		Expires:  expiresAt,
		HTTPOnly: true,
		Secure:   config.CookieSecure,
		SameSite: "Lax",
		Domain:   config.CookieDomain,
		Path:     "/",
	})
}

func SessionFromContext(c *fiber.Ctx) (*AuthenticatedSession, bool) {
	session, ok := c.Locals(sessionLocalKey).(*AuthenticatedSession)
	return session, ok
}
