package server

import (
	"log"
	"time"

	"github.com/gofiber/fiber/v2"
)

func Logging() fiber.Handler {
	return func(c *fiber.Ctx) error {
		start := time.Now()

		err := c.Next()

		log.Printf(
			"method=%s path=%s status=%d duration=%s",
			c.Method(),
			c.Path(),
			c.Response().StatusCode(),
			time.Since(start),
		)

		return err
	}
}
