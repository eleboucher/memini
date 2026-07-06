package service_test

import (
	"context"
	"testing"

	"github.com/eleboucher/memini/internal/embed/embedtest"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/service"
	"github.com/eleboucher/memini/internal/store"
)

// TestLevelTagging pins the safety invariant of derivation levels: only the
// configured distiller may set LevelDeduced — everything else is
// LevelExplicit or empty (legacy).
func TestLevelTagging(t *testing.T) {
	ctx := context.Background()
	ns := "alice"

	// assertNoLevel scans all memories and fails if any has the given level.
	assertNoLevel := func(t *testing.T, st store.Store, ns string, lvl memory.Level) {
		t.Helper()
		ms, err := st.List(ctx, ns, store.Filter{}, 0)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		for i, m := range ms {
			if m.Level == lvl {
				t.Fatalf("memory[%d] id=%s has level %q, want none", i, m.ID, lvl)
			}
		}
	}

	t.Run("heuristic extract tags explicit", func(t *testing.T) {
		st := openTestStore(t)
		svc := service.New(st, embedtest.New(dims), service.WithSyncReinforce(),
			service.WithExtractOnWrite(true)) // no distiller

		// "we decided" matches decision marker + "instead of" matches decision marker
		// two distinct markers ⇒ score 2+0=2 ⇒ confidence 0.4 ≥ 0.3 gate.
		_, err := svc.Remember(ctx, service.RememberInput{
			Namespace: ns,
			Content:   "We decided to switch to Postgres instead of SQLite",
			Tier:      memory.TierEpisodic,
		})
		if err != nil {
			t.Fatalf("remember: %v", err)
		}
		svc.WaitBackground()

		if got := durableCount(t, st, ns); got != 1 {
			t.Fatalf("want 1 semantic fact, got %d", got)
		}
		fact := durableOne(t, st, ns)
		if fact.Level != memory.LevelExplicit {
			t.Fatalf("Level = %q, want %q", fact.Level, memory.LevelExplicit)
		}
	})

	t.Run("distill tags deduced", func(t *testing.T) {
		st := openTestStore(t)
		dist := &countingDistiller{fact: "the db is postgres"}
		svc := service.New(st, embedtest.New(dims), service.WithSyncReinforce(),
			service.WithDistiller(dist),
			service.WithDistillOnWrite(true))

		_, err := svc.Remember(ctx, service.RememberInput{
			Namespace: ns,
			Content:   "User: what db?\nAssistant: we use postgres",
			Tier:      memory.TierEpisodic,
		})
		if err != nil {
			t.Fatalf("remember: %v", err)
		}
		svc.WaitBackground()

		fact := durableOne(t, st, ns)
		if fact.Level != memory.LevelDeduced {
			t.Fatalf("Level = %q, want %q", fact.Level, memory.LevelDeduced)
		}
		// Provenance completeness: source_ids always present.
		ids, ok := fact.Metadata["source_ids"].([]any)
		if !ok || len(ids) == 0 {
			t.Fatalf("source_ids missing or empty on distilled fact")
		}
	})

	t.Run("no distiller produces no deduced facts", func(t *testing.T) {
		st := openTestStore(t)
		// Only extract-on-write, no distiller — the extractor always marks explicit.
		svc := service.New(st, embedtest.New(dims), service.WithSyncReinforce(),
			service.WithExtractOnWrite(true))

		_, err := svc.Remember(ctx, service.RememberInput{
			Namespace: ns,
			Content:   "User: which db?\nAssistant: we decided PostgreSQL because of concurrent writes",
			Tier:      memory.TierEpisodic,
			Metadata:  map[string]any{"session_id": "s1"},
		})
		if err != nil {
			t.Fatalf("remember: %v", err)
		}
		svc.WaitBackground()

		assertNoLevel(t, st, ns, memory.LevelDeduced)
	})

	t.Run("distiller with extract: only distiller produces deduced", func(t *testing.T) {
		st := openTestStore(t)
		dist := fakeDistiller{fact: "auth is jose"}
		svc := service.New(st, embedtest.New(dims), service.WithSyncReinforce(),
			service.WithExtractOnWrite(true),
			service.WithDistillOnWrite(true),
			service.WithDistiller(dist))

		_, err := svc.Remember(ctx, service.RememberInput{
			Namespace: ns,
			Content:   "we use postgres for the main db", // would trigger the extractor too
			Tier:      memory.TierEpisodic,
		})
		if err != nil {
			t.Fatalf("remember: %v", err)
		}
		svc.WaitBackground()

		// With both configured, the distiller owns the path: one fact, deduced.
		ms, err := st.List(ctx, ns, store.Filter{Tiers: []memory.Tier{memory.TierSemantic}}, 0)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(ms) != 1 {
			t.Fatalf("want 1 semantic fact, got %d", len(ms))
		}
		if ms[0].Level != memory.LevelDeduced {
			t.Fatalf("Level = %q, want %q (distiller owns the fact)", ms[0].Level, memory.LevelDeduced)
		}
	})

	t.Run("rejects invalid level on remember", func(t *testing.T) {
		st := openTestStore(t)
		svc := service.New(st, embedtest.New(dims), service.WithSyncReinforce())
		_, err := svc.Remember(ctx, service.RememberInput{
			Namespace: ns,
			Content:   "some fact",
			Tier:      memory.TierEpisodic,
			Level:     "garbage",
		})
		if err == nil {
			t.Fatal("expected error for invalid level, got nil")
		}
	})

	t.Run("allows empty level (legacy default)", func(t *testing.T) {
		st := openTestStore(t)
		svc := service.New(st, embedtest.New(dims), service.WithSyncReinforce())
		_, err := svc.Remember(ctx, service.RememberInput{
			Namespace: ns,
			Content:   "some fact",
			Tier:      memory.TierEpisodic,
			Level:     memory.Level(""), // explicit empty — legacy default
		})
		if err != nil {
			t.Fatalf("expected empty level to be accepted, got error: %v", err)
		}
	})
}
