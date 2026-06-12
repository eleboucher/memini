package bench_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/eleboucher/memini/bench"
	"github.com/eleboucher/memini/internal/embed/embedtest"
	"github.com/eleboucher/memini/internal/maintenance"
	"github.com/eleboucher/memini/internal/store/sqlitevec"
)

// TestDedupRecoversPoisonedStore models a low-quality bulk import (a cluster of
// near-duplicate debris) collapsed into each namespace, and asserts the shipped
// dedup pass collapses the debris while leaving the real memories — and their
// recall — intact.
func TestDedupRecoversPoisonedStore(t *testing.T) {
	ctx := context.Background()
	ds, err := bench.Sample()
	if err != nil {
		t.Fatalf("sample: %v", err)
	}
	const debrisPerGroup = 50
	poisoned := bench.Poison(ds, debrisPerGroup, "")

	const dims = 256
	st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "poison.db"), dims)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	sys := bench.MeminiSystems(st, embedtest.New(dims), 4, "", -1, 0, 0)[0] // hybrid
	if err := sys.Ingest(ctx, poisoned.Items); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	// The bench maps an empty group to its own namespace; resolve it from the
	// store rather than assuming the literal group string.
	names, err := st.ListNamespaces(ctx)
	if err != nil || len(names) != 1 {
		t.Fatalf("expected one namespace, got %v (err %v)", names, err)
	}
	ns := names[0]

	// Pick a question whose gold is retrievable before dedup.
	q := poisoned.Questions[0]
	before, err := sys.Recall(ctx, q.Group, q.Query, 5)
	if err != nil {
		t.Fatalf("recall before: %v", err)
	}

	// Run the dedup pass over the poisoned namespace.
	rep, err := maintenance.Dedup(ctx, st, embedtest.New(dims), maintenance.DedupOptions{
		Namespaces: []string{ns},
		Similarity: 0.9,
	})
	if err != nil {
		t.Fatalf("dedup: %v", err)
	}
	// The debris cluster (identical content) should collapse to one representative.
	if rep.Tombstoned < debrisPerGroup-1 {
		t.Fatalf("dedup tombstoned %d, want >= %d (the debris cluster)", rep.Tombstoned, debrisPerGroup-1)
	}

	// Gold items must survive dedup and stay recallable.
	for _, g := range q.Gold {
		if _, err := st.Get(ctx, ns, g); err != nil {
			t.Errorf("gold %q must survive dedup: %v", g, err)
		}
	}
	after, err := sys.Recall(ctx, q.Group, q.Query, 5)
	if err != nil {
		t.Fatalf("recall after: %v", err)
	}
	if goldHits(after, q.Gold) < goldHits(before, q.Gold) {
		t.Fatalf("dedup must not reduce gold recall: before=%v after=%v gold=%v", before, after, q.Gold)
	}
}

func goldHits(got, gold []string) int {
	set := map[string]struct{}{}
	for _, g := range gold {
		set[g] = struct{}{}
	}
	n := 0
	for _, id := range got {
		if _, ok := set[id]; ok {
			n++
		}
	}
	return n
}
