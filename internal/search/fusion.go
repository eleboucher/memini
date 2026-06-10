package search

import (
	"sort"

	"github.com/eleboucher/memini/internal/store"
)

// DefaultFusionAlpha weights the vector leg against the keyword leg in
// FuseScores: combined = alpha*vectorNorm + (1-alpha)*keywordNorm. 0.5 is the
// neutral, deliberately-untuned default — balanced fusion is consistently
// optimal in the literature and we do not fit alpha to the benchmark.
const DefaultFusionAlpha = 0.5

// FuseScores combines best-first result lists by convex combination of their
// min-max-normalized scores (a.k.a. relative score fusion): within each list
// the raw scores are scaled so the best becomes 1 and the worst 0, then each
// memory's normalized scores are summed weighted by the per-list weights.
//
// Unlike Reciprocal Rank Fusion, this keeps score *magnitude*: a memory that a
// leg considers far better than its runners-up dominates, and one that is
// merely middling in both legs stays middling — so single-leg excellence is not
// drowned by both-leg consensus. weights are aligned with lists by index;
// missing or short weight slices default the remainder to 1. The top k are
// returned best-first (k <= 0 returns all), ties broken by first-seen order.
func FuseScores(lists [][]store.Scored, weights []float64, k int) []store.Scored {
	type agg struct {
		mem   *store.Scored
		score float64
	}
	byID := make(map[string]*agg)
	var order []string

	for li, list := range lists {
		w := 1.0
		if li < len(weights) {
			w = weights[li]
		}
		lo, hi := scoreRange(list)
		span := hi - lo
		for _, sc := range list {
			norm := 1.0 // when every score in the leg is equal, treat them as equally strong
			if span > 0 {
				norm = (sc.Score - lo) / span
			}
			id := sc.Memory.ID
			a, ok := byID[id]
			if !ok {
				scopy := sc
				a = &agg{mem: &scopy}
				byID[id] = a
				order = append(order, id)
			}
			a.score += w * norm
		}
	}

	pos := make(map[string]int, len(order))
	for i, id := range order {
		pos[id] = i
	}

	fused := make([]store.Scored, 0, len(byID))
	for _, id := range order {
		a := byID[id]
		fused = append(fused, store.Scored{Memory: a.mem.Memory, Score: a.score})
	}
	sort.SliceStable(fused, func(i, j int) bool {
		if fused[i].Score != fused[j].Score {
			return fused[i].Score > fused[j].Score
		}
		return pos[fused[i].Memory.ID] < pos[fused[j].Memory.ID]
	})

	if k > 0 && len(fused) > k {
		fused = fused[:k]
	}
	return fused
}

// scoreRange returns the min and max Score in a list (0,0 when empty).
func scoreRange(list []store.Scored) (lo, hi float64) {
	if len(list) == 0 {
		return 0, 0
	}
	lo, hi = list[0].Score, list[0].Score
	for _, sc := range list[1:] {
		if sc.Score < lo {
			lo = sc.Score
		}
		if sc.Score > hi {
			hi = sc.Score
		}
	}
	return lo, hi
}
