package connections

import (
	"errors"
	"net/http"

	"github.com/Jerannn24/Tugas-Besar-WBD.git/response"
	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
)

type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

func (h *Handler) list(c *fiber.Ctx) error {
	items, err := h.service.List(c.UserContext())
	if err != nil {
		return internal(c)
	}
	return response.JSON(c, http.StatusOK, fiber.Map{"items": items})
}

func (h *Handler) create(c *fiber.Ctx) error {
	var input Input
	if err := c.BodyParser(&input); err != nil {
		return invalid(c)
	}
	item, err := h.service.Create(c.UserContext(), input)
	if err != nil {
		return mapError(c, err)
	}
	return response.JSON(c, http.StatusCreated, item)
}

func (h *Handler) update(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("database_id"))
	if err != nil {
		return invalid(c)
	}
	var input Input
	if err := c.BodyParser(&input); err != nil {
		return invalid(c)
	}
	item, err := h.service.Update(c.UserContext(), id, input)
	if err != nil {
		return mapError(c, err)
	}
	return response.JSON(c, http.StatusOK, item)
}

func (h *Handler) test(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("database_id"))
	if err != nil {
		return invalid(c)
	}
	if err := h.service.Test(c.UserContext(), id); err != nil {
		return response.Fail(c, http.StatusBadGateway, "TARGET_UNAVAILABLE", "Target database is unavailable")
	}
	return response.JSON(c, http.StatusOK, fiber.Map{"reachable": true})
}

func (h *Handler) delete(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("database_id"))
	if err != nil {
		return invalid(c)
	}
	if err := h.service.Delete(c.UserContext(), id); err != nil {
		return mapError(c, err)
	}
	return response.JSON(c, http.StatusOK, fiber.Map{"deleted": true})
}

func invalid(c *fiber.Ctx) error {
	return response.Fail(c, http.StatusBadRequest, "INVALID_INPUT", "Request tidak valid")
}

func internal(c *fiber.Ctx) error {
	return response.Fail(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Terjadi kesalahan internal")
}

func mapError(c *fiber.Ctx, err error) error {
	switch {
	case errors.Is(err, ErrInvalidInput):
		return invalid(c)
	case errors.Is(err, ErrNotFound):
		return response.Fail(c, http.StatusNotFound, "NOT_FOUND", "Resource tidak ditemukan")
	case errors.Is(err, ErrConflict):
		return response.Fail(c, http.StatusConflict, "CONFLICT", "Target database sudah terdaftar")
	case errors.Is(err, ErrBusy):
		return response.Fail(c, http.StatusConflict, "DATABASE_BUSY", "Database sedang digunakan")
	case errors.Is(err, ErrTargetUnavailable):
		return response.Fail(c, http.StatusBadGateway, "TARGET_UNAVAILABLE", "Target database is unavailable")
	default:
		return internal(c)
	}
}
