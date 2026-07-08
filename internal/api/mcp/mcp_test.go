package mcp_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	meminimcp "github.com/eleboucher/memini/internal/api/mcp"
	"github.com/eleboucher/memini/internal/embed/embedtest"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/service"
	"github.com/eleboucher/memini/internal/store"
	"github.com/eleboucher/memini/internal/store/sqlitevec"
)

const dims = 64

// fakeAnswerer is a stand-in llm.Completer: it returns a canned answer without
// making any real call, so tests can exercise the memory_answer tool without a
// configured LLM backend.
type fakeAnswerer struct{ resp string }

func (f *fakeAnswerer) Complete(_ context.Context, _, _ string) (string, error) {
	return f.resp, nil
}

func connect(t *testing.T) *mcpsdk.ClientSession {
	t.Helper()
	return connectWithOptions(t)
}

// connectWithOptions builds an MCP server over a service.Service configured
// with opts (e.g. service.WithAnswerer), so tests can exercise
// answerer-gated behavior like the conditional memory_answer registration.
func connectWithOptions(t *testing.T, opts ...service.Option) *mcpsdk.ClientSession {
	t.Helper()
	st, err := sqlitevec.Open(context.Background(), filepath.Join(t.TempDir(), "mcp.db"), dims)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	svc := service.New(st, embedtest.New(dims), opts...)

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
		"memory_forget": false, "memory_briefing": false, "memory_list": false,
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

func TestBriefingToolPerSectionCaps(t *testing.T) {
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
	// 5 of each durable tier + 3 pinned semantic — enough to verify that
	// per_section_X can shrink one section without touching the others.
	for i := range 5 {
		remember(fmt.Sprintf("sem-u-%d", i), "semantic", nil)
	}
	for i := range 5 {
		remember(fmt.Sprintf("proc-u-%d", i), "procedural", nil)
	}
	for i := range 3 {
		remember(fmt.Sprintf("sem-p-%d", i), "semantic", []string{"pinned"})
	}

	type b struct {
		Namespace  string                     `json:"namespace"`
		Facts      []struct{ Content string } `json:"facts"`
		Procedures []struct{ Content string } `json:"procedures"`
		Pinned     []struct{ Content string } `json:"pinned"`
	}
	call := func(args map[string]any) b {
		res, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{Name: "memory_briefing", Arguments: args})
		if err != nil {
			t.Fatalf("briefing: %v", err)
		}
		var out b
		structured(t, res, &out)
		return out
	}

	// per_section acts as the default; per_section_pinned shrinks just pinned.
	got := call(map[string]any{"per_section": 5, "per_section_pinned": 2})
	if len(got.Facts) != 5 || len(got.Procedures) != 5 || len(got.Pinned) != 2 {
		t.Fatalf("per_section_pinned=2 should cap pinned at 2 while keeping facts/procs at 5, got facts=%d procs=%d pinned=%d",
			len(got.Facts), len(got.Procedures), len(got.Pinned))
	}

	// per_section_procedures shrinks just that section while keeping the
	// others at the per_section default.
	got = call(map[string]any{"per_section": 5, "per_section_procedures": 2})
	if len(got.Procedures) != 2 || len(got.Facts) != 5 || len(got.Pinned) != 3 {
		t.Fatalf("per_section_procedures=2 should cap procs at 2 while keeping facts=5 pinned=3, got facts=%d procs=%d pinned=%d",
			len(got.Facts), len(got.Procedures), len(got.Pinned))
	}
}

