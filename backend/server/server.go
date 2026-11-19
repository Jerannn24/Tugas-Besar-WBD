package server

import (
	"net/url"
	"os"
	stdpath "path"
	"path/filepath"
	"strings"

	"github.com/Jerannn24/Tugas-Besar-WBD.git/routes"
	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5/pgxpool"
)

var okBytes = []byte(`{"status":"ok"}`)

func New(db *pgxpool.Pool, jwtSecret string) *fiber.App {
	app := fiber.New(fiber.Config{
		ErrorHandler: ErrorHandler,
	})

	app.Get("/health", func(c *fiber.Ctx) error {
		c.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSONCharsetUTF8)
		return c.Send(okBytes)
	})

	app.Use(Logging())

	routes.Register(app, db, jwtSecret)
	Frontend(app, os.Getenv("FRONTEND_DIST"))

	return app
}

const (
	assetCacheControl = "public, max-age=31536000, immutable"
	pageCacheControl  = "no-cache"
)

func Frontend(app *fiber.App, dir string) {
	if dir == "" {
		dir = "web"
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return
	}
	root, err := filepath.Abs(dir)
	if err != nil {
		return
	}
	index := filepath.Join(root, "index.html")

	app.Static("/assets", filepath.Join(root, "assets"), fiber.Static{
		ModifyResponse: func(c *fiber.Ctx) error {
			c.Set(fiber.HeaderCacheControl, assetCacheControl)
			return nil
		},
	})

	app.Get("/*", func(c *fiber.Ctx) error {
		path := c.Path()
		if isPrefix(path, "/api") || isPrefix(path, "/assets") {
			return fiber.ErrNotFound
		}
		if file, ok := rootFile(root, path); ok {
			return sendFile(c, file)
		}
		if filepath.Ext(stdpath.Base(path)) != "" {
			return fiber.ErrNotFound
		}
		return sendFile(c, index)
	})
}

func isPrefix(path, prefix string) bool {
	return path == prefix || strings.HasPrefix(path, prefix+"/")
}

func rootFile(root, path string) (string, bool) {
	decoded, err := url.PathUnescape(path)
	if err != nil {
		return "", false
	}
	name := filepath.Join(root, filepath.FromSlash(stdpath.Clean("/"+decoded)))
	if rel, err := filepath.Rel(root, name); err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return "", false
	}
	info, err := os.Stat(name)
	if err != nil || !info.Mode().IsRegular() {
		return "", false
	}
	return name, true
}

func sendFile(c *fiber.Ctx, file string) error {
	if err := c.SendFile(file); err != nil {
		return err
	}
	c.Set(fiber.HeaderCacheControl, pageCacheControl)
	return nil
}
