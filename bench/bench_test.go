package bench_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/eleboucher/memini/bench"
	"github.com/eleboucher/memini/internal/embed/embedtest"
	"github.com/eleboucher/memini/internal/store/sqlitevec"
)

func TestSampleBenchmark(t *testing.T) {
	ctx := context.Background()
	ds, err := bench.Sample()
	if err != nil {
		t.Fatalf("sample: %v", err)
	}

	const dims = 256
	st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "bench.db"), dims)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	results := map[string]bench.Result{}
	for _, sys := range bench.MeminiSystems(st, embedtest.New(dims), 4) {
		rs, err := bench.Run(ctx, sys, ds, []int{3})
		if err != nil {
			t.Fatalf("run %s: %v", sys.Name(), err)
		}
		results[sys.Name()] = rs[0]
	}

	hybrid := results["memini-hybrid"]
	vector := results["memini-vector"]
	keyword := results["memini-keyword"]

	if keyword.RecallAtK == 0 {
		t.Fatal("keyword recall is 0 — OR-tokenization regression")
	}
	// Hybrid fusion should never do worse than either single strategy.
	if hybrid.RecallAtK < vector.RecallAtK || hybrid.RecallAtK < keyword.RecallAtK {
		t.Fatalf("hybrid (%.3f) should be >= vector (%.3f) and keyword (%.3f)",
			hybrid.RecallAtK, vector.RecallAtK, keyword.RecallAtK)
	}
	if hybrid.RecallAtK == 0 {
		t.Fatal("hybrid recall is 0 on the sample")
	}

	if md := bench.Markdown([]bench.Result{hybrid, vector, keyword}); len(md) == 0 {
		t.Fatal("empty markdown")
	}
}
