package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Jerannn24/Tugas-Besar-WBD.git/config"
	"github.com/Jerannn24/Tugas-Besar-WBD.git/modules/auth"
	"github.com/Jerannn24/Tugas-Besar-WBD.git/server"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context) (err error) {
	cfg := config.LoadConfig()
	if err := cfg.Validate(); err != nil {
		return err
	}
	db := config.LoadDB(cfg)
	closeDB := true
	defer func() {
		if closeDB {
			db.Close()
		}
	}()
	if cfg.BootstrapAdminUsername != "" {
		if err := auth.NewAdminRepository(db).BootstrapAdmin(ctx, cfg.BootstrapAdminUsername, cfg.BootstrapAdminPassword); err != nil {
			return fmt.Errorf("bootstrap admin: %w", err)
		}
	}
	if ctx.Err() != nil {
		return nil
	}
	listener, err := net.Listen("tcp", ":"+cfg.Port)
	if err != nil {
		return fmt.Errorf("bind HTTP listener: %w", err)
	}
	defer listener.Close()

	app, closeRuntime, err := server.New(ctx, db, cfg.JWTSecret)
	if err != nil {
		return fmt.Errorf("initialize server: %w", err)
	}
	defer func() {
		shutdownErr := closeRuntime()
		closeDB = shutdownErr == nil
		err = errors.Join(err, shutdownErr)
	}()
	if ctx.Err() != nil {
		return nil
	}
	ready := make(chan struct{})
	listenErr := make(chan error, 1)
	go func() {
		listenErr <- app.Listener(&startupListener{Listener: listener, ready: ready})
	}()
	select {
	case <-ready:
	case err := <-listenErr:
		return err
	}
	log.Printf("Server starting on port %s", cfg.Port)

	select {
	case err := <-listenErr:
		return errors.Join(err, app.ShutdownWithTimeout(15*time.Second))
	case <-ctx.Done():
		shutdownErr := app.ShutdownWithTimeout(15 * time.Second)
		return errors.Join(shutdownErr, <-listenErr)
	}
}

// The first Accept is after Fiber has registered the listener for Shutdown.
type startupListener struct {
	net.Listener
	ready chan struct{}
}

func (l *startupListener) Accept() (net.Conn, error) {
	if l.ready != nil {
		close(l.ready)
		l.ready = nil
	}
	return l.Listener.Accept()
}
