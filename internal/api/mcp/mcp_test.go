package mcp_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

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
		"memory_remember": false, "memory_recall": false, "memory_get": false, "memory_forget": false,
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
