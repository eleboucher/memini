package rest_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// budgetContent builds n-word content around a shared recall anchor with a
// known estimate: ceil(n*4/3) tokens + the 10-token per-item overhead.
func budgetContent(topic string, n int) string {
	parts := []string{"budget", "anchor", topic}
	for i := len(parts); i < n; i++ {
		parts = append(parts, fmt.Sprintf("w%d", i))
	}
	return strings.Join(parts, " ")
}

// budgetSearchWire is the slice of the /v1/search wire shape the budget tests
// care about: the results plus the budget's omitted count.
type budgetSearchWire struct {
	Results []struct {
		Memory struct {
			Id      string `json:"id"`
			Content string `json:"content"`
		} `json:"memory"`
	} `json:"results"`
	Omitted *int `json:"omitted"`
}

// TestSearchMaxTokens pins the /v1/search wire contract for server-enforced
// budgets: `max_tokens` fills results in final rank order until the estimated
// cost would exceed it, drops the tail, and reports the count in `omitted` —
// absent (never 0) when everything fit or no budget was set. At least one
// result always ships when anything matched.
func TestSearchMaxTokens(t *testing.T) {
	h := newServer(t)
	const ns = "alice"

	// Three 30-word memories: 40 content tokens + 10 overhead = 50 each.
	for i := range 3 {
		if rec := do(t, h, http.MethodPost, "/v1/memories", ns, apiKey, map[string]any{
			"content": budgetContent(fmt.Sprintf("t%d", i), 30), "tier": "semantic", "id": fmt.Sprintf("bud-%d", i),
		}); rec.Code != http.StatusCreated {
			t.Fatalf("remember %d: %d (%s)", i, rec.Code, rec.Body)
		}
	}

	search := func(body map[string]any) budgetSearchWire {
		t.Helper()
		rec := do(t, h, http.MethodPost, "/v1/search", ns, apiKey, body)
		if rec.Code != http.StatusOK {
			t.Fatalf("search %v: want 200, got %d (%s)", body, rec.Code, rec.Body)
		}
		var out budgetSearchWire
		mustJSON(t, rec, &out)
		return out
	}

	// No budget: all three, `omitted` absent.
	full := search(map[string]any{"query": "budget anchor", "limit": 10})
	if len(full.Results) != 3 || full.Omitted != nil {
		t.Fatalf("unbudgeted: %d results, omitted=%v; want 3, absent", len(full.Results), full.Omitted)
	}

	// A 100-token budget fits two 50-token items: tail dropped in rank order,
	// omitted = 1.
	got := search(map[string]any{"query": "budget anchor", "limit": 10, "max_tokens": 100})
	if len(got.Results) != 2 {
		t.Fatalf("budget 100: got %d results, want 2", len(got.Results))
	}
	for i := range got.Results {
		if got.Results[i].Memory.Id != full.Results[i].Memory.Id {
			t.Fatalf("budget reordered: rank %d = %s, want %s", i, got.Results[i].Memory.Id, full.Results[i].Memory.Id)
		}
	}
	if got.Omitted == nil || *got.Omitted != 1 {
		t.Fatalf("budget 100: omitted = %v, want 1", got.Omitted)
	}

	// A roomy budget: everything fits, `omitted` absent — never an explicit 0.
	got = search(map[string]any{"query": "budget anchor", "limit": 10, "max_tokens": 10000})
	if len(got.Results) != 3 || got.Omitted != nil {
		t.Fatalf("roomy budget: %d results, omitted=%v; want 3, absent", len(got.Results), got.Omitted)
	}

	// A budget below any single result: the top result still ships — a
	// non-empty recall never becomes empty by budget.
	got = search(map[string]any{"query": "budget anchor", "limit": 10, "max_tokens": 1})
	if len(got.Results) != 1 || got.Results[0].Memory.Id != full.Results[0].Memory.Id {
		t.Fatalf("tiny budget: got %+v, want just the top-ranked %s", got.Results, full.Results[0].Memory.Id)
	}
	if got.Omitted == nil || *got.Omitted != 2 {
		t.Fatalf("tiny budget: omitted = %v, want 2", got.Omitted)
	}
}

