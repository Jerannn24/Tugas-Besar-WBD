package auth

import (
	"errors"
	"time"

	"github.com/Jerannn24/Tugas-Besar-WBD.git/response"
	"github.com/gofiber/fiber/v2"
)

type Handler struct {
	service Service
}

func NewHandler(service Service) *Handler {
	return &Handler{
		service: service,
	}
}

type RegisterRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type LoginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (h *Handler) Register(c *fiber.Ctx) error {
	var req RegisterRequest
	if err := c.BodyParser(&req); err != nil {
		return response.Fail(c, fiber.StatusBadRequest, "BAD_REQUEST", "Request tidak valid")
	}

	user, err := h.service.Register(c.UserContext(), req.Username, req.Password)
	if err != nil {
		switch {
		case errors.Is(err, ErrInvalidInput):
			return response.Fail(c, fiber.StatusBadRequest, "INVALID_INPUT", "Nama atau password tidak valid")
		case errors.Is(err, ErrEmailExists):
			return response.Fail(c, fiber.StatusConflict, "USERNAME_EXISTS", "Username sudah terdaftar")
		default:
			return response.Fail(c, fiber.StatusInternalServerError, "INTERNAL_ERROR", "Terjadi kesalahan internal")

		}
	}

	return response.JSON(c, fiber.StatusCreated, fiber.Map{
		"Username": user.Username,
	})
}

func (h *Handler) Login(c *fiber.Ctx) error {
	var req LoginRequest
	if err := c.BodyParser(&req); err != nil {
		return response.Fail(c, fiber.StatusBadRequest, "BAD_REQUEST", "Request tidak valid")
	}

	result, err := h.service.Login(c.UserContext(), req.Username, req.Password)
	if err != nil {
		switch {
		case errors.Is(err, ErrInvalidInput), errors.Is(err, ErrUserNotFound):
			return response.Fail(c, fiber.StatusUnauthorized, "INVALID_CREDENTIALS", "Nama atau password salah")
		default:
			return response.Fail(c, fiber.StatusInternalServerError, "INTERNAL_ERROR", "Terjadi kesalahan internal")
		}
	}

	h.setSessionCookie(c, result.SessionID, result.ExpiresAt)
	if err := h.setCSRFCookie(c); err != nil {
		return response.Fail(c, fiber.StatusInternalServerError, "INTERNAL_ERROR", "Terjadi kesalahan internal")
	}

	return response.JSON(c, fiber.StatusOK, fiber.Map{
		"message": "Login berhasil",
	})
}

func (h *Handler) CSRF(c *fiber.Ctx) error {
	if err := h.setCSRFCookie(c); err != nil {
		return response.Fail(c, fiber.StatusInternalServerError, "INTERNAL_ERROR", "Terjadi kesalahan internal")
	}
	return response.JSON(c, fiber.StatusOK, fiber.Map{
		"message": "CSRF token siap",
	})
}

func (h *Handler) Logout(c *fiber.Ctx) error {
	err := h.service.Logout(c.UserContext(), c.Cookies(SessionCookieName))
	h.clearCookie(c, SessionCookieName)
	h.clearCookie(c, CSRFCookieName)
	if err != nil {
		return response.Fail(c, fiber.StatusInternalServerError, "INTERNAL_ERROR", "Terjadi kesalahan internal")
	}
	return response.JSON(c, fiber.StatusOK, fiber.Map{"message": "Logout berhasil"})
}

func (h *Handler) setSessionCookie(c *fiber.Ctx, sessionID string, expiresAt time.Time) {
	config := h.service.SessionConfig()
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

func (h *Handler) setCSRFCookie(c *fiber.Ctx) error {
	token, err := newSessionID()
	if err != nil {
		return err
	}

	config := h.service.SessionConfig()
	c.Cookie(&fiber.Cookie{
		Name:     CSRFCookieName,
		Value:    token,
		Expires:  time.Now().Add(config.AbsoluteLifetime),
		HTTPOnly: false,
		Secure:   config.CookieSecure,
		SameSite: "Lax",
		Domain:   config.CookieDomain,
		Path:     "/",
	})
	return nil
}

func (h *Handler) clearCookie(c *fiber.Ctx, name string) {
	config := h.service.SessionConfig()
	c.Cookie(&fiber.Cookie{
		Name:     name,
		Value:    "",
		Expires:  time.Unix(1, 0),
		MaxAge:   -1,
		HTTPOnly: name == SessionCookieName,
		Secure:   config.CookieSecure,
		SameSite: "Lax",
		Domain:   config.CookieDomain,
		Path:     "/",
	})
}
