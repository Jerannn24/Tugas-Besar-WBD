package auth

import (
	"errors"

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
		return err
	}

	user, err := h.service.Register(c.UserContext(), req.Username, req.Password)
	if err != nil {
		switch {
		case errors.Is(err, ErrInvalidInput):
			return response.Fail(c, fiber.StatusBadRequest, "INVALID_INPUT", "Nama atau password tidak valid")
		case errors.Is(err, ErrEmailExists):
			return response.Fail(c, fiber.StatusConflict, "EMAIL_EXISTS", "Email sudah terdaftar")
		default:
			return response.Fail(c, fiber.StatusInternalServerError, "INTERNAL_ERROR", "Terjadi kesalahan internal")

		}
	}

	return response.JSON(c, 200, user)
}

func (h *Handler) Login(c *fiber.Ctx) error {
	var req LoginRequest
	if err := c.BodyParser(&req); err != nil {
		return err
	}

	_, token, err := h.service.Login(c.UserContext(), req.Username, req.Password)
	if err != nil {
		switch {
		case errors.Is(err, ErrInvalidInput):
			return response.Fail(c, fiber.StatusBadRequest, "INVALID_INPUT", "Nama atau password tidak valid")
		case errors.Is(err, ErrEmailExists):
			return response.Fail(c, fiber.StatusConflict, "EMAIL_EXISTS", "Email sudah terdaftar")
		case errors.Is(err, ErrUserNotFound):
			return response.Fail(c, fiber.StatusNotFound, "USER_NOT_FOUND", "Pengguna tidak ditemukan")
		default:
			return response.Fail(c, fiber.StatusInternalServerError, "INTERNAL_ERROR", "Terjadi kesalahan internal")

		}
	}

	return response.JSON(
		c,
		200,
		fiber.Map{
			"message": "Login berhasil",
			"token":   token,
		},
	)
}
