package service_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/service"
	"github.com/eleboucher/memini/internal/store"
)

// words builds deterministic n-word content around a shared recall anchor so
// every seeded memory matches the same query while carrying a known token
// estimate (render.ApproxTokens: ceil(words*4/3), +10 overhead per item).
func words(topic string, n int) string {
	parts := make([]string, 0, n)
	parts = append(parts, "budget", "anchor", topic)
	for i := len(parts); i < n; i++ {
		parts = append(parts, fmt.Sprintf("w%d", i))
	}
	return strings.Join(parts, " ")
}

// TestRecallMaxTokensBudget pins the server-enforced search budget: results
// fill in final rank order until the estimated cost (content tokens + the
// 10-token per-item overhead) would exceed MaxTokens, the tail is dropped
// whole, the Omitted out-param reports the drop count (0 when everything
// fits), and a non-empty recall never becomes empty — the first result is
// served even when it alone exceeds the budget.
func TestRecallMaxTokensBudget(t *testing.T) {
	ctx := context.Background()
	svc := newEventSvc(t)

	// Three 30-word memories: each estimates ceil(120/3)+10 = 50 tokens.
	for i := range 3 {
		if _, err := svc.Remember(ctx, service.RememberInput{
			Namespace: "n", Content: words(fmt.Sprintf("t%d", i), 30), Tier: memory.TierSemantic,
		}); err != nil {
			t.Fatalf("remember %d: %v", i, err)
		}
	}

	recall := func(maxTokens int) ([]store.Scored, int) {
		t.Helper()
		var omitted int
		res, err := svc.Recall(ctx, service.RecallInput{
			Namespace: "n", Query: "budget anchor", Limit: 10,
			MaxTokens: maxTokens, Omitted: &omitted,
		})
		if err != nil {
			t.Fatalf("recall max_tokens=%d: %v", maxTokens, err)
		}
		return res, omitted
	}

	// Unbounded (0): all three, nothing omitted.
	res, omitted := recall(0)
	if len(res) != 3 || omitted != 0 {
		t.Fatalf("unbounded: got %d results, omitted %d; want 3, 0", len(res), omitted)
	}
	full := res

	// A budget that fits two items (100 tokens) but not three: the tail is
	// dropped in rank order — the survivors are exactly the top-2 of the
	// unbounded ranking.
	res, omitted = recall(100)
	if len(res) != 2 || omitted != 1 {
		t.Fatalf("budget 100: got %d results, omitted %d; want 2, 1", len(res), omitted)
	}
	for i := range res {
		if res[i].Memory.ID != full[i].Memory.ID {
			t.Fatalf("budget kept rank %d = %s, want the unbounded ranking's %s (tail-drop, not reorder)",
				i, res[i].Memory.ID, full[i].Memory.ID)
		}
	}

	// A generous budget that fits everything: omitted stays 0.
	if res, omitted = recall(10000); len(res) != 3 || omitted != 0 {
		t.Fatalf("roomy budget: got %d results, omitted %d; want 3, 0", len(res), omitted)
	}

	// A budget smaller than any single result: the first still ships — a
	// non-empty recall never becomes empty by budget.
	res, omitted = recall(1)
	if len(res) != 1 || omitted != 2 {
		t.Fatalf("tiny budget: got %d results, omitted %d; want 1, 2", len(res), omitted)
	}
	if res[0].Memory.ID != full[0].Memory.ID {
		t.Fatalf("tiny budget served %s, want the top-ranked %s", res[0].Memory.ID, full[0].Memory.ID)
	}
}

// TestRecallBudgetConciseEstimation pins that the budget estimates over the
// content the response will SHIP: with EstimateConcise (response_format=
// concise) the estimate uses the concise text — the summary here — so a
// budget that could never fit two full-content items still keeps both in
// concise mode, while detailed estimation drops the second.
func TestRecallBudgetConciseEstimation(t *testing.T) {
	ctx := context.Background()
	svc := newEventSvc(t)

	// Two memories: 200-word content (267+10 tokens detailed) with a 3-word
	// summary (4+10 tokens concise).
	for i := range 2 {
		if _, err := svc.Remember(ctx, service.RememberInput{
			Namespace: "n", Content: words(fmt.Sprintf("c%d", i), 200),
			Summary: "tiny summary here", Tier: memory.TierSemantic,
		}); err != nil {
			t.Fatalf("remember %d: %v", i, err)
		}
	}

	recall := func(concise bool) ([]store.Scored, int) {
		t.Helper()
		var omitted int
		res, err := svc.Recall(ctx, service.RecallInput{
			Namespace: "n", Query: "budget anchor", Limit: 10,
			MaxTokens: 40, EstimateConcise: concise, Omitted: &omitted,
		})
		if err != nil {
			t.Fatalf("recall concise=%v: %v", concise, err)
		}
		return res, omitted
	}

	if res, omitted := recall(false); len(res) != 1 || omitted != 1 {
		t.Fatalf("detailed estimation: got %d results, omitted %d; want 1, 1 (full content cannot fit 40 tokens)", len(res), omitted)
	}
	if res, omitted := recall(true); len(res) != 2 || omitted != 0 {
		t.Fatalf("concise estimation: got %d results, omitted %d; want 2, 0 (summaries fit the same budget)", len(res), omitted)
	}
}

