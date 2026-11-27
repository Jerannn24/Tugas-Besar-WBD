package workflow

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/Jerannn24/Tugas-Besar-WBD.git/executioncontract"
	"github.com/Jerannn24/Tugas-Besar-WBD.git/modules/authz"
	"github.com/Jerannn24/Tugas-Besar-WBD.git/response"
	"github.com/Jerannn24/Tugas-Besar-WBD.git/telemetry"
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
	if raw := c.Params("database_id"); raw != "" {
		databaseID, err := uuid.Parse(raw)
		if err != nil || input.Revision <= 0 || input.DatabaseID != "" && input.DatabaseID != databaseID.String() {
			return invalid(c)
		}
		input.DatabaseID = databaseID.String()
	}
	proposal, err := h.service.Create(c.UserContext(), actor.ID, input)
	if err != nil {
		return mapError(c, err)
	}
	proposal, err = h.service.present(c.UserContext(), actor.ID, proposal)
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
	revision := 0
	if c.Params("database_id") != "" {
		var input struct {
			Revision int `json:"revision"`
		}
		if err := c.BodyParser(&input); err != nil || input.Revision <= 0 {
			return invalid(c)
		}
		revision = input.Revision
		if err := h.scoped(c, actor.ID, id, input.Revision); err != nil {
			return mapError(c, err)
		}
	}
	proposal, err := h.service.Submit(c.UserContext(), actor.ID, id, revision)
	if err != nil {
		if errors.Is(err, ErrInvalidProposal) && proposal.Validation != nil && proposal.Validation.Status == "failed" {
			code, message := "VALIDATION_FAILED", "Validasi migration gagal"
			for _, issue := range proposal.Validation.Issues {
				if issue.Code == "SQL_UNSUPPORTED" {
					code, message = "SQL_UNSUPPORTED", "SQL batch di luar pola yang didukung"
					break
				}
			}
			return response.FailDetailed(
				c,
				http.StatusUnprocessableEntity,
				code,
				message,
				false,
				proposal.Validation.Issues,
			)
		}
		return mapError(c, err)
	}
	proposal, err = h.service.present(c.UserContext(), actor.ID, proposal)
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
	if err := h.scoped(c, actor.ID, id, input.Revision); err != nil {
		return mapError(c, err)
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
	proposal, err = h.service.present(c.UserContext(), actor.ID, proposal)
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
	if err := h.scoped(c, actor.ID, id, 0); err != nil {
		return mapError(c, err)
	}
	proposal, err := h.service.Get(c.UserContext(), actor.ID, id)
	if err != nil {
		return mapError(c, err)
	}
	proposal, err = h.service.present(c.UserContext(), actor.ID, proposal)
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
	if c.Params("database_id") != "" && input.Revision <= 0 {
		return invalid(c)
	}
	// Enqueue resolves revision conflicts after checking existing request intent.
	if err := h.scoped(c, actor.ID, id, 0); err != nil {
		return mapError(c, err)
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

func (h *Handler) scoped(c *fiber.Ctx, actor, id uuid.UUID, revision int) error {
	raw := c.Params("database_id")
	if raw == "" {
		return nil
	}
	databaseID, err := uuid.Parse(raw)
	if err != nil {
		return ErrInvalidProposal
	}
	proposal, err := h.service.Get(c.UserContext(), actor, id)
	if err != nil {
		return err
	}
	if proposal.DatabaseID != databaseID.String() {
		return ErrNotFound
	}
	if revision > 0 && revision != proposal.Revision {
		return ErrProposalStale
	}
	return nil
}

func (h *Handler) list(c *fiber.Ctx) error {
	actor, ok := authz.ActorFromContext(c)
	if !ok {
		return response.Fail(c, http.StatusUnauthorized, "UNAUTHENTICATED", "Sesi tidak valid")
	}
	databaseID, err := uuid.Parse(c.Params("database_id"))
	if err != nil {
		return invalid(c)
	}
	if err := h.service.authorizer.RequireAccess(c.UserContext(), actor.ID, databaseID, authz.AccessView); err != nil {
		return mapError(c, err)
	}
	limit := 20
	if c.Context().QueryArgs().Has("limit") {
		limit, err = strconv.Atoi(c.Query("limit"))
		if err != nil || limit < 1 || limit > 100 {
			return invalid(c)
		}
	}
	var cursor *uuid.UUID
	if c.Context().QueryArgs().Has("cursor") {
		id, err := uuid.Parse(c.Query("cursor"))
		if err != nil {
			return invalid(c)
		}
		cursor = &id
	}
	ids, err := h.service.repository.ListIDs(c.UserContext(), databaseID, cursor, limit+1)
	if err != nil {
		return mapError(c, err)
	}
	var next *string
	if len(ids) > limit {
		value := ids[limit-1].String()
		next = &value
		ids = ids[:limit]
	}
	items := make([]Proposal, 0, len(ids))
	for _, id := range ids {
		proposal, err := h.service.Get(c.UserContext(), actor.ID, id)
		if err != nil {
			return mapError(c, err)
		}
		proposal, err = h.service.present(c.UserContext(), actor.ID, proposal)
		if err != nil {
			return mapError(c, err)
		}
		items = append(items, proposal)
	}
	return response.JSON(c, http.StatusOK, fiber.Map{"items": items, "next_cursor": next})
}

func invalid(c *fiber.Ctx) error {
	return response.Fail(c, http.StatusBadRequest, "INVALID_REQUEST", "Request tidak valid")
}

func mapError(c *fiber.Ctx, err error) error {
	var contractErr executioncontract.ContractError
	if errors.As(err, &contractErr) {
		code := contractErr.Code
		status := http.StatusConflict
		switch code {
		case "INVALID_REQUEST", "INVALID_INPUT":
			status = http.StatusBadRequest
			code = "INVALID_REQUEST"
		case "NOT_FOUND":
			status = http.StatusNotFound
		case "FORBIDDEN":
			status = http.StatusForbidden
		case "LEADER_REQUIRED":
			status = http.StatusForbidden
		case "INTERNAL_ERROR", "NOT_CONFIGURED":
			status = http.StatusInternalServerError
		case "TARGET_UNAVAILABLE":
			status = http.StatusBadGateway
		case "VALIDATION_FAILED", "MIGRATION_IRREVERSIBLE":
			status = http.StatusUnprocessableEntity
		case "VERSION_CONFLICT", "PROPOSAL_STALE", "REVIEW_STALE", "CONFLICT":
			status = http.StatusConflict
			code = "VERSION_CONFLICT"
		}
		if status >= http.StatusInternalServerError {
			logInternalError(c, err)
		}
		return response.FailDetailed(c, status, code, contractErr.Message, contractErr.Retryable, contractErr.Issues)
	}
	switch {
	case errors.Is(err, ErrNotFound):
		return response.Fail(c, http.StatusNotFound, "NOT_FOUND", "Proposal tidak ditemukan")
	case errors.Is(err, ErrProposalStale), errors.Is(err, ErrConflict):
		return response.Fail(c, http.StatusConflict, "VERSION_CONFLICT", "Proposal sudah berubah atau tidak lagi sesuai dengan versi terkini")
	case errors.Is(err, ErrPendingApprovals):
		return response.Fail(c, http.StatusConflict, "APPROVAL_REQUIRED", "Persetujuan proposal belum lengkap")
	case errors.Is(err, ErrLeaderRequired):
		return response.Fail(c, http.StatusForbidden, "LEADER_REQUIRED", "Hanya leader group yang berhak memberikan persetujuan")
	case errors.Is(err, ErrForbidden), errors.Is(err, authz.ErrForbidden):
		return response.Fail(c, http.StatusForbidden, "FORBIDDEN", "Akses ditolak")
	case errors.Is(err, ErrInvalidProposal):
		return invalid(c)
	default:
		logInternalError(c, err)
		return response.Fail(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Terjadi kesalahan internal")
	}
}

func logInternalError(c *fiber.Ctx, err error) {
	message, _ := telemetry.SafeError(err)
	telemetry.DefaultLogger().Error(
		c.UserContext(),
		"workflow.request.internal_error",
		slog.String("method", c.Method()),
		slog.String("path", c.Path()),
		slog.Int("http_status", http.StatusInternalServerError),
		slog.String("error", message),
	)
}
