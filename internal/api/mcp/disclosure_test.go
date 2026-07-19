package mcp_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// wantHash mirrors the wire content_hash recipe (and the plugin client's
// injectedIdentity): first 16 hex chars of sha256 over content, falling back
// to summary only when content is empty.
func wantHash(content, summary string) string {
	text := content
	if text == "" {
		text = summary
	}
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])[:16]
}

// errText flattens an error tool result's content for substring assertions.
func errText(t *testing.T, res *mcpsdk.CallToolResult) string {
	t.Helper()
	if !res.IsError {
		t.Fatalf("want a tool error, got success: %+v", res.StructuredContent)
	}
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcpsdk.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String()
}

// TestMemoryGetIDPrefix pins the MCP short-id contract on memory_get: a
// unique ≥8-hex-char id prefix resolves (exact match winning), an ambiguous
// prefix errors listing the colliding full ids so the model can retry with a
// longer one, and a short prefix stays a plain not-found.
func TestMemoryGetIDPrefix(t *testing.T) {
	cs := connect(t)
	ctx := context.Background()
	ids := []string{
		"deadbeef-1111-4000-8000-000000000001",
		"deadbeef-2222-4000-8000-000000000002",
		"cafef00d-aaaa-4000-8000-000000000003",
	}
	for i, id := range ids {
		res, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{
			Name: "memory_remember",
			Arguments: map[string]any{
				"content": "prefix fixture number " + string(rune('a'+i)), "tier": "semantic", "id": id,
			},
		})
		if err != nil {
			t.Fatalf("remember %s: %v", id, err)
		}
		structured(t, res, &struct{}{})
	}

	// A unique prefix resolves to the full memory.
	res, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{
		Name: "memory_get", Arguments: map[string]any{"id": "cafef00d"},
	})
	if err != nil {
		t.Fatalf("get by prefix: %v", err)
	}
	var got struct {
		ID      string `json:"id"`
		Content string `json:"content"`
	}
	structured(t, res, &got)
	if got.ID != ids[2] {
		t.Fatalf("prefix resolved %q, want %q", got.ID, ids[2])
	}

	// An ambiguous prefix errors, listing every colliding full id.
	res, err = cs.CallTool(ctx, &mcpsdk.CallToolParams{
		Name: "memory_get", Arguments: map[string]any{"id": "deadbeef"},
	})
	if err != nil {
		t.Fatalf("ambiguous get: %v", err)
	}
	text := errText(t, res)
	for _, id := range ids[:2] {
		if !strings.Contains(text, id) {
			t.Fatalf("ambiguous error must list %s, got: %q", id, text)
		}
	}

	// Below 8 hex chars nothing is resolved: plain not-found error.
	res, err = cs.CallTool(ctx, &mcpsdk.CallToolParams{
		Name: "memory_get", Arguments: map[string]any{"id": "cafef00"},
	})
	if err != nil {
		t.Fatalf("short-prefix get: %v", err)
	}
	if text := errText(t, res); !strings.Contains(text, "no memory") {
		t.Fatalf("short prefix should be a plain not-found, got: %q", text)
	}

	// Mutations never resolve prefixes: forgetting by prefix is not-found and
	// the memory survives.
	res, err = cs.CallTool(ctx, &mcpsdk.CallToolParams{
		Name: "memory_forget", Arguments: map[string]any{"id": "cafef00d"},
	})
	if err != nil {
		t.Fatalf("forget by prefix: %v", err)
	}
	if text := errText(t, res); !strings.Contains(text, "no memory") {
		t.Fatalf("forget by prefix must be not-found, got: %q", text)
	}
}