// TestBriefingZeroDisablesSection verifies that per_section_recent=0
// explicitly disables the recent section over MCP, and that omitting it
// falls back to the default (recent included). Before the *int change, 0
// was indistinguishable from "unset" and could not disable a section.
func TestBriefingZeroDisablesSection(t *testing.T) {
	cs := connect(t)
	ctx := context.Background()

	if _, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      "memory_remember",
		Arguments: map[string]any{"content": "deployed the new build", "tier": "episodic"},
	}); err != nil {
		t.Fatalf("remember: %v", err)
	}

	type b struct {
		Recent []struct{ Content string } `json:"recent"`
	}
	call := func(args map[string]any) b {
		res, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{Name: "memory_briefing", Arguments: args})
		if err != nil {
			t.Fatalf("briefing: %v", err)
		}
		var out b
		structured(t, res, &out)
		return out
	}

	// per_section_recent unset: the recent episodic memory shows up.
	got := call(map[string]any{})
	if len(got.Recent) != 1 {
		t.Fatalf("recent should include the seeded episodic memory when unset, got %+v", got.Recent)
	}

	// per_section_recent=0: the recent section is disabled entirely.
	got = call(map[string]any{"per_section_recent": 0})
	if len(got.Recent) != 0 {
		t.Fatalf("per_section_recent=0 should disable the recent section, got %+v", got.Recent)
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

// errEmbedder fails every embed call, forcing recall onto the keyword-only
// fallback path (with a recall embed budget configured).
type errEmbedder struct{ dims int }

func (e errEmbedder) Embed(context.Context, []string) ([][]float32, error) {
	return nil, fmt.Errorf("embed boom")
}
func (e errEmbedder) Dims() int { return e.dims }

// TestRecallDegradedSurfacedViaMCP confirms that when recall falls back to
// keyword-only search (query embed erroring), the memory_recall structured
// result carries degraded="keyword_only" plus a human-readable note, so an
// agent consuming the tool knows the results may be incomplete. A healthy
// embedder must leave both fields absent (omitempty).
func TestRecallDegradedSurfacedViaMCP(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "degraded.db")

	st, err := sqlitevec.Open(ctx, dbPath, dims)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	// Seed with a healthy embedder so the memory exists for keyword search to find.
	seed := service.New(st, embedtest.New(dims))
	if _, err := seed.Remember(ctx, service.RememberInput{Namespace: "default", Content: "hello world", Tier: "semantic"}); err != nil {
		t.Fatalf("seed remember: %v", err)
	}

	connectDegraded := func(t *testing.T) *mcpsdk.ClientSession {
		t.Helper()
		svc := service.New(st, errEmbedder{dims: dims}, service.WithRecallEmbedTimeout(time.Second))
		srv := meminimcp.NewServer(svc, "default")
		clientT, serverT := mcpsdk.NewInMemoryTransports()
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

	t.Run("degraded", func(t *testing.T) {
		cs := connectDegraded(t)
		res, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{
			Name:      "memory_recall",
			Arguments: map[string]any{"query": "hello", "limit": 5},
		})
		if err != nil {
			t.Fatalf("recall: %v", err)
		}
		var out struct {
			Degraded string `json:"degraded"`
			Note     string `json:"note"`
		}
		structured(t, res, &out)
		if out.Degraded != "keyword_only" {
			t.Fatalf("degraded = %q, want %q", out.Degraded, "keyword_only")
		}
		if out.Note == "" {
			t.Fatal("note should explain the degradation, got empty")
		}
	})

	t.Run("healthy", func(t *testing.T) {
		cs := connect(t)
		ctx := context.Background()
		if _, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{
			Name:      "memory_remember",
			Arguments: map[string]any{"content": "hello world", "tier": "semantic"},
		}); err != nil {
			t.Fatalf("remember: %v", err)
		}
		res, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{
			Name:      "memory_recall",
			Arguments: map[string]any{"query": "hello", "limit": 5},
		})
		if err != nil {
			t.Fatalf("recall: %v", err)
		}
		if _, ok := res.StructuredContent.(map[string]any)["degraded"]; ok {
			t.Fatalf("degraded key should be omitted on healthy recall, got %+v", res.StructuredContent)
		}
		if _, ok := res.StructuredContent.(map[string]any)["note"]; ok {
			t.Fatalf("note key should be omitted on healthy recall, got %+v", res.StructuredContent)
		}
	})
}

// TestRememberDegradedSurfacedViaMCP confirms that when a write embed budget
// is set and the embedder is down, memory_remember stores the memory
// keyword-searchable only (no vector) and the structured result carries
// degraded="pending_embed" plus a human-readable note, so an agent consuming
// the tool knows the memory will need a vector backfill. A healthy embedder
// must leave both fields absent (omitempty).
func TestRememberDegradedSurfacedViaMCP(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "remember-degraded.db")

	st, err := sqlitevec.Open(ctx, dbPath, dims)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	svc := service.New(st, errEmbedder{dims: dims}, service.WithWriteEmbedTimeout(time.Second))
	srv := meminimcp.NewServer(svc, "default")
	clientT, serverT := mcpsdk.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, serverT, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })

	res, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      "memory_remember",
		Arguments: map[string]any{"content": "hello world", "tier": "semantic"},
	})
	if err != nil {
		t.Fatalf("remember: %v", err)
	}
	var out struct {
		Stored   bool   `json:"stored"`
		Degraded string `json:"degraded"`
		Note     string `json:"note"`
	}
	structured(t, res, &out)
	if !out.Stored {
		t.Fatal("stored = false, want true (degraded write is still stored)")
	}
	if out.Degraded != "pending_embed" {
		t.Fatalf("degraded = %q, want %q", out.Degraded, "pending_embed")
	}
	if out.Note == "" {
		t.Fatal("note should explain the degradation, got empty")
	}

	t.Run("healthy", func(t *testing.T) {
		cs := connect(t)
		res, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{
			Name:      "memory_remember",
			Arguments: map[string]any{"content": "hello healthy world", "tier": "semantic"},
		})
		if err != nil {
			t.Fatalf("remember: %v", err)
		}
		if _, ok := res.StructuredContent.(map[string]any)["degraded"]; ok {
			t.Fatalf("degraded key should be omitted on a healthy write, got %+v", res.StructuredContent)
		}
		if _, ok := res.StructuredContent.(map[string]any)["note"]; ok {
			t.Fatalf("note key should be omitted on a healthy write, got %+v", res.StructuredContent)
		}
	})
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

