package workflow

import "github.com/gofiber/fiber/v2"

func RegisterRoutes(r fiber.Router, handler *Handler, csrf fiber.Handler) {
	r.Get("/databases/:database_id/proposals", handler.list)
	r.Post("/databases/:database_id/proposals", csrf, handler.create)
	r.Get("/databases/:database_id/proposals/:proposal_id", handler.get)
	r.Post("/databases/:database_id/proposals/:proposal_id/submit-review", csrf, handler.submit)
	r.Post("/databases/:database_id/proposals/:proposal_id/approvals", csrf, handler.approve)
	r.Post("/databases/:database_id/proposals/:proposal_id/jobs", csrf, handler.enqueue)
	r.Post("/proposals", csrf, handler.create)
	r.Get("/proposals/:proposal_id", handler.get)
	r.Post("/proposals/:proposal_id/submit", csrf, handler.submit)
	r.Post("/proposals/:proposal_id/approve", csrf, handler.approve)
	r.Post("/proposals/:proposal_id/queue", csrf, handler.enqueue)
	r.Post("/proposals/:proposal_id/enqueue", csrf, handler.enqueue)
}
