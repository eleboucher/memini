package service_test

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/eleboucher/memini/internal/embed/embedtest"
	"github.com/eleboucher/memini/internal/llm"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/service"
	"github.com/eleboucher/memini/internal/store"
)

type fakeDistiller struct{ fact string }

func (f fakeDistiller) Distill(_ context.Context, _ llm.DistillInput) ([]llm.Fact, error) {
	return []llm.Fact{{Content: f.fact}}, nil
}

// countingDistiller records how many times Distill is invoked, so a test can
// assert distillation fired exactly once even when content dedup would absorb a
// second identical fact.
type countingDistiller struct {
	fact  string
	calls atomic.Int32
}

func (d *countingDistiller) Distill(_ context.Context, _ llm.DistillInput) ([]llm.Fact, error) {
	d.calls.Add(1)
	return []llm.Fact{{Content: d.fact}}, nil
}

// noFactDistiller extracts nothing — the "this turn carries no durable fact" case.
type noFactDistiller struct{}

func (noFactDistiller) Distill(_ context.Context, _ llm.DistillInput) ([]llm.Fact, error) {
	return nil, nil
}

func tierCount(t *testing.T, st store.Store, ns string, tier memory.Tier) int {
	t.Helper()
	ms, err := st.List(context.Background(), ns, store.Filter{Tiers: []memory.Tier{tier}}, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	return len(ms)
}

func durableCount(t *testing.T, st store.Store, ns string) int {
	t.Helper()
	return tierCount(t, st, ns, memory.TierSemantic)
}

// TestDistillOnWrite pins write-time distillation: a fresh episodic capture is
// distilled into a durable semantic fact in the background. Fires only for a
// fresh episodic create (not an update); needs a distiller; gated by the option.
func TestDistillOnWrite(t *testing.T) {
	ctx := context.Background()
	ns := "alice"
	const fact = "auth uses jose, not jsonwebtoken"

	t.Run("on: episodic write creates a durable fact", func(t *testing.T) {
		st := openTestStore(t)
		svc := service.New(st, embedtest.New(dims), service.WithSyncReinforce(),
			service.WithDistiller(fakeDistiller{fact: fact}), service.WithDistillOnWrite(true))
		if _, err := svc.Remember(ctx, service.RememberInput{Namespace: ns, Content: "User: how's auth wired?\nAssistant: it uses the jose library", Tier: memory.TierEpisodic}); err != nil {
			t.Fatalf("remember: %v", err)
		}
		svc.WaitBackground()
		if got := durableCount(t, st, ns); got != 1 {
			t.Fatalf("want 1 distilled semantic fact, got %d", got)
		}
	})

	t.Run("off by default: no distillation", func(t *testing.T) {
		st := openTestStore(t)
		svc := service.New(st, embedtest.New(dims), service.WithSyncReinforce(),
			service.WithDistiller(fakeDistiller{fact: fact})) // distiller set, but WithDistillOnWrite not passed
		if _, err := svc.Remember(ctx, service.RememberInput{Namespace: ns, Content: "User: how's auth wired?\nAssistant: jose", Tier: memory.TierEpisodic}); err != nil {
			t.Fatalf("remember: %v", err)
		}
		svc.WaitBackground()
		if got := durableCount(t, st, ns); got != 0 {
			t.Fatalf("default off should not distill, got %d durable", got)
		}
	})

	t.Run("durable writes are not re-distilled", func(t *testing.T) {
		st := openTestStore(t)
		svc := service.New(st, embedtest.New(dims), service.WithSyncReinforce(),
			service.WithDistiller(fakeDistiller{fact: fact}), service.WithDistillOnWrite(true))
		if _, err := svc.Remember(ctx, service.RememberInput{Namespace: ns, Content: "explicit fact", Tier: memory.TierSemantic}); err != nil {
			t.Fatalf("remember: %v", err)
		}
		svc.WaitBackground()
		// Only the explicit semantic write — no distilled fact on top of it.
		if got := durableCount(t, st, ns); got != 1 {
			t.Fatalf("semantic write should not trigger distillation, got %d", got)
		}
	})

	t.Run("explicit-id episodic create distills; re-fire as update does not", func(t *testing.T) {
		st := openTestStore(t)
		dist := &countingDistiller{fact: fact}
		svc := service.New(st, embedtest.New(dims), service.WithSyncReinforce(),
			service.WithDistiller(dist), service.WithDistillOnWrite(true))
		// A client-supplied ID (e.g. the plugin's session-end:<id> digest) is a
		// fresh create the first time, so it distills.
		in := service.RememberInput{Namespace: ns, ID: "session-end:s1", Tier: memory.TierEpisodic,
			Content: "User: how's auth wired?\nAssistant: it uses the jose library"}
		if _, err := svc.Remember(ctx, in); err != nil {
			t.Fatalf("remember: %v", err)
		}
		svc.WaitBackground()
		if got := durableCount(t, st, ns); got != 1 {
			t.Fatalf("explicit-id episodic create should distill, got %d durable", got)
		}
		// Re-fire the same ID with an updated digest (an update, not a create) —
		// must not distill again. New content bypasses the exact-restatement gate
		// so this exercises the update path, not the fingerprint fast path.
		in.Content += "; added tests"
		if _, err := svc.Remember(ctx, in); err != nil {
			t.Fatalf("remember (re-fire): %v", err)
		}
		svc.WaitBackground()
		if got := dist.calls.Load(); got != 1 {
			t.Fatalf("distiller should run once for create only, ran %d times", got)
		}
	})
}

// TestDistillDropNoFact pins the drop-when-no-fact filter: when distillation
// extracts nothing durable, the episodic is deleted (only with the opt-in).
func TestDistillDropNoFact(t *testing.T) {
	ctx := context.Background()
	ns := "alice"
	const cap = "User: how's it going?\nAssistant: making progress on the refactor"

	t.Run("drops episodic when no fact and drop enabled", func(t *testing.T) {
		st := openTestStore(t)
		svc := service.New(st, embedtest.New(dims), service.WithSyncReinforce(),
			service.WithDistiller(noFactDistiller{}), service.WithDistillOnWrite(true), service.WithDistillDropNoFact(true))
		if _, err := svc.Remember(ctx, service.RememberInput{Namespace: ns, Content: cap, Tier: memory.TierEpisodic}); err != nil {
			t.Fatalf("remember: %v", err)
		}
		svc.WaitBackground()
		if got := tierCount(t, st, ns, memory.TierEpisodic); got != 0 {
			t.Fatalf("episodic with no distillable fact should be dropped, got %d", got)
		}
	})

	t.Run("keeps episodic when drop disabled", func(t *testing.T) {
		st := openTestStore(t)
		svc := service.New(st, embedtest.New(dims), service.WithSyncReinforce(),
			service.WithDistiller(noFactDistiller{}), service.WithDistillOnWrite(true)) // drop-no-fact off
		if _, err := svc.Remember(ctx, service.RememberInput{Namespace: ns, Content: cap, Tier: memory.TierEpisodic}); err != nil {
			t.Fatalf("remember: %v", err)
		}
		svc.WaitBackground()
		if got := tierCount(t, st, ns, memory.TierEpisodic); got != 1 {
			t.Fatalf("episodic should be kept when drop disabled, got %d", got)
		}
	})
}
