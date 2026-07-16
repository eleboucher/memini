package search

import (
	"sort"

	"github.com/eleboucher/memini/internal/store"
)

// DefaultFusionAlpha is the vector-leg weight in FuseScores; the keyword leg
// gets 1-alpha. 0.5 weights both legs equally.
const DefaultFusionAlpha = 0.5

// FuseScores combines best-first result lists by a weighted sum of their
// min-max-normalized scores (relative score fusion): within each list the
// scores are scaled so the best is 1 and the worst 0, then each memory's
// normalized scores are summed, weighted per list. Unlike RRF this preserves
// score magnitude, so a leg's standout hit outranks one that is middling in
// both legs. weights align with lists by index; absent weights default to 1.
// The top k are returned best-first (k <= 0 returns all); ties keep first-seen
// order.
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
			norm := 1.0 // equal scores in a leg all normalize to 1
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
			} else if a.mem.MatchedChunk == "" && sc.MatchedChunk != "" {
				// The retained struct is the first-seen one per ID; backfill the
				// matched chunk from whichever leg carries it so its survival does
				// not depend on the chunk-vector leg being passed first.
				a.mem.MatchedChunk = sc.MatchedChunk
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
		// The retained struct is the first-seen hit for this ID, with
		// MatchedChunk backfilled from any leg above; WithScore keeps its other
		// fields instead of silently dropping what a retrieval leg attached.
		fused = append(fused, a.mem.WithScore(a.score))
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
