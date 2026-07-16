package service

import (
	"testing"

	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/store"
)

func scored(id string, score float64) store.Scored {
	return store.Scored{Memory: &memory.Memory{ID: id}, Score: score}
}

// TestMergeVectorLegsKeepsBestPerMemory pins the union's core contract: one row
// per memory at its best score, whichever leg produced it. Everything
// downstream (fusion, the semantic gate, dedup) assumes one row per memory, so
// a duplicate here would silently double-count a memory through the whole
// ranking stack.
func TestMergeVectorLegsKeepsBestPerMemory(t *testing.T) {
	docs := []store.Scored{scored("a", 0.9), scored("b", 0.2)}
	chunks := []store.Scored{scored("b", 0.8), scored("c", 0.5)}

	got := mergeVectorLegs(docs, chunks, 1, 10)
	if len(got) != 3 {
		t.Fatalf("got %d rows, want 3 (one per distinct memory)", len(got))
	}
	want := map[string]float64{"a": 0.9, "b": 0.8, "c": 0.5}
	for _, g := range got {
		if w := want[g.Memory.ID]; g.Score != w {
			t.Errorf("%s = %v, want %v (the better leg must win)", g.Memory.ID, g.Score, w)
		}
	}
	if got[0].Memory.ID != "a" {
		t.Errorf("results not best-first: %v", got[0].Memory.ID)
	}
}

// TestMergeVectorLegsWeightsTheChunkLeg pins the knob the plan calls for. The
// weight exists to counter max-pool's length bias against recall's ABSOLUTE
// gates, so it has to actually scale the chunk leg and leave the document leg
// alone.
func TestMergeVectorLegsWeightsTheChunkLeg(t *testing.T) {
	docs := []store.Scored{scored("doc", 0.5)}
	chunks := []store.Scored{scored("chunk", 0.8)}

	// At 1.0 the chunk hit wins on merit.
	full := mergeVectorLegs(docs, chunks, 1, 10)
	if full[0].Memory.ID != "chunk" {
		t.Fatalf("at weight 1.0 the better chunk hit should lead, got %q", full[0].Memory.ID)
	}
	// Damped, it must clear the document hit by a margin to win, and does not.
	damped := mergeVectorLegs(docs, chunks, 0.5, 10)
	if damped[0].Memory.ID != "doc" {
		t.Fatalf("at weight 0.5 a 0.8 chunk (=0.4) should lose to a 0.5 document hit, got %q",
			damped[0].Memory.ID)
	}
	for _, g := range damped {
		if g.Memory.ID == "doc" && g.Score != 0.5 {
			t.Errorf("the weight must not touch the document leg: doc = %v, want 0.5", g.Score)
		}
		if g.Memory.ID == "chunk" && g.Score != 0.4 {
			t.Errorf("chunk = %v, want 0.8*0.5 = 0.4", g.Score)
		}
	}
}

// TestMergeVectorLegsIsDeterministic pins tie-breaking. The merge runs through a
// map, whose iteration order is deliberately random in Go, so equal scores would
// otherwise reshuffle between identical calls: flaky tests and unreproducible
// recall.
func TestMergeVectorLegsIsDeterministic(t *testing.T) {
	docs := []store.Scored{scored("b", 0.5), scored("a", 0.5), scored("c", 0.5)}
	first := mergeVectorLegs(docs, nil, 1, 10)
	for range 20 {
		got := mergeVectorLegs(docs, nil, 1, 10)
		for i := range got {
			if got[i].Memory.ID != first[i].Memory.ID {
				t.Fatalf("order is not stable across calls: %v then %v",
					first[0].Memory.ID, got[i].Memory.ID)
			}
		}
	}
	if first[0].Memory.ID != "a" {
		t.Errorf("ties should break by ID for reproducibility, got %q first", first[0].Memory.ID)
	}
}

// TestMergeVectorLegsRespectsK pins that the merge honours the caller's budget:
// the union of two k-sized legs is up to 2k, and handing that on would inflate
// the pool every stage downstream sized for k.
func TestMergeVectorLegsRespectsK(t *testing.T) {
	docs := []store.Scored{scored("a", 0.9), scored("b", 0.8)}
	chunks := []store.Scored{scored("c", 0.7), scored("d", 0.6)}
	got := mergeVectorLegs(docs, chunks, 1, 3)
	if len(got) != 3 {
		t.Fatalf("got %d rows, want the k of 3", len(got))
	}
	if got[len(got)-1].Memory.ID != "c" {
		t.Errorf("k should keep the best 3, got %q last", got[len(got)-1].Memory.ID)
	}
}

// TestMergeIsSkippedWhenThereAreNoChunks pins that an empty chunk leg leaves
// the document leg exactly as the store returned it.
//
// This is a regression. mergeVectorLegs re-sorts, and its ID tie-break (chosen
// for determinism) is not the store's order for equally-scoring rows, so
// running it over the documents alone REORDERED tied hits. Turning chunking on
// then changed recall on a corpus where it built no chunks at all: the sample
// benchmark's injected-token count moved from 78 to 81 with zero memories
// chunked. "Purely additive" is only true if the path with nothing to add is
// bit-for-bit the old path.
func TestMergeIsSkippedWhenThereAreNoChunks(t *testing.T) {
	// Equal scores, in an order the ID tie-break would NOT produce.
	docs := []store.Scored{scored("c", 0.5), scored("a", 0.5), scored("b", 0.5)}

	// What the merge would do to them, for contrast.
	reordered := mergeVectorLegs(docs, nil, 1, 10)
	if reordered[0].Memory.ID != "a" {
		t.Fatalf("precondition: the merge should sort ties by ID, got %q", reordered[0].Memory.ID)
	}

	// vectorLeg must not call it at all, so the store's order survives. Asserted
	// on the same input the store would have returned.
	s := &Service{chunkEmbed: true, chunkScoreWeight: 1}
	got := s.mergeIfChunked(docs, nil, 10)
	for i := range docs {
		if got[i].Memory.ID != docs[i].Memory.ID {
			t.Fatalf("an empty chunk leg reordered the documents: got %q at %d, want %q",
				got[i].Memory.ID, i, docs[i].Memory.ID)
		}
	}
}
