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
			got := reserveDurableTiers(pool(tt.tiers...), tt.limit, tt.reserve, defaultReservePromoteRatio, 0)
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

// scoredPool is pool with explicit composite scores per position.
func scoredPool(tiers []memory.Tier, scores []float64) []store.Scored {
	out := pool(tiers...)
	for i := range out {
		out[i].Score = scores[i]
	}
	return out
}

func TestReserveDurableTiersRelevanceGate(t *testing.T) {
	tests := []struct {
		name           string
		tiers          []memory.Tier
		scores         []float64
		limit, reserve int
		wantTop        string
	}{
		{
			name:   "off-topic durables are not promoted",
			tiers:  []memory.Tier{ep, ep, ep, se, pr},
			scores: []float64{0.9, 0.8, 0.7, 0.3, 0.2},
			limit:  3, reserve: 2,
			// 0.3 < 0.6*0.7: no durable clears the bar, window stays pure relevance.
			wantTop: "abc",
		},
		{
			name:   "competitive durable is still promoted",
			tiers:  []memory.Tier{ep, ep, ep, se},
			scores: []float64{0.9, 0.8, 0.7, 0.6},
			limit:  3, reserve: 1,
			// 0.6 >= 0.6*0.7: the crowded-out fact is recovered as before.
			wantTop: "abd",
		},
		{
			name:   "each eviction raises the bar",
			tiers:  []memory.Tier{ep, ep, ep, se, se},
			scores: []float64{0.9, 0.8, 0.7, 0.6, 0.45},
			limit:  3, reserve: 2,
			// 'd' clears vs 'c' (0.6 >= 0.42); 'e' would clear c's bar but the
			// next evictee is 'b' (bar 0.48 > 0.45), so only one is promoted.
			wantTop: "abd",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := reserveDurableTiers(scoredPool(tt.tiers, tt.scores), tt.limit, tt.reserve, defaultReservePromoteRatio, 0)
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
	got := reserveDurableTiers(p, 5, 2, defaultReservePromoteRatio, 0)
	if ids(got) != "ab" {
		t.Fatalf("small pool should pass through, got %q", ids(got))
	}
}

func TestReserveDurableTiersAdaptiveGate(t *testing.T) {
	tests := []struct {
		name           string
		tiers          []memory.Tier
		scores         []float64
		limit, reserve int
		gatePct        float64
		wantTop        string
	}{
		{
			name:   "off-topic durable below the window percentile is not promoted",
			tiers:  []memory.Tier{ep, ep, ep, se},
			scores: []float64{0.9, 0.8, 0.7, 0.3},
			limit:  3, reserve: 1, gatePct: 10,
			// P10 of {0.7,0.8,0.9} = 0.72 > 0.3: window stays pure relevance.
			wantTop: "abc",
		},
		{
			name:   "durable exactly at the percentile bar is promoted",
			tiers:  []memory.Tier{ep, ep, ep, se},
			scores: []float64{0.9, 0.8, 0.7, 0.72},
			limit:  3, reserve: 1, gatePct: 10,
			// P10 of {0.7,0.8,0.9} = 0.72 <= 0.72: promoted, evicting 'c'.
			wantTop: "abd",
		},
		{
			name:   "durable above the window floor is promoted",
			tiers:  []memory.Tier{ep, ep, ep, se},
			scores: []float64{0.9, 0.8, 0.7, 0.85},
			limit:  3, reserve: 1, gatePct: 50,
			// P50 of {0.7,0.8,0.9} = 0.8 <= 0.85: promoted, evicting 'c'.
			wantTop: "abd",
		},
		{
			name:   "bar is the original window's percentile, not re-derived per eviction",
			tiers:  []memory.Tier{ep, ep, ep, se, se},
			scores: []float64{0.9, 0.8, 0.7, 0.85, 0.82},
			limit:  3, reserve: 2, gatePct: 50,
			// Both durables clear P50=0.8 of the original window {0.7,0.8,0.9}.
			wantTop: "ade",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := reserveDurableTiers(scoredPool(tt.tiers, tt.scores), tt.limit, tt.reserve, defaultReservePromoteRatio, tt.gatePct)
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
