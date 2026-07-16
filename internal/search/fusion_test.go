package search

import (
	"testing"

	"github.com/eleboucher/memini/internal/store"
)

func TestFuseScoresMagnitudeBeatsRank(t *testing.T) {
	// "a" is the runaway leader of the vector leg (1.0 vs 0.2) and mid-pack in
	// keyword; "b" is rank-2 in both legs but far behind by score. Rank fusion
	// would reward "b" for its two rank-2 finishes; score fusion keeps magnitude,
	// so "a"'s dominance carries it ahead.
	vector := []store.Scored{sc("a", 1.0), sc("b", 0.2), sc("c", 0.0)}
	keyword := []store.Scored{sc("d", 1.0), sc("a", 0.85), sc("b", 0.8)}

	got := FuseScores([][]store.Scored{vector, keyword}, []float64{0.5, 0.5}, 10)
	// a: 0.5*1.0 + 0.5*(0.85-0.8)/(1.0-0.8)=0.5+0.125=0.625
	// b: 0.5*(0.2-0.0)/1.0 + 0.5*0.0 = 0.10
	if got[0].Memory.ID != "a" {
		t.Fatalf("expected 'a' first (dominant magnitude), got %q (order %v)", got[0].Memory.ID, order(got))
	}
}

func TestFuseScoresNormalizesPerLeg(t *testing.T) {
	// Legs on wildly different scales (BM25-like vs cosine-like) must be
	// comparable after min-max normalization: the per-leg best both map to 1.
	vector := []store.Scored{sc("a", 0.9), sc("b", 0.1)}
	keyword := []store.Scored{sc("b", 80), sc("a", 10)}

	got := FuseScores([][]store.Scored{vector, keyword}, []float64{0.5, 0.5}, 10)
	// a: 0.5*1 + 0.5*0 = 0.5 ; b: 0.5*0 + 0.5*1 = 0.5 — a tie, first-seen wins.
	if len(got) != 2 {
		t.Fatalf("expected 2 unique, got %v", order(got))
	}
	for _, r := range got {
		if r.Score < 0.49 || r.Score > 0.51 {
			t.Fatalf("normalized tie expected ~0.5, got %v for %q", r.Score, r.Memory.ID)
		}
	}
}

func TestFuseScoresWeightingFavorsLeg(t *testing.T) {
	// With alpha=0.9 the vector leg dominates: its top should win even when the
	// keyword leg disagrees.
	vector := []store.Scored{sc("v", 1.0), sc("k", 0.0)}
	keyword := []store.Scored{sc("k", 1.0), sc("v", 0.0)}

	got := FuseScores([][]store.Scored{vector, keyword}, []float64{0.9, 0.1}, 10)
	if got[0].Memory.ID != "v" {
		t.Fatalf("alpha=0.9 should favor vector leg, got %q", got[0].Memory.ID)
	}
}

func TestFuseScoresKeepsMatchedChunkFromFirstLeg(t *testing.T) {
	// The chunk-carrying (vector) leg is fused first, so its struct is the one
	// retained per ID; MatchedChunk must ride through to the reranker.
	vector := []store.Scored{scChunk("a", 1.0, "the matched passage"), sc("b", 0.5)}
	keyword := []store.Scored{sc("a", 0.9), sc("b", 0.8)}

	got := FuseScores([][]store.Scored{vector, keyword}, []float64{0.5, 0.5}, 10)
	if c := chunkOf(got, "a"); c != "the matched passage" {
		t.Fatalf("MatchedChunk dropped when its leg is fused first, got %q", c)
	}
}

func TestFuseScoresBackfillsMatchedChunkFromLaterLeg(t *testing.T) {
	// Same fusion with the legs swapped: the chunk-less keyword struct is
	// first-seen, so MatchedChunk must be backfilled from the vector leg —
	// survival cannot depend on argument order.
	keyword := []store.Scored{sc("a", 0.9), sc("b", 0.8)}
	vector := []store.Scored{scChunk("a", 1.0, "the matched passage"), sc("b", 0.5)}

	got := FuseScores([][]store.Scored{keyword, vector}, []float64{0.5, 0.5}, 10)
	if c := chunkOf(got, "a"); c != "the matched passage" {
		t.Fatalf("MatchedChunk not backfilled from the later leg, got %q", c)
	}
}

func TestFuseScoresEqualLegCollapsesToWeight(t *testing.T) {
	// A leg whose scores are all equal has zero span; every entry normalizes to
	// 1 (equally strong) rather than dividing by zero.
	flat := []store.Scored{sc("a", 7), sc("b", 7)}
	got := FuseScores([][]store.Scored{flat}, []float64{1}, 10)
	if len(got) != 2 || got[0].Score != 1 || got[1].Score != 1 {
		t.Fatalf("equal-score leg should map all to 1, got %v / scores %v,%v", order(got), got[0].Score, got[1].Score)
	}
}