// TestSearchMaxTokensConciseEstimation pins that the budget prices what the
// response SHIPS: under response_format=concise the estimate covers the
// concise text (the summary here), so a budget that drops an item in detailed
// mode keeps it in concise mode — and the shipped content actually is the
// concise form.
func TestSearchMaxTokensConciseEstimation(t *testing.T) {
	h := newServer(t)
	const ns = "alice"

	// Two long-content memories (200 words → 277 tokens each detailed) with
	// tiny summaries (3 words → 14 tokens each concise).
	for i := range 2 {
		if rec := do(t, h, http.MethodPost, "/v1/memories", ns, apiKey, map[string]any{
			"content": budgetContent(fmt.Sprintf("c%d", i), 200), "summary": "tiny summary here",
			"tier": "semantic", "id": fmt.Sprintf("con-%d", i),
		}); rec.Code != http.StatusCreated {
			t.Fatalf("remember %d: %d (%s)", i, rec.Code, rec.Body)
		}
	}

	search := func(format string) budgetSearchWire {
		t.Helper()
		body := map[string]any{"query": "budget anchor", "limit": 10, "max_tokens": 40}
		if format != "" {
			body["response_format"] = format
		}
		rec := do(t, h, http.MethodPost, "/v1/search", ns, apiKey, body)
		if rec.Code != http.StatusOK {
			t.Fatalf("search format=%q: want 200, got %d (%s)", format, rec.Code, rec.Body)
		}
		var out budgetSearchWire
		mustJSON(t, rec, &out)
		return out
	}

	if got := search(""); len(got.Results) != 1 || got.Omitted == nil || *got.Omitted != 1 {
		t.Fatalf("detailed: %d results, omitted=%v; want 1, 1 — full content cannot fit 40 tokens", len(got.Results), got.Omitted)
	}
	got := search("concise")
	if len(got.Results) != 2 || got.Omitted != nil {
		t.Fatalf("concise: %d results, omitted=%v; want both to fit (summaries), omitted absent", len(got.Results), got.Omitted)
	}
	for _, r := range got.Results {
		if r.Memory.Content != "tiny summary here" {
			t.Fatalf("concise budgeted result must ship the concise form, got %q", r.Memory.Content)
		}
	}
}

// budgetBriefingWire is the slice of the briefing wire shape the budget tests
// care about.
type budgetBriefingWire struct {
	Pinned *[]struct {
		Memory struct {
			Id      string `json:"id"`
			Content string `json:"content"`
		} `json:"memory"`
	} `json:"pinned"`
	Facts      *[]map[string]any `json:"facts"`
	Procedures *[]map[string]any `json:"procedures"`
	Recent     *[]map[string]any `json:"recent"`
	Omitted    *int              `json:"omitted"`
}

// TestBriefingMaxTokens pins the briefing budget's wire contract: the
// max_tokens query param fills whole items in section order pinned → facts →
// procedures → recent (pinned fills first, recent starves first — never the
// reverse), drops whole tail items, and reports the total in `omitted` —
// absent when everything fit or no budget was set.
func TestBriefingMaxTokens(t *testing.T) {
	h := newServer(t)
	const ns = "alice"

	seed := func(id, tier string, tags []string) {
		t.Helper()
		body := map[string]any{"content": budgetContent(id, 30), "tier": tier, "id": id}
		if tags != nil {
			body["tags"] = tags
		}
		if rec := do(t, h, http.MethodPost, "/v1/memories", ns, apiKey, body); rec.Code != http.StatusCreated {
			t.Fatalf("remember %s: %d (%s)", id, rec.Code, rec.Body)
		}
	}
	seed("pin-1", "semantic", []string{"pinned"})
	seed("fact-1", "semantic", nil)
	seed("proc-1", "procedural", nil)
	seed("recent-1", "episodic", nil)

	brief := func(query string) budgetBriefingWire {
		t.Helper()
		rec := do(t, h, http.MethodGet, "/v1/namespaces/briefing"+query, ns, apiKey, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("briefing %q: want 200, got %d (%s)", query, rec.Code, rec.Body)
		}
		var out budgetBriefingWire
		mustJSON(t, rec, &out)
		return out
	}
	sectionLen := func(s *[]map[string]any) int {
		if s == nil {
			return 0
		}
		return len(*s)
	}

	// No budget: every section ships, `omitted` absent.
	full := brief("")
	if full.Pinned == nil || sectionLen(full.Recent) != 1 || full.Omitted != nil {
		t.Fatalf("unbudgeted briefing: %+v — want all sections and no omitted", full)
	}
	total := len(*full.Pinned) + sectionLen(full.Facts) + sectionLen(full.Procedures) + sectionLen(full.Recent)

	// A ~3-item budget (160 tokens; items are 50 each): pinned survives,
	// recent starves — fill order IS priority order.
	got := brief("?max_tokens=160")
	if got.Pinned == nil || len(*got.Pinned) != 1 {
		t.Fatalf("budget 160: pinned starved (%+v), but pinned fills first", got.Pinned)
	}
	if sectionLen(got.Recent) != 0 {
		t.Fatalf("budget 160: recent survived while the tail starves — recent must starve first")
	}
	kept := len(*got.Pinned) + sectionLen(got.Facts) + sectionLen(got.Procedures)
	if got.Omitted == nil || *got.Omitted != total-kept {
		t.Fatalf("budget 160: omitted = %v, want %d", got.Omitted, total-kept)
	}
	// Never split an item: survivors carry their full content.
	if c := (*got.Pinned)[0].Memory.Content; len(strings.Fields(c)) != 30 {
		t.Fatalf("briefing split an item: %q", c)
	}

	// A budget below the first item: the first pinned item still ships.
	got = brief("?max_tokens=1")
	if got.Pinned == nil || len(*got.Pinned) != 1 {
		t.Fatalf("tiny budget: pinned = %+v, want the first item to always fit", got.Pinned)
	}
	if got.Omitted == nil || *got.Omitted != total-1 {
		t.Fatalf("tiny budget: omitted = %v, want %d", got.Omitted, total-1)
	}
}