// TestUpdateToolContentOnly pins memory_update's partial-update semantics:
// passing only content leaves tags, tier, and metadata untouched while the
// content itself changes.
func TestUpdateToolContentOnly(t *testing.T) {
	cs := connect(t)
	ctx := context.Background()

	res, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{
		Name: "memory_remember",
		Arguments: map[string]any{
			"content":  "the deploy pipeline runs on forgejo",
			"tier":     "semantic",
			"tags":     []string{"ci", "deploy"},
			"metadata": map[string]any{"source": "test"},
			"id":       "update-content-1",
		},
	})
	if err != nil {
		t.Fatalf("remember: %v", err)
	}
	structured(t, res, &struct{}{})

	res, err = cs.CallTool(ctx, &mcpsdk.CallToolParams{
		Name: "memory_update",
		Arguments: map[string]any{
			"id":      "update-content-1",
			"content": "the deploy pipeline runs on gitea actions now",
		},
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	var updated struct {
		ID       string         `json:"id"`
		Content  string         `json:"content"`
		Tier     string         `json:"tier"`
		Tags     []string       `json:"tags"`
		Metadata map[string]any `json:"metadata"`
	}
	structured(t, res, &updated)
	if updated.Content != "the deploy pipeline runs on gitea actions now" {
		t.Fatalf("content = %q, want the updated content", updated.Content)
	}
	if updated.Tier != "semantic" {
		t.Fatalf("tier = %q, want preserved semantic", updated.Tier)
	}
	if len(updated.Tags) != 2 {
		t.Fatalf("tags = %+v, want preserved [ci deploy]", updated.Tags)
	}
	if updated.Metadata["source"] != "test" {
		t.Fatalf("metadata = %+v, want preserved source=test", updated.Metadata)
	}

	// The change must persist, not just be reflected in the call's return value.
	res, err = cs.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      "memory_get",
		Arguments: map[string]any{"id": "update-content-1"},
	})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	var got struct {
		Content string `json:"content"`
	}
	structured(t, res, &got)
	if got.Content != "the deploy pipeline runs on gitea actions now" {
		t.Fatalf("persisted content = %q, want the updated content", got.Content)
	}
}

// TestUpdateToolTagsOnly pins that updating only tags leaves content
// untouched.
func TestUpdateToolTagsOnly(t *testing.T) {
	cs := connect(t)
	ctx := context.Background()

	res, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{
		Name: "memory_remember",
		Arguments: map[string]any{
			"content": "the deploy pipeline runs on forgejo",
			"tags":    []string{"ci"},
			"id":      "update-tags-1",
		},
	})
	if err != nil {
		t.Fatalf("remember: %v", err)
	}
	structured(t, res, &struct{}{})

	res, err = cs.CallTool(ctx, &mcpsdk.CallToolParams{
		Name: "memory_update",
		Arguments: map[string]any{
			"id":   "update-tags-1",
			"tags": []string{"ci", "deploy", "forgejo"},
		},
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	var updated struct {
		Content string   `json:"content"`
		Tags    []string `json:"tags"`
	}
	structured(t, res, &updated)
	if updated.Content != "the deploy pipeline runs on forgejo" {
		t.Fatalf("content = %q, want preserved", updated.Content)
	}
	if len(updated.Tags) != 3 {
		t.Fatalf("tags = %+v, want the replacement set of 3", updated.Tags)
	}
}

// TestUpdateToolValueGatedResult pins that memory_update survives the
// episodic value gate: service.Remember returns (nil, nil) when it drops a
// low-signal episodic write, and the handler must turn that into a tool
// error — not a nil dereference that kills the whole MCP server — leaving
// the original memory untouched.
func TestUpdateToolValueGatedResult(t *testing.T) {
	cs := connectWithOptions(t, service.WithEpisodicMinChars(120))
	ctx := context.Background()

	longContent := strings.Repeat("the deploy pipeline failed on the flaky integration step again ", 3)
	res, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{
		Name: "memory_remember",
		Arguments: map[string]any{
			"content": longContent,
			"tier":    "episodic",
			"id":      "update-gated-1",
		},
	})
	if err != nil {
		t.Fatalf("remember: %v", err)
	}
	var remembered struct {
		Stored bool `json:"stored"`
	}
	structured(t, res, &remembered)
	if !remembered.Stored {
		t.Fatal("setup write was value-gated; content must clear the 120-char gate")
	}

	res, err = cs.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      "memory_update",
		Arguments: map[string]any{"id": "update-gated-1", "content": "x"},
	})
	if err != nil {
		t.Fatalf("update transport: %v", err)
	}
	if !res.IsError {
		t.Fatal("a value-gated update must be a tool error, not success or a panic")
	}
	if len(res.Content) == 0 {
		t.Fatal("error result has no content")
	}
	tc, ok := res.Content[0].(*mcpsdk.TextContent)
	if !ok {
		t.Fatalf("error content = %T, want *mcpsdk.TextContent", res.Content[0])
	}
	if !strings.Contains(tc.Text, "value gate") {
		t.Fatalf("error text = %q, want it to mention the value gate", tc.Text)
	}

	// The server must survive and the original memory must be unchanged.
	res, err = cs.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      "memory_get",
		Arguments: map[string]any{"id": "update-gated-1"},
	})
	if err != nil {
		t.Fatalf("get after gated update: %v", err)
	}
	var got struct {
		Content string `json:"content"`
	}
	structured(t, res, &got)
	if got.Content != longContent {
		t.Fatalf("original content changed after a gated update: %q", got.Content)
	}
}

