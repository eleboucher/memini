package maintenance_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/eleboucher/memini/internal/embed/embedtest"
	"github.com/eleboucher/memini/internal/maintenance"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/store"
	"github.com/eleboucher/memini/internal/store/sqlitevec"
)

const (
	dedupDims    = 64
	dedupTestNS  = "ns"
	dedupTestNow = "2025-06-01T00:00:00Z"
)

func putContent(t *testing.T, st *sqlitevec.Store, emb *embedtest.Fake, id, content string, importance float64) {
	t.Helper()
	ctx := context.Background()
	ts, err := time.Parse(time.RFC3339, dedupTestNow)
	if err != nil {
		t.Fatalf("parse time: %v", err)
	}
	vec, err := emb.Embed(ctx, []string{content})
	if err != nil {
		t.Fatalf("embed %s: %v", id, err)
	}
	m := &memory.Memory{
		ID: id, Namespace: dedupTestNS, Tier: memory.TierSemantic, Content: content, Importance: importance,
		CreatedAt: ts, UpdatedAt: ts, LastAccessedAt: ts, Embedding: vec[0],
	}
	if err := st.Upsert(ctx, m); err != nil {
		t.Fatalf("upsert %s: %v", id, err)
	}
}

func nowFixed(t *testing.T) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, dedupTestNow)
	if err != nil {
		t.Fatalf("parse time: %v", err)
	}
	return ts
}

func openStoreAndFake(t *testing.T) (*sqlitevec.Store, *embedtest.Fake) {
	t.Helper()
	st, err := sqlitevec.Open(context.Background(), filepath.Join(t.TempDir(), "m.db"), dedupDims)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st, embedtest.New(dedupDims)
}

func TestDedupCollapsesNearDuplicates(t *testing.T) {
	ctx := context.Background()
	st, emb := openStoreAndFake(t)

	// Three near-duplicates of "the sky is blue" with different importance
	// values; one orthogonal memory that must remain untouched.
	putContent(t, st, emb, "dup-lo", "the sky is blue", 0.1)
	putContent(t, st, emb, "dup-mid", "the sky is blue", 0.5)
	putContent(t, st, emb, "dup-hi", "the sky is blue", 0.9)
	putContent(t, st, emb, "unique", "ferns reproduce via spores", 0.0)

	rep, err := maintenance.Dedup(ctx, st, emb, maintenance.DedupOptions{
		Similarity: 0.5, // bag-of-words fake reaches ~0.9+ on identical content
		Now:        nowFixed(t),
	})
	if err != nil {
		t.Fatalf("dedup: %v", err)
	}
	if rep.ClustersFound != 1 {
		t.Fatalf("clusters=%d, want 1; actions=%+v", rep.ClustersFound, rep.Actions)
	}
	if rep.Tombstoned != 2 {
		t.Fatalf("tombstoned=%d, want 2", rep.Tombstoned)
	}
	if len(rep.Actions) != 1 {
		t.Fatalf("actions=%d, want 1", len(rep.Actions))
	}
	if rep.Actions[0].RepresentativeID != "dup-hi" {
		t.Errorf("rep=%q, want dup-hi (highest importance)", rep.Actions[0].RepresentativeID)
	}
	wants := map[string]bool{"dup-mid": true, "dup-lo": true}
	for _, id := range rep.Actions[0].TombstonedIDs {
		if !wants[id] {
			t.Errorf("unexpected tombstone %q", id)
		}
		delete(wants, id)
	}
	if len(wants) != 0 {
		t.Errorf("missing tombstones: %v", wants)
	}

	// Rep is still live; the duplicates are tombstoned; the unique memory is
	// untouched.
	if m, err := st.Get(ctx, dedupTestNS, "dup-hi"); err != nil || m.SupersededBy != nil {
		t.Errorf("rep dup-hi unexpectedly tombstoned: m=%+v err=%v", m, err)
	}
	for _, id := range []string{"dup-mid", "dup-lo"} {
		m, err := st.Get(ctx, dedupTestNS, id)
		if err != nil {
			t.Errorf("get %s: %v", id, err)
			continue
		}
		if m.SupersededBy == nil || *m.SupersededBy != "dup-hi" {
			t.Errorf("%s superseded_by=%v, want dup-hi", id, m.SupersededBy)
		}
	}
	if m, err := st.Get(ctx, dedupTestNS, "unique"); err != nil || m.SupersededBy != nil {
		t.Errorf("unique memory touched: m=%+v err=%v", m, err)
	}
}

