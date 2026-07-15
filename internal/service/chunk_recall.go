package service

import (
	"context"
	"log/slog"
	"sort"

	"github.com/eleboucher/memini/internal/store"
)

// vectorLeg is recall's vector search: the document vectors always, plus the
// chunk vectors when chunking is on, merged into one row per memory at its best
// score.
//
// It is a union rather than a replacement, and that is the safety property.
// store.VectorSearch keeps its exact meaning — "how close is this memory's own
// vector" — which matters because three of its six callers destroy data
// (write-dedup's coalesce/supersede, contradiction routing, and the maintenance
// dedup sweep). Max-pooled chunk similarity makes a long memory a near-duplicate
// of anything matching any one of its paragraphs; had that leaked into
// VectorSearch, those three would tombstone unrelated memories. Only recall
// reads the union, so turning chunking on can add hits and improve a score, and
// can never delete, tombstone, or invalidate anything.
//
// Everything downstream is unchanged. The result is one store.Scored per
// memory, best-first, exactly what a plain VectorSearch returns — so fusion,
// the semantic gate, temporal boost, reserve, and dedup all see the shape they
// already expect.
func (s *Service) vectorLeg(ctx context.Context, ns string, vec []float32, f store.Filter, k int) ([]store.Scored, error) {
	docs, err := s.store.VectorSearch(ctx, ns, vec, f, k)
	if err != nil {
		return nil, err
	}
	if !s.chunkEmbed || s.chunkStore == nil {
		return docs, nil
	}

	chunks, err := s.chunkStore.ChunkVectorSearch(ctx, ns, vec, f, k)
	if err != nil {
		// Chunk recall is additive, so a failure here costs the extra hits, not
		// the recall. Degrading to the document leg is strictly better than
		// failing a query that would have worked before the feature existed.
		slog.WarnContext(ctx, "recall: chunk vector search failed, using document vectors only",
			"namespace", ns, "err", err)
		return docs, nil
	}
	return mergeVectorLegs(docs, chunks, s.chunkScoreWeight, k), nil
}

// mergeVectorLegs merges the two legs keyed by memory ID, keeping each memory's
// best score, and returns the top k best-first.
//
// weight scales the chunk leg before the comparison. It exists because
// max-pooling has a length bias — a max over more samples is higher in
// expectation, so long memories systematically out-score short ones — and
// recall's gates (the semantic floor, min-score) are absolute thresholds
// calibrated against today's distribution rather than ranks. 1.0 leaves the
// legs comparable; below 1.0 makes a chunk hit have to beat a document hit by a
// margin. The benchmark harness is what should settle it.
func mergeVectorLegs(docs, chunks []store.Scored, weight float64, k int) []store.Scored {
	byID := make(map[string]store.Scored, len(docs)+len(chunks))
	best := func(s store.Scored) {
		if cur, ok := byID[s.Memory.ID]; !ok || s.Score > cur.Score {
			byID[s.Memory.ID] = s
		}
	}
	for _, d := range docs {
		best(d)
	}
	for _, c := range chunks {
		c.Score *= weight
		best(c)
	}

	out := make([]store.Scored, 0, len(byID))
	for _, v := range byID {
		out = append(out, v)
	}
	// Ties broken by ID so the order is deterministic: map iteration is not, and
	// a recall that reshuffles equal-scoring memories between identical calls is
	// a debugging nightmare and a flaky test.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].Memory.ID < out[j].Memory.ID
	})
	if k > 0 && len(out) > k {
		out = out[:k]
	}
	return out
}
