package connections

import (
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"
)

func TestMapErrorStatuses(t *testing.T) {
	cases := map[error]int{
		ErrBusy: 409, ErrConflict: 409, ErrInvalidInput: 400, ErrNotFound: 404,
		ErrTargetUnavailable: 502, errors.New("x"): 500,
	}
	for in, want := range cases {
		app := fiber.New()
		app.Get("/", func(c *fiber.Ctx) error { return mapError(c, in) })
		resp, err := app.Test(httptest.NewRequest("GET", "/", nil))
		if err != nil || resp.StatusCode != want {
			t.Errorf("%v: status %d err %v, want %d", in, resp.StatusCode, err, want)
		}
	}
}
