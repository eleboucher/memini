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

func TestRerankQualityPrefersCorroboratedDurableAtEqualRelevance(t *testing.T) {
	now := time.Now().UTC()
	conf := 0.9
	// Equal relevance, equal recency, no access. A corroborated semantic fact
	// outranks an episodic observation purely on quality (tier salience ×
	// confidence) — the structural anti-poisoning property.
	sem := store.Scored{Score: 1.0, Memory: &memory.Memory{
		ID: "sem", Tier: memory.TierSemantic, Importance: 0.5, Confidence: &conf, LastAccessedAt: now,
	}}
	epi := store.Scored{Score: 1.0, Memory: &memory.Memory{
		ID: "epi", Tier: memory.TierEpisodic, Importance: 0.5, LastAccessedAt: now,
	}}
	out := Rerank([]store.Scored{epi, sem}, now)
	if out[0].Memory.ID != "sem" {
		t.Fatalf("expected the corroborated semantic fact first, got %s", out[0].Memory.ID)
	}
}

func TestRerankQualityRecoversGoldFromRelevantDebris(t *testing.T) {
	now := time.Now().UTC()
	conf := 0.9
	// The gold answer is a corroborated semantic fact of slightly-lower raw
	// relevance than a flood of episodic debris that matches the query terms
	// (the poisoning case). Relevance-only ranking buries it; the quality term
	// should pull it up.
	gold := store.Scored{Score: 0.70, Memory: &memory.Memory{
		ID: "gold", Tier: memory.TierSemantic, Importance: 0.7, Confidence: &conf, LastAccessedAt: now,
	}}
	results := make([]store.Scored, 0, 9)
	results = append(results, gold)
	for i := range 8 {
		results = append(results, store.Scored{Score: 0.74, Memory: &memory.Memory{
			ID: "debris" + string(rune('a'+i)), Tier: memory.TierEpisodic, Importance: 0.1, LastAccessedAt: now,
		}})
	}

	posOf := func(res []store.Scored, id string) int {
		for i, r := range res {
			if r.Memory.ID == id {
				return i
			}
		}
		return -1
	}

	relevanceOnly := RerankWith(results, now, RerankWeights{Relevance: 1})
	quality := Rerank(results, now)

	relPos := posOf(relevanceOnly, "gold")
	qPos := posOf(quality, "gold")
	if qPos >= relPos {
		t.Fatalf("quality ranking should lift gold above the debris: relevance-only pos=%d, quality pos=%d", relPos, qPos)
	}
	if qPos != 0 {
		t.Errorf("expected gold first under quality ranking, got position %d", qPos)
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
