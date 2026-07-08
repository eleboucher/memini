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

// countingMetrics counts recall error and keyword-only-degrade events;
// everything else is a no-op. Safe for concurrent use.
type countingMetrics struct {
	mu                   sync.Mutex
	recallErr            int
	degraded             map[string]int
	rememberDegraded     map[string]int
	embedBackfillPending int
	embedBackfillCalls   int
}

func (m *countingMetrics) RecallResult(result, _, _ string) {
	if result == "error" {
		m.mu.Lock()
		m.recallErr++
		m.mu.Unlock()
	}
}
func (m *countingMetrics) RecallDegraded(reason string) {
	m.mu.Lock()
	if m.degraded == nil {
		m.degraded = map[string]int{}
	}
	m.degraded[reason]++
	m.mu.Unlock()
}
func (m *countingMetrics) RememberDegraded(reason string) {
	m.mu.Lock()
	if m.rememberDegraded == nil {
		m.rememberDegraded = map[string]int{}
	}
	m.rememberDegraded[reason]++
	m.mu.Unlock()
}
func (m *countingMetrics) WriteSanitized(string)            {}
func (m *countingMetrics) ConsolidateResult(string)         {}
func (m *countingMetrics) ConsolidateQueueDepth(int)        {}
func (m *countingMetrics) RememberResult(string, string)    {}
func (m *countingMetrics) ForgetResult(string)              {}
func (m *countingMetrics) SupersedeResult(string)           {}
func (m *countingMetrics) PromoteResult(string, int)        {}
func (m *countingMetrics) FsckResult(string)                {}
func (m *countingMetrics) OpDuration(string, time.Duration) {}
func (m *countingMetrics) AnswerResult(string)              {}
func (m *countingMetrics) RerankResult(string, string)      {}
func (m *countingMetrics) ReinforceResult(string)           {}
func (m *countingMetrics) DedupTombstoned(int)              {}
func (m *countingMetrics) CorroborateResult(string)         {}
func (m *countingMetrics) ContradictResult(string)          {}
func (m *countingMetrics) TierClassified(string)            {}
func (m *countingMetrics) EmbedBackfillPending(n int) {
	m.mu.Lock()
	m.embedBackfillPending = n
	m.embedBackfillCalls++
	m.mu.Unlock()
}

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

// slowEmbedder blocks until its deadline or d elapses, simulating a slow
// embeddings backend; it honors ctx so a recall embed budget can cut it off.
type slowEmbedder struct{ d time.Duration }

