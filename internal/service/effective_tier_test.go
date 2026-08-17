package service_test

import (
	"context"
	"testing"

	"github.com/eleboucher/memini/internal/embed/embedtest"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/service"
)

// TestRememberEffectiveTierOutParam pins the EffectiveTier out-param: callers
// (the MCP handler in particular) need the tier the write RESOLVED to — after
// auto-classification and defaulting — even when the value gate drops the
// write and Remember returns (nil, nil). Without it, a dropped write with an
// omitted tier is reported back to the agent as tier "" instead of the tier
// the write would have had.
//
// Referenced by docs/how-it-works/write-path.md.
func TestRememberEffectiveTierOutParam(t *testing.T) {
	ctx := context.Background()
	ns := "alice"

	t.Run("dropped episodic write reports episodic", func(t *testing.T) {
		svc := service.New(openTestStore(t), embedtest.New(dims), service.WithSyncReinforce(), service.WithEpisodicMinChars(80))
		var eff memory.Tier
		m, err := svc.Remember(ctx, service.RememberInput{
			Namespace:     ns,
			Content:       "keep going",
			Tier:          memory.TierEpisodic,
			EffectiveTier: &eff,
		})
		if err != nil || m != nil {
			t.Fatalf("want gate drop (nil, nil), got m=%v err=%v", m, err)
		}
		if eff != memory.TierEpisodic {
			t.Fatalf("EffectiveTier = %q, want %q", eff, memory.TierEpisodic)
		}
	})

	t.Run("dropped capture with omitted tier reports the resolved default", func(t *testing.T) {
		svc := service.New(openTestStore(t), embedtest.New(dims), service.WithSyncReinforce())
		var eff memory.Tier
		// A turn capture whose content is pure harness boilerplate strips to
		// empty and is dropped outright, whatever the tier resolved to.
		m, err := svc.Remember(ctx, service.RememberInput{
			Namespace:     ns,
			Content:       "<memini-context project=\"x\">noise</memini-context>",
			Metadata:      map[string]any{"format": "turn"},
			EffectiveTier: &eff,
		})
		if err != nil || m != nil {
			t.Fatalf("want capture drop (nil, nil), got m=%v err=%v", m, err)
		}
		if eff != memory.TierWorking {
			t.Fatalf("EffectiveTier = %q, want the resolved default %q", eff, memory.TierWorking)
		}
	})

	t.Run("stored write with omitted tier reports the classified tier", func(t *testing.T) {
		svc := service.New(openTestStore(t), embedtest.New(dims), service.WithSyncReinforce())
		var eff memory.Tier
		m, err := svc.Remember(ctx, service.RememberInput{
			Namespace:     ns,
			Content:       "we decided to use pnpm for every workspace instead of npm, the reason is peer resolution kept breaking",
			EffectiveTier: &eff,
		})
		if err != nil || m == nil {
			t.Fatalf("want stored, got m=%v err=%v", m, err)
		}
		if eff != m.Tier {
			t.Fatalf("EffectiveTier = %q, want the stored tier %q", eff, m.Tier)
		}
		if eff == memory.TierWorking {
			t.Fatalf("classifier should have raised this decision above %q", memory.TierWorking)
		}
	})
}
