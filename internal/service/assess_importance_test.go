package service_test

import (
	"context"
	"testing"

	"github.com/eleboucher/memini/internal/embed/embedtest"
	"github.com/eleboucher/memini/internal/llm"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/service"
)

// assessingDistiller emits one fact carrying the scripted self-assessed
// importance (nil when the model omitted the key).
type assessingDistiller struct {
	fact       string
	importance *float64
}

func (d assessingDistiller) Distill(_ context.Context, _ llm.DistillInput) ([]llm.Fact, error) {
	return []llm.Fact{{Content: d.fact, Importance: d.importance}}, nil
}

// distillWithImportance runs one distill-on-write cycle with the scripted
// assessment and returns the durable fact it produced.
func distillWithImportance(t *testing.T, importance *float64) *memory.Memory {
	t.Helper()
	ctx := context.Background()
	st := openTestStore(t)
	svc := service.New(st, embedtest.New(dims), service.WithSyncReinforce(),
		service.WithDistiller(assessingDistiller{fact: "deploys go through the staging gate", importance: importance}),
		service.WithDistillOnWrite(true))
	if _, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "alice", Content: "User: how do we deploy?\nAssistant: everything goes through the staging gate",
		Tier: memory.TierEpisodic,
	}); err != nil {
		t.Fatalf("remember: %v", err)
	}
	svc.WaitBackground()
	return durableOne(t, st, "alice")
}

// reread pulls the stored row back so an assertion proves the assessment was
// persisted, not merely set on the in-memory copy Remember returns.
func reread(t *testing.T, svc *service.Service, id string) *memory.Memory {
	t.Helper()
	m, err := svc.Get(context.Background(), "alice", id)
	if err != nil {
		t.Fatalf("get %s: %v", id, err)
	}
	return m
}

func assertAssessed(t *testing.T, m *memory.Memory, want *float64) {
	t.Helper()
	switch {
	case want == nil && m.AssessedImportance != nil:
		t.Fatalf("assessed importance = %v, want nil", *m.AssessedImportance)
	case want != nil && m.AssessedImportance == nil:
		t.Fatalf("assessed importance = nil, want %v", *want)
	case want != nil && *m.AssessedImportance != *want:
		t.Fatalf("assessed importance = %v, want %v", *m.AssessedImportance, *want)
	}
}

// TestDistilledFactCarriesAssessedImportance pins the piggyback: what the
// distiller rated a fact lands on the stored row, clamped into the storable
// range, and a model that rated nothing stores nothing (rather than a zero that
// would read as "worthless").
func TestDistilledFactCarriesAssessedImportance(t *testing.T) {
	tests := []struct {
		name string
		rate *float64
		want *float64
	}{
		{"in range", new(0.85), new(0.85)},
		{"clamped down", new(0.99), new(0.9)},
		{"clamped up", new(0.02), new(0.1)},
		{"omitted", nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fact := distillWithImportance(t, tt.rate)
			assertAssessed(t, fact, tt.want)
			// The assessment never touches the importance the tier seeded.
			if fact.Importance != memory.SeedImportance(memory.TierSemantic) {
				t.Fatalf("importance = %v, want the tier seed %v (the assessment must not overwrite it)",
					fact.Importance, memory.SeedImportance(memory.TierSemantic))
			}
		})
	}
}

// TestExplicitImportanceClearsAssessment pins the write-time invariant on a
// fresh write: naming an importance is a deliberate choice, so nothing the LLM
// assessed survives alongside it.
func TestExplicitImportanceClearsAssessment(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	svc := service.New(st, embedtest.New(dims), service.WithSyncReinforce())
	m, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "alice", Content: "the deploy gate is staging", Tier: memory.TierSemantic,
		Importance: 0.4, AssessedImportance: new(0.85),
	})
	if err != nil {
		t.Fatalf("remember: %v", err)
	}
	assertAssessed(t, reread(t, svc, m.ID), nil)
	if m.Importance != 0.4 {
		t.Fatalf("importance = %v, want 0.4", m.Importance)
	}
}

