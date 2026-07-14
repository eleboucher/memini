package ui

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

func TestMountWellKnown404(t *testing.T) {
	r := chi.NewRouter()
	if err := Mount(r); err != nil {
		t.Fatalf("mount: %v", err)
	}
	srv := httptest.NewServer(r)
	defer srv.Close()

	t.Run("well-known returns a JSON 404 not the SPA shell", func(t *testing.T) {
		for _, p := range []string{
			"/.well-known/oauth-protected-resource",
			"/.well-known/oauth-authorization-server",
		} {
			resp, err := http.Get(srv.URL + p)
			if err != nil {
				t.Fatalf("get %s: %v", p, err)
			}
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("%s: got %d, want 404", p, resp.StatusCode)
			}
			// The body must be valid JSON: MCP clients that probe for OAuth
			// discovery parse the 404 body and abort the connection on a parse
			// error (Go's default text/plain "404 page not found" breaks them).
			var v any
			if err := json.Unmarshal(body, &v); err != nil {
				t.Fatalf("%s: 404 body is not JSON (%v): %q", p, err, body)
			}
		}
	})

	t.Run("non-GET probe returns 404 not an empty-body 405", func(t *testing.T) {
		resp, err := http.Post(srv.URL+"/register", "application/json", nil)
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("POST /register: got %d, want 404", resp.StatusCode)
		}
	})

	t.Run("deep link still falls back to the SPA shell", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/some/client/route")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("deep link: got %d, want 200", resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); ct != "text/html; charset=utf-8" {
			t.Fatalf("deep link content-type: got %q", ct)
		}
	})
}

// TestShellIsTokenFree pins the security property that made the meta-tag
// injection worth removing: the shell served to an anonymous GET / (or any deep
// link that falls back to it) carries no credential. The SPA signs in against
// GET /v1/self in the browser; the server never embeds MEMINI_API_KEY (or any
// bearer) into the HTML it hands out, so serving it publicly leaks nothing.
func TestShellIsTokenFree(t *testing.T) {
	// A sentinel that would be the operator's admin key if the old injection
	// path still existed. Handler takes no key and reads no key, so there is no
	// wiring by which it could reach the served bytes — this asserts that.
	const sentinel = "s3cret-admin-key-do-not-serve"
	t.Setenv("MEMINI_API_KEY", sentinel)

	r := chi.NewRouter()
	if err := Mount(r); err != nil {
		t.Fatalf("mount: %v", err)
	}
	srv := httptest.NewServer(r)
	defer srv.Close()

	for _, path := range []string{"/", "/keys", "/some/deep/route"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("get %s: %v", path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		shell := string(body)
		if strings.Contains(shell, sentinel) {
			t.Fatalf("%s: served shell leaked the configured api key", path)
		}
		// The retired injection point: no <meta name="memini-token"> may ever
		// reappear in the shell, whatever it is seeded with.
		if strings.Contains(shell, "memini-token") {
			t.Fatalf("%s: served shell still carries a memini-token meta tag", path)
		}
	}
}