// TestRecallContentHash pins that MCP recall results carry content_hash in
// BOTH response formats, equal across them, computed over the FULL stored
// content (summary only when content is empty) — and that a concise cut is
// flagged content_truncated while a summary rendering is not.
func TestRecallContentHash(t *testing.T) {
	cs := connect(t)
	ctx := context.Background()

	longContent := strings.TrimSpace(strings.Repeat("the deployment pipeline runs its checks across many cores ", 10))
	const theSummary = "retries back off exponentially"
	summaryContent := strings.TrimSpace(strings.Repeat("every retry backs off exponentially before the breaker opens ", 10))

	for id, args := range map[string]map[string]any{
		"hash-long-1": {"content": longContent, "tier": "semantic"},
		"hash-sum-1":  {"content": summaryContent, "summary": theSummary, "tier": "semantic"},
	} {
		args["id"] = id
		res, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{Name: "memory_remember", Arguments: args})
		if err != nil {
			t.Fatalf("remember %s: %v", id, err)
		}
		structured(t, res, &struct{}{})
	}

	type item struct {
		ID               string `json:"id"`
		Content          string `json:"content"`
		ContentHash      string `json:"content_hash"`
		ContentTruncated bool   `json:"content_truncated"`
	}
	recall := func(format string) map[string]item {
		args := map[string]any{"query": "deployment pipeline retry breaker", "limit": 5}
		if format != "" {
			args["response_format"] = format
		}
		res, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{Name: "memory_recall", Arguments: args})
		if err != nil {
			t.Fatalf("recall %q: %v", format, err)
		}
		var out struct {
			Results []item `json:"results"`
		}
		structured(t, res, &out)
		byID := make(map[string]item, len(out.Results))
		for _, it := range out.Results {
			byID[it.ID] = it
		}
		return byID
	}

	detailed := recall("")
	if len(detailed) < 2 {
		t.Fatalf("recall returned %d results, want 2", len(detailed))
	}
	if detailed["hash-long-1"].ContentHash != wantHash(longContent, "") {
		t.Fatalf("detailed hash = %q, want %q", detailed["hash-long-1"].ContentHash, wantHash(longContent, ""))
	}
	// Content wins over summary in the hash input.
	if detailed["hash-sum-1"].ContentHash != wantHash(summaryContent, theSummary) {
		t.Fatalf("summary memory hash = %q, want over-content %q",
			detailed["hash-sum-1"].ContentHash, wantHash(summaryContent, theSummary))
	}
	if detailed["hash-long-1"].ContentTruncated || detailed["hash-sum-1"].ContentTruncated {
		t.Fatal("detailed results must not set content_truncated")
	}

	concise := recall("concise")
	long := concise["hash-long-1"]
	if !long.ContentTruncated || !strings.HasSuffix(long.Content, "…") {
		t.Fatalf("concise cut = (%q…, truncated=%v), want boundary cut with content_truncated", long.Content[:20], long.ContentTruncated)
	}
	if sum := concise["hash-sum-1"]; sum.Content != theSummary || sum.ContentTruncated {
		t.Fatalf("concise summary item = (%q, %v), want summary verbatim unmarked", sum.Content, sum.ContentTruncated)
	}
	// The hash never follows the projection.
	if long.ContentHash != detailed["hash-long-1"].ContentHash {
		t.Fatal("content_hash must be identical across response formats")
	}
}

// TestBriefingItemsCarryContentHash pins that MCP briefing sections carry
// content_hash per item, same recipe as recall.
func TestBriefingItemsCarryContentHash(t *testing.T) {
	cs := connect(t)
	ctx := context.Background()

	const content = "the ingestion worker drains the queue before rotating its lease"
	res, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      "memory_remember",
		Arguments: map[string]any{"content": content, "tier": "semantic", "id": "brief-hash-1"},
	})
	if err != nil {
		t.Fatalf("remember: %v", err)
	}
	structured(t, res, &struct{}{})

	res, err = cs.CallTool(ctx, &mcpsdk.CallToolParams{Name: "memory_briefing", Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("briefing: %v", err)
	}
	var out struct {
		Facts []struct {
			ID          string `json:"id"`
			ContentHash string `json:"content_hash"`
		} `json:"facts"`
	}
	structured(t, res, &out)
	if len(out.Facts) == 0 {
		t.Fatal("briefing returned no facts")
	}
	if out.Facts[0].ContentHash != wantHash(content, "") {
		t.Fatalf("briefing item hash = %q, want %q", out.Facts[0].ContentHash, wantHash(content, ""))
	}
}