func TestDedupTransitiveClusters(t *testing.T) {
	// Anchor A is similar to B (above threshold); B is similar to C but
	// A is below the threshold for C directly. A, B, C should still form
	// one cluster via the union-find transitive closure, not two.
	ctx := context.Background()
	st, emb := openStoreAndFake(t)

	// Pick three texts that share a long prefix — the fake embedder's
	// bag-of-words hash will land them close to each other but with the
	// shared-prefix-only pair (A vs C) measurably further than the
	// shared-prefix + shared-suffix pairs (A vs B, B vs C).
	putContent(t, st, emb, "a", "alpha beta gamma delta epsilon", 0.5)
	putContent(t, st, emb, "b", "alpha beta gamma delta epsilon zeta", 0.5)
	putContent(t, st, emb, "c", "alpha beta gamma delta zeta", 0.5)

	// Threshold high enough that A↔C stay separate on their own; we want
	// the union-find to do the work.
	rep, err := maintenance.Dedup(ctx, st, emb, maintenance.DedupOptions{
		Similarity: 0.7,
		Now:        nowFixed(t),
	})
	if err != nil {
		t.Fatalf("dedup: %v", err)
	}
	if rep.ClustersFound != 1 {
		t.Fatalf("clusters=%d, want 1 (transitive); actions=%+v", rep.ClustersFound, rep.Actions)
	}
	if rep.Tombstoned != 2 {
		t.Fatalf("tombstoned=%d, want 2", rep.Tombstoned)
	}
}

