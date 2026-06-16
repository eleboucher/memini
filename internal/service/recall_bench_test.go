package service_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/eleboucher/memini/internal/service"
	"github.com/eleboucher/memini/internal/store"
	"github.com/eleboucher/memini/internal/store/sqlitevec"
)

// sleepyEmbedder simulates a fixed network latency per embed call.
type sleepyEmbedder struct{ d time.Duration }

func (e sleepyEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	time.Sleep(e.d)
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = make([]float32, dims)
	}
	return out, nil
}
func (e sleepyEmbedder) Dims() int { return dims }

// sleepyStore simulates a fixed latency per search leg and returns no hits, so
// the benchmark isolates orchestration wall-clock from store internals.
type sleepyStore struct {
	store.Store
	d time.Duration
}

func (s sleepyStore) VectorSearch(_ context.Context, _ string, _ []float32, _ store.Filter, _ int) ([]store.Scored, error) {
	time.Sleep(s.d)
	return nil, nil
}
func (s sleepyStore) KeywordSearch(_ context.Context, _, _ string, _ store.Filter, _ int) ([]store.Scored, error) {
	time.Sleep(s.d)
	return nil, nil
}

// BenchmarkRecall reports recall wall-clock with fixed embed (50ms) and per-leg
// search (30ms) latencies. With the legs running concurrently a single-namespace
// recall costs ~embed + one search leg (~80ms) rather than the sequential
// embed + both legs (~110ms).
func BenchmarkRecall(b *testing.B) {
	st, err := sqlitevec.Open(context.Background(), filepath.Join(b.TempDir(), "bench.db"), dims)
	if err != nil {
		b.Fatalf("open store: %v", err)
	}
	b.Cleanup(func() { _ = st.Close() })

	svc := service.New(
		sleepyStore{Store: st, d: 30 * time.Millisecond},
		sleepyEmbedder{d: 50 * time.Millisecond},
		service.WithSyncReinforce(),
	)
	in := service.RecallInput{Namespace: "alice", Query: "hello", Limit: 5}

	for b.Loop() {
		if _, err := svc.Recall(context.Background(), in); err != nil {
			b.Fatalf("recall: %v", err)
		}
	}
}
