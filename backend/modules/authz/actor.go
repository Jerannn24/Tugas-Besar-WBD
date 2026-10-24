package authz

import "github.com/google/uuid"

type Actor struct {
	ID          uuid.UUID
	IsAdmin     bool
	AuthVersion int64
}
