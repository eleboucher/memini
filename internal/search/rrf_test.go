package search

import (
	"testing"

	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/store"
)

func sc(id string, score float64) store.Scored {
	return store.Scored{Memory: &memory.Memory{ID: id}, Score: score}
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

func order(res []store.Scored) []string {
	out := make([]string, len(res))
	for i, r := range res {
		out[i] = r.Memory.ID
	}
	return out
}
