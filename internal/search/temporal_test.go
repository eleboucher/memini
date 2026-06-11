package search

import (
	"testing"
	"time"

	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/store"
)

func TestRegexAnchorExtractor(t *testing.T) {
	ex := RegexAnchorExtractor{}
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
		{"a totally atemporal question", 0, 0, false},
		{"two days ago", 0, 0, false}, // spelled-out numbers are out of scope (regex tier)
	}
	for _, c := range cases {
		a, ok := ex.Anchor(c.q)
		if ok != c.ok {
			t.Errorf("%q: ok=%v want %v", c.q, ok, c.ok)
			continue
		}
		if ok && (a.Days != c.days || a.Tolerance != c.tol) {
			t.Errorf("%q: got (%d,%d) want (%d,%d)", c.q, a.Days, a.Tolerance, c.days, c.tol)
		}
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
