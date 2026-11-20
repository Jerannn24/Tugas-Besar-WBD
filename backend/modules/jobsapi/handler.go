package jobsapi

import (
	"context"
	"errors"
	"strconv"

	"github.com/Jerannn24/Tugas-Besar-WBD.git/executioncontract"
	"github.com/Jerannn24/Tugas-Besar-WBD.git/modules/authz"
	"github.com/Jerannn24/Tugas-Besar-WBD.git/response"
	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
)

const (
	defaultLimit = 20
	maxLimit     = 100
)

type JobReader interface {
	GetJob(ctx context.Context, databaseID, jobID string) (executioncontract.JobView, error)
	ListJobs(ctx context.Context, databaseID string, page executioncontract.Page) (executioncontract.JobPage, error)
	ListAttempts(ctx context.Context, databaseID string, page executioncontract.Page) (executioncontract.AttemptPage, error)
}

type ActionReader interface {
	AllowedActions(ctx context.Context, actorID, databaseID, executionID string) ([]executioncontract.AllowedAction, error)
}

var (
	_ JobReader    = executioncontract.Jobs(nil)
	_ ActionReader = executioncontract.WorkflowGate(nil)
)

type Dependencies struct {
	Jobs       JobReader
	Actions    ActionReader
	Authorizer authz.Authorizer
}

type Handler struct {
	jobs       JobReader
	actions    ActionReader
	authorizer authz.Authorizer
}

func New(deps Dependencies) (*Handler, error) {
	if deps.Jobs == nil || deps.Actions == nil || deps.Authorizer == nil {
		return nil, errors.New("jobs API dependencies are required")
	}
	return &Handler{jobs: deps.Jobs, actions: deps.Actions, authorizer: deps.Authorizer}, nil
}

// Register must run before the M1 /databases/* wildcard so these routes match first.
func Register(router fiber.Router, deps Dependencies) error {
	h, err := New(deps)
	if err != nil {
		return err
	}
	router.Get("/databases/:database_id/jobs", h.ListJobs)
	router.Get("/databases/:database_id/jobs/:job_id", h.GetJob)
	router.Get("/databases/:database_id/attempts", h.ListAttempts)
	return nil
}

func (h *Handler) ListJobs(c *fiber.Ctx) error {
	req, err := h.guard(c, "")
	if req == nil {
		return err
	}
	page, fieldErrors := parsePage(c)
	if fieldErrors != nil {
		return invalidRequest(c, fieldErrors)
	}
	result, err := h.jobs.ListJobs(c.UserContext(), req.databaseID, page)
	if err != nil {
		return modelError(c, err)
	}
	items := result.Items
	if items == nil {
		items = []executioncontract.JobView{}
	}
	cache := map[string][]executioncontract.AllowedAction{}
	for i := range items {
		items[i].AllowedActions = h.allowedActions(c.UserContext(), req, items[i].ExecutionID, cache)
	}
	return response.JSON(c, fiber.StatusOK, executioncontract.JobPage{Items: items, NextCursor: result.NextCursor})
}

func (h *Handler) GetJob(c *fiber.Ctx) error {
	req, err := h.guard(c, "job_id")
	if req == nil {
		return err
	}
	job, err := h.jobs.GetJob(c.UserContext(), req.databaseID, req.resourceID)
	if err != nil {
		return modelError(c, err)
	}
	job.AllowedActions = h.allowedActions(c.UserContext(), req, job.ExecutionID, nil)
	return response.JSON(c, fiber.StatusOK, job)
}

func (h *Handler) ListAttempts(c *fiber.Ctx) error {
	req, err := h.guard(c, "")
	if req == nil {
		return err
	}
	page, fieldErrors := parsePage(c)
	if fieldErrors != nil {
		return invalidRequest(c, fieldErrors)
	}
	result, err := h.jobs.ListAttempts(c.UserContext(), req.databaseID, page)
	if err != nil {
		return modelError(c, err)
	}
	if result.Items == nil {
		result.Items = []executioncontract.Attempt{}
	}
	return response.JSON(c, fiber.StatusOK, result)
}

type request struct {
	actorID    string
	databaseID string
	resourceID string
}

