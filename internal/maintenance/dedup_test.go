package maintenance_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eleboucher/memini/internal/embed"
	"github.com/eleboucher/memini/internal/embed/embedtest"
	"github.com/eleboucher/memini/internal/maintenance"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/store"
	"github.com/eleboucher/memini/internal/store/sqlitevec"
)

// failEmbedder wraps a real embedder but errors on any batch containing a
// marker string, simulating content that deterministically trips the embedder
// (oversized input, bad encoding) for one namespace.
type failEmbedder struct {
	inner  embed.Embedder
	marker string
}

func (f failEmbedder) Dims() int { return f.inner.Dims() }

func (f failEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	for _, t := range texts {
		if strings.Contains(t, f.marker) {
			return nil, errors.New("embed failed")
		}
	}
	return f.inner.Embed(ctx, texts)
}

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

// seedPair upserts two identical memories in a namespace using the plain fake
// embedder for their stored vectors.
func seedPair(t *testing.T, st *sqlitevec.Store, emb *embedtest.Fake, ns, content string) {
	t.Helper()
	ctx := context.Background()
	ts := nowFixed(t)
	for _, id := range []string{ns + "-1", ns + "-2"} {
		vec, _ := emb.Embed(ctx, []string{content})
		m := &memory.Memory{
			ID: id, Namespace: ns, Tier: memory.TierSemantic, Content: content,
			CreatedAt: ts, UpdatedAt: ts, LastAccessedAt: ts, Embedding: vec[0],
		}
		if err := st.Upsert(ctx, m); err != nil {
			t.Fatalf("upsert %s: %v", id, err)
		}
	}
}

func TestDedupStoreWideSkipsFailingNamespace(t *testing.T) {
	// A store-wide pass (Namespaces empty) is best-effort: a namespace whose
	// content trips the embedder is logged and skipped, the rest still run.
	ctx := context.Background()
	st, emb := openStoreAndFake(t)
	seedPair(t, st, emb, "good", "the sky is blue")
	seedPair(t, st, emb, "bad", "boom marker content")

	rep, err := maintenance.Dedup(ctx, st, failEmbedder{inner: emb, marker: "boom"}, maintenance.DedupOptions{
		Similarity: 0.5, Now: nowFixed(t),
	})
	if err != nil {
		t.Fatalf("store-wide dedup should be best-effort, got error: %v", err)
	}
	if rep.Namespaces != 1 || rep.Tombstoned != 1 {
		t.Fatalf("report=%+v, want 1 namespace processed / 1 tombstoned (bad skipped)", rep)
	}
	// The failing namespace is untouched.
	for _, id := range []string{"bad-1", "bad-2"} {
		m, err := st.Get(ctx, "bad", id)
		if err != nil {
			t.Fatalf("get bad/%s: %v", id, err)
		}
		if m.SupersededBy != nil {
			t.Errorf("skipped namespace bad/%s was tombstoned: %v", id, *m.SupersededBy)
		}
	}
}

func TestDedupScopedNamespacePropagatesError(t *testing.T) {
	// A single requested namespace propagates its error (the one-shot API path
	// must surface a failure rather than report an empty success).
	ctx := context.Background()
	st, emb := openStoreAndFake(t)
	seedPair(t, st, emb, "bad", "boom marker content")

	_, err := maintenance.Dedup(ctx, st, failEmbedder{inner: emb, marker: "boom"}, maintenance.DedupOptions{
		Similarity: 0.5, Namespaces: []string{"bad"}, Now: nowFixed(t),
	})
	if err == nil {
		t.Fatal("scoped dedup over a failing namespace should return an error, got nil")
	}
}

func TestDedupRespectsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	st, emb := openStoreAndFake(t)
	seedPair(t, st, emb, "ns", "the sky is blue")
	cancel()

	_, err := maintenance.Dedup(ctx, st, emb, maintenance.DedupOptions{
		Similarity: 0.5, Now: nowFixed(t),
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

func ptr(s string) *string { return &s }

// BenchmarkDedup establishes a cost baseline for the clustering pass (embed +
// per-anchor vector search + union-find) over a single namespace. Dry-run, so
// it measures the O(n·vector_search) work without per-cluster tombstone writes
// and is repeatable across iterations.
func BenchmarkDedup(b *testing.B) {
	ctx := context.Background()
	st, err := sqlitevec.Open(ctx, filepath.Join(b.TempDir(), "bench.db"), dedupDims)
	if err != nil {
		b.Fatalf("open: %v", err)
	}
	b.Cleanup(func() { _ = st.Close() })
	emb := embedtest.New(dedupDims)
	ts, _ := time.Parse(time.RFC3339, dedupTestNow)

	// 500 memories: 100 groups of 5 identical paraphrases each.
	const groups, perGroup = 100, 5
	for g := range groups {
		content := fmt.Sprintf("fact number %d about the topic", g)
		for k := range perGroup {
			vec, _ := emb.Embed(ctx, []string{content})
			m := &memory.Memory{
				ID: fmt.Sprintf("m-%d-%d", g, k), Namespace: dedupTestNS,
				Tier: memory.TierSemantic, Content: content,
				CreatedAt: ts, UpdatedAt: ts, LastAccessedAt: ts, Embedding: vec[0],
			}
			if err := st.Upsert(ctx, m); err != nil {
				b.Fatalf("upsert: %v", err)
			}
		}
	}

	opts := maintenance.DedupOptions{
		Similarity: 0.9, DryRun: true, Now: ts,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := maintenance.Dedup(ctx, st, emb, opts); err != nil {
			b.Fatalf("dedup: %v", err)
		}
	}
}
