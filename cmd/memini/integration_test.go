//go:build integration

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/eleboucher/memini/internal/config"
	"github.com/eleboucher/memini/internal/embed/embedtest"
	"github.com/eleboucher/memini/internal/logging"
)

// These tests boot the real service stack via buildServiceStack + newServer and
// drive it over HTTP. The embeddings endpoint is faked with the deterministic
// embedtest embedder so recall is repeatable; everything else is production
// wiring. SQLite always runs; Postgres runs when MEMINI_TEST_POSTGRES_DSN is set.

const embedDims = 64

// harness is a booted memini server reachable over HTTP.
type harness struct {
	baseURL   string
	apiKey    string
	nsHeader  string
	namespace string
}

// fakeEmbedServer serves an OpenAI-compatible /v1/embeddings endpoint backed by
// the deterministic test embedder.
func fakeEmbedServer(t *testing.T) *httptest.Server {
	t.Helper()
	fake := embedtest.New(embedDims)
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/embeddings", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input json.RawMessage `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// input is either a JSON string or an array of strings.
		var texts []string
		if err := json.Unmarshal(req.Input, &texts); err != nil {
			var single string
			if err := json.Unmarshal(req.Input, &single); err != nil {
				http.Error(w, "input must be string or []string", http.StatusBadRequest)
				return
			}
			texts = []string{single}
		}
		vecs, err := fake.Embed(r.Context(), texts)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		type datum struct {
			Object    string    `json:"object"`
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		}
		resp := struct {
			Object string  `json:"object"`
			Model  string  `json:"model"`
			Data   []datum `json:"data"`
			Usage  struct {
				PromptTokens int `json:"prompt_tokens"`
				TotalTokens  int `json:"total_tokens"`
			} `json:"usage"`
		}{Object: "list", Model: "fake"}
		for i, v := range vecs {
			resp.Data = append(resp.Data, datum{Object: "embedding", Index: i, Embedding: v})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// startStack boots the full service stack for one backend and returns a harness
// pointing at it. The namespace is per-test so a shared Postgres stays isolated.
func startStack(t *testing.T, backend, dsn string) harness {
	t.Helper()

	embed := fakeEmbedServer(t)
	ns := "it-" + strings.NewReplacer("/", "-", " ", "-").Replace(strings.ToLower(t.Name()))

	t.Setenv("MEMINI_BACKEND", backend)
	switch backend {
	case "sqlite":
		t.Setenv("MEMINI_SQLITE_PATH", filepath.Join(t.TempDir(), "memini.db"))
	case "postgres":
		t.Setenv("MEMINI_POSTGRES_DSN", dsn)
	}
	t.Setenv("MEMINI_EMBED_BASE_URL", embed.URL+"/v1")
	t.Setenv("MEMINI_EMBED_MODEL", "fake")
	t.Setenv("MEMINI_EMBED_DIMS", fmt.Sprint(embedDims))
	t.Setenv("MEMINI_API_KEY", "secret-token")
	t.Setenv("MEMINI_DEFAULT_NAMESPACE", ns)
	t.Setenv("MEMINI_UI_ENABLED", "false")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	log := logging.New("error", "text")

	ctx, cancel := context.WithCancel(context.Background())
	svc, st, joinWorkers, cleanup, err := buildServiceStack(ctx, cfg, log)
	if err != nil {
		cancel()
		t.Fatalf("buildServiceStack: %v", err)
	}

	srv, err := newServer(cfg, svc, st, log)
	if err != nil {
		cancel()
		joinWorkers()
		cleanup()
		t.Fatalf("newServer: %v", err)
	}

	ts := httptest.NewServer(srv.Router())
	t.Cleanup(func() {
		ts.Close()
		cancel()
		joinWorkers()
		cleanup()
	})

	return harness{
		baseURL:   ts.URL,
		apiKey:    cfg.APIKey,
		nsHeader:  cfg.NamespaceHeader,
		namespace: cfg.DefaultNamespace,
	}
}

// forEachBackend runs fn against SQLite, and Postgres when a DSN is configured.
func forEachBackend(t *testing.T, fn func(t *testing.T, h harness)) {
	t.Helper()
	t.Run("sqlite", func(t *testing.T) { fn(t, startStack(t, "sqlite", "")) })

	dsn := os.Getenv("MEMINI_TEST_POSTGRES_DSN")
	t.Run("postgres", func(t *testing.T) {
		if dsn == "" {
			t.Skip("set MEMINI_TEST_POSTGRES_DSN to run the Postgres backend")
		}
		fn(t, startStack(t, "postgres", dsn))
	})
}

// req issues an authenticated HTTP request to the harness and returns the
// status code and (if non-empty) the JSON-decoded body into out.
func (h harness) req(t *testing.T, method, path string, body, out any) int {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		r = strings.NewReader(string(b))
	}
	httpReq, err := http.NewRequest(method, h.baseURL+path, r)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+h.apiKey)
	httpReq.Header.Set(h.nsHeader, h.namespace)
	if body != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil && err != io.EOF {
			t.Fatalf("decode %s %s: %v", method, path, err)
		}
	}
	return resp.StatusCode
}