// TestUpdatePreservesAssessment covers both halves of the Update contract: an
// edit that names no importance keeps the assessment (Update re-sends the stored
// importance on every edit, which must not read as a caller choice), and one
// that names it — even the value already stored — clears the assessment.
func TestUpdatePreservesAssessment(t *testing.T) {
	ctx := context.Background()

	seed := func(t *testing.T) (*service.Service, *memory.Memory) {
		t.Helper()
		st := openTestStore(t)
		svc := service.New(st, embedtest.New(dims), service.WithSyncReinforce())
		m, err := svc.Remember(ctx, service.RememberInput{
			Namespace: "alice", Content: "the deploy gate is staging", Tier: memory.TierSemantic,
			AssessedImportance: new(0.85),
		})
		if err != nil {
			t.Fatalf("remember: %v", err)
		}
		assertAssessed(t, m, new(0.85))
		return svc, m
	}

	t.Run("without importance", func(t *testing.T) {
		svc, m := seed(t)
		summary := "deploys gate on staging"
		got, err := svc.Update(ctx, service.UpdateInput{
			Namespace: "alice", ID: m.ID, Summary: &summary,
		})
		if err != nil {
			t.Fatalf("update: %v", err)
		}
		assertAssessed(t, got, new(0.85))
	})

	t.Run("with importance equal to the stored one", func(t *testing.T) {
		svc, m := seed(t)
		got, err := svc.Update(ctx, service.UpdateInput{
			Namespace: "alice", ID: m.ID, Importance: &m.Importance,
		})
		if err != nil {
			t.Fatalf("update: %v", err)
		}
		assertAssessed(t, got, nil)
		if got.Importance != m.Importance {
			t.Fatalf("importance = %v, want %v", got.Importance, m.Importance)
		}
	})
}

// TestConsolidateStampsAssessedImportance pins the consolidator's own rating
// reaching the stored memory on both branches that persist one: a merge into an
// existing target, and a plain insert.
func TestConsolidateStampsAssessedImportance(t *testing.T) {
	t.Run("update stamps a target still at its tier seed", func(t *testing.T) {
		fc := &fakeConsolidator{}
		svc := newConsolidatingService(t, fc)
		first := remember(t, svc, "the deploy gate is staging")
		if first.Importance != memory.SeedImportance(memory.TierSemantic) {
			t.Fatalf("fixture importance = %v, want the untouched tier seed", first.Importance)
		}

		fc.dec = llm.Decision{
			Action: llm.ActionUpdate, Target: first.ID,
			Content: "deploys go through the staging gate", Importance: new(0.7),
		}
		got := remember(t, svc, "deploys go through the staging gate")
		if got.ID != first.ID {
			t.Fatalf("id = %q, want the merge target %q", got.ID, first.ID)
		}
		assertAssessed(t, reread(t, svc, got.ID), new(0.7))
	})

	t.Run("new clamps and stamps the write", func(t *testing.T) {
		fc := &fakeConsolidator{}
		svc := newConsolidatingService(t, fc)
		remember(t, svc, "the deploy gate is staging")

		fc.dec = llm.Decision{Action: llm.ActionNew, Importance: new(1.0)}
		got := remember(t, svc, "the release train runs on Thursdays")
		assertAssessed(t, reread(t, svc, got.ID), new(0.9))
	})
}

// TestConsolidateLeavesExplicitImportanceAlone pins the other half of the
// write-time invariant, which consolidation runs late enough to break: an
// assessment stamped after the resolve step would sit on a row whose importance
// the caller chose, and EffectiveImportance reads the assessment first — so the
// consolidator's opinion would silently outrank the number they asked for.
func TestConsolidateLeavesExplicitImportanceAlone(t *testing.T) {
	ctx := context.Background()

	t.Run("fresh write the caller gave an importance", func(t *testing.T) {
		fc := &fakeConsolidator{}
		svc := newConsolidatingService(t, fc)
		remember(t, svc, "the deploy gate is staging")

		fc.dec = llm.Decision{Action: llm.ActionNew, Importance: new(0.9)}
		got, err := svc.Remember(ctx, service.RememberInput{
			Namespace: "alice", Content: "the release train runs on Thursdays",
			Tier: memory.TierSemantic, Importance: 0.85,
		})
		if err != nil {
			t.Fatalf("remember: %v", err)
		}
		stored := reread(t, svc, got.ID)
		assertAssessed(t, stored, nil)
		if stored.Importance != 0.85 {
			t.Fatalf("importance = %v, want the caller's 0.85", stored.Importance)
		}
	})

	t.Run("merge target the caller gave an importance", func(t *testing.T) {
		fc := &fakeConsolidator{}
		svc := newConsolidatingService(t, fc)
		first, err := svc.Remember(ctx, service.RememberInput{
			Namespace: "alice", Content: "the deploy gate is staging",
			Tier: memory.TierSemantic, Importance: 0.85,
		})
		if err != nil {
			t.Fatalf("remember: %v", err)
		}

		fc.dec = llm.Decision{
			Action: llm.ActionUpdate, Target: first.ID,
			Content: "deploys go through the staging gate", Importance: new(0.7),
		}
		got := remember(t, svc, "deploys go through the staging gate")
		if got.ID != first.ID {
			t.Fatalf("id = %q, want the merge target %q", got.ID, first.ID)
		}
		stored := reread(t, svc, got.ID)
		assertAssessed(t, stored, nil)
		if stored.Importance != 0.85 {
			t.Fatalf("importance = %v, want the caller's 0.85 preserved through the merge", stored.Importance)
		}
	})
}

