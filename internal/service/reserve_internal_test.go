package service

import (
	"testing"

	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/store"
)

// pool builds a relevance-ordered candidate list from a tier sequence; ids are
// the position so assertions can read the resulting order.
func pool(tiers ...memory.Tier) []store.Scored {
	out := make([]store.Scored, len(tiers))
	for i, t := range tiers {
		out[i] = store.Scored{Memory: &memory.Memory{ID: string(rune('a' + i)), Tier: t}}
	}
	return out
}

func ids(rs []store.Scored) string {
	b := make([]byte, len(rs))
	for i, r := range rs {
		b[i] = r.Memory.ID[0]
	}
	return string(b)
}

const (
	ep = memory.TierEpisodic
	se = memory.TierSemantic
	pr = memory.TierProcedural
)

func TestReserveDurableTiers(t *testing.T) {
	tests := []struct {
		name           string
		tiers          []memory.Tier
		limit, reserve int
		wantTop        string // expected first `limit` ids (the selection), in order
	}{
		{
			name:  "reserve 0 is plain top-N",
			tiers: []memory.Tier{ep, ep, ep, se, pr},
			limit: 3, reserve: 0,
			wantTop: "abc",
		},
		{
			name:  "reserve promotes one durable over lowest episodic, keeps order",
			tiers: []memory.Tier{ep, ep, ep, se},
			limit: 3, reserve: 1,
			// 'd' (semantic) promoted, lowest episodic 'c' evicted; output in relevance order.
			wantTop: "abd",
		},
		{
			name:  "reserve 2 promotes semantic and procedural",
			tiers: []memory.Tier{ep, ep, ep, se, pr},
			limit: 3, reserve: 2,
			// evict 'c','b' (lowest episodics), promote 'd','e'; relevance order → a,d,e.
			wantTop: "ade",
		},
		{
			name:  "already enough durable in window is unchanged",
			tiers: []memory.Tier{se, ep, ep, pr},
			limit: 3, reserve: 1,
			wantTop: "abc",
		},
		{
			name:  "reserve capped at limit",
			tiers: []memory.Tier{ep, ep, se, pr, se},
			limit: 2, reserve: 9,
			// only 2 slots; promote the two highest-relevance durables c,d; order c,d.
			wantTop: "cd",
		},
		{
			name:  "not enough durables to fill reserve takes what exists",
			tiers: []memory.Tier{ep, ep, ep, se},
			limit: 3, reserve: 2,
			// only one durable exists; promote 'd', evict 'c'.
			wantTop: "abd",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := reserveDurableTiers(pool(tt.tiers...), tt.limit, tt.reserve)
			top := got
			if len(top) > tt.limit {
				top = top[:tt.limit]
			}
			if ids(top) != tt.wantTop {
				t.Fatalf("top %q, want %q (full: %q)", ids(top), tt.wantTop, ids(got))
			}
		})
	}
}

func TestReserveDurableTiersPassthrough(t *testing.T) {
	// Pool no deeper than limit: nothing to compose, returned as-is.
	p := pool(ep, se)
	got := reserveDurableTiers(p, 5, 2)
	if ids(got) != "ab" {
		t.Fatalf("small pool should pass through, got %q", ids(got))
	}
}
