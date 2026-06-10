// Package search fuses results from multiple retrieval strategies (vector,
// keyword) into a single ranking, via either Reciprocal Rank Fusion (Fuse) or
// convex-combination score fusion (FuseScores), then re-ranks the result.
package search

import (
	"sort"

	"github.com/eleboucher/memini/internal/store"
)

// DefaultRRFK is the RRF damping constant; larger values flatten the
// contribution of top ranks. The classic value is 60, but with deep per-leg
// pools that lets many both-leg candidates outscore a memory ranked first in a
// single leg (2/(60+20) > 1/(60+0)); a steeper decay keeps single-leg hits
// dominant. 5 is at the low end of the [2,5] plateau measured on LongMemEval-S
// and LoCoMo.
const DefaultRRFK = 5.0

// Fuse combines several best-first result lists into one ranking via Reciprocal
// Rank Fusion: each memory's fused score is the sum over lists of
// 1/(rrfK + rank), where rank is its 0-based position in that list. Memories are
// deduplicated by ID. The top k are returned, best-first.
func Fuse(lists [][]store.Scored, k int, rrfK float64) []store.Scored {
	if rrfK <= 0 {
		rrfK = DefaultRRFK
	}

	type agg struct {
		mem   *store.Scored
		score float64
	}
	byID := make(map[string]*agg)
	var order []string // preserves first-seen order for stable tie-breaking

	for _, list := range lists {
		for rank, sc := range list {
			id := sc.Memory.ID
			a, ok := byID[id]
			if !ok {
				scopy := sc
				a = &agg{mem: &scopy}
				byID[id] = a
				order = append(order, id)
			}
			a.score += 1.0 / (rrfK + float64(rank))
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
