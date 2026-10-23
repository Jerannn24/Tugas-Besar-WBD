package server

import (
	"github.com/Jerannn24/Tugas-Besar-WBD.git/response"
	"github.com/gofiber/fiber/v2"
)

func ErrorHandler(c *fiber.Ctx, err error) error {
	status := fiber.StatusInternalServerError
	message := "Terjadi kesalahan internal"
	code := "INTERNAL_ERROR"

	if e, ok := err.(*fiber.Error); ok {
		status = e.Code
	}

	switch status {
	case fiber.StatusNotFound:
		code = "NOT_FOUND"
		message = "Endpoint tidak ditemukan"
	case fiber.StatusMethodNotAllowed:
		code = "METHOD_NOT_ALLOWED"
		message = "Method tidak diizinkan"
	case fiber.StatusBadRequest:
		code = "BAD_REQUEST"
		message = "Request tidak valid"
	}

	return response.Fail(c, status, code, message)
}
