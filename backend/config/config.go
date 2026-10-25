package config

import (
	"context"
	"errors"
	"log"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Config struct {
	Port                   string
	JWTSecret              string
	DB                     string
	BootstrapAdminUsername string
	BootstrapAdminPassword string
}

func LoadConfig() Config {
	return Config{
		Port:                   LoadEnv("PORT", "8080"),
		JWTSecret:              os.Getenv("JWT_SECRET"),
		DB:                     os.Getenv("DATABASE_URL"),
		BootstrapAdminUsername: os.Getenv("ADMIN_BOOTSTRAP_USERNAME"),
		BootstrapAdminPassword: os.Getenv("ADMIN_BOOTSTRAP_PASSWORD"),
	}
}

func (c Config) Validate() error {
	if c.JWTSecret == "" {
		return errors.New("JWT_SECRET is required")
	}
	if c.DB == "" {
		return errors.New("DATABASE_URL is required")
	}
	if (c.BootstrapAdminUsername == "") != (c.BootstrapAdminPassword == "") {
		return errors.New("ADMIN_BOOTSTRAP_USERNAME and ADMIN_BOOTSTRAP_PASSWORD must be provided together")
	}
	return nil
}

func LoadDB(config Config) *pgxpool.Pool {
	dbpool, err := pgxpool.New(context.Background(), config.DB)
	if err != nil {
		log.Fatalf("Unable to connect to database: %v\n", err)
	}

	if err := dbpool.Ping(context.Background()); err != nil {
		log.Fatalf("Unable to ping database: %v\n", err)
	}

	return dbpool
}

func LoadEnv(key, fallback string) string {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	return value
}