// TestUpdateToolMissingID pins that updating an unknown id fails with an
// error that points the caller at memory_recall/memory_list to find a valid
// id, rather than a bare not-found error.
func TestUpdateToolMissingID(t *testing.T) {
	cs := connect(t)
	res, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name:      "memory_update",
		Arguments: map[string]any{"id": "does-not-exist", "content": "x"},
	})
	if err != nil {
		t.Fatalf("update transport: %v", err)
	}
	if !res.IsError {
		t.Fatal("updating a missing id must be a tool error")
	}
	if len(res.Content) == 0 {
		t.Fatal("error result has no content")
	}
	tc, ok := res.Content[0].(*mcpsdk.TextContent)
	if !ok {
		t.Fatalf("error content = %T, want *mcpsdk.TextContent", res.Content[0])
	}
	if !strings.Contains(tc.Text, "memory_recall") {
		t.Fatalf("error text = %q, want it to mention memory_recall", tc.Text)
	}
}

// errGetStore wraps a store.Store and makes every Get call fail with a
// transient, non-ErrNotFound error, so tests can distinguish "the store is
// having trouble" from "this id doesn't exist".
type errGetStore struct {
	store.Store
}

func (errGetStore) Get(context.Context, string, string) (*memory.Memory, error) {
	return nil, errors.New("connection reset by peer")
}

// TestUpdateToolTransientStoreErrorNotMisreportedAsNotFound pins that
// memory_update surfaces a transient (non-ErrNotFound) store error as-is: it
// must not wrap it in the "no memory ... " not-found guidance, which would
// falsely tell the caller the id doesn't exist when the store is merely
// unavailable.
func TestUpdateToolTransientStoreErrorNotMisreportedAsNotFound(t *testing.T) {
	base, err := sqlitevec.Open(context.Background(), filepath.Join(t.TempDir(), "mcp-errget.db"), dims)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = base.Close() })
	svc := service.New(errGetStore{Store: base}, embedtest.New(dims))

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

	res, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      "memory_update",
		Arguments: map[string]any{"id": "whatever", "content": "x"},
	})
	if err != nil {
		t.Fatalf("update transport: %v", err)
	}
	if !res.IsError {
		t.Fatal("update over a broken store must be a tool error")
	}
	if len(res.Content) == 0 {
		t.Fatal("error result has no content")
	}
	tc, ok := res.Content[0].(*mcpsdk.TextContent)
	if !ok {
		t.Fatalf("error content = %T, want *mcpsdk.TextContent", res.Content[0])
	}
	if strings.Contains(tc.Text, "no memory") {
		t.Fatalf("error text = %q, must not claim the id doesn't exist for a transient store error", tc.Text)
	}
	if !strings.Contains(tc.Text, "connection reset by peer") {
		t.Fatalf("error text = %q, want the underlying store error preserved", tc.Text)
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

// TestMemoryAnswerToolConditional pins that memory_answer is only advertised
// when the service has an LLM answerer configured: headless deployments (no
// answerer) don't list a tool that would only ever error at call time, and
// answerer-backed deployments still get it, with its read-only annotation
// intact.
func TestMemoryAnswerToolConditional(t *testing.T) {
	t.Run("no answerer", func(t *testing.T) {
		tools := listTools(t)
		if _, ok := tools["memory_answer"]; ok {
			t.Fatal("memory_answer must not be listed when the service has no answerer configured")
		}
	})
	t.Run("with answerer", func(t *testing.T) {
		tools := listTools(t, service.WithAnswerer(&fakeAnswerer{resp: "n/a"}))
		tool, ok := tools["memory_answer"]
		if !ok {
			t.Fatal("memory_answer must be listed when the service has an answerer configured")
		}
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Fatalf("memory_answer readOnlyHint: got %+v, want true", tool.Annotations)
		}
	})
}

// TestAnswerToolValidatesTiers pins that memory_answer exposes and validates the
// same tiers filter as recall/list (parity with the service AnswerInput and the
// REST /v1/answer surface). An unknown tier is rejected before the answerer is
// consulted, but memory_answer is only registered when an LLM is configured,
// so this test still needs a (fake) answerer to reach the tool at all.
func TestAnswerToolValidatesTiers(t *testing.T) {
	cs := connectWithOptions(t, service.WithAnswerer(&fakeAnswerer{resp: "n/a"}))
	res, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name:      "memory_answer",
		Arguments: map[string]any{"query": "anything", "tiers": []string{"semantik"}},
	})
	if err != nil {
		t.Fatalf("answer transport: %v", err)
	}
	if !res.IsError {
		t.Fatal("memory_answer must reject an unknown tier, not silently ignore the filter")
	}
}

