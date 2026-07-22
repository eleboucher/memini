package service_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/eleboucher/memini/internal/embed/embedtest"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/service"
	"github.com/eleboucher/memini/internal/store"
)

// TestRecallMinSemanticScoreGate pins the gate semantics: the semantic-score
// floor excludes a candidate entirely, and the keyword leg cannot
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

type semanticGateFailingEmbedder struct{ dims int }

func (e semanticGateFailingEmbedder) Embed(context.Context, []string) ([][]float32, error) {
	return nil, errors.New("embed unavailable")
}

func (e semanticGateFailingEmbedder) Dims() int { return e.dims }

// semanticGateVectorFilter makes a persisted vector-backed memory invisible to
// the bounded vector pool while leaving the keyword leg untouched.
type semanticGateVectorFilter struct {
	store.Store
	excludedID string
}

func (s semanticGateVectorFilter) VectorSearch(ctx context.Context, ns string, vec []float32, f store.Filter, k int) ([]store.Scored, error) {
	rows, err := s.Store.VectorSearch(ctx, ns, vec, f, k)
	if err != nil {
		return nil, err
	}
	out := rows[:0]
	for _, row := range rows {
		if row.Memory.ID != s.excludedID {
			out = append(out, row)
		}
	}
	return out, nil
}

// TestRecallMinSemanticScoreKeepsUnknownKeywordCandidates proves the raw
// semantic floor does not turn the bounded vector pool into a keyword allowlist:
// vectorless pending memories and keyword hits with no vector score remain
// recallable, while a known low-vector candidate is removed.
func TestRecallMinSemanticScoreKeepsUnknownKeywordCandidates(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	seed := service.New(st, embedtest.New(dims), service.WithSyncReinforce())
	ns := "alice"

	if _, err := seed.Remember(ctx, service.RememberInput{Namespace: ns, Content: "memory", Tier: memory.TierSemantic}); err != nil {
		t.Fatalf("remember relevant: %v", err)
	}
	if _, err := seed.Remember(ctx, service.RememberInput{Namespace: ns, Content: "memory workspace cleanup dispatch grooming reorganization bloat duplication chores", Tier: memory.TierSemantic}); err != nil {
		t.Fatalf("remember low vector: %v", err)
	}
	outside, err := seed.Remember(ctx, service.RememberInput{Namespace: ns, Content: "memory keyword candidate outside vector results", Tier: memory.TierSemantic})
	if err != nil {
		t.Fatalf("remember outside-pool candidate: %v", err)
	}

	degraded := service.New(st, semanticGateFailingEmbedder{dims: dims}, service.WithWriteEmbedTimeout(time.Second))
	if _, err := degraded.Remember(ctx, service.RememberInput{Namespace: ns, Content: "memory pending embed keyword match", Tier: memory.TierSemantic}); err != nil {
		t.Fatalf("remember pending_embed: %v", err)
	}

	svc := service.New(semanticGateVectorFilter{Store: st, excludedID: outside.ID}, embedtest.New(dims),
		service.WithSyncReinforce(), service.WithRecallMinSemanticScore(0.9))
	res, err := svc.Recall(ctx, service.RecallInput{Namespace: ns, Query: "memory", Limit: 10})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	got := make(map[string]bool, len(res))
	for _, row := range res {
		got[row.Memory.Content] = true
	}
	if !got["memory"] || !got["memory keyword candidate outside vector results"] || !got["memory pending embed keyword match"] {
		t.Fatalf("floor should retain relevant and unknown keyword candidates, got %v", got)
	}
	if got["memory workspace cleanup dispatch grooming reorganization bloat duplication chores"] {
		t.Fatal("floor should remove the known low-vector candidate")
	}
}
