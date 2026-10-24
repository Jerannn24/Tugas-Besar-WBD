package authz

import "errors"

var (
	ErrUnauthenticated = errors.New("unauthenticated")
	ErrForbidden       = errors.New("forbidden")
	ErrInvalidID       = errors.New("invalid identifier")
	ErrCSRF            = errors.New("csrf validation failed")
)
