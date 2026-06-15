package service_test

import (
	"context"
	"errors"
	"testing"
	"time"

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

// slowReranker blocks until its context is canceled, simulating a backend that
// is too slow to answer within the rerank timeout.
type slowReranker struct{ err error }

func (r *slowReranker) Rerank(ctx context.Context, _ string, _ []rerank.Candidate) ([]string, error) {
	<-ctx.Done()
	r.err = ctx.Err()
	return nil, ctx.Err()
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

func TestRecallRerankerPoolSatisfiesLimit(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	// reverseReranker returns every candidate it is given, so the result size is
	// bounded by the pool the service hands it.
	rr := &reverseReranker{}
	// rerankTopN is deliberately smaller (2) than the caller's limit (5).
	svc := service.New(st, embedtest.New(dims), service.WithSyncReinforce(), service.WithReranker(rr, "test", 2))
	for _, d := range []string{"alpha fruit", "beta fruit", "gamma fruit", "delta fruit", "epsilon fruit"} {
		if _, err := svc.Remember(ctx, service.RememberInput{Namespace: "alice", Content: d, Tier: memory.TierSemantic}); err != nil {
			t.Fatalf("remember: %v", err)
		}
	}
	res, err := svc.Recall(ctx, service.RecallInput{Namespace: "alice", Query: "fruit", Limit: 5})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	// The pool must be sized to max(limit, rerankTopN); the old pool=rerankTopN
	// capped the reranked result at 2 even though the caller asked for 5.
	if len(res) != 5 {
		t.Fatalf("reranked recall returned %d, want 5 (pool must satisfy the limit, not cap at rerankTopN=2)", len(res))
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

func TestRecallRerankerTimeoutFallsBackToComposite(t *testing.T) {
	st := openTestStore(t)
	base := service.New(st, embedtest.New(dims), service.WithSyncReinforce())
	ingestTwo(t, base)
	baseIDs := recallIDs(t, base)

	rr := &slowReranker{}
	svc := service.New(st, embedtest.New(dims), service.WithSyncReinforce(),
		service.WithReranker(rr, "test", 0), service.WithRerankTimeout(10*time.Millisecond))
	got := recallIDs(t, svc)
	if !errors.Is(rr.err, context.DeadlineExceeded) {
		t.Fatalf("reranker should be canceled by the rerank timeout, got %v", rr.err)
	}
	if len(got) != len(baseIDs) || got[0] != baseIDs[0] {
		t.Fatalf("timed-out rerank should keep composite order: base=%v got=%v", baseIDs, got)
	}
}
