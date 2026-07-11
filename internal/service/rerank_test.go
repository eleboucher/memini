package service_test

import (
	"context"
	"errors"
	"fmt"
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

// emptyReranker returns no IDs, simulating an LLM that answers "none" or a
// cross-encoder that scored every candidate below its cutoff.
type emptyReranker struct{ called bool }

func (r *emptyReranker) Rerank(_ context.Context, _ string, _ []rerank.Candidate) ([]string, error) {
	r.called = true
	return nil, nil
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
	svc := service.New(st, embedtest.New(dims), service.WithSyncReinforce(), service.WithReranker(rr, "test"))
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
	svc := service.New(st, embedtest.New(dims), service.WithSyncReinforce(), service.WithReranker(rr, "test"))
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
		service.WithReranker(rr, "test"), service.WithRerankTimeout(10*time.Millisecond))
	got := recallIDs(t, svc)
	if !errors.Is(rr.err, context.DeadlineExceeded) {
		t.Fatalf("reranker should be canceled by the rerank timeout, got %v", rr.err)
	}
	if len(got) != len(baseIDs) || got[0] != baseIDs[0] {
		t.Fatalf("timed-out rerank should keep composite order: base=%v got=%v", baseIDs, got)
	}
}

func TestRecallRerankerFallsBackOnEmptyVerdict(t *testing.T) {
	st := openTestStore(t)
	base := service.New(st, embedtest.New(dims), service.WithSyncReinforce())
	ingestTwo(t, base)
	baseIDs := recallIDs(t, base)

	rr := &emptyReranker{}
	svc := service.New(st, embedtest.New(dims), service.WithSyncReinforce(), service.WithReranker(rr, "test"))
	got := recallIDs(t, svc)
	if !rr.called {
		t.Fatal("reranker not invoked")
	}
	if len(got) != len(baseIDs) || got[0] != baseIDs[0] {
		t.Fatalf("empty rerank verdict should keep composite order: base=%v got=%v", baseIDs, got)
	}
}

// poolReranker records the candidate pool it was handed and moves the last
// candidate to the front, so a memory the composite ranker left outside the
// caller's limit surfaces in the results iff it reached the reranker at all.
type poolReranker struct{ seen int }

func (r *poolReranker) Rerank(_ context.Context, _ string, c []rerank.Candidate) ([]string, error) {
	r.seen = len(c)
	out := make([]string, 0, len(c))
	if len(c) > 0 {
		out = append(out, c[len(c)-1].ID)
	}
	for _, cand := range c[:max(len(c)-1, 0)] {
		out = append(out, cand.ID)
	}
	return out, nil
}

// ingestFruit stores n distinct memories that all match the query "fruit", so
// the retrieval legs return them all and composite ranking has to order them.
func ingestFruit(t *testing.T, svc *service.Service, n int) {
	t.Helper()
	ctx := context.Background()
	for i := range n {
		content := fmt.Sprintf("fruit number %d is tasty", i)
		if _, err := svc.Remember(ctx, service.RememberInput{
			Namespace: "alice", Content: content, Tier: memory.TierSemantic,
		}); err != nil {
			t.Fatalf("remember %d: %v", i, err)
		}
	}
}

// The reranker's whole value is rescuing candidates the composite ranker left
// just outside the limit. Before WithRerankPool it only ever saw the final k,
// so a cross-encoder could reorder the top k but never promote rank k+1.
func TestRecallRerankPoolExceedsLimit(t *testing.T) {
	const total, limit = 12, 3

	st := openTestStore(t)
	seed := service.New(st, embedtest.New(dims), service.WithSyncReinforce())
	ingestFruit(t, seed, total)

	rr := &poolReranker{}
	svc := service.New(st, embedtest.New(dims), service.WithSyncReinforce(),
		service.WithReranker(rr, "test"), service.WithRerankPool(total))

	res, err := svc.Recall(context.Background(), service.RecallInput{
		Namespace: "alice", Query: "fruit", Limit: limit,
	})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if rr.seen <= limit {
		t.Fatalf("reranker saw %d candidates, want more than the limit of %d — "+
			"it cannot rescue a memory ranked outside the top k", rr.seen, limit)
	}
	if len(res) != limit {
		t.Fatalf("recall returned %d results, want it truncated to the limit of %d", len(res), limit)
	}
}

// The pool is a ceiling on what the reranker sees, not on what recall returns.
func TestRecallRerankPoolCapsCandidates(t *testing.T) {
	const total, limit, pool = 12, 2, 5

	st := openTestStore(t)
	seed := service.New(st, embedtest.New(dims), service.WithSyncReinforce())
	ingestFruit(t, seed, total)

	rr := &poolReranker{}
	svc := service.New(st, embedtest.New(dims), service.WithSyncReinforce(),
		service.WithReranker(rr, "test"), service.WithRerankPool(pool))

	res, err := svc.Recall(context.Background(), service.RecallInput{
		Namespace: "alice", Query: "fruit", Limit: limit,
	})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if rr.seen != pool {
		t.Fatalf("reranker saw %d candidates, want exactly the pool size %d", rr.seen, pool)
	}
	if len(res) != limit {
		t.Fatalf("recall returned %d results, want %d", len(res), limit)
	}
}

// An unset pool must not silently deepen an existing deployment's rerank calls.
func TestRecallRerankPoolDefaultsToLimit(t *testing.T) {
	const total, limit = 12, 3

	st := openTestStore(t)
	seed := service.New(st, embedtest.New(dims), service.WithSyncReinforce())
	ingestFruit(t, seed, total)

	rr := &poolReranker{}
	svc := service.New(st, embedtest.New(dims), service.WithSyncReinforce(),
		service.WithReranker(rr, "test"))

	if _, err := svc.Recall(context.Background(), service.RecallInput{
		Namespace: "alice", Query: "fruit", Limit: limit,
	}); err != nil {
		t.Fatalf("recall: %v", err)
	}
	if rr.seen != limit {
		t.Fatalf("reranker saw %d candidates with no pool configured, want the limit %d", rr.seen, limit)
	}
}
