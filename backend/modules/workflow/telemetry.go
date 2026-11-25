package workflow

import (
	"log/slog"
	"strconv"
	"time"

	"github.com/Jerannn24/Tugas-Besar-WBD.git/telemetry"
	"github.com/gofiber/fiber/v2"
)

func observeWorkflow(event string, next fiber.Handler) fiber.Handler {
	return func(c *fiber.Ctx) error {
		started := time.Now()
		err := next(c)
		attrs := []slog.Attr{
			slog.String("actor_id", safeTelemetryLocal(c, "actor_id")),
			slog.String("database_id", c.Params("database_id")),
			slog.String("proposal_id", c.Params("proposal_id")),
			slog.String("status", strconv.Itoa(c.Response().StatusCode())),
			slog.Int("http_status", c.Response().StatusCode()),
			slog.Float64("duration_ms", float64(time.Since(started))/float64(time.Millisecond)),
		}
		if err != nil || c.Response().StatusCode() >= 400 {
			attrs = append(attrs, slog.String("result", "failure"))
			telemetry.DefaultLogger().Warn(c.UserContext(), event, attrs...)
		} else {
			attrs = append(attrs, slog.String("result", "success"))
			telemetry.DefaultLogger().Info(c.UserContext(), event, attrs...)
		}
		return err
	}
}

func safeTelemetryLocal(c *fiber.Ctx, key string) string {
	value, _ := c.Locals(key).(string)
	if len(value) > 128 {
		return ""
	}
	return value
}
