package config

import "testing"

func TestValidateRequiredEnvironmentAndBootstrapPair(t *testing.T) {
	for _, tc := range []struct {
		name string
		c    Config
		want string
	}{
		{"missing secret", Config{DB: "postgres://db"}, "JWT_SECRET is required"},
		{"missing database", Config{JWTSecret: "secret"}, "DATABASE_URL is required"},
		{"username alone", Config{JWTSecret: "secret", DB: "db", BootstrapAdminUsername: "admin"}, "ADMIN_BOOTSTRAP_USERNAME and ADMIN_BOOTSTRAP_PASSWORD must be provided together"},
		{"password alone", Config{JWTSecret: "secret", DB: "db", BootstrapAdminPassword: "password"}, "ADMIN_BOOTSTRAP_USERNAME and ADMIN_BOOTSTRAP_PASSWORD must be provided together"},
		{"without bootstrap", Config{JWTSecret: "secret", DB: "db"}, ""},
		{"with bootstrap", Config{JWTSecret: "secret", DB: "db", BootstrapAdminUsername: "admin", BootstrapAdminPassword: "password"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.c.Validate()
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || err.Error() != tc.want {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
		})
	}
}

func TestLoadConfigDefaultsAndOverrides(t *testing.T) {
	for _, key := range []string{"PORT", "JWT_SECRET", "DATABASE_URL", "ADMIN_BOOTSTRAP_USERNAME", "ADMIN_BOOTSTRAP_PASSWORD"} {
		t.Setenv(key, "")
	}
	if got := LoadConfig(); got.Port != "8080" || got.JWTSecret != "" || got.DB != "" || got.Validate() == nil {
		t.Fatalf("unexpected defaults: %+v", got)
	}
	t.Setenv("PORT", "9001")
	t.Setenv("JWT_SECRET", "secret")
	t.Setenv("DATABASE_URL", "postgres://db")
	t.Setenv("ADMIN_BOOTSTRAP_USERNAME", "admin")
	t.Setenv("ADMIN_BOOTSTRAP_PASSWORD", "password")
	want := Config{Port: "9001", JWTSecret: "secret", DB: "postgres://db", BootstrapAdminUsername: "admin", BootstrapAdminPassword: "password"}
	if got := LoadConfig(); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	t.Setenv("CONFIG_TEST_VALUE", "")
	if got := LoadEnv("CONFIG_TEST_VALUE", "fallback"); got != "fallback" {
		t.Fatal(got)
	}
	t.Setenv("CONFIG_TEST_VALUE", "custom")
	if got := LoadEnv("CONFIG_TEST_VALUE", "fallback"); got != "custom" {
		t.Fatal(got)
	}
}