func TestListToolFilters(t *testing.T) {
	cs := connect(t)
	ctx := context.Background()

	for _, m := range []map[string]any{
		{"content": "fixed the auth race condition", "tier": "semantic",
			"tags": []string{"bug", "auth"}, "metadata": map[string]any{"category": "bug_fixes"}},
		{"content": "tuned the auth handler latency", "tier": "semantic",
			"tags": []string{"perf", "auth"}, "metadata": map[string]any{"category": "performance_findings"}},
		{"content": "a transient scratch note", "tier": "working"},
	} {
		if _, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{Name: "memory_remember", Arguments: m}); err != nil {
			t.Fatalf("remember: %v", err)
		}
	}

	var listed struct {
		Memories []struct {
			Content string `json:"content"`
			Tier    string `json:"tier"`
		} `json:"memories"`
	}

	// Query-less browse by tier.
	res, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      "memory_list",
		Arguments: map[string]any{"tiers": []string{"working"}},
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	structured(t, res, &listed)
	if len(listed.Memories) != 1 || listed.Memories[0].Tier != "working" {
		t.Fatalf("tier browse: want 1 working memory, got %+v", listed.Memories)
	}

	// Browse by metadata category.
	res, err = cs.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      "memory_list",
		Arguments: map[string]any{"metadata": map[string]any{"category": "bug_fixes"}},
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	structured(t, res, &listed)
	if len(listed.Memories) != 1 || listed.Memories[0].Content != "fixed the auth race condition" {
		t.Fatalf("category browse: want only the bug_fix, got %+v", listed.Memories)
	}

	// Tags are ANDed.
	res, err = cs.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      "memory_list",
		Arguments: map[string]any{"tags": []string{"auth", "perf"}},
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	structured(t, res, &listed)
	if len(listed.Memories) != 1 || listed.Memories[0].Content != "tuned the auth handler latency" {
		t.Fatalf("tag AND browse: want only the perf memory, got %+v", listed.Memories)
	}
}

// TestListToolDefaultLimitAndOffset verifies memory_list caps at 20 results
// by default (an unbounded list is a context blowout for LLM callers) and
// that offset pages past the first page.
func TestListToolDefaultLimitAndOffset(t *testing.T) {
	cs := connect(t)
	ctx := context.Background()

	for i := range 25 {
		if _, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{
			Name:      "memory_remember",
			Arguments: map[string]any{"content": fmt.Sprintf("memo-%02d", i), "tier": "semantic"},
		}); err != nil {
			t.Fatalf("remember: %v", err)
		}
	}

	var listed struct {
		Memories []struct {
			Content string `json:"content"`
		} `json:"memories"`
	}

	// No limit given: capped at the default of 20, not all 25.
	res, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      "memory_list",
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	structured(t, res, &listed)
	if len(listed.Memories) != 20 {
		t.Fatalf("default list should cap at 20, got %d", len(listed.Memories))
	}

	// offset=20 pages past the first 20, returning the remaining 5.
	res, err = cs.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      "memory_list",
		Arguments: map[string]any{"offset": 20},
	})
	if err != nil {
		t.Fatalf("list offset: %v", err)
	}
	structured(t, res, &listed)
	if len(listed.Memories) != 5 {
		t.Fatalf("offset=20 should return the remaining 5, got %d", len(listed.Memories))
	}

	// A negative offset must not defeat the default cap: without clamping,
	// limit+offset would be <= 0, which the service treats as "no limit"
	// and all 25 memories would come back.
	res, err = cs.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      "memory_list",
		Arguments: map[string]any{"offset": -20},
	})
	if err != nil {
		t.Fatalf("list negative offset: %v", err)
	}
	structured(t, res, &listed)
	if len(listed.Memories) != 20 {
		t.Fatalf("offset=-20 should be clamped and keep the default cap of 20, got %d", len(listed.Memories))
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

func listTools(t *testing.T, opts ...service.Option) map[string]*mcpsdk.Tool {
	t.Helper()
	cs := connectWithOptions(t, opts...)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	tools := make(map[string]*mcpsdk.Tool)
	for _, tool := range res.Tools {
		tools[tool.Name] = tool
	}
	return tools
}

// TestToolDescriptions pins that each tool's Description carries the
// load-bearing usage-protocol phrases ported from plugin/skills/{recall,
// remember}/SKILL.md, so bare-MCP clients (no plugin) get the same
// when-to-call / when-not / result-handling guidance as plugin clients.
func TestToolDescriptions(t *testing.T) {
	tools := listTools(t, service.WithAnswerer(&fakeAnswerer{resp: "n/a"}))
	want := map[string][]string{
		"memory_remember": {"atomic", "merge_hint", "proactively", "CLAUDE.md", "stored=false"},
		"memory_recall":   {"created_at", "Empty results", "degraded", "BEFORE starting work"},
		"memory_briefing": {"session start"},
		"memory_answer":   {"memory_recall"},
		"memory_list":     {"offset"},
		"memory_get":      {"memory_recall"},
		"memory_forget":   {"delete", "memory_update"},
		"memory_update":   {"partial", "memory_forget"},
	}
	for name, phrases := range want {
		tool := tools[name]
		if tool == nil {
			t.Fatalf("%s: tool not found", name)
		}
		for _, phrase := range phrases {
			if !strings.Contains(tool.Description, phrase) {
				t.Errorf("%s description missing phrase %q, got: %s", name, phrase, tool.Description)
			}
		}
	}
}

func TestToolAnnotations(t *testing.T) {
	tools := listTools(t, service.WithAnswerer(&fakeAnswerer{resp: "n/a"}))
	want := map[string]struct{ readOnly, destructive bool }{
		"memory_recall":   {readOnly: true},
		"memory_get":      {readOnly: true},
		"memory_list":     {readOnly: true},
		"memory_briefing": {readOnly: true},
		"memory_answer":   {readOnly: true},
		"memory_remember": {readOnly: false, destructive: false},
		"memory_update":   {readOnly: false, destructive: false},
		"memory_forget":   {readOnly: false, destructive: true},
	}
	for name, w := range want {
		tool := tools[name]
		if tool.Annotations == nil {
			t.Fatalf("%s: no annotations", name)
		}
		if tool.Annotations.ReadOnlyHint != w.readOnly {
			t.Errorf("%s readOnlyHint: got %v, want %v", name, tool.Annotations.ReadOnlyHint, w.readOnly)
		}
		if !w.readOnly && (tool.Annotations.DestructiveHint == nil || *tool.Annotations.DestructiveHint != w.destructive) {
			t.Errorf("%s destructiveHint", name)
		}
		if tool.Annotations.OpenWorldHint == nil || *tool.Annotations.OpenWorldHint {
			t.Errorf("%s openWorldHint should be false", name)
		}
	}
}

// TestGetUnknownIDErrorMessage pins that memory_get with unknown id returns
// an error telling the caller to use memory_recall or memory_list to find ids.
func TestGetUnknownIDErrorMessage(t *testing.T) {
	cs := connect(t)
	res, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name:      "memory_get",
		Arguments: map[string]any{"id": "does-not-exist"},
	})
	if err != nil {
		t.Fatalf("get transport: %v", err)
	}
	if !res.IsError {
		t.Fatal("getting unknown id must be a tool error")
	}
	if len(res.Content) == 0 {
		t.Fatal("error result has no content")
	}
	tc, ok := res.Content[0].(*mcpsdk.TextContent)
	if !ok {
		t.Fatalf("error content = %T, want *mcpsdk.TextContent", res.Content[0])
	}
	if !strings.Contains(tc.Text, "memory_recall") {
		t.Fatalf("error text = %q, want it to mention memory_recall", tc.Text)
	}
}

