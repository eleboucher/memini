package service_test

import (
	"context"
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

func durableCount(t *testing.T, st store.Store, ns string) int {
	t.Helper()
	ms, err := st.List(context.Background(), ns, store.Filter{Tiers: []memory.Tier{memory.TierSemantic}}, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	return len(ms)
}

// TestDistillOnWrite pins the mem0-style write gate: a fresh episodic capture is
// distilled into a durable semantic fact in the background. Off by default; only
// fires for episodic; needs a distiller.
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
}
