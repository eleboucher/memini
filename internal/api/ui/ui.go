// Package ui serves memini's embedded single-page admin UI (Preact + Vite).
// dist/ is a build artifact (gitignored; only .gitkeep is tracked) embedded at
// compile time. The Docker image builds it; locally use `mise run ui`. Without
// it the binary still boots and serves a placeholder.
package ui

import (
	"bytes"
	"embed"
	"html"
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
// The shell is intentionally public (no bearer auth): when MEMINI_API_KEY is
// set the API it calls (/v1) enforces the token. apiKey, when non-empty, is
// injected into the shell so the same-origin UI authenticates without the
// operator pasting it — which exposes the key to anyone who can load the page,
// so only set it where reaching the UI already implies trust.
func Mount(r chi.Router, apiKey string) error {
	dist, err := fs.Sub(assets, "dist")
	if err != nil {
		return err
	}
	index, err := fs.ReadFile(dist, "index.html")
	if err != nil {
		index = []byte(placeholder)
	}
	index = injectToken(index, apiKey)
	fileServer := http.FileServer(http.FS(dist))

	r.Get("/*", func(w http.ResponseWriter, req *http.Request) {
		name := strings.TrimPrefix(req.URL.Path, "/")
		// /.well-known/* is reserved (RFC 8615), never an SPA route: serving the
		// shell here makes MCP OAuth-discovery probes parse HTML as JSON and fail.
		if strings.HasPrefix(name, ".well-known/") {
			http.NotFound(w, req)
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
	})
	return nil
}

func exists(fsys fs.FS, name string) bool {
	info, err := fs.Stat(fsys, name)
	return err == nil && !info.IsDir()
}

// injectToken embeds apiKey into the shell as a <meta> tag the SPA reads to seed
// its bearer token. It is inserted before </head> when present (otherwise
// prepended). The value is HTML-attribute escaped. A blank key is a no-op.
func injectToken(index []byte, apiKey string) []byte {
	if apiKey == "" {
		return index
	}
	tag := []byte(`<meta name="memini-token" content="` + html.EscapeString(apiKey) + `">`)
	if i := bytes.Index(index, []byte("</head>")); i >= 0 {
		out := make([]byte, 0, len(index)+len(tag))
		out = append(out, index[:i]...)
		out = append(out, tag...)
		return append(out, index[i:]...)
	}
	return append(tag, index...)
}

// placeholder is served when the UI bundle was not built into the binary.
const placeholder = `<!doctype html><meta charset="utf-8"><title>memini</title>` +
	`<body style="font:14px system-ui;margin:3rem;max-width:40rem">` +
	`<h1>memini</h1><p>The admin UI was not built into this binary. ` +
	`Run <code>mise run ui</code> and rebuild, or use the official container image. ` +
	`The API is available at <code>/v1</code>.</p>`
