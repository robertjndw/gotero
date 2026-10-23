// Package webui serves the embedded admin UI built from web/ into dist/.
package webui

import (
	"bytes"
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

//go:embed all:dist
var dist embed.FS

// reserved prefixes never fall back to index.html, so API typos get a real
// 404 instead of an HTML page that confuses clients.
var reserved = []string{"/api", "/admin/api", "/stream", "/files", "/healthz", "/auth"}

// Handler serves static assets and falls back to index.html for client-side
// routes. It is meant to be installed as the engine's NoRoute handler.
func Handler() gin.HandlerFunc {
	sub, _ := fs.Sub(dist, "dist")
	index, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		index = []byte("<!doctype html><title>gotero</title><p>The web UI was not built into this binary. Run <code>make web</code> and rebuild.</p>")
	}
	files := http.FileServerFS(sub)
	return func(c *gin.Context) {
		p := c.Request.URL.Path
		for _, r := range reserved {
			if p == r || strings.HasPrefix(p, r+"/") {
				c.JSON(http.StatusNotFound, gin.H{"error": gin.H{"code": "not_found", "message": "Not found"}})
				return
			}
		}
		if c.Request.Method != http.MethodGet && c.Request.Method != http.MethodHead {
			c.Status(http.StatusMethodNotAllowed)
			return
		}
		name := strings.TrimPrefix(path.Clean(p), "/")
		if name != "" && name != "index.html" {
			if st, err := fs.Stat(sub, name); err == nil && !st.IsDir() {
				// Vite fingerprints everything under assets/, so it can be cached forever.
				if strings.HasPrefix(name, "assets/") {
					c.Header("Cache-Control", "public, max-age=31536000, immutable")
				}
				files.ServeHTTP(c.Writer, c.Request)
				return
			}
		}
		c.Header("Cache-Control", "no-cache")
		c.Header("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; frame-ancestors 'none'")
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("Referrer-Policy", "same-origin")
		http.ServeContent(c.Writer, c.Request, "index.html", time.Time{}, bytes.NewReader(index))
	}
}
