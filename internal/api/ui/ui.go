// Package ui serves memini's embedded single-page admin UI (Preact + Vite).
// dist/ is a build artifact (gitignored; only .gitkeep is tracked) embedded at
// compile time. The Docker image builds it; locally use `mise run ui`. Without
// it the binary still boots and serves a placeholder.
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
// The shell is intentionally public (no bearer auth) and carries no credential
// of its own: it never contains MEMINI_API_KEY. The SPA authenticates in-app —
// the operator signs in once at the Login gate, the token is verified against
// GET /v1/self and persisted in the browser's localStorage, and every /v1 call
// carries it as a bearer. Serving the shell to an anonymous GET / therefore
// leaks nothing; the API it calls still enforces the token when MEMINI_API_KEY
// is set.
func Mount(r chi.Router) error {
	h, err := Handler()
	if err != nil {
		return err
	}
	r.Handle("/*", h)
	return nil
}

// Handler returns the SPA handler that Mount installs as a catch-all. It is
// exposed so the server can serve the shell on a dedicated listener (see
// MEMINI_UI_ADDR) instead of the main API port. The shell is token-free either
// way; the dedicated listener also serves /v1 so the same-origin SPA can reach
// the API it authenticates against.
func Handler() (http.Handler, error) {
	dist, err := fs.Sub(assets, "dist")
	if err != nil {
		return nil, err
	}
	index, err := fs.ReadFile(dist, "index.html")
	if err != nil {
		index = []byte(placeholder)
	}
	fileServer := http.FileServer(http.FS(dist))

	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		name := strings.TrimPrefix(req.URL.Path, "/")
		// SPA shell is GET-only; non-GET methods and the reserved /.well-known/*
		// namespace (RFC 8615) 404 so MCP clients never parse the shell or chi's
		// empty-body 405 as an OAuth response. The body is JSON, not Go's default
		// text/plain "404 page not found": memini has no OAuth, so the discovery
		// probes (RFC 9728 oauth-protected-resource, RFC 8414
		// oauth-authorization-server) hit here, and some MCP clients (Claude Code)
		// JSON-parse the discovery 404 body and abort the whole connection on a
		// parse error instead of treating the 404 as "no OAuth, use the bearer
		// token". A parseable empty object lets them fall back to static auth.
		if req.Method != http.MethodGet || strings.HasPrefix(name, ".well-known/") {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte("{}"))
			return
		}
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
	}), nil
}

func exists(fsys fs.FS, name string) bool {
	info, err := fs.Stat(fsys, name)
	return err == nil && !info.IsDir()
}

// placeholder is served when the UI bundle was not built into the binary.
const placeholder = `<!doctype html><meta charset="utf-8"><title>memini</title>` +
	`<body style="font:14px system-ui;margin:3rem;max-width:40rem">` +
	`<h1>memini</h1><p>The admin UI was not built into this binary. ` +
	`Run <code>mise run ui</code> and rebuild, or use the official container image. ` +
	`The API is available at <code>/v1</code>.</p>`
