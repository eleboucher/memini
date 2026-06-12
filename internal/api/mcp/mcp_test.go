package mcp_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	meminimcp "github.com/eleboucher/memini/internal/api/mcp"
	"github.com/eleboucher/memini/internal/embed/embedtest"
	"github.com/eleboucher/memini/internal/service"
	"github.com/eleboucher/memini/internal/store/sqlitevec"
)

const dims = 64

func connect(t *testing.T) *mcpsdk.ClientSession {
	t.Helper()
	st, err := sqlitevec.Open(context.Background(), filepath.Join(t.TempDir(), "mcp.db"), dims)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	svc := service.New(st, embedtest.New(dims))

	srv := meminimcp.NewServer(svc, "default")
	clientT, serverT := mcpsdk.NewInMemoryTransports()

	ctx := context.Background()
	if _, err := srv.Connect(ctx, serverT, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func structured(t *testing.T, res *mcpsdk.CallToolResult, v any) {
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

func TestToolsListed(t *testing.T) {
	cs := connect(t)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	want := map[string]bool{
		"memory_remember": false, "memory_recall": false, "memory_get": false,
		"memory_forget": false, "memory_briefing": false,
	}
	for _, tool := range res.Tools {
		if _, ok := want[tool.Name]; ok {
			want[tool.Name] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("tool %q not advertised", name)
		}
	}
}

func TestRememberRecallRoundTrip(t *testing.T) {
	cs := connect(t)
	ctx := context.Background()

	res, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      "memory_remember",
		Arguments: map[string]any{"content": "kubernetes schedules pods onto nodes", "tier": "semantic"},
	})
	if err != nil {
		t.Fatalf("remember: %v", err)
	}
	var remembered struct {
		ID   string `json:"id"`
		Tier string `json:"tier"`
	}
	structured(t, res, &remembered)
	if remembered.ID == "" || remembered.Tier != "semantic" {
		t.Fatalf("unexpected remember result: %+v", remembered)
	}

	res, err = cs.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      "memory_recall",
		Arguments: map[string]any{"query": "kubernetes pods", "limit": 5},
	})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	var recalled struct {
		Results []struct {
			ID    string  `json:"id"`
			Score float64 `json:"score"`
		} `json:"results"`
	}
	structured(t, res, &recalled)
	if len(recalled.Results) == 0 || recalled.Results[0].ID != remembered.ID {
		t.Fatalf("recall did not return remembered memory: %+v", recalled.Results)
	}
}

func TestBriefingTool(t *testing.T) {
	cs := connect(t)
	ctx := context.Background()

	remember := func(content, tier string, tags []string) {
		args := map[string]any{"content": content, "tier": tier}
		if tags != nil {
			args["tags"] = tags
		}
		if _, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{Name: "memory_remember", Arguments: args}); err != nil {
			t.Fatalf("remember: %v", err)
		}
	}
	remember("the service is written in Go", "semantic", nil)
	remember("to deploy run make release", "procedural", nil)
	remember("the user is Erwan", "semantic", []string{"pinned"})

	res, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{Name: "memory_briefing", Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("briefing: %v", err)
	}
	var b struct {
		Namespace  string                     `json:"namespace"`
		Facts      []struct{ Content string } `json:"facts"`
		Procedures []struct{ Content string } `json:"procedures"`
		Pinned     []struct{ Content string } `json:"pinned"`
	}
	structured(t, res, &b)
	if b.Namespace != "default" || len(b.Facts) < 1 || len(b.Procedures) != 1 || len(b.Pinned) != 1 {
		t.Fatalf("unexpected briefing: %+v", b)
	}
}