// TestForgetUnknownIDErrorMessage pins that memory_forget with unknown id returns
// an error telling the caller to use memory_recall or memory_list to find ids.
func TestForgetUnknownIDErrorMessage(t *testing.T) {
	cs := connect(t)
	res, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name:      "memory_forget",
		Arguments: map[string]any{"id": "does-not-exist"},
	})
	if err != nil {
		t.Fatalf("forget transport: %v", err)
	}
	if !res.IsError {
		t.Fatal("forgetting unknown id must be a tool error")
	}
	if len(res.Content) == 0 {
		t.Fatal("error result has no content")
	}
	tc, ok := res.Content[0].(*mcpsdk.TextContent)
	if !ok {
		t.Fatalf("error content = %T, want *mcpsdk.TextContent", res.Content[0])
	}
	if !strings.Contains(tc.Text, "memory_recall") {
		t.Fatalf("error text = %q, want it to mention memory_recall", tc.Text)
	}
}

// TestRecallInvalidTierListsValidValues pins that memory_recall with invalid
// tier lists all four valid tiers in the error message.
func TestRecallInvalidTierListsValidValues(t *testing.T) {
	cs := connect(t)
	res, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name:      "memory_recall",
		Arguments: map[string]any{"query": "anything", "tiers": []string{"bogus"}},
	})
	if err != nil {
		t.Fatalf("recall transport: %v", err)
	}
	if !res.IsError {
		t.Fatal("recall with invalid tier must be a tool error")
	}
	if len(res.Content) == 0 {
		t.Fatal("error result has no content")
	}
	tc, ok := res.Content[0].(*mcpsdk.TextContent)
	if !ok {
		t.Fatalf("error content = %T, want *mcpsdk.TextContent", res.Content[0])
	}
	errText := tc.Text
	validTiers := []string{"working", "episodic", "semantic", "procedural"}
	for _, tier := range validTiers {
		if !strings.Contains(errText, tier) {
			t.Fatalf("error text = %q, want it to include valid tier %q", errText, tier)
		}
	}
}

