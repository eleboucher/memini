package service_test

import (
	"context"
	"strings"
	"testing"

	"github.com/eleboucher/memini/internal/embed/embedtest"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/service"
)

// TestEpisodicMinCharsGate pins the write-time value gate: when enabled, a
// low-signal episodic turn is dropped (accepted, not stored — nil memory, nil
// error); substantive episodic and any durable write still persist; and the
// default (0) stores everything as before.
func TestEpisodicMinCharsGate(t *testing.T) {
	ctx := context.Background()
	ns := "alice"
	substantive := "User: deploy plan\n\nAssistant: " + strings.Repeat("postgres failover steps ", 10)

	t.Run("gate off stores trivial episodic", func(t *testing.T) {
		svc := service.New(openTestStore(t), embedtest.New(dims), service.WithSyncReinforce())
		m, err := svc.Remember(ctx, service.RememberInput{Namespace: ns, Content: "keep going", Tier: memory.TierEpisodic})
		if err != nil || m == nil {
			t.Fatalf("gate off: want stored, got m=%v err=%v", m, err)
		}
	})

	t.Run("gate on drops trivial episodic", func(t *testing.T) {
		svc := service.New(openTestStore(t), embedtest.New(dims), service.WithSyncReinforce(), service.WithEpisodicMinChars(80))
		m, err := svc.Remember(ctx, service.RememberInput{Namespace: ns, Content: "keep going", Tier: memory.TierEpisodic})
		if err != nil {
			t.Fatalf("drop should not error: %v", err)
		}
		if m != nil {
			t.Fatalf("trivial episodic should be dropped (nil), got %q", m.Content)
		}
	})

	t.Run("gate on keeps substantive episodic", func(t *testing.T) {
		svc := service.New(openTestStore(t), embedtest.New(dims), service.WithSyncReinforce(), service.WithEpisodicMinChars(80))
		m, err := svc.Remember(ctx, service.RememberInput{Namespace: ns, Content: substantive, Tier: memory.TierEpisodic})
		if err != nil || m == nil {
			t.Fatalf("substantive episodic should persist, got m=%v err=%v", m, err)
		}
	})

	t.Run("gate never touches durable tiers", func(t *testing.T) {
		svc := service.New(openTestStore(t), embedtest.New(dims), service.WithSyncReinforce(), service.WithEpisodicMinChars(80))
		m, err := svc.Remember(ctx, service.RememberInput{Namespace: ns, Content: "jose not jsonwebtoken", Tier: memory.TierSemantic})
		if err != nil || m == nil {
			t.Fatalf("short semantic must persist (not gated), got m=%v err=%v", m, err)
		}
	})
}
