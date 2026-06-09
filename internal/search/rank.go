package search

import (
	"sort"
	"time"

	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/store"
)

// RerankWeights weights the composite ranking signals. Relevance (query
// similarity) dominates; recency and importance are secondary tie-breakers so a
// fresh or explicitly-important memory can edge a marginally-more-similar one.
type RerankWeights struct {
	Relevance, Recency, Importance float64
}

// DefaultRerankWeights is the production composite. Recency is a deliberately
// light tie-breaker: benchmarking on LongMemEval-S showed that heavier recency
// weighting buries correct-but-older memories (the gold session is not always
// the most recent), so relevance dominates and recency only decides near-ties.
var DefaultRerankWeights = RerankWeights{Relevance: 0.80, Recency: 0.05, Importance: 0.15}

// Rerank re-scores a fused result list with the default composite weights.
func Rerank(results []store.Scored, now time.Time) []store.Scored {
	return RerankWith(results, now, DefaultRerankWeights)
}

// RerankWith re-scores a fused result list with a composite of normalized
// relevance, access recency, and stored importance, then returns it best-first.
// The input Score is treated as the relevance signal (e.g. an RRF score) and is
// normalized by the maximum in the set so it mixes sanely with the [0,1]
// recency/importance factors. Order is stable for equal composite scores.
func RerankWith(results []store.Scored, now time.Time, w RerankWeights) []store.Scored {
	if len(results) == 0 {
		return results
	}

	maxRel := 0.0
	for _, r := range results {
		if r.Score > maxRel {
			maxRel = r.Score
		}
	}

	type ranked struct {
		sc    store.Scored
		score float64
		pos   int
	}
	out := make([]ranked, len(results))
	for i, r := range results {
		relevance := 0.0
		if maxRel > 0 {
			relevance = r.Score / maxRel
		}
		recency := r.Memory.Recency(now)
		importance := clamp01(r.Memory.Importance)
		composite := w.Relevance*relevance + w.Recency*recency + w.Importance*importance
		out[i] = ranked{sc: store.Scored{Memory: r.Memory, Score: composite}, score: composite, pos: i}
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].score != out[j].score {
			return out[i].score > out[j].score
		}
		return out[i].pos < out[j].pos
	})

	reranked := make([]store.Scored, len(out))
	for i, r := range out {
		reranked[i] = r.sc
	}
	return reranked
}

// Dedup drops results whose normalized content matches an earlier (higher-ranked)
// result, keeping the first occurrence. It preserves order and returns at most
// limit results (limit <= 0 means no cap).
func Dedup(results []store.Scored, limit int) []store.Scored {
	seen := make(map[string]struct{}, len(results))
	out := make([]store.Scored, 0, len(results))
	for _, r := range results {
		key := memory.NormalizeContent(r.Memory.Content)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, r)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

func clamp01(x float64) float64 {
	if x < 0 {
		return 0
	}
	if x > 1 {
		return 1
	}
	return x
}