func TestRecallSubtreeScopeViaMCP(t *testing.T) {
	cs := connect(t)
	ctx := context.Background()

	remember := func(ns, content string) {
		if _, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{
			Name:      "memory_remember",
			Arguments: map[string]any{"content": content, "tier": "semantic", "namespace": ns},
		}); err != nil {
			t.Fatalf("remember %s: %v", ns, err)
		}
	}
	remember("proj", "shared: the service is written in Go")
	remember("proj/agent-a", "private: agent-a prefers table tests in Go")

	res, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      "memory_recall",
		Arguments: map[string]any{"query": "Go", "namespace": "proj", "scope": "subtree", "limit": 10},
	})
	if err != nil {
		t.Fatalf("recall subtree: %v", err)
	}
	var recalled struct {
		Results []struct{ Content string } `json:"results"`
	}
	structured(t, res, &recalled)
	if len(recalled.Results) < 2 {
		t.Fatalf("subtree recall should span proj and proj/agent-a, got %d results", len(recalled.Results))
	}
}

func TestHTTPHandlerAuth(t *testing.T) {
	ctx := context.Background()
	st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "auth.db"), dims)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	h := meminimcp.HTTPHandler(service.New(st, embedtest.New(dims)), "X-Memini-Namespace", "default", "secret")

	const body = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`
	req := func(token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Accept", "application/json, text/event-stream")
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}

	if got := req("").Code; got != http.StatusUnauthorized {
		t.Errorf("no token: got %d, want 401", got)
	}
	if got := req("wrong").Code; got != http.StatusUnauthorized {
		t.Errorf("bad token: got %d, want 401", got)
	}
	if got := req("secret").Code; got == http.StatusUnauthorized {
		t.Errorf("good token: got 401, want it to pass auth")
	}
}

func TestRememberFullArgsRoundTripViaGet(t *testing.T) {
	cs := connect(t)
	ctx := context.Background()

	res, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{
		Name: "memory_remember",
		Arguments: map[string]any{
			"content":     "the deploy pipeline runs on forgejo",
			"tier":        "semantic",
			"summary":     "deploy pipeline location",
			"tags":        []string{"ci", "deploy"},
			"metadata":    map[string]any{"source": "test"},
			"importance":  0.8,
			"ttl_seconds": -1,
			"id":          "fixed-id-1",
		},
	})
	if err != nil {
		t.Fatalf("remember: %v", err)
	}
	var remembered struct {
		ID string `json:"id"`
	}
	structured(t, res, &remembered)
	if remembered.ID != "fixed-id-1" {
		t.Fatalf("id = %q, want the upsert id", remembered.ID)
	}

	res, err = cs.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      "memory_get",
		Arguments: map[string]any{"id": "fixed-id-1"},
	})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	var got struct {
		Content    string         `json:"content"`
		Summary    string         `json:"summary"`
		Tags       []string       `json:"tags"`
		Metadata   map[string]any `json:"metadata"`
		Importance float64        `json:"importance"`
		CreatedAt  string         `json:"created_at"`
		ExpiresAt  string         `json:"expires_at"`
	}
	structured(t, res, &got)
	if got.Summary != "deploy pipeline location" || len(got.Tags) != 2 ||
		got.Metadata["source"] != "test" || got.Importance != 0.8 || got.CreatedAt == "" {
		t.Fatalf("get dropped fields: %+v", got)
	}
	if got.ExpiresAt != "" {
		t.Fatalf("negative ttl should mean no expiry, got %q", got.ExpiresAt)
	}
}

func TestRecallTierFilter(t *testing.T) {
	cs := connect(t)
	ctx := context.Background()

	for _, m := range []map[string]any{
		{"content": "fact about kubernetes nodes", "tier": "semantic"},
		{"content": "note about kubernetes nodes", "tier": "working"},
	} {
		if _, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{Name: "memory_remember", Arguments: m}); err != nil {
			t.Fatalf("remember: %v", err)
		}
	}

	res, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      "memory_recall",
		Arguments: map[string]any{"query": "kubernetes nodes", "tiers": []string{"semantic"}},
	})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	var recalled struct {
		Results []struct {
			Tier string `json:"tier"`
		} `json:"results"`
	}
	structured(t, res, &recalled)
	if len(recalled.Results) == 0 {
		t.Fatal("tier-filtered recall returned nothing")
	}
	for _, r := range recalled.Results {
		if r.Tier != "semantic" {
			t.Fatalf("tier filter leaked %q results: %+v", r.Tier, recalled.Results)
		}
	}

	res, err = cs.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      "memory_recall",
		Arguments: map[string]any{"query": "kubernetes nodes", "tiers": []string{"semantik"}},
	})
	if err != nil {
		t.Fatalf("recall transport: %v", err)
	}
	if !res.IsError {
		t.Fatal("unknown tier should be a tool error, not silently unfiltered")
	}
}

func TestInvalidNamespaceIsRejected(t *testing.T) {
	cs := connect(t)
	res, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name:      "memory_remember",
		Arguments: map[string]any{"content": "x", "namespace": strings.Repeat("n", 300)},
	})
	if err != nil {
		t.Fatalf("transport: %v", err)
	}
	if !res.IsError {
		t.Fatal("over-long namespace must error, never fall back to the default tenant")
	}
}

// TestHTTPHandlerNamespaceHeader pins that an invalid X-Memini-Namespace is
// rejected with 400 on the HTTP surface (matching REST) — with and without an
// API key configured. The keyless case matters most: the pre-fix code returned
// the inner handler directly when no key was set, silently routing invalid
// namespaces to the default tenant.
func TestHTTPHandlerNamespaceHeader(t *testing.T) {
	ctx := context.Background()
	st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "ns.db"), dims)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	svc := service.New(st, embedtest.New(dims))

	const body = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`
	req := func(h http.Handler, ns, token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Accept", "application/json, text/event-stream")
		if ns != "" {
			r.Header.Set("X-Memini-Namespace", ns)
		}
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	badNS := strings.Repeat("n", 300)

	t.Run("no api key", func(t *testing.T) {
		h := meminimcp.HTTPHandler(svc, "X-Memini-Namespace", "default", "")
		if got := req(h, badNS, "").Code; got != http.StatusBadRequest {
			t.Errorf("invalid namespace without auth: got %d, want 400", got)
		}
		if got := req(h, "tenant-a", "").Code; got == http.StatusBadRequest {
			t.Errorf("valid namespace: got 400, want it to pass")
		}
	})

	t.Run("with api key", func(t *testing.T) {
		h := meminimcp.HTTPHandler(svc, "X-Memini-Namespace", "default", "secret")
		if got := req(h, badNS, "secret").Code; got != http.StatusBadRequest {
			t.Errorf("invalid namespace with valid token: got %d, want 400", got)
		}
		// Auth still runs first: bad token beats bad namespace.
		if got := req(h, badNS, "wrong").Code; got != http.StatusUnauthorized {
			t.Errorf("bad token + bad namespace: got %d, want 401", got)
		}
	})
}

