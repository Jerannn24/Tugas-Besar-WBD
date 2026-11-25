package telemetry

import (
	"strings"
	"testing"
)

func TestMaskSQLHidesLiteralsCommentsAndNumbers(t *testing.T) {
	query := `SELECT * FROM users WHERE email = 'secret@example.com' AND id = 123 /* private comment */ AND token = $tag$secret-token$tag$ -- another secret`
	masked := MaskSQL(query)
	for _, secret := range []string{"secret@example.com", "123", "private comment", "secret-token", "another secret"} {
		if strings.Contains(masked, secret) {
			t.Fatalf("masked SQL leaked %q: %s", secret, masked)
		}
	}
	if !strings.Contains(masked, "SELECT * FROM users") {
		t.Fatalf("SQL structure was not preserved: %s", masked)
	}
}

func TestMaskSQLPreservesBindPlaceholders(t *testing.T) {
	masked := MaskSQL(`SELECT * FROM accounts WHERE id = $1 AND balance > 500`)
	if !strings.Contains(masked, "$1") {
		t.Fatalf("bind placeholder was changed: %s", masked)
	}
	if strings.Contains(masked, "500") {
		t.Fatalf("numeric literal was not masked: %s", masked)
	}
}

func TestMaskSecrets(t *testing.T) {
	masked := MaskSecrets("password=hunter2 postgres://user:secret@db/app")
	if strings.Contains(masked, "hunter2") || strings.Contains(masked, "user:secret") {
		t.Fatalf("secret leaked: %s", masked)
	}
}
