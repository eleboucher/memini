package service_test

import (
	"context"
	"testing"

	"github.com/eleboucher/memini/internal/embed/embedtest"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/service"
	"github.com/eleboucher/memini/internal/store"
)

func durableOne(t *testing.T, st store.Store, ns string) *memory.Memory {
	t.Helper()
	ms, err := st.List(context.Background(), ns, store.Filter{Tiers: []memory.Tier{memory.TierSemantic}}, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(ms) != 1 {
		t.Fatalf("want exactly 1 semantic fact, got %d", len(ms))
	}
	return ms[0]
}

// TestExtractOnWrite pins the no-LLM write-time extractor: a fresh episodic
// capture is run through the heuristic extractor into durable typed facts, but
// only when no distiller is configured (the LLM distill path supersedes it).
func TestExtractOnWrite(t *testing.T) {
	ctx := context.Background()
	ns := "alice"
	const decision = "user: which db?\nassistant: We decided to use Postgres instead of SQLite because we need concurrent writes."

	t.Run("no LLM: episodic decision yields a durable semantic fact", func(t *testing.T) {
		st := openTestStore(t)
		svc := service.New(st, embedtest.New(dims), service.WithSyncReinforce(),
			service.WithExtractOnWrite(true)) // no distiller
		if _, err := svc.Remember(ctx, service.RememberInput{Namespace: ns, Content: decision, Tier: memory.TierEpisodic}); err != nil {
			t.Fatalf("remember: %v", err)
		}
		svc.WaitBackground()
		if got := durableCount(t, st, ns); got != 1 {
			t.Fatalf("want 1 extracted semantic fact, got %d", got)
		}
		// The raw episodic is kept — the extractor adds, never replaces.
		if got := tierCount(t, st, ns, memory.TierEpisodic); got != 1 {
			t.Fatalf("episodic capture should be kept, got %d", got)
		}
	})

	t.Run("extracted fact inherits the capture's session_id", func(t *testing.T) {
		st := openTestStore(t)
		svc := service.New(st, embedtest.New(dims), service.WithSyncReinforce(),
			service.WithExtractOnWrite(true))
		if _, err := svc.Remember(ctx, service.RememberInput{
			Namespace: ns, Content: decision, Tier: memory.TierEpisodic,
			Metadata: map[string]any{"source": "turn_capture", "session_id": "sess-1", "format": "turn"},
		}); err != nil {
			t.Fatalf("remember: %v", err)
		}
		svc.WaitBackground()
		fact := durableOne(t, st, ns)
		if got, _ := fact.Metadata["session_id"].(string); got != "sess-1" {
			t.Fatalf("extracted fact session_id = %q, want %q (session exclusion must reach assistant-derived facts)", got, "sess-1")
		}
		if got, _ := fact.Metadata["source"].(string); got != "extract" {
			t.Fatalf("extracted fact source = %q, want %q", got, "extract")
		}
	})

	t.Run("no LLM: chatter yields nothing durable", func(t *testing.T) {
		st := openTestStore(t)
		svc := service.New(st, embedtest.New(dims), service.WithSyncReinforce(),
			service.WithExtractOnWrite(true))
		if _, err := svc.Remember(ctx, service.RememberInput{Namespace: ns, Content: "user: hi\nassistant: hello, how can I help?", Tier: memory.TierEpisodic}); err != nil {
			t.Fatalf("remember: %v", err)
		}
		svc.WaitBackground()
		if got := durableCount(t, st, ns); got != 0 {
			t.Fatalf("chatter should extract nothing, got %d durable", got)
		}
	})

	t.Run("off: no extraction", func(t *testing.T) {
		st := openTestStore(t)
		svc := service.New(st, embedtest.New(dims), service.WithSyncReinforce()) // extract-on-write not passed
		if _, err := svc.Remember(ctx, service.RememberInput{Namespace: ns, Content: decision, Tier: memory.TierEpisodic}); err != nil {
			t.Fatalf("remember: %v", err)
		}
		svc.WaitBackground()
		if got := durableCount(t, st, ns); got != 0 {
			t.Fatalf("extractor off should not extract, got %d durable", got)
		}
	})

	t.Run("distiller present supersedes the heuristic extractor", func(t *testing.T) {
		st := openTestStore(t)
		// Both paths enabled, but a distiller is configured: only distill runs.
		svc := service.New(st, embedtest.New(dims), service.WithSyncReinforce(),
			service.WithExtractOnWrite(true), service.WithDistillOnWrite(true),
			service.WithDistiller(fakeDistiller{fact: "auth uses jose"}))
		if _, err := svc.Remember(ctx, service.RememberInput{Namespace: ns, Content: decision, Tier: memory.TierEpisodic}); err != nil {
			t.Fatalf("remember: %v", err)
		}
		svc.WaitBackground()
		// Only the distilled fact: if the extractor had also run, it would add a
		// second, distinct semantic fact from the decision segment.
		if got := durableCount(t, st, ns); got != 1 {
			t.Fatalf("distiller should own the write-time path, got %d durable facts", got)
		}
	})
}