// guard returns nil after writing the error response; resourceParam names an extra UUID path param.
func (h *Handler) guard(c *fiber.Ctx, resourceParam string) (*request, error) {
	actor, ok := authz.ActorFromContext(c)
	if !ok || actor.ID == uuid.Nil {
		return nil, unauthenticated(c)
	}
	databaseID, err := uuid.Parse(c.Params("database_id"))
	if err != nil {
		return nil, invalidRequest(c, map[string]string{"database_id": "Format ID tidak valid"})
	}
	req := &request{actorID: actor.ID.String(), databaseID: databaseID.String()}
	if resourceParam != "" {
		resourceID, err := uuid.Parse(c.Params(resourceParam))
		if err != nil {
			return nil, invalidRequest(c, map[string]string{resourceParam: "Format ID tidak valid"})
		}
		req.resourceID = resourceID.String()
	}
	if err := h.authorizer.RequireAccess(c.UserContext(), actor.ID, databaseID, authz.AccessView); err != nil {
		return nil, accessError(c, err)
	}
	return req, nil
}

func (h *Handler) allowedActions(ctx context.Context, req *request, executionID string, cache map[string][]executioncontract.AllowedAction) []executioncontract.AllowedAction {
	if actions, ok := cache[executionID]; ok {
		return actions
	}
	actions, err := h.actions.AllowedActions(ctx, req.actorID, req.databaseID, executionID)
	if err != nil || actions == nil {
		actions = []executioncontract.AllowedAction{}
	}
	if cache != nil {
		cache[executionID] = actions
	}
	return actions
}

func parsePage(c *fiber.Ctx) (executioncontract.Page, map[string]string) {
	args := c.Context().QueryArgs()
	page := executioncontract.Page{Limit: defaultLimit}
	fieldErrors := map[string]string{}
	if args.Has("limit") {
		limit, err := strconv.Atoi(string(args.Peek("limit")))
		if err != nil || limit < 1 || limit > maxLimit {
			fieldErrors["limit"] = "Limit harus bilangan bulat 1–100"
		} else {
			page.Limit = limit
		}
	}
	if args.Has("cursor") {
		cursor := string(args.Peek("cursor"))
		if cursor == "" {
			fieldErrors["cursor"] = "Cursor tidak boleh kosong"
		} else {
			page.Cursor = &cursor
		}
	}
	if len(fieldErrors) > 0 {
		return executioncontract.Page{}, fieldErrors
	}
	return page, nil
}

func accessError(c *fiber.Ctx, err error) error {
	switch {
	case errors.Is(err, authz.ErrUnauthenticated):
		return unauthenticated(c)
	case errors.Is(err, authz.ErrForbidden):
		return response.Fail(c, fiber.StatusForbidden, "FORBIDDEN", "Akses ditolak")
	case errors.Is(err, authz.ErrNotFound):
		return notFound(c)
	case errors.Is(err, authz.ErrInvalidID):
		return invalidRequest(c, map[string]string{})
	default:
		return internalError(c)
	}
}

func modelError(c *fiber.Ctx, err error) error {
	var value executioncontract.ContractError
	var pointer *executioncontract.ContractError
	code := ""
	if errors.As(err, &value) {
		code = value.Code
	} else if errors.As(err, &pointer) && pointer != nil {
		code = pointer.Code
	}
	switch code {
	case "NOT_FOUND":
		return notFound(c)
	case "INVALID_REQUEST":
		return invalidRequest(c, map[string]string{})
	default:
		return internalError(c)
	}
}

func invalidRequest(c *fiber.Ctx, fieldErrors map[string]string) error {
	return c.Status(fiber.StatusBadRequest).JSON(response.APIResponse{
		Success: false,
		Error: &response.APIError{
			Code:        "INVALID_REQUEST",
			Message:     "Permintaan tidak valid",
			FieldErrors: fieldErrors,
		},
	})
}

func unauthenticated(c *fiber.Ctx) error {
	return response.Fail(c, fiber.StatusUnauthorized, "UNAUTHENTICATED", "Sesi tidak valid")
}

func notFound(c *fiber.Ctx) error {
	return response.Fail(c, fiber.StatusNotFound, "NOT_FOUND", "Data tidak ditemukan")
}

func internalError(c *fiber.Ctx) error {
	return response.Fail(c, fiber.StatusInternalServerError, "INTERNAL_ERROR", "Terjadi kesalahan internal")
}
