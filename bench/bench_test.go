//go:build bench

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
	for _, sys := range bench.MeminiSystems(st, embedtest.New(dims), 4, "", -1, 0, 0, bench.IngestUpsert, nil) {
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

// TestWriteModeIngest exercises -ingest=write end-to-end offline: items enter
// via service.Remember (which generates its own IDs), so a gold hit proves the
// memory-ID -> item-ID alias translation works, and the duplicated item proves
// fingerprint-merged items keep answering for both IDs.
func TestWriteModeIngest(t *testing.T) {
	ctx := context.Background()
	// Contents must clear the episodic low-signal gate (>= 120 signal chars) and
	// stay lexically distinct from each other so the write path's contradiction
	// detector has no reason to pair them.
	const releaseDoc = "The billing service release process builds the container image first, runs the integration " +
		"suite against a disposable database, and only then promotes the release tag to the production registry."
	const grafanaDoc = "Grafana dashboards for the ingest cluster live under the observability folder; every panel " +
		"alerts to the on-call channel and the datasource is the central victoria-metrics instance."
	ds := &bench.Dataset{
		Name: "write-mode-sample",
		Items: []bench.Item{
			{ID: "it-1", Group: "g1", Content: releaseDoc},
			{ID: "it-2", Group: "g1", Content: grafanaDoc},
			// Exact duplicate of it-2: the fingerprint path must merge it onto the
			// same stored memory, aliasing both item IDs.
			{ID: "it-2-dup", Group: "g1", Content: grafanaDoc},
		},
		Questions: []bench.Question{
			{Query: "where do the grafana dashboards for the ingest cluster live", Gold: []string{"it-2-dup"}, Group: "g1"},
		},
	}

	const dims = 256
	st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "bench.db"), dims)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	sys := bench.MeminiSystems(st, embedtest.New(dims), 4, "", 0.5, 0, 0, bench.IngestWrite, nil)[0] // hybrid
	rs, err := bench.Run(ctx, sys, ds, []int{3})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if rs[0].RecallAtK != 1 {
		t.Fatalf("write-mode hybrid recall@3 = %.2f, want 1 (alias translation or fingerprint merge broken)", rs[0].RecallAtK)
	}
	if rs[0].TokenEfficiency <= 0 {
		t.Fatalf("token efficiency = %v, want > 0", rs[0].TokenEfficiency)
	}
}
