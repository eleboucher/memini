package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/eleboucher/memini/internal/config"
)

// TestNewServerSharesAuthCacheBetweenRESTAndMCP pins the fix for a bootstrap
// bypass: REST and MCP must authenticate off the SAME apiauth.Config so a
// key-mutation Invalidate() call (internal/api/rest/apikeys.go, on POST
// /v1/keys) is visible to the /mcp surface immediately, rather than up to
// keyTableCacheTTL (10s) later. Before the fix, newServer built two
// independent apiauth.Config values — one for REST's AuthConfig, one inside
// mcpapi.HTTPHandler — each holding its OWN table-emptiness cache pointer, so
// a REST-side Invalidate() never reached MCP's copy: /mcp kept accepting
// unauthenticated requests for up to 10s after the first key was created via
// REST. This test drives the real newServer wiring (no admin key configured,
// the affected bootstrap/dev-mode scenario) end to end over HTTP.
func TestNewServerSharesAuthCacheBetweenRESTAndMCP(t *testing.T) {
	t.Setenv("MEMINI_BACKEND", "sqlite")
	t.Setenv("MEMINI_SQLITE_PATH", filepath.Join(t.TempDir(), "memini.db"))
	t.Setenv("MEMINI_EMBED_DIMS", "8")
	t.Setenv("MEMINI_UI_ENABLED", "false")
	t.Setenv("MEMINI_DEFAULT_NAMESPACE", "it-shared-auth-cache")
	// Deliberately blank MEMINI_API_KEY: dev/bootstrap mode is the scenario
	// this bug affects (an admin key configured makes the no-bearer path
	// reject unconditionally regardless of table state, so the cache never
	// matters). Explicitly overridden (not just left unset) because a real
	// admin key is commonly exported in an interactive dev shell (e.g. for
	// the memini MCP plugin) and t.Setenv would otherwise leave it in place.
	t.Setenv("MEMINI_API_KEY", "")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg := prometheus.NewRegistry()
	svc, st, deps, joinWorkers, cleanup, err := buildServiceStack(context.Background(), cfg, log, reg)
	if err != nil {
		t.Fatalf("buildServiceStack: %v", err)
	}
	t.Cleanup(func() { joinWorkers(); cleanup() })

	srv, err := newServer(cfg, svc, st, deps, log, reg)
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	ts := httptest.NewServer(srv.Router())
	t.Cleanup(ts.Close)

	// Dev mode: an unauthenticated MCP request is accepted (never 401) before
	// any key exists.
	if code := postMCP(t, ts.URL, ""); code == http.StatusUnauthorized {
		t.Fatalf("pre-bootstrap /mcp with no bearer: want != 401 (dev mode), got %d", code)
	}

	// Bootstrap: create the first key via REST while auth is still open.
	rec, err := http.Post(ts.URL+"/v1/keys", "application/json", strings.NewReader(`{"name":"first-admin"}`))
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	body, _ := io.ReadAll(rec.Body)
	_ = rec.Body.Close()
	if rec.StatusCode != http.StatusCreated {
		t.Fatalf("create key: want 201, got %d (%s)", rec.StatusCode, body)
	}

	// Immediately afterward (same test, no sleep): an unauthenticated MCP
	// request must now be rejected. This is the exact same immediacy
	// guarantee TestBootstrapFlowEndToEnd (internal/api/rest) proves for REST
	// itself — this test proves it reaches the MCP surface too, which is only
	// true when both surfaces share one apiauth.Config cache.
	if code := postMCP(t, ts.URL, ""); code != http.StatusUnauthorized {
		t.Fatalf("post-bootstrap /mcp with no bearer: want 401 immediately, got %d", code)
	}
}

// postMCP sends a bare POST to /mcp (auth is checked before the request ever
// reaches MCP session/protocol handling, so the body need not be a valid
// JSON-RPC message) and returns the status code.
func postMCP(t *testing.T, baseURL, bearer string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, baseURL+"/mcp", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post /mcp: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	t.Logf("postMCP status=%d body=%s", resp.StatusCode, b)
	return resp.StatusCode
}