func TestIntegrationHealth(t *testing.T) {
	forEachBackend(t, func(t *testing.T, h harness) {
		for _, path := range []string{"/healthz", "/readyz"} {
			resp, err := http.Get(h.baseURL + path)
			if err != nil {
				t.Fatalf("GET %s: %v", path, err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Errorf("GET %s: want 200, got %d", path, resp.StatusCode)
			}
		}
	})
}

func TestIntegrationAuth(t *testing.T) {
	forEachBackend(t, func(t *testing.T, h harness) {
		// No bearer token: the API is closed.
		req, _ := http.NewRequest(http.MethodPost, h.baseURL+"/v1/search", strings.NewReader(`{"query":"x"}`))
		req.Header.Set(h.nsHeader, h.namespace)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("search without token: %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("search without token: want 401, got %d", resp.StatusCode)
		}
	})
}

func TestIntegrationRESTRoundTrip(t *testing.T) {
	forEachBackend(t, func(t *testing.T, h harness) {
		// Remember.
		var created struct {
			ID string `json:"id"`
		}
		if code := h.req(t, http.MethodPost, "/v1/memories", map[string]any{
			"content": "kubernetes schedules containers onto nodes", "tier": "semantic",
		}, &created); code != http.StatusCreated {
			t.Fatalf("remember: want 201, got %d", code)
		}
		if created.ID == "" {
			t.Fatal("remember returned no id")
		}

		// Get by id.
		var got struct {
			ID      string `json:"id"`
			Content string `json:"content"`
		}
		if code := h.req(t, http.MethodGet, "/v1/memories/"+created.ID, nil, &got); code != http.StatusOK {
			t.Fatalf("get: want 200, got %d", code)
		}
		if got.Content != "kubernetes schedules containers onto nodes" {
			t.Fatalf("get content = %q", got.Content)
		}

		// Search finds it (real embedder client → fake embeddings → store).
		var sr struct {
			Results []struct {
				Memory struct {
					ID string `json:"id"`
				} `json:"memory"`
			} `json:"results"`
		}
		if code := h.req(t, http.MethodPost, "/v1/search", map[string]any{
			"query": "kubernetes containers", "limit": 5,
		}, &sr); code != http.StatusOK {
			t.Fatalf("search: want 200, got %d", code)
		}
		if len(sr.Results) == 0 || sr.Results[0].Memory.ID != created.ID {
			t.Fatalf("search did not return created memory: %+v", sr.Results)
		}

		// List includes it.
		var lr struct {
			Memories []struct {
				ID string `json:"id"`
			} `json:"memories"`
		}
		if code := h.req(t, http.MethodGet, "/v1/memories", nil, &lr); code != http.StatusOK {
			t.Fatalf("list: want 200, got %d", code)
		}
		if len(lr.Memories) != 1 || lr.Memories[0].ID != created.ID {
			t.Fatalf("list: want the one created memory, got %+v", lr.Memories)
		}

		// Forget, then it is gone.
		if code := h.req(t, http.MethodDelete, "/v1/memories/"+created.ID, nil, nil); code != http.StatusNoContent {
			t.Fatalf("forget: want 204, got %d", code)
		}
		if code := h.req(t, http.MethodGet, "/v1/memories/"+created.ID, nil, nil); code != http.StatusNotFound {
			t.Fatalf("get after forget: want 404, got %d", code)
		}
	})
}

func TestIntegrationMCPRoundTrip(t *testing.T) {
	forEachBackend(t, func(t *testing.T, h harness) {
		cs := h.mcpConnect(t)
		ctx := context.Background()

		var remembered struct {
			ID   string `json:"id"`
			Tier string `json:"tier"`
		}
		res, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{
			Name:      "memory_remember",
			Arguments: map[string]any{"content": "go favours composition over inheritance", "tier": "semantic"},
		})
		if err != nil {
			t.Fatalf("remember tool: %v", err)
		}
		mcpStructured(t, res, &remembered)
		if remembered.ID == "" || remembered.Tier != "semantic" {
			t.Fatalf("unexpected remember result: %+v", remembered)
		}

		var recalled struct {
			Results []struct {
				ID string `json:"id"`
			} `json:"results"`
		}
		res, err = cs.CallTool(ctx, &mcpsdk.CallToolParams{
			Name:      "memory_recall",
			Arguments: map[string]any{"query": "go composition", "limit": 5},
		})
		if err != nil {
			t.Fatalf("recall tool: %v", err)
		}
		mcpStructured(t, res, &recalled)
		if len(recalled.Results) == 0 || recalled.Results[0].ID != remembered.ID {
			t.Fatalf("recall did not return remembered memory: %+v", recalled.Results)
		}
	})
}