// TestQuarantinedWriteStoresNoAssessment pins that the corruption downrank holds:
// quarantine zeroes Importance so garbled content sinks in recall, and an
// assessment on the same row would float it right back up.
func TestQuarantinedWriteStoresNoAssessment(t *testing.T) {
	ctx := context.Background()

	t.Run("assessment supplied by the distill path", func(t *testing.T) {
		svc := newService(t, service.WithCorruptionQuarantine(true))
		m, err := svc.Remember(ctx, service.RememberInput{
			Namespace: "alice", Content: garbledContent, Tier: memory.TierSemantic,
			AssessedImportance: new(0.8),
		})
		if err != nil {
			t.Fatalf("remember: %v", err)
		}
		if m.Importance != 0 {
			t.Fatalf("importance = %v, want 0 (the quarantine downrank)", m.Importance)
		}
		assertAssessed(t, m, nil)
	})

	t.Run("assessment stamped by the consolidator", func(t *testing.T) {
		fc := &fakeConsolidator{}
		svc := newConsolidatingService(t, fc, service.WithCorruptionQuarantine(true))
		remember(t, svc, "the deploy gate is staging")

		fc.dec = llm.Decision{Action: llm.ActionNew, Importance: new(0.8)}
		m, err := svc.Remember(ctx, service.RememberInput{
			Namespace: "alice", Content: garbledContent, Tier: memory.TierSemantic,
		})
		if err != nil {
			t.Fatalf("remember: %v", err)
		}
		if m.Importance != 0 {
			t.Fatalf("importance = %v, want 0 (the quarantine downrank)", m.Importance)
		}
		assertAssessed(t, reread(t, svc, m.ID), nil)
	})
}

// TestCoalesceReplacementAssessmentCarry pins the write-time invariant across
// the coalesce informativeness tiebreak. A richer restatement that replaces the
// stored copy inherits its assessment — the fact is the same, so the rating it
// earned survives the swap instead of falling back to the tier seed — but only
// when the replacement is one the invariant lets carry an assessment at all. A
// nil assessment on the incoming write is ambiguous: resolveAssessedImportance
// also clears it when the caller named an importance, and inheriting the old
// row's rating there would silently outrank the number they asked for, since
// EffectiveImportance reads the assessment first.
func TestCoalesceReplacementAssessmentCarry(t *testing.T) {
	ctx := context.Background()

	// Threshold 0.1 so the nearest same-tier match always enters the coalesce
	// path regardless of embedder geometry — the carry-over is what's under test.
	seed := func(t *testing.T) (*service.Service, *memory.Memory) {
		t.Helper()
		svc := service.New(openTestStore(t), embedtest.New(dims), service.WithSyncReinforce(),
			service.WithWriteDedup(0.1, service.WriteDedupCoalesce))
		first, err := svc.Remember(ctx, service.RememberInput{
			Namespace: "alice", Content: "the user likes coffee", Tier: memory.TierSemantic,
			AssessedImportance: new(0.85),
		})
		if err != nil {
			t.Fatalf("remember: %v", err)
		}
		assertAssessed(t, first, new(0.85))
		return svc, first
	}

	const richer = "the user likes coffee strong and black every morning before standup"

	t.Run("carried onto an assessable replacement", func(t *testing.T) {
		svc, first := seed(t)
		got, err := svc.Remember(ctx, service.RememberInput{
			Namespace: "alice", Content: richer, Tier: memory.TierSemantic,
		})
		if err != nil {
			t.Fatalf("remember richer: %v", err)
		}
		svc.WaitBackground() // the auto-supersede of the replaced copy runs in the background
		if got.ID == first.ID {
			t.Fatal("richer phrasing should replace the stored copy, not coalesce into it")
		}
		assertAssessed(t, reread(t, svc, got.ID), new(0.85))
	})

	t.Run("dropped when the replacement names an importance", func(t *testing.T) {
		svc, first := seed(t)
		got, err := svc.Remember(ctx, service.RememberInput{
			Namespace: "alice", Content: richer, Tier: memory.TierSemantic, Importance: 0.85,
		})
		if err != nil {
			t.Fatalf("remember richer: %v", err)
		}
		svc.WaitBackground() // the auto-supersede of the replaced copy runs in the background
		if got.ID == first.ID {
			t.Fatal("richer phrasing should replace the stored copy, not coalesce into it")
		}
		stored := reread(t, svc, got.ID)
		assertAssessed(t, stored, nil)
		if stored.Importance != 0.85 {
			t.Fatalf("importance = %v, want the caller's 0.85", stored.Importance)
		}
	})
}
