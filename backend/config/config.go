package config

import (
	"context"
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
		JWTSecret:              LoadEnv("JWT_SECRET", "secret_key"),
		DB:                     LoadEnv("DATABASE_URL", "postgres://username:password@localhost:5432/dbname"),
		BootstrapAdminUsername: LoadEnv("DB_DEV_BOOTSTRAP_USER", "admin"),
		BootstrapAdminPassword: LoadEnv("DB_DEV_BOOTSTRAP_PASSWORD", "admin123"),
	}
}

func LoadDB() *pgxpool.Pool {
	config := LoadConfig()
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
