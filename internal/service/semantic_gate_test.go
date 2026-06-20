package service_test

import (
	"context"
	"testing"

	"github.com/eleboucher/memini/internal/embed/embedtest"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/service"
)

// TestRecallMinSemanticScoreGate pins the mem0 score_and_rank semantics: the
// semantic-score floor excludes a candidate entirely, and the keyword leg cannot
// reintroduce it. The "poison" memory shares the query token (so the keyword leg
// matches it) but is semantically far; without the gate it's recalled, with the
// gate it's dropped while the on-topic memory survives.
func TestRecallMinSemanticScoreGate(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	svc := service.New(st, embedtest.New(dims), service.WithSyncReinforce())
	ns := "alice"

	// Identical token bag to the query → top vector score (~1.0).
	if _, err := svc.Remember(ctx, service.RememberInput{Namespace: ns, Content: "memory", Tier: memory.TierSemantic}); err != nil {
		t.Fatalf("remember relevant: %v", err)
	}
	// Shares the token "memory" (keyword leg matches) but is mostly off-topic →
	// low vector score. This is the cross-topic poison the gate must suppress.
	const poison = "memory workspace cleanup dispatch grooming reorganization bloat duplication chores"
	if _, err := svc.Remember(ctx, service.RememberInput{Namespace: ns, Content: poison, Tier: memory.TierSemantic}); err != nil {
		t.Fatalf("remember poison: %v", err)
	}

	contents := func(in service.RecallInput) []string {
		res, err := svc.Recall(ctx, in)
		if err != nil {
			t.Fatalf("recall: %v", err)
		}
		out := make([]string, len(res))
		for i, r := range res {
			out[i] = r.Memory.Content
		}
		return out
	}

	// Gate off: both surface (the poison via its keyword match).
	if got := contents(service.RecallInput{Namespace: ns, Query: "memory", Limit: 10}); len(got) != 2 {
		t.Fatalf("gate off: want 2 results (relevant + poison), got %d: %v", len(got), got)
	}

	// Gate on at a high floor: only the near-identical memory clears it; the
	// keyword-matched poison is excluded despite its token overlap.
	got := contents(service.RecallInput{Namespace: ns, Query: "memory", Limit: 10, MinSemanticScore: 0.9})
	if len(got) != 1 || got[0] != "memory" {
		t.Fatalf("gate on: want only the on-topic memory, got %v", got)
	}

	// Floor above the max possible score → nothing clears it → empty recall
	// (inject nothing), rather than the keyword leg backfilling the poison.
	if got := contents(service.RecallInput{Namespace: ns, Query: "memory", Limit: 10, MinSemanticScore: 1.01}); len(got) != 0 {
		t.Fatalf("floor above max score: want empty recall, got %v", got)
	}
}
