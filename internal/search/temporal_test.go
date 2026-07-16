package search

import (
	"testing"
	"time"

	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/store"
)

func TestRegexAnchorExtractor(t *testing.T) {
	ex := RegexAnchorExtractor{}
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	// ago computes the expected days-ago for an absolute target date, so the
	// expectations don't hand-count calendar days.
	ago := func(y int, m time.Month, d int) int {
		return int(now.Sub(time.Date(y, m, d, 0, 0, 0, 0, time.UTC)).Hours() / 24)
	}
	cases := []struct {
		q         string
		days, tol int
		ok        bool
	}{
		{"what did I do 3 weeks ago", 21, 5, true},
		{"the milestone yesterday", 1, 1, true},
		{"my trip last month", 30, 7, true},
		{"5 days ago I decided", 5, 2, true},
		{"a year ago", 365, 30, true},
		// Absolute references resolve against now (2026-06-01): the most
		// recent past occurrence.
		{"what did we ship back in march", ago(2026, time.March, 15), 15, true},
		{"the decision on march 14th", ago(2026, time.March, 14), 2, true},
		{"in december we migrated", ago(2025, time.December, 15), 15, true},
		{"the conference last summer", ago(2025, time.July, 15), 45, true},
		{"the migration in 2025", ago(2025, time.July, 1), 120, true},
		{"a totally atemporal question", 0, 0, false},
		{"two days ago", 0, 0, false}, // spelled-out numbers are out of scope (regex tier)
		{"may i ask a question", 0, 0, false},
	}
	for _, c := range cases {
		a, ok := ex.Anchor(c.q, now)
		if ok != c.ok {
			t.Errorf("%q: ok=%v want %v", c.q, ok, c.ok)
			continue
		}
		if ok && (a.Days != c.days || a.Tolerance != c.tol) {
			t.Errorf("%q: got (%d,%d) want (%d,%d)", c.q, a.Days, a.Tolerance, c.days, c.tol)
		}
	}
}

func TestRerankTemporalUsesValidFrom(t *testing.T) {
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	written := now.AddDate(0, 0, -1)
	backdated := now.AddDate(0, 0, -21) // matches "3 weeks ago"
	results := []store.Scored{
		{Memory: &memory.Memory{ID: "plain", CreatedAt: written}, Score: 0.5},
		{Memory: &memory.Memory{ID: "backdated", CreatedAt: written, ValidFrom: &backdated}, Score: 0.5},
	}
	out := RerankTemporal(results, "what did I decide 3 weeks ago", now,
		DefaultRerankWeights, RegexAnchorExtractor{}, DefaultTemporalBoost)
	if out[0].Memory.ID != "backdated" {
		t.Fatalf("ValidFrom-dated memory should win the temporal boost, got %s first", out[0].Memory.ID)
	}
}

func TestRerankTemporalBoostsNearTarget(t *testing.T) {
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	mem := func(id string, daysAgo int) store.Scored {
		return store.Scored{
			Memory: &memory.Memory{ID: id, CreatedAt: now.AddDate(0, 0, -daysAgo)},
			Score:  0.5, // identical relevance, so only the temporal boost can reorder
		}
	}
	// "old" is dated ~21 days back (the referenced time); "fresh" is recent.
	// Equal relevance → without targeting, first-seen ("fresh") wins; with
	// targeting toward "3 weeks ago", "old" must climb to the top.
	results := []store.Scored{mem("fresh", 1), mem("old", 21)}

	plain := RerankTemporal(results, "no time here", now, DefaultRerankWeights, RegexAnchorExtractor{}, DefaultTemporalBoost)
	if plain[0].Memory.ID != "fresh" {
		t.Fatalf("no time reference → no boost, expected first-seen 'fresh' first, got %q", plain[0].Memory.ID)
	}

	targeted := RerankTemporal(results, "what did I decide 3 weeks ago", now, DefaultRerankWeights, RegexAnchorExtractor{}, DefaultTemporalBoost)
	if targeted[0].Memory.ID != "old" {
		t.Fatalf("temporal targeting should lift the on-target memory, got %q first", targeted[0].Memory.ID)
	}
}

// TestRerankTemporalDoesNotDisplaceRelevance guards the multiplicative boost's
// point: a strongly relevant memory dated far from the target must still beat a
// weakly relevant one dated exactly on target. An additive boost would let the
// on-date memory leapfrog on date alone; the multiplicative form must not.
func TestRerankTemporalDoesNotDisplaceRelevance(t *testing.T) {
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	results := []store.Scored{
		// Highly relevant, but written yesterday (far from "3 weeks ago").
		{Memory: &memory.Memory{ID: "relevant", CreatedAt: now.AddDate(0, 0, -1)}, Score: 1.0},
		// Weakly relevant, but dated exactly on the referenced time.
		{Memory: &memory.Memory{ID: "on-date", CreatedAt: now.AddDate(0, 0, -21)}, Score: 0.6},
	}
	out := RerankTemporal(results, "what did I decide 3 weeks ago", now,
		DefaultRerankWeights, RegexAnchorExtractor{}, DefaultTemporalBoost)
	if out[0].Memory.ID != "relevant" {
		t.Fatalf("date proximity must not displace a much stronger match, got %q first", out[0].Memory.ID)
	}
}

func TestRerankTemporalPreservesMatchedChunk(t *testing.T) {
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	// The query names a time ("3 weeks ago"), so the amplification loop
	// rebuilds every score; MatchedChunk must ride through it. The chunk
	// carrier is dated on-target so its score is actually amplified.
	onDate := store.Scored{
		Memory:       &memory.Memory{ID: "on-date", CreatedAt: now.AddDate(0, 0, -21)},
		Score:        0.5,
		MatchedChunk: "the matched passage",
	}
	fresh := store.Scored{Memory: &memory.Memory{ID: "fresh", CreatedAt: now.AddDate(0, 0, -1)}, Score: 0.5}

	out := RerankTemporal([]store.Scored{fresh, onDate}, "what did I decide 3 weeks ago", now,
		DefaultRerankWeights, RegexAnchorExtractor{}, DefaultTemporalBoost)
	if c := chunkOf(out, "on-date"); c != "the matched passage" {
		t.Fatalf("temporal amplification must not drop MatchedChunk, got %q", c)
	}
}

func TestAbsoluteAnchorExplicitYear(t *testing.T) {
	ex := RegexAnchorExtractor{}
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	ago := func(y int, m time.Month, d int) int {
		return int(now.Sub(time.Date(y, m, d, 0, 0, 0, 0, time.UTC)).Hours() / 24)
	}
	cases := []struct {
		q         string
		days, tol int
	}{
		{"the painting shared on october 13, 2023", ago(2023, time.October, 13), 2},
		{"the project in july 2023", ago(2023, time.July, 15), 15},
		{"the setback in october 2023", ago(2023, time.October, 15), 15},
	}
	for _, c := range cases {
		a, ok := ex.Anchor(c.q, now)
		if !ok || a.Days != c.days || a.Tolerance != c.tol {
			t.Errorf("%q: got (%d,%d,%v) want (%d,%d,true)", c.q, a.Days, a.Tolerance, ok, c.days, c.tol)
		}
	}
}
