package mcp_test

import (
	"context"
	"path/filepath"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	meminimcp "github.com/eleboucher/memini/internal/api/mcp"
	"github.com/eleboucher/memini/internal/embed/embedtest"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/service"
	"github.com/eleboucher/memini/internal/store"
	"github.com/eleboucher/memini/internal/store/sqlitevec"
)

// noLinkStore wraps a store.Store, hiding any LinkStore methods the wrapped
// concrete type happens to implement (embedding the interface, not the
// concrete type, so LinkStore's methods aren't promoted) — simulates a
// backend without link support, to exercise the memory_namespace_link
// conditional-registration gate.
type noLinkStore struct{ store.Store }

func connectNoLinkStore(t *testing.T) *mcpsdk.ClientSession {
	t.Helper()
	st, err := sqlitevec.Open(context.Background(), filepath.Join(t.TempDir(), "mcp-nolink.db"), dims)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	svc := service.New(&noLinkStore{Store: st}, embedtest.New(dims))

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

// TestMemoryNamespaceLinkToolConditional mirrors TestMemoryAnswerToolConditional:
// the tool is only advertised when the backing store supports links, so a
// headless/unsupported deployment never lists a tool that would error on
// every call.
func TestMemoryNamespaceLinkToolConditional(t *testing.T) {
	t.Run("no link store", func(t *testing.T) {
		cs := connectNoLinkStore(t)
		res, err := cs.ListTools(context.Background(), nil)
		if err != nil {
			t.Fatalf("list tools: %v", err)
		}
		for _, tool := range res.Tools {
			if tool.Name == "memory_namespace_link" {
				t.Fatal("memory_namespace_link must not be listed when the backend has no LinkStore")
			}
		}
	})
	t.Run("with link store", func(t *testing.T) {
		tools := listTools(t)
		tool, ok := tools["memory_namespace_link"]
		if !ok {
			t.Fatal("memory_namespace_link must be listed when the backend implements LinkStore")
		}
		if tool.Annotations == nil || tool.Annotations.IdempotentHint != true {
			t.Errorf("memory_namespace_link should be idempotentHint=true, got %+v", tool.Annotations)
		}
		if tool.Annotations == nil || tool.Annotations.DestructiveHint == nil || *tool.Annotations.DestructiveHint {
			t.Errorf("memory_namespace_link should be destructiveHint=false, got %+v", tool.Annotations)
		}
	})
}

// TestMemoryNamespaceLinkAddListRemove drives the add/list/remove happy path
// end to end over the MCP transport, checking the returned link list after
// each action (the tool always returns the current list, not just an ack).
func TestMemoryNamespaceLinkAddListRemove(t *testing.T) {
	cs := connect(t)
	ctx := context.Background()

	type linkItem struct {
		Target    string `json:"target"`
		Tiers     string `json:"tiers"`
		CreatedAt string `json:"created_at"`
	}
	type linkResult struct {
		Links []linkItem `json:"links"`
	}

	call := func(args map[string]any) linkResult {
		t.Helper()
		res, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{Name: "memory_namespace_link", Arguments: args})
		if err != nil {
			t.Fatalf("namespace_link transport: %v", err)
		}
		if res.IsError {
			t.Fatalf("namespace_link tool error: %+v", res.Content)
		}
		var out linkResult
		structured(t, res, &out)
		return out
	}

	// list on a fresh namespace: no links.
	got := call(map[string]any{"action": "list", "namespace": "A"})
	if len(got.Links) != 0 {
		t.Fatalf("list (empty) = %+v, want none", got.Links)
	}

	// add defaults to durable.
	got = call(map[string]any{"action": "add", "namespace": "A", "target": "B"})
	if len(got.Links) != 1 || got.Links[0].Target != "B" || got.Links[0].Tiers != "durable" {
		t.Fatalf("add result = %+v, want one durable link to B", got.Links)
	}

	// add again with tiers=all overwrites (idempotent upsert).
	got = call(map[string]any{"action": "add", "namespace": "A", "target": "B", "tiers": "all"})
	if len(got.Links) != 1 || got.Links[0].Tiers != "all" {
		t.Fatalf("add (overwrite) result = %+v, want a single link with tiers=all", got.Links)
	}

	// remove.
	got = call(map[string]any{"action": "remove", "namespace": "A", "target": "B"})
	if len(got.Links) != 0 {
		t.Fatalf("remove result = %+v, want none", got.Links)
	}
}

// TestMemoryNamespaceLinkValidationErrors checks that an invalid action, a
// missing target for add/remove, an invalid tiers value, and a self-link all
// come back as tool errors rather than silently succeeding.
func TestMemoryNamespaceLinkValidationErrors(t *testing.T) {
	cs := connect(t)
	ctx := context.Background()

	cases := []struct {
		name string
		args map[string]any
	}{
		{"invalid action", map[string]any{"action": "bogus"}},
		{"add without target", map[string]any{"action": "add"}},
		{"remove without target", map[string]any{"action": "remove"}},
		{"invalid tiers", map[string]any{"action": "add", "target": "B", "tiers": "bogus"}},
		{"self-link", map[string]any{"action": "add", "namespace": "A", "target": "A"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{Name: "memory_namespace_link", Arguments: tc.args})
			if err != nil {
				t.Fatalf("transport: %v", err)
			}
			if !res.IsError {
				t.Fatalf("%s: expected a tool error, got success", tc.name)
			}
		})
	}
}

// TestMemoryNamespaceLinkRecallReflectsImmediately proves the link takes
// effect immediately: recall in A sees B's durable fact right after
// memory_namespace_link add, with no separate propagation step.
func TestMemoryNamespaceLinkRecallReflectsImmediately(t *testing.T) {
	cs := connect(t)
	ctx := context.Background()

	rememberRes, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{
		Name: "memory_remember",
		Arguments: map[string]any{
			"content": "B: uses trunk-based development", "tier": string(memory.TierSemantic), "namespace": "B",
		},
	})
	if err != nil || rememberRes.IsError {
		t.Fatalf("remember: err=%v res=%+v", err, rememberRes)
	}

	linkRes, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      "memory_namespace_link",
		Arguments: map[string]any{"action": "add", "namespace": "A", "target": "B"},
	})
	if err != nil || linkRes.IsError {
		t.Fatalf("namespace_link add: err=%v res=%+v", err, linkRes)
	}

	recallRes, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      "memory_recall",
		Arguments: map[string]any{"query": "trunk-based development", "namespace": "A"},
	})
	if err != nil {
		t.Fatalf("recall transport: %v", err)
	}
	if recallRes.IsError {
		t.Fatalf("recall tool error: %+v", recallRes.Content)
	}
	var out struct {
		Results []struct {
			Content string `json:"content"`
		} `json:"results"`
	}
	structured(t, recallRes, &out)
	found := false
	for _, r := range out.Results {
		if r.Content == "B: uses trunk-based development" {
			found = true
		}
	}
	if !found {
		t.Fatalf("recall in A right after linking to B should surface B's fact, got %+v", out.Results)
	}
}
