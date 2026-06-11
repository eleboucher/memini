package service

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eleboucher/memini/internal/embed/embedtest"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/store"
	"github.com/eleboucher/memini/internal/store/sqlitevec"
)

func openLifecycleStore(t *testing.T) store.Store {
	t.Helper()
	st, err := sqlitevec.Open(context.Background(), filepath.Join(t.TempDir(), "lc.db"), testDims)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// failingReinforceStore fails every Reinforce, to prove recall survives and
// the failure is observable.
type failingReinforceStore struct {
	store.Store
}

func (failingReinforceStore) Reinforce(context.Context, string, []string, time.Time, *time.Time) error {
	return errors.New("reinforce broken")
}

// TestReinforceFailureIsObservableAndNonFatal pins the contract of best-effort
// reinforcement: a failing Reinforce must never fail the recall, but must emit
// the "error" metric (the whole point of logging/counting it is that TTLs stop
// sliding and promotion stops firing when this breaks persistently).
func TestReinforceFailureIsObservableAndNonFatal(t *testing.T) {
	ctx := context.Background()
	st := openLifecycleStore(t)

	remember := func(t *testing.T, svc *Service) {
		t.Helper()
		if _, err := svc.Remember(ctx, RememberInput{
			Namespace: "ns", Content: "the cache lives in redis", Tier: memory.TierWorking,
		}); err != nil {
			t.Fatalf("remember: %v", err)
		}
	}

	t.Run("failure", func(t *testing.T) {
		mx := newCountingMetrics()
		svc := New(failingReinforceStore{st}, embedtest.New(testDims), WithSyncReinforce(), WithMetrics(mx))
		remember(t, svc)
		res, err := svc.Recall(ctx, RecallInput{Namespace: "ns", Query: "redis cache"})
		if err != nil || len(res) == 0 {
			t.Fatalf("recall must survive a reinforce failure: res=%d err=%v", len(res), err)
		}
		if mx.results["reinforce:error"] == 0 {
			t.Fatal("reinforce failure was not counted")
		}
	})

	t.Run("success", func(t *testing.T) {
		mx := newCountingMetrics()
		svc := New(st, embedtest.New(testDims), WithSyncReinforce(), WithMetrics(mx))
		remember(t, svc)
		if _, err := svc.Recall(ctx, RecallInput{Namespace: "ns", Query: "redis cache"}); err != nil {
			t.Fatalf("recall: %v", err)
		}
		if mx.results["reinforce:ok"] == 0 {
			t.Fatal("successful reinforce was not counted")
		}
	})
}

// blockingReinforceStore parks every Reinforce until release is closed.
type blockingReinforceStore struct {
	store.Store
	release  chan struct{}
	finished atomic.Bool
}

func (b *blockingReinforceStore) Reinforce(context.Context, string, []string, time.Time, *time.Time) error {
	<-b.release
	b.finished.Store(true)
	return nil
}

// TestWaitBackgroundJoinsAsyncReinforce pins the shutdown contract: the
// detached recall-reinforcement goroutine must be joined by WaitBackground
// before the caller closes the store. Without this, shutdown races an
// in-flight write against st.Close().
func TestWaitBackgroundJoinsAsyncReinforce(t *testing.T) {
	ctx := context.Background()
	bst := &blockingReinforceStore{Store: openLifecycleStore(t), release: make(chan struct{})}
	// No WithSyncReinforce: exercise the real detached path.
	svc := New(bst, embedtest.New(testDims))

	if _, err := svc.Remember(ctx, RememberInput{
		Namespace: "ns", Content: "the deploy runs on fridays", Tier: memory.TierWorking,
	}); err != nil {
		t.Fatalf("remember: %v", err)
	}
	res, err := svc.Recall(ctx, RecallInput{Namespace: "ns", Query: "deploy fridays"})
	if err != nil || len(res) == 0 {
		t.Fatalf("recall: res=%d err=%v", len(res), err)
	}

	waited := make(chan struct{})
	go func() {
		svc.WaitBackground()
		close(waited)
	}()
	select {
	case <-waited:
		t.Fatal("WaitBackground returned while reinforcement was still running")
	case <-time.After(50 * time.Millisecond):
	}
	close(bst.release)
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("WaitBackground never returned after reinforcement finished")
	}
	if !bst.finished.Load() {
		t.Fatal("reinforcement goroutine did not run to completion")
	}
}

// TestStartConsolidatorDrainsOnCancel pins the drain-on-shutdown semantics:
// jobs queued while the worker is down are still consolidated when the worker
// is started with an already-cancelled context (the shutdown path).
func TestStartConsolidatorDrainsOnCancel(t *testing.T) {
	ctx := context.Background()
	mx := newCountingMetrics()
	// minScore 0.99 gates every job, so each drained job records "gated".
	svc, _ := newAsyncSvc(t, &recordingConsolidator{}, 0.99, mx)

	const jobs = 3
	contents := []string{"alpha fact", "beta fact", "gamma fact"}
	for i := range jobs {
		if _, err := svc.Remember(ctx, RememberInput{
			Namespace: "ns", Content: contents[i], Tier: memory.TierSemantic,
		}); err != nil {
			t.Fatalf("remember %d: %v", i, err)
		}
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	svc.StartConsolidator(cancelled) // returns after draining the queue

	if got := mx.results["gated"]; got != jobs {
		t.Fatalf("drained consolidations = %d, want %d (results: %v)", got, jobs, mx.results)
	}
}

// TestRecallHonorsInjectedClockForExpiry pins Filter.Now threading end to end:
// a TTL'd memory expires when the injected clock advances, with no time.Sleep
// and no dependence on the wall clock.
func TestRecallHonorsInjectedClockForExpiry(t *testing.T) {
	ctx := context.Background()
	st := openLifecycleStore(t)
	base := time.Unix(1_700_000_000, 0).UTC()
	current := base
	svc := New(st, embedtest.New(testDims),
		WithSyncReinforce(),
		WithClock(func() time.Time { return current }))

	ttl := time.Minute
	if _, err := svc.Remember(ctx, RememberInput{
		Namespace: "ns", Content: "ephemeral scratch note", Tier: memory.TierWorking, TTL: &ttl,
	}); err != nil {
		t.Fatalf("remember: %v", err)
	}

	current = base.Add(2 * time.Minute) // past the TTL on the injected clock
	res, err := svc.Recall(ctx, RecallInput{Namespace: "ns", Query: "ephemeral scratch"})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(res) != 0 {
		t.Fatalf("expired memory leaked into recall: %d results", len(res))
	}
	mems, err := svc.List(ctx, ListInput{Namespace: "ns", IncludeExpired: true})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(mems) != 1 {
		t.Fatalf("IncludeExpired must still see it: got %d", len(mems))
	}
}