func (e slowEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	select {
	case <-time.After(e.d):
		out := make([][]float32, len(texts))
		for i := range out {
			out[i] = make([]float32, dims)
		}
		return out, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (e slowEmbedder) Dims() int { return dims }

func seedHello(t *testing.T, st store.Store) {
	t.Helper()
	seed := service.New(st, embedtest.New(dims), service.WithSyncReinforce())
	if _, err := seed.Remember(context.Background(), service.RememberInput{Namespace: "alice", Content: "hello world", Tier: memory.TierSemantic}); err != nil {
		t.Fatalf("seed remember: %v", err)
	}
}

// TestRecallEmbedTimeoutFallsBackToKeyword confirms that with an embed budget
// set, a query embed that exceeds it degrades to keyword-only search instead of
// failing, and records the degrade reason.
func TestRecallEmbedTimeoutFallsBackToKeyword(t *testing.T) {
	st := openTestStore(t)
	seedHello(t, st)

	m := &countingMetrics{}
	svc := service.New(st, slowEmbedder{d: 2 * time.Second}, service.WithSyncReinforce(),
		service.WithRecallEmbedTimeout(20*time.Millisecond), service.WithMetrics(m))

	res, err := svc.Recall(context.Background(), service.RecallInput{Namespace: "alice", Query: "hello", Limit: 5})
	if err != nil {
		t.Fatalf("recall should degrade, not error: %v", err)
	}
	if len(res) == 0 {
		t.Fatal("keyword-only fallback returned no results")
	}
	if m.degraded["embed_timeout"] != 1 {
		t.Fatalf("RecallDegraded(embed_timeout) = %d, want 1", m.degraded["embed_timeout"])
	}
}

// TestRecallEmbedErrorFallsBackToKeyword confirms that with an embed budget set,
// a hard embed error degrades to keyword-only search rather than failing.
func TestRecallEmbedErrorFallsBackToKeyword(t *testing.T) {
	st := openTestStore(t)
	seedHello(t, st)

	m := &countingMetrics{}
	svc := service.New(st, errEmbedder{dims: dims}, service.WithSyncReinforce(),
		service.WithRecallEmbedTimeout(time.Second), service.WithMetrics(m))

	res, err := svc.Recall(context.Background(), service.RecallInput{Namespace: "alice", Query: "hello", Limit: 5})
	if err != nil {
		t.Fatalf("recall should degrade, not error: %v", err)
	}
	if len(res) == 0 {
		t.Fatal("keyword-only fallback returned no results")
	}
	if m.degraded["embed_error"] != 1 {
		t.Fatalf("RecallDegraded(embed_error) = %d, want 1", m.degraded["embed_error"])
	}
}

// TestRecallDegradedOutParamSetOnFallback confirms that when a recall falls
// back to keyword-only search, RecallInput.Degraded (when non-nil) receives
// the same reason string recorded via RecallDegraded, so callers (MCP/REST)
// can surface the degradation without scraping metrics.
func TestRecallDegradedOutParamSetOnFallback(t *testing.T) {
	st := openTestStore(t)
	seedHello(t, st)

	m := &countingMetrics{}
	svc := service.New(st, errEmbedder{dims: dims}, service.WithSyncReinforce(),
		service.WithRecallEmbedTimeout(time.Second), service.WithMetrics(m))

	var degraded string
	_, err := svc.Recall(context.Background(), service.RecallInput{
		Namespace: "alice", Query: "hello", Limit: 5, Degraded: &degraded,
	})
	if err != nil {
		t.Fatalf("recall should degrade, not error: %v", err)
	}
	if degraded != "embed_error" {
		t.Fatalf("Degraded out-param = %q, want %q", degraded, "embed_error")
	}
}

// TestRecallDegradedOutParamEmptyOnHealthyEmbed confirms the Degraded
// out-param is left untouched (empty) when the query embed succeeds, so
// callers can distinguish a healthy recall from a degraded one.
func TestRecallDegradedOutParamEmptyOnHealthyEmbed(t *testing.T) {
	st := openTestStore(t)
	seedHello(t, st)

	svc := service.New(st, embedtest.New(dims), service.WithSyncReinforce())

	var degraded string
	_, err := svc.Recall(context.Background(), service.RecallInput{
		Namespace: "alice", Query: "hello", Limit: 5, Degraded: &degraded,
	})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if degraded != "" {
		t.Fatalf("Degraded out-param = %q, want empty on healthy embed", degraded)
	}
}

// TestRecallRerankSkippedWhenNoTimeLeft confirms that when the caller's deadline
// leaves no margin, recall skips the reranker and returns composite order rather
// than racing (or blowing) the deadline.
func TestRecallRerankSkippedWhenNoTimeLeft(t *testing.T) {
	st := openTestStore(t)
	base := service.New(st, embedtest.New(dims), service.WithSyncReinforce())
	ingestTwo(t, base)
	baseIDs := recallIDs(t, base)

	rr := &reverseReranker{}
	svc := service.New(st, embedtest.New(dims), service.WithSyncReinforce(), service.WithReranker(rr, "test"))

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	res, err := svc.Recall(ctx, service.RecallInput{Namespace: "alice", Query: "fruit", Limit: 2})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if rr.called {
		t.Fatal("reranker should be skipped when the deadline leaves no response margin")
	}
	if len(res) != 2 || res[0].Memory.ID != baseIDs[0] || res[1].Memory.ID != baseIDs[1] {
		t.Fatalf("expected composite order %v, got %v", baseIDs, res)
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
