package maintenance_test

import (
	"context"
	"testing"

	"github.com/eleboucher/memini/internal/maintenance"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/store"
)

// TestReembedRewritesVectors stores memories with deliberately wrong (zero)
// embeddings, then verifies Reembed rewrites every vector to the embedder's
// output so a content-matching vector search finds them.
func TestReembedRewritesVectors(t *testing.T) {
	ctx := context.Background()
	st, emb := openStoreAndFake(t)
	ts := nowFixed(t)

	contents := map[string]string{
		"a": "the sky is blue",
		"b": "ferns reproduce via spores",
		"c": "postgres uses MVCC for concurrency",
	}
	zero := make([]float32, emb.Dims())
	for id, content := range contents {
		m := &memory.Memory{
			ID: id, Namespace: dedupTestNS, Tier: memory.TierSemantic, Content: content,
			CreatedAt: ts, UpdatedAt: ts, LastAccessedAt: ts, Embedding: zero,
		}
		if err := st.Upsert(ctx, m); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}

	rep, err := maintenance.Reembed(ctx, st, emb, nil, 2, nil)
	if err != nil {
		t.Fatalf("reembed: %v", err)
	}
	if rep.Total != 3 || rep.Reembedded != 3 || rep.Namespaces != 1 {
		t.Fatalf("report = %+v, want total=3 reembedded=3 namespaces=1", rep)
	}

	// Each content now embeds to a vector that finds its own memory first.
	for id, content := range contents {
		vec, err := emb.Embed(ctx, []string{content})
		if err != nil {
			t.Fatalf("embed %s: %v", id, err)
		}
		hits, err := st.VectorSearch(ctx, dedupTestNS, vec[0], store.Filter{Now: ts}, 1)
		if err != nil {
			t.Fatalf("search %s: %v", id, err)
		}
		if len(hits) == 0 || hits[0].Memory.ID != id {
			t.Errorf("content %q: top hit = %v, want %q", content, hits, id)
		}
	}
}

// TestReembedIncludesTombstoned verifies superseded rows are re-embedded too, so
// the vector index isn't left half-migrated after a model switch.
func TestReembedIncludesTombstoned(t *testing.T) {
	ctx := context.Background()
	st, emb := openStoreAndFake(t)
	ts := nowFixed(t)

	zero := make([]float32, emb.Dims())
	m := &memory.Memory{
		ID: "dead", Namespace: dedupTestNS, Tier: memory.TierSemantic, Content: "an obsolete fact",
		CreatedAt: ts, UpdatedAt: ts, LastAccessedAt: ts, Embedding: zero,
	}
	if err := st.Upsert(ctx, m); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := st.SetSuperseded(ctx, dedupTestNS, "dead", "newer"); err != nil {
		t.Fatalf("supersede: %v", err)
	}

	rep, err := maintenance.Reembed(ctx, st, emb, []string{dedupTestNS}, 0, nil)
	if err != nil {
		t.Fatalf("reembed: %v", err)
	}
	if rep.Reembedded != 1 {
		t.Fatalf("reembedded=%d, want 1 (tombstoned row included)", rep.Reembedded)
	}

	vec, _ := emb.Embed(ctx, []string{"an obsolete fact"})
	hits, err := st.VectorSearch(ctx, dedupTestNS, vec[0],
		store.Filter{IncludeSuperseded: true, Now: ts}, 1)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) == 0 || hits[0].Memory.ID != "dead" {
		t.Errorf("tombstoned row not re-embedded: hits=%v", hits)
	}
}
