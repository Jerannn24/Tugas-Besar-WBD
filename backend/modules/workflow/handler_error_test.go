package workflow

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Jerannn24/Tugas-Besar-WBD.git/executioncontract"
	"github.com/gofiber/fiber/v2"
)

func TestMapErrorContractMatchesAPIContract(t *testing.T) {
	issue := executioncontract.Issue{Code: "INVALID_SQL", Message: "sql is required"}
	tests := []struct {
		name      string
		err       error
		status    int
		code      string
		retryable bool
		hasIssues bool
	}{
		{name: "invalid request", err: executioncontract.ContractError{Code: "INVALID_REQUEST", Message: "bad request"}, status: http.StatusBadRequest, code: "INVALID_REQUEST"},
		{name: "legacy invalid input", err: executioncontract.ContractError{Code: "INVALID_INPUT", Message: "bad request"}, status: http.StatusBadRequest, code: "INVALID_REQUEST"},
		{name: "legacy conflict", err: executioncontract.ContractError{Code: "CONFLICT", Message: "stale"}, status: http.StatusConflict, code: "VERSION_CONFLICT"},
		{name: "version conflict", err: ErrProposalStale, status: http.StatusConflict, code: "VERSION_CONFLICT"},
		{name: "conflict", err: ErrConflict, status: http.StatusConflict, code: "VERSION_CONFLICT"},
		{name: "approval required", err: ErrPendingApprovals, status: http.StatusConflict, code: "APPROVAL_REQUIRED"},
		{name: "leader required", err: ErrLeaderRequired, status: http.StatusForbidden, code: "LEADER_REQUIRED"},
		{name: "validation failed", err: executioncontract.ContractError{Code: "VALIDATION_FAILED", Message: "invalid", Issues: []executioncontract.Issue{issue}}, status: http.StatusUnprocessableEntity, code: "VALIDATION_FAILED", hasIssues: true},
		{name: "retryable internal", err: executioncontract.ContractError{Code: "INTERNAL_ERROR", Message: "temporarily unavailable", Retryable: true}, status: http.StatusInternalServerError, code: "INTERNAL_ERROR", retryable: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := fiber.New()
			app.Get("/", func(c *fiber.Ctx) error {
				return mapError(c, tt.err)
			})
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			res, err := app.Test(req)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()

			var payload struct {
				Error struct {
					Code      string                    `json:"code"`
					Retryable bool                      `json:"retryable"`
					Issues    []executioncontract.Issue `json:"issues"`
				} `json:"error"`
			}
			if err := json.NewDecoder(res.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			if res.StatusCode != tt.status || payload.Error.Code != tt.code {
				t.Fatalf("got status=%d code=%q, want status=%d code=%q", res.StatusCode, payload.Error.Code, tt.status, tt.code)
			}
			if payload.Error.Retryable != tt.retryable {
				t.Fatalf("retryable=%v, want %v", payload.Error.Retryable, tt.retryable)
			}
			if got := len(payload.Error.Issues) > 0; got != tt.hasIssues {
				t.Fatalf("has issues=%v, want %v", got, tt.hasIssues)
			}
		})
	}
}

func TestMapErrorLogsUnexpectedServerErrors(t *testing.T) {
	app := fiber.New()
	app.Get("/", func(c *fiber.Ctx) error {
		return mapError(c, errors.New("database password=secret failed"))
	})
	res, err := app.Test(httptest.NewRequest(http.MethodGet, "/", nil))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500", res.StatusCode)
	}
}
