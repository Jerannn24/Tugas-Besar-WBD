package main

import (
	"log"

	"github.com/Jerannn24/Tugas-Besar-WBD.git/config"
	"github.com/Jerannn24/Tugas-Besar-WBD.git/server"
)

func main() {
	cfg := config.LoadConfig()
	db := config.LoadDB()
	app := server.New(db, cfg.JWTSecret)

	log.Printf("Server starting on port %s", cfg.Port)

	if err := app.Listen(":" + cfg.Port); err != nil {
		log.Fatal(err)
	}
}
