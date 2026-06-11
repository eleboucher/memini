package service_test

import (
	"context"
	"errors"
	"testing"

	"github.com/eleboucher/memini/internal/embed/embedtest"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/rerank"
	"github.com/eleboucher/memini/internal/service"
)

// reverseReranker reorders candidates last-first; errReranker always fails.
type reverseReranker struct{ called bool }

func (r *reverseReranker) Rerank(_ context.Context, _ string, c []rerank.Candidate) ([]string, error) {
	r.called = true
	out := make([]string, len(c))
	for i := range c {
		out[len(c)-1-i] = c[i].ID
	}
	return out, nil
}

type errReranker struct{ called bool }

func (r *errReranker) Rerank(_ context.Context, _ string, _ []rerank.Candidate) ([]string, error) {
	r.called = true
	return nil, errors.New("boom")
}

func ingestTwo(t *testing.T, svc *service.Service) {
	t.Helper()
	ctx := context.Background()
	for _, d := range []string{"alpha apple fruit", "beta banana fruit"} {
		if _, err := svc.Remember(ctx, service.RememberInput{Namespace: "alice", Content: d, Tier: memory.TierSemantic}); err != nil {
			t.Fatalf("remember: %v", err)
		}
	}
}

func recallIDs(t *testing.T, svc *service.Service) []string {
	t.Helper()
	res, err := svc.Recall(context.Background(), service.RecallInput{Namespace: "alice", Query: "fruit", Limit: 2})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	ids := make([]string, len(res))
	for i, r := range res {
		ids[i] = r.Memory.ID
	}
	return ids
}

func TestRecallRerankerReordersByVerdict(t *testing.T) {
	st := openTestStore(t)
	base := service.New(st, embedtest.New(dims), service.WithSyncReinforce())
	ingestTwo(t, base)
	baseIDs := recallIDs(t, base)
	if len(baseIDs) != 2 {
		t.Fatalf("baseline returned %d, want 2", len(baseIDs))
	}

	rr := &reverseReranker{}
	svc := service.New(st, embedtest.New(dims), service.WithSyncReinforce(), service.WithReranker(rr, "test", 0))
	got := recallIDs(t, svc)
	if !rr.called {
		t.Fatal("reranker not invoked")
	}
	if got[0] != baseIDs[1] || got[1] != baseIDs[0] {
		t.Fatalf("rerank did not reverse: base=%v got=%v", baseIDs, got)
	}
}

func TestRecallRerankerFallsBackOnError(t *testing.T) {
	st := openTestStore(t)
	base := service.New(st, embedtest.New(dims), service.WithSyncReinforce())
	ingestTwo(t, base)
	baseIDs := recallIDs(t, base)

	rr := &errReranker{}
	svc := service.New(st, embedtest.New(dims), service.WithSyncReinforce(), service.WithReranker(rr, "test", 0))
	got := recallIDs(t, svc)
	if !rr.called {
		t.Fatal("reranker not invoked")
	}
	if len(got) != len(baseIDs) || got[0] != baseIDs[0] {
		t.Fatalf("failed rerank should keep composite order: base=%v got=%v", baseIDs, got)
	}
}
