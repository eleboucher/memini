package search

import (
	"testing"
	"time"

	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/store"
)

func scored(id, content string, relevance, importance float64, lastAccessed time.Time) store.Scored {
	return store.Scored{
		Memory: &memory.Memory{
			ID: id, Content: content, Importance: importance, LastAccessedAt: lastAccessed,
		},
		Score: relevance,
	}
}

func TestRerankRecencyBeatsMarginalRelevance(t *testing.T) {
	now := time.Now().UTC()
	stale := now.Add(-60 * 24 * time.Hour) // ~8 half-lives: recency ≈ 0
	fresh := now

	// "b" is marginally more relevant but very stale; "a" is slightly less
	// relevant but fresh and equally important. Composite should favour "a".
	in := []store.Scored{
		scored("b", "older", 1.00, 0.5, stale),
		scored("a", "fresh", 0.95, 0.5, fresh),
	}
	out := Rerank(in, now)
	if out[0].Memory.ID != "a" {
		t.Fatalf("expected fresh 'a' to outrank stale 'b', got %s first", out[0].Memory.ID)
	}
}

func TestRerankImportanceTieBreak(t *testing.T) {
	now := time.Now().UTC()
	// Equal relevance and recency; importance decides.
	in := []store.Scored{
		scored("low", "x", 1.0, 0.0, now),
		scored("high", "y", 1.0, 1.0, now),
	}
	out := Rerank(in, now)
	if out[0].Memory.ID != "high" {
		t.Fatalf("expected higher-importance memory first, got %s", out[0].Memory.ID)
	}
}

func TestRerankUsagePromotesReinforcedMemory(t *testing.T) {
	now := time.Now().UTC()
	// Equal relevance, recency and importance; the frequently-recalled memory
	// should edge ahead on the usage term.
	hot := scored("hot", "x", 1.0, 0.5, now)
	hot.Memory.AccessCount = 20
	cold := scored("cold", "y", 1.0, 0.5, now)
	cold.Memory.AccessCount = 0

	out := Rerank([]store.Scored{cold, hot}, now)
	if out[0].Memory.ID != "hot" {
		t.Fatalf("expected reinforced 'hot' first, got %s", out[0].Memory.ID)
	}
}

func TestRerankUsageInertWithoutAccessHistory(t *testing.T) {
	now := time.Now().UTC()
	// With no access history (the benchmark case), the usage term is uniformly
	// zero, so importance still decides — ranking is unchanged.
	in := []store.Scored{
		scored("low", "x", 1.0, 0.0, now),
		scored("high", "y", 1.0, 1.0, now),
	}
	out := Rerank(in, now)
	if out[0].Memory.ID != "high" {
		t.Fatalf("usage term must be inert without access history; got %s first", out[0].Memory.ID)
	}
}

func TestRerankStableForEqualScores(t *testing.T) {
	now := time.Now().UTC()
	in := []store.Scored{
		scored("first", "x", 1.0, 0.5, now),
		scored("second", "x", 1.0, 0.5, now),
	}
	out := Rerank(in, now)
	if out[0].Memory.ID != "first" || out[1].Memory.ID != "second" {
		t.Fatalf("equal scores should preserve input order, got %s,%s", out[0].Memory.ID, out[1].Memory.ID)
	}
}

func TestRerankEmpty(t *testing.T) {
	if got := Rerank(nil, time.Now()); got != nil {
		t.Fatalf("expected nil for empty input, got %v", got)
	}
}

func TestDedupCollapsesAndCaps(t *testing.T) {
	now := time.Now().UTC()
	in := []store.Scored{
		scored("a", "The sky is blue", 1.0, 0, now),
		scored("b", "the   sky is blue", 0.9, 0, now), // dup after normalization
		scored("c", "grass is green", 0.8, 0, now),
		scored("d", "water is wet", 0.7, 0, now),
	}
	out := Dedup(in, 2)
	if len(out) != 2 {
		t.Fatalf("expected 2 results after dedup+cap, got %d", len(out))
	}
	if out[0].Memory.ID != "a" || out[1].Memory.ID != "c" {
		t.Fatalf("dedup kept wrong/duplicate entries: %s,%s", out[0].Memory.ID, out[1].Memory.ID)
	}
}

func TestDedupNoLimit(t *testing.T) {
	now := time.Now().UTC()
	in := []store.Scored{
		scored("a", "one", 1.0, 0, now),
		scored("b", "two", 0.9, 0, now),
	}
	if got := Dedup(in, 0); len(got) != 2 {
		t.Fatalf("limit 0 should keep all, got %d", len(got))
	}
}