// TestRecallResponseFormatConcise pins that memory_recall with response_format="concise"
// returns truncated content (~240 chars) for memories without a summary, but
// returns the summary verbatim if one exists. Default/omitted response_format
// returns full content for backward compatibility.
func TestRecallResponseFormatConcise(t *testing.T) {
	cs := connect(t)
	ctx := context.Background()

	// Remember a long memory without a summary
	longContent := strings.Repeat("the deployment pipeline runs tests in parallel across 16 cores to minimize latency. ", 30)
	if len(longContent) < 2000 {
		t.Fatalf("setup: long content must be >2000 chars, got %d", len(longContent))
	}

	res, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{
		Name: "memory_remember",
		Arguments: map[string]any{
			"content": longContent,
			"tier":    "semantic",
			"id":      "long-memory-1",
		},
	})
	if err != nil {
		t.Fatalf("remember long: %v", err)
	}
	structured(t, res, &struct{}{})

	// Remember a memory with a summary
	summaryText := "deployment pipeline runs tests in parallel"
	res, err = cs.CallTool(ctx, &mcpsdk.CallToolParams{
		Name: "memory_remember",
		Arguments: map[string]any{
			"content": "this is a long detailed explanation about the deployment pipeline " + longContent,
			"summary": summaryText,
			"tier":    "semantic",
			"id":      "summary-memory-1",
		},
	})
	if err != nil {
		t.Fatalf("remember with summary: %v", err)
	}
	structured(t, res, &struct{}{})

	type recallResult struct {
		Results []struct {
			ID      string `json:"id"`
			Content string `json:"content"`
		} `json:"results"`
	}

	// Recall with response_format="detailed" → full content
	res, err = cs.CallTool(ctx, &mcpsdk.CallToolParams{
		Name: "memory_recall",
		Arguments: map[string]any{
			"query":           "deployment pipeline",
			"limit":           5,
			"response_format": "detailed",
		},
	})
	if err != nil {
		t.Fatalf("recall detailed: %v", err)
	}
	var detailed recallResult
	structured(t, res, &detailed)
	if len(detailed.Results) < 2 {
		t.Fatalf("recall returned %d results, want at least 2", len(detailed.Results))
	}

	// Find the two memories by ID
	var longDetailedContent string
	var summaryDetailedContent string
	for _, item := range detailed.Results {
		if item.ID == "long-memory-1" {
			longDetailedContent = item.Content
		}
		if item.ID == "summary-memory-1" {
			summaryDetailedContent = item.Content
		}
	}
	if longDetailedContent == "" {
		t.Fatal("detailed recall: long memory not found")
	}
	if summaryDetailedContent == "" {
		t.Fatal("detailed recall: summary memory not found")
	}
	if len(longDetailedContent) < 2000 {
		t.Fatalf("detailed recall: long memory should be >=2000 chars, got %d", len(longDetailedContent))
	}

	// Recall without response_format → full content (backward compat)
	res, err = cs.CallTool(ctx, &mcpsdk.CallToolParams{
		Name: "memory_recall",
		Arguments: map[string]any{
			"query": "deployment pipeline",
			"limit": 5,
		},
	})
	if err != nil {
		t.Fatalf("recall default: %v", err)
	}
	var defaultResult recallResult
	structured(t, res, &defaultResult)
	if len(defaultResult.Results) < 2 {
		t.Fatalf("recall returned %d results, want at least 2", len(defaultResult.Results))
	}

	// Verify full content is returned by default (same as detailed)
	var longDefaultContent string
	for _, item := range defaultResult.Results {
		if item.ID == "long-memory-1" {
			longDefaultContent = item.Content
		}
	}
	if longDefaultContent != longDetailedContent {
		t.Fatalf("default recall should match detailed: got %d vs %d chars", len(longDefaultContent), len(longDetailedContent))
	}

	// Recall with response_format="concise" → truncated or summary
	res, err = cs.CallTool(ctx, &mcpsdk.CallToolParams{
		Name: "memory_recall",
		Arguments: map[string]any{
			"query":           "deployment pipeline",
			"limit":           5,
			"response_format": "concise",
		},
	})
	if err != nil {
		t.Fatalf("recall concise: %v", err)
	}
	var concise recallResult
	structured(t, res, &concise)
	if len(concise.Results) < 2 {
		t.Fatalf("recall returned %d results, want at least 2", len(concise.Results))
	}

	// Find the memories by ID and verify concise behavior
	var longConciseContent string
	var summaryConciseContent string
	for _, item := range concise.Results {
		if item.ID == "long-memory-1" {
			longConciseContent = item.Content
		}
		if item.ID == "summary-memory-1" {
			summaryConciseContent = item.Content
		}
	}
	if longConciseContent == "" {
		t.Fatal("concise recall: long memory not found")
	}
	if summaryConciseContent == "" {
		t.Fatal("concise recall: summary memory not found")
	}

	// Long memory without summary should be truncated
	if len(longConciseContent) > 250 {
		t.Fatalf("concise recall: long memory should be truncated to ~240 chars, got %d", len(longConciseContent))
	}
	if !strings.HasSuffix(longConciseContent, "…") {
		t.Fatalf("concise recall: truncated content should end with …, got: %q", longConciseContent)
	}

	// Memory with summary should return the summary verbatim
	if summaryConciseContent != summaryText {
		t.Fatalf("concise recall: summary memory should return summary verbatim %q, got %q", summaryText, summaryConciseContent)
	}
}

