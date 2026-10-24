package admin

import (
	"errors"
	"net/http"

	"github.com/Jerannn24/Tugas-Besar-WBD.git/response"
	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
)

type Handler struct{ service *Service }

func NewHandler(service *Service) *Handler { return &Handler{service: service} }

type userInput struct {
	Username string `json:"username"`
	Password string `json:"password"`
	IsAdmin  bool   `json:"is_admin"`
}

type userPatch struct {
	Username *string `json:"username"`
	Password *string `json:"password"`
	IsAdmin  *bool   `json:"is_admin"`
	IsActive *bool   `json:"is_active"`
}

type groupInput struct {
	Name string `json:"name"`
}

type membersInput struct {
	UserIDs []uuid.UUID `json:"user_ids"`
}

type grantsInput struct {
	Grants []Grant `json:"grants"`
}

func (h *Handler) listUsers(c *fiber.Ctx) error {
	items, err := h.service.ListUsers(c.UserContext())
	if err != nil {
		return internal(c)
	}
	return response.JSON(c, http.StatusOK, fiber.Map{"items": items})
}

func (h *Handler) createUser(c *fiber.Ctx) error {
	var input userInput
	if err := c.BodyParser(&input); err != nil {
		return invalid(c)
	}
	item, err := h.service.CreateUser(c.UserContext(), input.Username, input.Password, input.IsAdmin)
	if err != nil {
		return mapError(c, err)
	}
	return response.JSON(c, http.StatusCreated, item)
}

func (h *Handler) updateUser(c *fiber.Ctx) error {
	id, err := parseID(c.Params("user_id"))
	if err != nil {
		return invalid(c)
	}
	var input userPatch
	if err := c.BodyParser(&input); err != nil {
		return invalid(c)
	}
	item, err := h.service.UpdateUser(c.UserContext(), id, input.Username, input.Password, input.IsAdmin, input.IsActive)
	if err != nil {
		return mapError(c, err)
	}
	return response.JSON(c, http.StatusOK, item)
}

func (h *Handler) listGroups(c *fiber.Ctx) error {
	items, err := h.service.ListGroups(c.UserContext())
	if err != nil {
		return internal(c)
	}
	return response.JSON(c, http.StatusOK, fiber.Map{"items": items})
}

func (h *Handler) createGroup(c *fiber.Ctx) error {
	var input groupInput
	if err := c.BodyParser(&input); err != nil {
		return invalid(c)
	}
	item, err := h.service.CreateGroup(c.UserContext(), input.Name)
	if err != nil {
		return mapError(c, err)
	}
	return response.JSON(c, http.StatusCreated, item)
}

func (h *Handler) updateGroup(c *fiber.Ctx) error {
	id, err := parseID(c.Params("group_id"))
	if err != nil {
		return invalid(c)
	}
	var input groupInput
	if err := c.BodyParser(&input); err != nil {
		return invalid(c)
	}
	item, err := h.service.UpdateGroup(c.UserContext(), id, input.Name)
	if err != nil {
		return mapError(c, err)
	}
	return response.JSON(c, http.StatusOK, item)
}

func (h *Handler) deleteGroup(c *fiber.Ctx) error {
	id, err := parseID(c.Params("group_id"))
	if err != nil {
		return invalid(c)
	}
	if err := h.service.DeleteGroup(c.UserContext(), id); err != nil {
		return mapError(c, err)
	}
	return response.JSON(c, http.StatusOK, fiber.Map{"deleted": true})
}

func (h *Handler) members(c *fiber.Ctx) error {
	id, err := parseID(c.Params("group_id"))
	if err != nil {
		return invalid(c)
	}
	items, err := h.service.Members(c.UserContext(), id)
	if err != nil {
		return internal(c)
	}
	return response.JSON(c, http.StatusOK, fiber.Map{"group_id": id, "user_ids": items})
}

func (h *Handler) replaceMembers(c *fiber.Ctx) error {
	id, err := parseID(c.Params("group_id"))
	if err != nil {
		return invalid(c)
	}
	var input membersInput
	if err := c.BodyParser(&input); err != nil {
		return invalid(c)
	}
	items, err := h.service.ReplaceMembers(c.UserContext(), id, input.UserIDs)
	if err != nil {
		return mapError(c, err)
	}
	return response.JSON(c, http.StatusOK, fiber.Map{"group_id": id, "user_ids": items})
}

func (h *Handler) grants(c *fiber.Ctx) error {
	id, err := parseID(c.Params("group_id"))
	if err != nil {
		return invalid(c)
	}
	items, err := h.service.Grants(c.UserContext(), id)
	if err != nil {
		return internal(c)
	}
	return response.JSON(c, http.StatusOK, fiber.Map{"group_id": id, "grants": items})
}

func (h *Handler) replaceGrants(c *fiber.Ctx) error {
	id, err := parseID(c.Params("group_id"))
	if err != nil {
		return invalid(c)
	}
	var input grantsInput
	if err := c.BodyParser(&input); err != nil {
		return invalid(c)
	}
	items, err := h.service.ReplaceGrants(c.UserContext(), id, input.Grants)
	if err != nil {
		return mapError(c, err)
	}
	return response.JSON(c, http.StatusOK, fiber.Map{"group_id": id, "grants": items})
}

func parseID(value string) (uuid.UUID, error) {
	id, err := uuid.Parse(value)
	if err != nil || id == uuid.Nil {
		return uuid.Nil, ErrInvalidInput
	}
	return id, nil
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
		return response.Fail(c, http.StatusConflict, "CONFLICT", "Resource sudah ada")
	case errors.Is(err, ErrLastAdmin):
		return response.Fail(c, http.StatusConflict, "LAST_ACTIVE_ADMIN", "Admin aktif terakhir tidak dapat dinonaktifkan")
	default:
		return internal(c)
	}
}
