package workflow

import (
	"errors"
	"net/http"

	"github.com/Jerannn24/Tugas-Besar-WBD.git/executioncontract"
	"github.com/Jerannn24/Tugas-Besar-WBD.git/modules/authz"
	"github.com/Jerannn24/Tugas-Besar-WBD.git/response"
	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
)

type Handler struct {
	service *Service
}
type ApproveInput struct {
	GroupID  string `json:"group_id"`
	Revision int    `json:"revision"`
}

func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

func (h *Handler) create(c *fiber.Ctx) error {
	actor, ok := authz.ActorFromContext(c)
	if !ok {
		return response.Fail(c, http.StatusUnauthorized, "UNAUTHENTICATED", "Sesi tidak valid")
	}
	var input CreateInput
	if err := c.BodyParser(&input); err != nil {
		return invalid(c)
	}
	proposal, err := h.service.Create(c.UserContext(), actor.ID, input)
	if err != nil {
		return mapError(c, err)
	}
	return response.JSON(c, http.StatusCreated, proposal)
}

func (h *Handler) submit(c *fiber.Ctx) error {
	actor, ok := authz.ActorFromContext(c)
	if !ok {
		return response.Fail(c, http.StatusUnauthorized, "UNAUTHENTICATED", "Sesi tidak valid")
	}
	id, err := uuid.Parse(c.Params("proposal_id"))
	if err != nil {
		return invalid(c)
	}
	proposal, err := h.service.Submit(c.UserContext(), actor.ID, id)
	if err != nil {
		return mapError(c, err)
	}
	return response.JSON(c, http.StatusOK, proposal)
}

func (h *Handler) approve(c *fiber.Ctx) error {
	actor, ok := authz.ActorFromContext(c)
	if !ok {
		return response.Fail(
			c,
			http.StatusUnauthorized,
			"UNAUTHENTICATED",
			"Sesi tidak valid",
		)
	}

	id, err := uuid.Parse(c.Params("proposal_id"))
	if err != nil {
		return invalid(c)
	}

	var input ApproveInput
	if err := c.BodyParser(&input); err != nil {
		return invalid(c)
	}

	groupID, err := uuid.Parse(input.GroupID)
	if err != nil || input.Revision <= 0 {
		return invalid(c)
	}

	proposal, err := h.service.Approve(
		c.UserContext(),
		actor.ID,
		id,
		groupID,
		input.Revision,
	)
	if err != nil {
		return mapError(c, err)
	}

	return response.JSON(c, http.StatusOK, proposal)
}

func (h *Handler) get(c *fiber.Ctx) error {
	actor, ok := authz.ActorFromContext(c)
	if !ok {
		return response.Fail(c, http.StatusUnauthorized, "UNAUTHENTICATED", "Sesi tidak valid")
	}
	id, err := uuid.Parse(c.Params("proposal_id"))
	if err != nil {
		return invalid(c)
	}
	proposal, err := h.service.Get(c.UserContext(), actor.ID, id)
	if err != nil {
		return mapError(c, err)
	}
	return response.JSON(c, http.StatusOK, proposal)
}

func (h *Handler) enqueue(c *fiber.Ctx) error {
	actor, ok := authz.ActorFromContext(c)
	if !ok {
		return response.Fail(c, http.StatusUnauthorized, "UNAUTHENTICATED", "Sesi tidak valid")
	}
	id, err := uuid.Parse(c.Params("proposal_id"))
	if err != nil {
		return invalid(c)
	}
	var input EnqueueInput
	if err := c.BodyParser(&input); err != nil {
		return invalid(c)
	}
	result, err := h.service.Enqueue(c.UserContext(), actor.ID, id, input)
	if err != nil {
		return mapError(c, err)
	}
	status := http.StatusAccepted
	if result.Replayed {
		status = http.StatusOK
	}

	return response.JSON(c, status, result)
}

func invalid(c *fiber.Ctx) error {
	return response.Fail(c, http.StatusBadRequest, "INVALID_INPUT", "Request tidak valid")
}

func mapError(c *fiber.Ctx, err error) error {
	var contractErr executioncontract.ContractError
	if errors.As(err, &contractErr) {
		status := http.StatusConflict
		switch contractErr.Code {
		case "INVALID_REQUEST":
			status = http.StatusBadRequest
		case "NOT_FOUND":
			status = http.StatusNotFound
		case "FORBIDDEN":
			status = http.StatusForbidden
		case "INTERNAL_ERROR", "NOT_CONFIGURED":
			status = http.StatusInternalServerError
		case "TARGET_UNAVAILABLE":
			status = http.StatusBadGateway
		}
		return response.Fail(c, status, contractErr.Code, contractErr.Message)
	}
	switch {
	case errors.Is(err, ErrNotFound):
		return response.Fail(c, http.StatusNotFound, "NOT_FOUND", "Proposal tidak ditemukan")
	case errors.Is(err, ErrProposalStale):
		return response.Fail(c, http.StatusConflict, "PROPOSAL_STALE", "Proposal sudah tidak sesuai dengan snapshot terkini")
	case errors.Is(err, ErrConflict):
		return response.Fail(c, http.StatusConflict, "CONFLICT", "Proposal tidak dapat diubah pada status ini")
	case errors.Is(err, ErrPendingApprovals):
		return response.Fail(c, http.StatusConflict, "APPROVAL_REQUIRED", "Persetujuan proposal belum lengkap")
	case errors.Is(err, authz.ErrForbidden):
		return response.Fail(c, http.StatusForbidden, "FORBIDDEN", "Akses ditolak")
	case errors.Is(err, ErrInvalidProposal):
		return invalid(c)
	default:
		return response.Fail(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Terjadi kesalahan internal")
	}
}
