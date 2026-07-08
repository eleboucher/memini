package mcp_test

import (
	"context"
	"encoding/json"
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

// TestAnswerToolValidatesTiers pins that memory_answer exposes and validates the
// same tiers filter as recall/list (parity with the service AnswerInput and the
// REST /v1/answer surface). An unknown tier is rejected before the answerer is
// consulted, so this needs no LLM.
func TestAnswerToolValidatesTiers(t *testing.T) {
	cs := connect(t)
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

func listTools(t *testing.T) map[string]*mcpsdk.Tool {
	t.Helper()
	cs := connect(t)
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
	tools := listTools(t)
	want := map[string][]string{
		"memory_remember": {"atomic", "merge_hint", "proactively", "CLAUDE.md", "stored=false"},
		"memory_recall":   {"created_at", "Empty results", "degraded", "BEFORE starting work"},
		"memory_briefing": {"session start"},
		"memory_answer":   {"memory_recall"},
		"memory_list":     {"offset"},
		"memory_get":      {"memory_recall"},
		"memory_forget":   {"delete", "memory_update"},
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
	tools := listTools(t)
	want := map[string]struct{ readOnly, destructive bool }{
		"memory_recall":   {readOnly: true},
		"memory_get":      {readOnly: true},
		"memory_list":     {readOnly: true},
		"memory_briefing": {readOnly: true},
		"memory_remember": {readOnly: false, destructive: false},
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