// TestRecallResponseFormatConciseMultiByte pins that concise truncation is
// decided on RUNE length, not byte length. Two regressions guarded:
//  1. content over 240 runes of multi-byte chars is truncated to exactly 240
//     runes + "…" without splitting a UTF-8 sequence;
//  2. content whose BYTE length exceeds 240 but rune length does not (e.g.
//     200 three-byte CJK chars = 600 bytes) is returned verbatim with NO
//     spurious "…" — the pre-fix code appended the ellipsis to unsliced
//     content because the guard checked bytes while truncation checked runes.
func TestRecallResponseFormatConciseMultiByte(t *testing.T) {
	cs := connect(t)
	ctx := context.Background()

	// 300 runes of a 3-byte CJK char: 900 bytes, 300 runes → must truncate.
	overRunes := strings.Repeat("記", 300)
	// 200 runes of the same class: 600 bytes (>240), 200 runes (<=240) → verbatim.
	underRunes := strings.Repeat("憶", 200)

	for id, content := range map[string]string{
		"multibyte-over-1":  overRunes,
		"multibyte-under-1": underRunes,
	} {
		res, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{
			Name: "memory_remember",
			Arguments: map[string]any{
				"content": content, "tier": "semantic", "id": id,
			},
		})
		if err != nil {
			t.Fatalf("remember %s: %v", id, err)
		}
		structured(t, res, &struct{}{})
	}

	res, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{
		Name: "memory_recall",
		Arguments: map[string]any{
			"query":           "記 憶",
			"limit":           5,
			"response_format": "concise",
		},
	})
	if err != nil {
		t.Fatalf("recall concise: %v", err)
	}
	var concise struct {
		Results []struct {
			ID      string `json:"id"`
			Content string `json:"content"`
		} `json:"results"`
	}
	structured(t, res, &concise)

	var over, under string
	for _, item := range concise.Results {
		switch item.ID {
		case "multibyte-over-1":
			over = item.Content
		case "multibyte-under-1":
			under = item.Content
		}
	}
	if over == "" || under == "" {
		t.Fatalf("concise recall did not return both multi-byte memories: %+v", concise.Results)
	}

	// Over 240 runes: truncated to exactly 240 runes + "…".
	wantOver := strings.Repeat("記", 240) + "…"
	if over != wantOver {
		t.Fatalf("over-240-rune content: got %d runes, want exactly 240 runes + ellipsis", len([]rune(over)))
	}

	// Under 240 runes (but over 240 bytes): verbatim, no spurious ellipsis.
	if under != underRunes {
		t.Fatalf("under-240-rune content must be returned verbatim without ellipsis; got %d runes, ends with %q",
			len([]rune(under)), under[len(under)-3:])
	}
}

func TestRecallAndBriefingIncludeCreatedAtAndTags(t *testing.T) {
	cs := connect(t)
	ctx := context.Background()

	// Remember a memory with tags
	res, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{
		Name: "memory_remember",
		Arguments: map[string]any{
			"content": "kubernetes uses etcd for state management",
			"tier":    "semantic",
			"tags":    []string{"distributed-systems", "kubernetes"},
		},
	})
	if err != nil {
		t.Fatalf("remember: %v", err)
	}
	var remembered struct {
		ID string `json:"id"`
	}
	structured(t, res, &remembered)
	if remembered.ID == "" {
		t.Fatalf("remember returned empty ID")
	}

	// Recall and verify created_at and tags are present
	res, err = cs.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      "memory_recall",
		Arguments: map[string]any{"query": "etcd kubernetes", "limit": 5},
	})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	var recalled struct {
		Results []struct {
			ID        string   `json:"id"`
			Content   string   `json:"content"`
			CreatedAt string   `json:"created_at"`
			Tags      []string `json:"tags"`
		} `json:"results"`
	}
	structured(t, res, &recalled)
	if len(recalled.Results) == 0 {
		t.Fatalf("recall returned no results")
	}

	item := recalled.Results[0]
	if item.ID != remembered.ID {
		t.Errorf("recall returned wrong memory ID: got %q, want %q", item.ID, remembered.ID)
	}

	// Verify created_at is present and valid RFC3339
	if item.CreatedAt == "" {
		t.Errorf("recall item missing created_at")
	} else {
		_, err := time.Parse(time.RFC3339, item.CreatedAt)
		if err != nil {
			t.Errorf("created_at is not valid RFC3339: %q (error: %v)", item.CreatedAt, err)
		}
	}

	// Verify tags round-trip
	if len(item.Tags) != 2 {
		t.Errorf("recall item has wrong number of tags: got %d, want 2", len(item.Tags))
	}
	expectedTags := map[string]bool{"distributed-systems": false, "kubernetes": false}
	for _, tag := range item.Tags {
		if _, ok := expectedTags[tag]; !ok {
			t.Errorf("unexpected tag in recall item: %q", tag)
		} else {
			expectedTags[tag] = true
		}
	}
	for tag, found := range expectedTags {
		if !found {
			t.Errorf("recall item missing expected tag: %q", tag)
		}
	}

	// Test briefing also includes created_at
	res, err = cs.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      "memory_briefing",
		Arguments: map[string]any{"per_section": 10},
	})
	if err != nil {
		t.Fatalf("briefing: %v", err)
	}
	var briefed struct {
		Facts []struct {
			ID        string   `json:"id"`
			CreatedAt string   `json:"created_at"`
			Tags      []string `json:"tags"`
		} `json:"facts"`
	}
	structured(t, res, &briefed)
	if len(briefed.Facts) == 0 {
		t.Fatalf("briefing returned no facts")
	}

	// Find our memory in the facts
	found := false
	for _, fact := range briefed.Facts {
		if fact.ID == remembered.ID {
			found = true
			if fact.CreatedAt == "" {
				t.Errorf("briefing fact missing created_at")
			} else {
				_, err := time.Parse(time.RFC3339, fact.CreatedAt)
				if err != nil {
					t.Errorf("briefing fact created_at is not valid RFC3339: %q (error: %v)", fact.CreatedAt, err)
				}
			}
			if len(fact.Tags) != 2 {
				t.Errorf("briefing fact has wrong number of tags: got %d, want 2", len(fact.Tags))
			}
			break
		}
	}
	if !found {
		t.Errorf("briefing did not include our remembered memory")
	}
}
