package service

// Server-enforced token budgets (PR-F). The server — not the client — decides
// how many results/briefing items fit a caller's token budget, and reports
// what it omitted, so filtering is authoritative and visible: the client's
// own fitByTokens trim survives only as an old-server fallback and a guard
// for the render skeleton the server cannot see.
//
// Estimation is the SHARED estimator (render.ApproxTokens — the client's
// approxTokens recipe replicated exactly) over the content the response will
// actually ship — the concise text when the response projection is concise —
// plus render.ItemOverheadTokens per item for the client's render skeleton.
// An estimate, deliberately: the budget bounds token spend approximately.
//
// The one dependency quirk: internal/api/render is the response-projection
// package, and estimating "what the response ships" IS a projection concern,
// so importing it here is the honest direction — the alternative (trimming in
// the REST layer) would log activity events for results that never shipped.

import (
	"github.com/eleboucher/memini/internal/api/render"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/store"
)

// budgetItemCost estimates one memory's shipped size: the concise projection
// (under conciseMax) when concise is set, else the full content, plus the
// per-item render overhead.
func budgetItemCost(m *memory.Memory, concise bool, conciseMax int) int {
	text := m.Content
	if concise {
		text, _ = render.Concise(m.Content, m.Summary, conciseMax)
	}
	return render.ApproxTokens(text) + render.ItemOverheadTokens
}

// applyRecallBudget trims ranked results to in.MaxTokens: items fill in final
// rank order until the next would exceed the budget, and the whole tail is
// dropped from there. The FIRST result always ships even when it alone
// exceeds the budget — a non-empty recall never becomes empty by budget.
// Returns the surviving prefix and the omitted count, also reporting the
// count through the in.Omitted out-param when wired (kept here, not at the
// call sites, so Recall pays one call instead of branch-per-site — it sits
// at its cyclomatic limit). MaxTokens <= 0 is unbounded (no trim, omitted 0).
func applyRecallBudget(in RecallInput, results []store.Scored) ([]store.Scored, int) {
	omitted := 0
	if in.MaxTokens > 0 && len(results) > 0 {
		used := 0
		kept := 0
		for i, r := range results {
			cost := budgetItemCost(r.Memory, in.EstimateConcise, render.SearchMax)
			if i > 0 && used+cost > in.MaxTokens {
				break
			}
			used += cost
			kept++
		}
		results, omitted = results[:kept], len(results)-kept
	}
	if in.Omitted != nil {
		*in.Omitted = omitted
	}
	return results, omitted
}

// applyBriefingBudget trims a built briefing to maxTokens, filling WHOLE
// items in section order pinned → facts → procedures → recent — fill order
// IS priority order, so pinned fills first and recent starves first — and
// never splitting an item. The first item overall always ships (the search
// budget's non-empty guarantee, applied to the briefing). Returns the total
// number of items dropped across sections. The child rollup is neither
// counted nor trimmed: the budget covers the briefing's own sections, and
// the rollup is already bounded by its own caps. maxTokens <= 0 is
// unbounded. Applied BEFORE logBriefingEvent so the activity log records
// what was actually served.
func applyBriefingBudget(b *Briefing, maxTokens int, concise bool) int {
	if maxTokens <= 0 {
		return 0
	}
	used := 0
	keptAny := false
	exhausted := false
	omitted := 0
	fill := func(sec []*memory.Memory) []*memory.Memory {
		if exhausted {
			omitted += len(sec)
			return nil
		}
		for i, m := range sec {
			cost := budgetItemCost(m, concise, render.BriefingMax)
			if keptAny && used+cost > maxTokens {
				exhausted = true
				omitted += len(sec) - i
				return sec[:i]
			}
			used += cost
			keptAny = true
		}
		return sec
	}
	b.Pinned = fill(b.Pinned)
	b.Facts = fill(b.Facts)
	b.Procedures = fill(b.Procedures)
	b.Recent = fill(b.Recent)
	return omitted
}
