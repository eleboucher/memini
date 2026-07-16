package rerank_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eleboucher/memini/internal/rerank"
)

type blockingReranker struct {
	enter chan struct{}
	hold  chan struct{}
	calls atomic.Int32
}

func (b *blockingReranker) Rerank(_ context.Context, _ string, cands []rerank.Candidate) ([]string, error) {
	b.calls.Add(1)
	b.enter <- struct{}{}
	<-b.hold
	ids := make([]string, len(cands))
	for i, c := range cands {
		ids[i] = c.ID
	}
	return ids, nil
}

func cands() []rerank.Candidate {
	return []rerank.Candidate{{ID: "0", Content: "x"}}
}

func TestRerankLimitedBounded(t *testing.T) {
	inner := &blockingReranker{enter: make(chan struct{}, 4), hold: make(chan struct{})}
	const cap = 2
	l := rerank.NewLimited(inner, cap, nil)
	if l == nil {
		t.Fatal("NewLimited returned nil with cap > 0")
	}

	ctx := t.Context()

	var wg sync.WaitGroup
	results := make([]error, cap+1)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := l.Rerank(ctx, "q", cands())
			results[i] = err
		}(i)
	}

	for range cap {
		select {
		case <-inner.enter:
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d of %d in-flight calls reached inner", inner.calls.Load(), cap)
		}
	}
	select {
	case <-inner.enter:
		t.Fatalf("(N+1)th call reached inner: cap=%d was not enforced", cap)
	case <-time.After(50 * time.Millisecond):
	}

	close(inner.hold)
	wg.Wait()
	for i, err := range results {
		if err != nil {
			t.Fatalf("result %d: unexpected error: %v", i, err)
		}
	}
	if got := inner.calls.Load(); got != int32(cap+1) {
		t.Fatalf("inner saw %d calls, want %d", got, cap+1)
	}
}

func TestRerankLimitedContextCancelAbortsWait(t *testing.T) {
	inner := &blockingReranker{enter: make(chan struct{}, 4), hold: make(chan struct{})}
	l := rerank.NewLimited(inner, 1, nil)

	go func() { _, _ = l.Rerank(context.Background(), "q", cands()) }()
	<-inner.enter

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { _, err := l.Rerank(ctx, "q", cands()); errCh <- err }()

	cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("want context.Canceled, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled caller did not return")
	}
	if got := inner.calls.Load(); got != 1 {
		t.Fatalf("inner saw %d calls, want 1 (cancelled waiter must not have entered)", got)
	}
	close(inner.hold)
}

func TestRerankLimitedCallbackReportsInFlight(t *testing.T) {
	inner := &blockingReranker{enter: make(chan struct{}, 2), hold: make(chan struct{})}
	var (
		mu    sync.Mutex
		trace []int64
	)
	l := rerank.NewLimited(inner, 1, func(n int64) {
		mu.Lock()
		trace = append(trace, n)
		mu.Unlock()
	})

	go func() { _, _ = l.Rerank(context.Background(), "q", cands()) }()
	<-inner.enter
	time.Sleep(20 * time.Millisecond)

	close(inner.hold)
	time.Sleep(20 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(trace) < 2 {
		t.Fatalf("callback fired %d times, want at least 2", len(trace))
	}
	if trace[0] != 1 {
		t.Fatalf("first callback n=%d, want 1", trace[0])
	}
	if trace[len(trace)-1] != 0 {
		t.Fatalf("last callback n=%d, want 0", trace[len(trace)-1])
	}
}

func TestRerankLimitedZeroCapIsNoop(t *testing.T) {
	inner := rerank.NewLLM(nil, rerank.DefaultLLMMaxChars) // NewLimited ignores inner when cap<=0
	if got := rerank.NewLimited(inner, 0, nil); got != inner {
		t.Fatal("NewLimited(inner, 0) should return inner unchanged")
	}
	if got := rerank.NewLimited(inner, -1, nil); got != inner {
		t.Fatal("NewLimited(inner, -1) should return inner unchanged")
	}
	if got := rerank.NewLimited(nil, 5, nil); got != nil {
		t.Fatal("NewLimited(nil, 5) should return nil")
	}
}
