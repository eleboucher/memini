package search

import (
	"testing"

	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/store"
)

func sc(id string, score float64) store.Scored {
	return store.Scored{Memory: &memory.Memory{ID: id}, Score: score}
}

// scChunk is sc with a matched chunk attached, as ChunkVectorSearch sets it.
func scChunk(id string, score float64, chunk string) store.Scored {
	s := sc(id, score)
	s.MatchedChunk = chunk
	return s
}

// chunkOf returns the MatchedChunk of the result with the given id, or "" when
// the id is absent.
func chunkOf(res []store.Scored, id string) string {
	for _, r := range res {
		if r.Memory.ID == id {
			return r.MatchedChunk
		}
	}
	return ""
}

func TestFuseRewardsAgreement(t *testing.T) {
	// "b" tops both lists; agreement should make it the clear winner over items
	// that rank well in only one list.
	vector := []store.Scored{sc("b", 1), sc("a", 0.9), sc("c", 0.8)}
	keyword := []store.Scored{sc("b", 5), sc("c", 4), sc("d", 1)}

	got := Fuse([][]store.Scored{vector, keyword}, 10, DefaultRRFK)
	if got[0].Memory.ID != "b" {
		t.Fatalf("expected 'b' first (agreed by both), got %q (order %v)", got[0].Memory.ID, order(got))
	}
	if len(got) != 4 {
		t.Fatalf("expected 4 unique memories, got %d: %v", len(got), order(got))
	}
}

func TestFuseDeduplicatesAndLimits(t *testing.T) {
	l1 := []store.Scored{sc("a", 1), sc("b", 1)}
	l2 := []store.Scored{sc("a", 1), sc("b", 1)}
	got := Fuse([][]store.Scored{l1, l2}, 1, DefaultRRFK)
	if len(got) != 1 {
		t.Fatalf("limit not applied: %v", order(got))
	}
}

func TestFuseKeepsMatchedChunkEitherLegOrder(t *testing.T) {
	// MatchedChunk must survive RRF fusion whether the chunk-carrying leg is
	// fused first (its struct is the one retained) or second (backfilled onto
	// the first-seen struct).
	withChunk := []store.Scored{scChunk("a", 1, "the matched passage")}
	plain := []store.Scored{sc("a", 1)}
	cases := []struct {
		name  string
		lists [][]store.Scored
	}{
		{"chunk leg first", [][]store.Scored{withChunk, plain}},
		{"chunk leg second", [][]store.Scored{plain, withChunk}},
	}
	for _, c := range cases {
		got := Fuse(c.lists, 10, DefaultRRFK)
		if ch := chunkOf(got, "a"); ch != "the matched passage" {
			t.Errorf("%s: MatchedChunk lost in fusion, got %q", c.name, ch)
		}
	}
}

func order(res []store.Scored) []string {
	out := make([]string, len(res))
	for i, r := range res {
		out[i] = r.Memory.ID
	}
	return out
}
