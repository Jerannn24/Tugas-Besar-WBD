package authz

import (
	"net/http"

	"github.com/Jerannn24/Tugas-Besar-WBD.git/modules/auth"
)

func CheckCSRF(r *http.Request) error {
	cookie, err := r.Cookie(auth.CSRFCookieName)
	if err != nil || cookie.Value == "" || r.Header.Get("X-CSRF-Token") != cookie.Value {
		return ErrCSRF
	}
	return nil
}

func IsCSRFRequired(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	default:
		return true
	}
}