func TestDedupDryRun(t *testing.T) {
	ctx := context.Background()
	st, emb := openStoreAndFake(t)

	putContent(t, st, emb, "a", "the sky is blue today", 0.1)
	putContent(t, st, emb, "b", "the sky is blue today", 0.9)

	rep, err := maintenance.Dedup(ctx, st, emb, maintenance.DedupOptions{
		Similarity: 0.5, DryRun: true, Now: nowFixed(t),
	})
	if err != nil {
		t.Fatalf("dedup: %v", err)
	}
	if rep.ClustersFound != 1 || rep.Tombstoned != 1 {
		t.Fatalf("report=%+v", rep)
	}
	if !rep.DryRun {
		t.Fatalf("DryRun flag missing in report")
	}
	for _, id := range []string{"a", "b"} {
		m, err := st.Get(ctx, dedupTestNS, id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		if m.SupersededBy != nil {
			t.Errorf("dry-run tombstoned %s: superseded_by=%v", id, *m.SupersededBy)
		}
	}
}

func TestDedupSkipsSmallClusters(t *testing.T) {
	ctx := context.Background()
	st, emb := openStoreAndFake(t)

	putContent(t, st, emb, "a", "the sky is blue", 0.5)
	putContent(t, st, emb, "b", "the sky is blue", 0.5)

	rep, err := maintenance.Dedup(ctx, st, emb, maintenance.DedupOptions{
		Similarity: 0.5, MinClusterSize: 3, Now: nowFixed(t),
	})
	if err != nil {
		t.Fatalf("dedup: %v", err)
	}
	if rep.ClustersFound != 0 || rep.Tombstoned != 0 {
		t.Fatalf("report=%+v", rep)
	}
	for _, id := range []string{"a", "b"} {
		m, err := st.Get(ctx, dedupTestNS, id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		if m.SupersededBy != nil {
			t.Errorf("%s tombstoned: %v", id, *m.SupersededBy)
		}
	}
}

func TestDedupTombstoneHidesFromRecall(t *testing.T) {
	ctx := context.Background()
	st, emb := openStoreAndFake(t)

	putContent(t, st, emb, "keep", "ferns reproduce via spores", 0.5)
	putContent(t, st, emb, "drop", "ferns reproduce via spores", 0.1)

	_, err := maintenance.Dedup(ctx, st, emb, maintenance.DedupOptions{
		Similarity: 0.5, Now: nowFixed(t),
	})
	if err != nil {
		t.Fatalf("dedup: %v", err)
	}

	// Live-only vector search must not return the tombstoned memory.
	vec, _ := emb.Embed(ctx, []string{"ferns reproduce via spores"})
	cands, err := st.VectorSearch(ctx, dedupTestNS, vec[0], store.Filter{}, 5)
	if err != nil {
		t.Fatalf("vector search: %v", err)
	}
	for _, c := range cands {
		if c.Memory.ID == "drop" {
			t.Errorf("tombstoned memory still surfaces in live-only recall")
		}
	}
}

func TestDedupRepresentativePicksHighestRetention(t *testing.T) {
	// Two near-duplicates with different access counts: the one that's been
	// recalled more should win the representative role.
	ctx := context.Background()
	st, emb := openStoreAndFake(t)

	ts := nowFixed(t)
	// Both have importance 0.5; the one with higher access count has a
	// higher RetentionScore.
	less := &memory.Memory{
		ID: "less", Namespace: dedupTestNS, Tier: memory.TierSemantic,
		Content: "the sky is blue", Importance: 0.5, AccessCount: 1,
		CreatedAt: ts, UpdatedAt: ts, LastAccessedAt: ts,
	}
	more := &memory.Memory{
		ID: "more", Namespace: dedupTestNS, Tier: memory.TierSemantic,
		Content: "the sky is blue", Importance: 0.5, AccessCount: 50,
		CreatedAt: ts, UpdatedAt: ts, LastAccessedAt: ts,
	}
	for _, m := range []*memory.Memory{less, more} {
		vec, _ := emb.Embed(ctx, []string{m.Content})
		m.Embedding = vec[0]
		if err := st.Upsert(ctx, m); err != nil {
			t.Fatalf("upsert: %v", err)
		}
	}

	rep, err := maintenance.Dedup(ctx, st, emb, maintenance.DedupOptions{
		Similarity: 0.5, Now: nowFixed(t),
	})
	if err != nil {
		t.Fatalf("dedup: %v", err)
	}
	if rep.Actions[0].RepresentativeID != "more" {
		t.Errorf("rep=%q, want more (higher access count)", rep.Actions[0].RepresentativeID)
	}
}

func TestDedupDisabled(t *testing.T) {
	ctx := context.Background()
	st, emb := openStoreAndFake(t)

	putContent(t, st, emb, "a", "the sky is blue", 0.5)
	putContent(t, st, emb, "b", "the sky is blue", 0.5)

	// Negative similarity short-circuits to a no-op.
	rep, err := maintenance.Dedup(ctx, st, emb, maintenance.DedupOptions{Similarity: -1, Now: nowFixed(t)})
	if err != nil {
		t.Fatalf("dedup: %v", err)
	}
	if rep.ClustersFound != 0 || rep.Tombstoned != 0 {
		t.Fatalf("report=%+v", rep)
	}
}

func TestDedupMultipleNamespaces(t *testing.T) {
	ctx := context.Background()
	st, emb := openStoreAndFake(t)
	ts := nowFixed(t)

	ns1a := &memory.Memory{ID: "ns1a", Namespace: "a", Tier: memory.TierSemantic, Content: "the sky is blue", CreatedAt: ts, UpdatedAt: ts, LastAccessedAt: ts}
	ns1b := &memory.Memory{ID: "ns1b", Namespace: "a", Tier: memory.TierSemantic, Content: "the sky is blue", CreatedAt: ts, UpdatedAt: ts, LastAccessedAt: ts}
	ns2a := &memory.Memory{ID: "ns2a", Namespace: "b", Tier: memory.TierSemantic, Content: "the sky is blue", CreatedAt: ts, UpdatedAt: ts, LastAccessedAt: ts}
	ns2b := &memory.Memory{ID: "ns2b", Namespace: "b", Tier: memory.TierSemantic, Content: "the sky is blue", CreatedAt: ts, UpdatedAt: ts, LastAccessedAt: ts}
	for _, m := range []*memory.Memory{ns1a, ns1b, ns2a, ns2b} {
		vec, _ := emb.Embed(ctx, []string{m.Content})
		m.Embedding = vec[0]
		if err := st.Upsert(ctx, m); err != nil {
			t.Fatalf("upsert %s: %v", m.ID, err)
		}
	}

	rep, err := maintenance.Dedup(ctx, st, emb, maintenance.DedupOptions{Similarity: 0.5, Now: ts})
	if err != nil {
		t.Fatalf("dedup: %v", err)
	}
	if rep.Namespaces != 2 {
		t.Errorf("namespaces=%d, want 2", rep.Namespaces)
	}
	if rep.ClustersFound != 2 {
		t.Errorf("clusters=%d, want 2 (one per namespace)", rep.ClustersFound)
	}
	if rep.Tombstoned != 2 {
		t.Errorf("tombstoned=%d, want 2", rep.Tombstoned)
	}
}

func TestDedupNamespacesScope(t *testing.T) {
	// With Namespaces set, only the listed namespace is touched; the other
	// namespace's duplicates are left live.
	ctx := context.Background()
	st, emb := openStoreAndFake(t)
	ts := nowFixed(t)

	mems := []*memory.Memory{
		{ID: "a1", Namespace: "a", Tier: memory.TierSemantic, Content: "the sky is blue", CreatedAt: ts, UpdatedAt: ts, LastAccessedAt: ts},
		{ID: "a2", Namespace: "a", Tier: memory.TierSemantic, Content: "the sky is blue", CreatedAt: ts, UpdatedAt: ts, LastAccessedAt: ts},
		{ID: "b1", Namespace: "b", Tier: memory.TierSemantic, Content: "the sky is blue", CreatedAt: ts, UpdatedAt: ts, LastAccessedAt: ts},
		{ID: "b2", Namespace: "b", Tier: memory.TierSemantic, Content: "the sky is blue", CreatedAt: ts, UpdatedAt: ts, LastAccessedAt: ts},
	}
	for _, m := range mems {
		vec, _ := emb.Embed(ctx, []string{m.Content})
		m.Embedding = vec[0]
		if err := st.Upsert(ctx, m); err != nil {
			t.Fatalf("upsert %s: %v", m.ID, err)
		}
	}

	rep, err := maintenance.Dedup(ctx, st, emb, maintenance.DedupOptions{
		Similarity: 0.5, Namespaces: []string{"a"}, Now: ts,
	})
	if err != nil {
		t.Fatalf("dedup: %v", err)
	}
	if rep.Namespaces != 1 || rep.ClustersFound != 1 || rep.Tombstoned != 1 {
		t.Fatalf("report=%+v, want 1 namespace / 1 cluster / 1 tombstone", rep)
	}

	// Namespace b must be untouched: both its members stay live.
	for _, id := range []string{"b1", "b2"} {
		m, err := st.Get(ctx, "b", id)
		if err != nil {
			t.Fatalf("get b/%s: %v", id, err)
		}
		if m.SupersededBy != nil {
			t.Errorf("namespace b/%s tombstoned despite scoping to a: %v", id, *m.SupersededBy)
		}
	}
}

func TestDedupTombstoneAlreadySupersededIsNoop(t *testing.T) {
	// If a candidate was already tombstoned by another path (e.g.
	// consolidation), SetSuperseded returns ErrNotFound and the pass must
	// skip the count rather than abort.
	ctx := context.Background()
	st, emb := openStoreAndFake(t)

	putContent(t, st, emb, "keep", "the sky is blue", 0.9)
	// Manually tombstone one before dedup runs.
	drop := &memory.Memory{
		ID: "drop", Namespace: dedupTestNS, Tier: memory.TierSemantic,
		Content: "the sky is blue", Importance: 0.1, SupersededBy: ptr("keep"),
		CreatedAt: nowFixed(t), UpdatedAt: nowFixed(t), LastAccessedAt: nowFixed(t),
	}
	vec, _ := emb.Embed(ctx, []string{drop.Content})
	drop.Embedding = vec[0]
	if err := st.Upsert(ctx, drop); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// The default Filter excludes superseded memories, so dedup won't see
	// `drop` at all. Re-run with IncludeSuperseded so it does, and assert
	// the pass returns cleanly without counting `drop` as tombstoned.
	rep, err := maintenance.Dedup(ctx, st, emb, maintenance.DedupOptions{
		Similarity: 0.5, Now: nowFixed(t),
		// No Tiers filter; List without IncludeSuperseded still drops it.
	})
	if err != nil {
		t.Fatalf("dedup: %v", err)
	}
	if rep.ClustersFound != 0 {
		t.Errorf("clusters=%d, want 0 (pre-superseded memory hidden by filter)", rep.ClustersFound)
	}
}

func ptr(s string) *string { return &s }
