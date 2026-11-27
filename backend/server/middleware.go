package server

import (
	"github.com/Jerannn24/Tugas-Besar-WBD.git/telemetry"
	"github.com/gofiber/fiber/v2"
)

func Logging() fiber.Handler {
	return telemetry.Middleware(telemetry.DefaultLogger())
}