// TestRememberPositiveTTL pins the seconds→duration conversion: a positive
// ttl_seconds must produce an expiry (the full-args test only covers the
// never-expire case).
func TestRememberPositiveTTL(t *testing.T) {
	cs := connect(t)
	ctx := context.Background()

	res, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{
		Name: "memory_remember",
		Arguments: map[string]any{
			"content": "short lived note", "tier": "semantic", "ttl_seconds": 3600, "id": "ttl-1",
		},
	})
	if err != nil {
		t.Fatalf("remember: %v", err)
	}
	if res.IsError {
		t.Fatalf("remember errored: %+v", res.Content)
	}

	res, err = cs.CallTool(ctx, &mcpsdk.CallToolParams{
		Name: "memory_get", Arguments: map[string]any{"id": "ttl-1"},
	})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	var got struct {
		CreatedAt string `json:"created_at"`
		ExpiresAt string `json:"expires_at"`
	}
	structured(t, res, &got)
	if got.ExpiresAt == "" {
		t.Fatal("positive ttl_seconds must set an expiry")
	}
	created, err1 := time.Parse(time.RFC3339, got.CreatedAt)
	expires, err2 := time.Parse(time.RFC3339, got.ExpiresAt)
	if err1 != nil || err2 != nil {
		t.Fatalf("parse timestamps: %v / %v", err1, err2)
	}
	if d := expires.Sub(created); d != time.Hour {
		t.Fatalf("ttl_seconds=3600 produced expiry %v after creation, want 1h", d)
	}
}