// TestRecallBudgetOmittedLandsInDetail covers the budget's server-side
// visibility: a recall whose MaxTokens dropped results records the count as
// budget_omitted in the recall event detail — alongside excluded_count —
// and the key is absent (not zero) on an unbudgeted or fully-fitting recall.
func TestRecallBudgetOmittedLandsInDetail(t *testing.T) {
	ctx := context.Background()
	svc := newEventSvc(t)

	for i := range 3 {
		if _, err := svc.Remember(ctx, service.RememberInput{
			Namespace: "n", Content: words(fmt.Sprintf("e%d", i), 30), Tier: memory.TierSemantic,
		}); err != nil {
			t.Fatalf("remember %d: %v", i, err)
		}
	}

	// Budgeted: 100 tokens fits two of the three 50-token items.
	if _, err := svc.Recall(ctx, service.RecallInput{
		Namespace: "n", Query: "budget anchor", Limit: 10, MaxTokens: 100,
	}); err != nil {
		t.Fatalf("budgeted recall: %v", err)
	}
	// Unbudgeted: the key must be absent.
	if _, err := svc.Recall(ctx, service.RecallInput{
		Namespace: "n", Query: "budget anchor w4", Limit: 10,
	}); err != nil {
		t.Fatalf("unbudgeted recall: %v", err)
	}

	page, err := svc.Events(ctx, service.EventsInput{
		Namespace: "n", Kinds: []store.EventKind{store.EventRecall},
	})
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	var budgeted, unbudgeted *service.ActivityEvent
	for i := range page.Events {
		switch page.Events[i].Query {
		case "budget anchor":
			budgeted = &page.Events[i]
		case "budget anchor w4":
			unbudgeted = &page.Events[i]
		}
	}
	if budgeted == nil || unbudgeted == nil {
		t.Fatalf("expected both recall events, got %+v", page.Events)
	}
	if len(budgeted.Memories) != 2 {
		t.Fatalf("budgeted event served %d memories, want the trimmed 2", len(budgeted.Memories))
	}
	if got := budgeted.Detail["budget_omitted"]; !eqInt(got, 1) {
		t.Errorf("budgeted recall detail budget_omitted = %v (%T), want 1", got, got)
	}
	if got, ok := unbudgeted.Detail["budget_omitted"]; ok {
		t.Errorf("unbudgeted recall carries budget_omitted = %v, want the key absent", got)
	}
}

// TestBriefingMaxTokensBudget pins the briefing budget's fill order and
// visibility: max_tokens fills whole items in section order pinned → facts →
// procedures → recent (fill order IS priority order — pinned fills first,
// recent starves first), drops whole tail items (never splits one), reports
// the total drop count in Briefing.Omitted (0 when everything fits), and
// always ships at least the first item.
func TestBriefingMaxTokensBudget(t *testing.T) {
	ctx := context.Background()
	svc := newEventSvc(t)

	// One 30-word memory per section: each estimates 40+10 = 50 tokens.
	seed := func(id string, tier memory.Tier, tags []string) {
		t.Helper()
		if _, err := svc.Remember(ctx, service.RememberInput{
			Namespace: "n", ID: id, Content: words(id, 30), Tier: tier, Tags: tags,
		}); err != nil {
			t.Fatalf("remember %s: %v", id, err)
		}
	}
	seed("pin-1", memory.TierSemantic, []string{"pinned"})
	seed("fact-1", memory.TierSemantic, nil)
	seed("proc-1", memory.TierProcedural, nil)
	seed("recent-1", memory.TierEpisodic, nil)

	brief := func(maxTokens int) service.Briefing {
		t.Helper()
		b, err := svc.Briefing(ctx, "n", service.BriefingOpts{MaxTokens: maxTokens})
		if err != nil {
			t.Fatalf("briefing max_tokens=%d: %v", maxTokens, err)
		}
		return b
	}
	count := func(b service.Briefing) int {
		return len(b.Pinned) + len(b.Facts) + len(b.Procedures) + len(b.Recent)
	}

	// Unbounded: every section ships, nothing omitted. (pin-1 appears in both
	// Pinned and Facts — sections bucket independently.)
	b := brief(0)
	if len(b.Recent) != 1 || b.Omitted != 0 {
		t.Fatalf("unbounded briefing: recent=%d omitted=%d, want 1, 0 (%+v)", len(b.Recent), b.Omitted, b)
	}
	total := count(b)

	// A budget for ~3 items (160 tokens): recent starves FIRST — pinned and
	// facts survive, the tail (recent, and procedures if it doesn't fit) is
	// dropped whole. Omitted = total items dropped across sections.
	b = brief(160)
	if len(b.Pinned) != 1 {
		t.Fatalf("budget 160: pinned starved (%d), but pinned fills FIRST", len(b.Pinned))
	}
	if len(b.Recent) != 0 {
		t.Fatalf("budget 160: recent survived (%d) while the budget starves the tail — recent must starve first", len(b.Recent))
	}
	if got := count(b); b.Omitted != total-got {
		t.Fatalf("budget 160: omitted = %d, want %d (total %d - kept %d)", b.Omitted, total-got, total, got)
	}
	// No item was split: every surviving item carries its full content.
	for _, m := range append(append([]*memory.Memory{}, b.Pinned...), b.Facts...) {
		if len(strings.Fields(m.Content)) != 30 {
			t.Fatalf("briefing split an item: %q", m.Content)
		}
	}

	// A budget smaller than the first item: the first pinned item still ships.
	b = brief(1)
	if len(b.Pinned) != 1 {
		t.Fatalf("tiny budget: pinned = %d, want the first item to always fit", len(b.Pinned))
	}
	if len(b.Facts)+len(b.Procedures)+len(b.Recent) != 0 {
		t.Fatalf("tiny budget: non-pinned sections survived: %+v", b)
	}
	if b.Omitted != total-1 {
		t.Fatalf("tiny budget: omitted = %d, want %d", b.Omitted, total-1)
	}
}
