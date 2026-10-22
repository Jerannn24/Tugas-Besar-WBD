package config

import (
	"context"
	"log"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Config struct {
	Port      string
	JWTSecret string
	DB        string
}

func LoadConfig() Config {
	return Config{
		Port:      "8080",
		JWTSecret: "secret_key",
		DB:        "postgres://username:password@localhost:5432/dbname",
	}
}

func LoadDB() *pgxpool.Pool {
	config := LoadConfig()
	dbpool, err := pgxpool.New(context.Background(), config.DB)
	if err != nil {
		log.Fatalf("Unable to connect to database: %v\n", err)
	}
	return dbpool
}
