package service_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eleboucher/memini/internal/embed/embedtest"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/service"
	"github.com/eleboucher/memini/internal/store"
)

// blockingStore parks both search legs on entry until release is closed, so a
// test can prove the legs are in flight at the same time.
type blockingStore struct {
	store.Store
	entered chan struct{}
	release chan struct{}
}

func (b *blockingStore) VectorSearch(ctx context.Context, ns string, vec []float32, f store.Filter, k int) ([]store.Scored, error) {
	b.entered <- struct{}{}
	<-b.release
	return b.Store.VectorSearch(ctx, ns, vec, f, k)
}

func (b *blockingStore) KeywordSearch(ctx context.Context, ns, query string, f store.Filter, k int) ([]store.Scored, error) {
	b.entered <- struct{}{}
	<-b.release
	return b.Store.KeywordSearch(ctx, ns, query, f, k)
}

// TestRecallSearchLegsRunConcurrently proves the vector and keyword legs run at
// the same time: both must enter the store before either is released, which is
// impossible if the legs run sequentially.
func TestRecallSearchLegsRunConcurrently(t *testing.T) {
	st := openTestStore(t)
	seed := service.New(st, embedtest.New(dims), service.WithSyncReinforce())
	if _, err := seed.Remember(context.Background(), service.RememberInput{Namespace: "alice", Content: "hello world", Tier: memory.TierSemantic}); err != nil {
		t.Fatalf("seed remember: %v", err)
	}

	bs := &blockingStore{Store: st, entered: make(chan struct{}, 2), release: make(chan struct{})}
	svc := service.New(bs, embedtest.New(dims), service.WithSyncReinforce())

	done := make(chan error, 1)
	go func() {
		_, err := svc.Recall(context.Background(), service.RecallInput{Namespace: "alice", Query: "hello", Limit: 5})
		done <- err
	}()

	for range 2 {
		select {
		case <-bs.entered:
		case <-time.After(2 * time.Second):
			t.Fatal("only one search leg ran before release; legs are not concurrent")
		}
	}
	close(bs.release)
	if err := <-done; err != nil {
		t.Fatalf("recall: %v", err)
	}
}

// errEmbedder fails every embed call.
type errEmbedder struct{ dims int }

func (e errEmbedder) Embed(context.Context, []string) ([][]float32, error) {
	return nil, errors.New("embed boom")
}
func (e errEmbedder) Dims() int { return e.dims }

// countingMetrics counts RecallResult("error", ...) calls; everything else is a
// no-op. Safe for concurrent use.
type countingMetrics struct {
	mu        sync.Mutex
	recallErr int
}

func (m *countingMetrics) RecallResult(result, _, _ string) {
	if result == "error" {
		m.mu.Lock()
		m.recallErr++
		m.mu.Unlock()
	}
}
func (m *countingMetrics) ConsolidateResult(string)         {}
func (m *countingMetrics) ConsolidateQueueDepth(int)        {}
func (m *countingMetrics) RememberResult(string, string)    {}
func (m *countingMetrics) ForgetResult(string)              {}
func (m *countingMetrics) PromoteResult(string, int)        {}
func (m *countingMetrics) FsckResult(string)                {}
func (m *countingMetrics) OpDuration(string, time.Duration) {}
func (m *countingMetrics) AnswerResult(string)              {}
func (m *countingMetrics) RerankResult(string, string)      {}
func (m *countingMetrics) ReinforceResult(string)           {}
func (m *countingMetrics) DedupTombstoned(int)              {}

// TestRecallEmbedErrorFailsOnce confirms an embed failure still hard-fails
// recall with the original wrap and reports the error metric exactly once.
func TestRecallEmbedErrorFailsOnce(t *testing.T) {
	st := openTestStore(t)
	m := &countingMetrics{}
	svc := service.New(st, errEmbedder{dims: dims}, service.WithSyncReinforce(), service.WithMetrics(m))

	_, err := svc.Recall(context.Background(), service.RecallInput{Namespace: "alice", Query: "hello", Limit: 5})
	if err == nil || !strings.Contains(err.Error(), "recall: embed:") {
		t.Fatalf("want a wrapped recall: embed error, got %v", err)
	}
	if m.recallErr != 1 {
		t.Fatalf("RecallResult(error) fired %d times, want exactly 1", m.recallErr)
	}
}

// vectorErrStore fails the vector leg.
type vectorErrStore struct{ store.Store }

func (vectorErrStore) VectorSearch(context.Context, string, []float32, store.Filter, int) ([]store.Scored, error) {
	return nil, errors.New("vector boom")
}

// TestRecallVectorSearchErrorWraps confirms a failing search leg aborts recall
// with its original wrap when the legs run concurrently.
func TestRecallVectorSearchErrorWraps(t *testing.T) {
	st := openTestStore(t)
	svc := service.New(vectorErrStore{Store: st}, embedtest.New(dims), service.WithSyncReinforce())

	_, err := svc.Recall(context.Background(), service.RecallInput{Namespace: "alice", Query: "hello", Limit: 5})
	if err == nil || !strings.Contains(err.Error(), "recall: vector search:") {
		t.Fatalf("want a wrapped recall: vector search error, got %v", err)
	}
}
