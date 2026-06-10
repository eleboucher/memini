// Package ui serves memini's embedded single-page admin UI (Preact + Vite).
// The built assets live in dist/ and are embedded into the binary at compile
// time, so the service stays a single static binary with no separate frontend
// to deploy. Regenerate dist/ with `mise run ui`.
package ui

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
)

//go:embed all:dist
var assets embed.FS

// Mount serves the SPA at / on r. Requests for real built assets are served
// from the embedded filesystem; everything else falls back to index.html so
// client-side routing works on deep links and reloads.
//
// The shell is intentionally public (no bearer auth): the API it calls (/v1)
// enforces the token, and the user needs to load the page to enter one.
func Mount(r chi.Router) error {
	dist, err := fs.Sub(assets, "dist")
	if err != nil {
		return err
	}
	index, err := fs.ReadFile(dist, "index.html")
	if err != nil {
		return err
	}
	fileServer := http.FileServer(http.FS(dist))

	r.Get("/*", func(w http.ResponseWriter, req *http.Request) {
		name := strings.TrimPrefix(req.URL.Path, "/")
		if name != "" && exists(dist, name) {
			// Vite emits content-hashed filenames under assets/, so they are
			// safe to cache indefinitely.
			if strings.HasPrefix(name, "assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
			fileServer.ServeHTTP(w, req)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(index)
	})
	return nil
}

func exists(fsys fs.FS, name string) bool {
	info, err := fs.Stat(fsys, name)
	return err == nil && !info.IsDir()
}
