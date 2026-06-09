package service_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/eleboucher/memini/internal/embed/embedtest"
	"github.com/eleboucher/memini/internal/llm"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/service"
	"github.com/eleboucher/memini/internal/store/sqlitevec"
)

// fakeConsolidator returns a scripted decision and records the last input.
type fakeConsolidator struct {
	dec  llm.Decision
	last llm.Input
}

func (f *fakeConsolidator) Consolidate(_ context.Context, in llm.Input) (llm.Decision, error) {
	f.last = in
	return f.dec, nil
}

func newConsolidatingService(t *testing.T, fc *fakeConsolidator) *service.Service {
	t.Helper()
	st, err := sqlitevec.Open(context.Background(), filepath.Join(t.TempDir(), "c.db"), dims)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return service.New(st, embedtest.New(dims),
		service.WithConsolidator(fc),
		// Sync mode so a write reflects its consolidated result immediately, and
		// gate disabled so the LLM is always consulted (the fake embedder's
		// similar fixtures score below the production gate).
		service.WithConsolidateMode(service.ConsolidateSync),
		service.WithConsolidateMinScore(0),
		service.WithSyncReinforce(),
		service.WithClock(func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }),
	)
}

func remember(t *testing.T, svc *service.Service, content string) *memory.Memory {
	t.Helper()
	m, err := svc.Remember(context.Background(), service.RememberInput{
		Namespace: "alice", Content: content, Tier: memory.TierSemantic,
	})
	if err != nil {
		t.Fatalf("remember %q: %v", content, err)
	}
	return m
}

func TestConsolidateSupersede(t *testing.T) {
	fc := &fakeConsolidator{}
	svc := newConsolidatingService(t, fc)
	ctx := context.Background()

	// First write: no candidates yet, stored as-is regardless of the decision.
	first := remember(t, svc, "the sky is green")

	// Second write contradicts the first; script a supersede of it.
	fc.dec = llm.Decision{Action: llm.ActionSupersede, Target: first.ID}
	second := remember(t, svc, "the sky is blue")

	if len(fc.last.Candidates) == 0 {
		t.Fatal("expected the first memory to be offered as a candidate")
	}

	// Default recall returns only the new memory; the old one is tombstoned.
	res, err := svc.Recall(ctx, service.RecallInput{Namespace: "alice", Query: "the sky", Limit: 10})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	for _, r := range res {
		if r.Memory.ID == first.ID {
			t.Fatalf("superseded memory %s should not appear in recall", first.ID)
		}
	}
	old, err := svc.Get(ctx, "alice", first.ID)
	if err != nil {
		t.Fatalf("get old: %v", err)
	}
	if old.SupersededBy == nil || *old.SupersededBy != second.ID {
		t.Fatalf("old.SupersededBy = %v, want %s", old.SupersededBy, second.ID)
	}
}

func TestConsolidateUpdateMergesInPlace(t *testing.T) {
	fc := &fakeConsolidator{}
	svc := newConsolidatingService(t, fc)
	ctx := context.Background()

	first := remember(t, svc, "postgres is a database")

	fc.dec = llm.Decision{
		Action: llm.ActionUpdate, Target: first.ID,
		Content: "postgres is an advanced relational database",
	}
	got := remember(t, svc, "postgres rdbms")

	if got.ID != first.ID {
		t.Fatalf("update should return the merged target id %s, got %s", first.ID, got.ID)
	}
	if got.Content != "postgres is an advanced relational database" {
		t.Fatalf("merged content not applied: %q", got.Content)
	}
	// Only one memory should exist.
	res, err := svc.Recall(ctx, service.RecallInput{Namespace: "alice", Query: "postgres database", Limit: 10})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(res) != 1 {
		t.Fatalf("expected a single merged memory, got %d", len(res))
	}
}

func TestConsolidateNewKeepsBoth(t *testing.T) {
	fc := &fakeConsolidator{dec: llm.Decision{Action: llm.ActionNew}}
	svc := newConsolidatingService(t, fc)
	ctx := context.Background()

	remember(t, svc, "postgres is a database")
	remember(t, svc, "redis is an in-memory store")

	res, err := svc.Recall(ctx, service.RecallInput{Namespace: "alice", Query: "data store", Limit: 10})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(res) != 2 {
		t.Fatalf("expected both memories, got %d", len(res))
	}
}
