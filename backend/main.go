package main

import (
	"context"
	"log"

	"github.com/Jerannn24/Tugas-Besar-WBD.git/config"
	"github.com/Jerannn24/Tugas-Besar-WBD.git/modules/auth"
	"github.com/Jerannn24/Tugas-Besar-WBD.git/server"
)

func main() {
	cfg := config.LoadConfig()
	if err := cfg.Validate(); err != nil {
		log.Fatal(err)
	}
	db := config.LoadDB(cfg)
	defer db.Close()
	if cfg.BootstrapAdminUsername != "" {
		if err := auth.NewAdminRepository(db).BootstrapAdmin(context.Background(), cfg.BootstrapAdminUsername, cfg.BootstrapAdminPassword); err != nil {
			log.Fatalf("unable to bootstrap admin: %v", err)
		}
	}
	app := server.New(db, cfg.JWTSecret)

	log.Printf("Server starting on port %s", cfg.Port)

	if err := app.Listen(":" + cfg.Port); err != nil {
		log.Fatal(err)
	}
}