// TestIntegrationCrossSurface remembers through MCP and recalls through REST,
// confirming both surfaces share one store and namespace.
func TestIntegrationCrossSurface(t *testing.T) {
	forEachBackend(t, func(t *testing.T, h harness) {
		var remembered struct {
			ID string `json:"id"`
		}
		res, err := h.mcpConnect(t).CallTool(context.Background(), &mcpsdk.CallToolParams{
			Name:      "memory_remember",
			Arguments: map[string]any{"content": "the deploy pipeline runs on forgejo actions", "tier": "procedural"},
		})
		if err != nil {
			t.Fatalf("remember tool: %v", err)
		}
		mcpStructured(t, res, &remembered)

		var sr struct {
			Results []struct {
				Memory struct {
					ID string `json:"id"`
				} `json:"memory"`
			} `json:"results"`
		}
		if code := h.req(t, http.MethodPost, "/v1/search", map[string]any{
			"query": "deploy pipeline forgejo", "limit": 5,
		}, &sr); code != http.StatusOK {
			t.Fatalf("search: want 200, got %d", code)
		}
		if len(sr.Results) == 0 || sr.Results[0].Memory.ID != remembered.ID {
			t.Fatalf("REST recall did not see the MCP-written memory: %+v", sr.Results)
		}
	})
}

// mcpConnect opens an MCP session against the harness's /mcp endpoint over real
// HTTP, injecting the bearer token and namespace header on every request.
func (h harness) mcpConnect(t *testing.T) *mcpsdk.ClientSession {
	t.Helper()
	transport := &mcpsdk.StreamableClientTransport{
		Endpoint: h.baseURL + "/mcp",
		HTTPClient: &http.Client{Transport: headerRoundTripper{
			rt:    http.DefaultTransport,
			key:   h.apiKey,
			nsHdr: h.nsHeader,
			ns:    h.namespace,
		}},
		// Request/response only; no server-initiated notifications needed.
		DisableStandaloneSSE: true,
	}
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "integration", Version: "0"}, nil)
	cs, err := client.Connect(context.Background(), transport, nil)
	if err != nil {
		t.Fatalf("mcp connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// headerRoundTripper injects auth and namespace headers onto every MCP request.
type headerRoundTripper struct {
	rt        http.RoundTripper
	key       string
	nsHdr, ns string
}

func (h headerRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+h.key)
	r.Header.Set(h.nsHdr, h.ns)
	return h.rt.RoundTrip(r)
}

func mcpStructured(t *testing.T, res *mcpsdk.CallToolResult, v any) {
	t.Helper()
	if res.IsError {
		t.Fatalf("tool returned error: %+v", res.Content)
	}
	b, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured: %v", err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("unmarshal structured: %v", err)
	}
}
