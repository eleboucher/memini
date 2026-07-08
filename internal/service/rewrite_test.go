package service_test

import (
	"context"
	"testing"
	"time"

	"github.com/eleboucher/memini/internal/embed/embedtest"
	"github.com/eleboucher/memini/internal/search"
	"github.com/eleboucher/memini/internal/service"
	"github.com/eleboucher/memini/internal/store"
)

// blockingAnswerer simulates a slow/wedged LLM backend: it blocks for d or
// until ctx is cancelled, whichever comes first, so a bounded rewrite timeout
// can be proven to cut it off rather than actually waiting d.
type blockingAnswerer struct{ d time.Duration }

func (b *blockingAnswerer) Complete(ctx context.Context, _, _ string) (string, error) {
	select {
	case <-time.After(b.d):
		return "variant one\nvariant two", nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// fixedRewriteAnswerer is a deterministic stand-in for query expansion: it
// always splits into the same two rewrite lines.
type fixedRewriteAnswerer struct{}

func (fixedRewriteAnswerer) Complete(context.Context, string, string) (string, error) {
	return "second phrasing\nthird phrasing", nil
}

// TestQueryRewriteTimeoutFallsBackToSingleQuery confirms that a bounded
// rewrite timeout (MEMINI_RECALL_REWRITE_TIMEOUT / WithRecallRewriteTimeout)
// cuts off a slow/wedged answerer so recall proceeds with the original query
// alone, instead of riding along the LLM client's much longer HTTP timeout
// (120s).
func TestQueryRewriteTimeoutFallsBackToSingleQuery(t *testing.T) {
	st := openTestStore(t)
	seedHello(t, st)

	ans := &blockingAnswerer{d: 10 * time.Second}
	svc := service.New(st, embedtest.New(dims), service.WithSyncReinforce(), service.WithAnswerer(ans),
		service.WithRecallRewriteTimeout(50*time.Millisecond))

	start := time.Now()
	res, err := svc.Recall(context.Background(), service.RecallInput{
		Namespace: "alice", Query: "what does hello mean here", Limit: 5, QueryRewrite: true,
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if elapsed > time.Second {
		t.Fatalf("recall took %v, want well under 1s (rewrite timeout should cut off the blocking answerer)", elapsed)
	}
	if len(res) == 0 {
		t.Fatal("expected single-query fallback results, got none")
	}
}

// TestQueryRewriteVariantRecallsRunConcurrently proves the per-variant recalls
// that tryQueryRewrite fans out run in parallel (not one at a time), and that
// the fused result matches running each variant sequentially and fusing in
// variant order (same order, just parallel execution).
func TestQueryRewriteVariantRecallsRunConcurrently(t *testing.T) {
	st := openTestStore(t)
	seedHello(t, st)

	// 3 variants (original + 2 rewrites) x 2 legs (vector + keyword) = 6 store
	// calls. All 6 must enter before any is released -- impossible if variant
	// recalls run one at a time, since a sequential variant can't start its
	// store calls until the previous variant's Recall has already returned.
	bs := &blockingStore{Store: st, entered: make(chan struct{}, 6), release: make(chan struct{})}
	svc := service.New(bs, embedtest.New(dims), service.WithSyncReinforce(), service.WithAnswerer(fixedRewriteAnswerer{}))

	type recallOut struct {
		res []store.Scored
		err error
	}
	done := make(chan recallOut, 1)
	go func() {
		res, err := svc.Recall(context.Background(), service.RecallInput{
			Namespace: "alice", Query: "hello there general question", Limit: 5, QueryRewrite: true,
		})
		done <- recallOut{res, err}
	}()

	for i := range 6 {
		select {
		case <-bs.entered:
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d/6 store calls entered before release; variant recalls are not concurrent", i)
		}
	}
	close(bs.release)

	out := <-done
	if out.err != nil {
		t.Fatalf("recall: %v", out.err)
	}
	if len(out.res) == 0 {
		t.Fatal("expected fused results")
	}

	// Recompute the expected fusion by running each variant sequentially
	// against the unwrapped store, in the same order tryQueryRewrite uses.
	plain := service.New(st, embedtest.New(dims), service.WithSyncReinforce())
	variantQueries := []string{"hello there general question", "second phrasing", "third phrasing"}
	serial := make([][]store.Scored, 0, len(variantQueries))
	for _, q := range variantQueries {
		r, err := plain.Recall(context.Background(), service.RecallInput{Namespace: "alice", Query: q, Limit: 5})
		if err != nil {
			t.Fatalf("serial recall(%q): %v", q, err)
		}
		serial = append(serial, r)
	}
	want := search.Fuse(serial, 5, search.DefaultRRFK)

	if len(want) != len(out.res) {
		t.Fatalf("result count = %d, want %d", len(out.res), len(want))
	}
	for i := range want {
		if want[i].Memory.ID != out.res[i].Memory.ID {
			t.Fatalf("result[%d] = %s, want %s (parallel fuse order should match serial fuse order)",
				i, out.res[i].Memory.ID, want[i].Memory.ID)
		}
	}
}

// TestQueryRewriteDegradedOutParamNoRace exercises the race the parallel
// variant recalls introduced: every variant sub-recall degrades (the
// embedder always errors) and writes RecallInput.Degraded. Run with -race:
// before the fix, all variant goroutines wrote through the SAME *string
// (RecallInput.Degraded is a pointer, and `sub := in` only copies the
// pointer) -- a data race. After the fix each variant gets its own slot and
// the caller's out-param is set once, after all variants have joined.
func TestQueryRewriteDegradedOutParamNoRace(t *testing.T) {
	st := openTestStore(t)
	seedHello(t, st)

	svc := service.New(st, errEmbedder{dims: dims}, service.WithSyncReinforce(), service.WithAnswerer(fixedRewriteAnswerer{}),
		service.WithRecallEmbedTimeout(time.Second))

	var degraded string
	res, err := svc.Recall(context.Background(), service.RecallInput{
		Namespace: "alice", Query: "hello there general question", Limit: 5, QueryRewrite: true, Degraded: &degraded,
	})
	if err != nil {
		t.Fatalf("recall should degrade, not error: %v", err)
	}
	if len(res) == 0 {
		t.Fatal("keyword-only fallback returned no results")
	}
	if degraded != "embed_error" {
		t.Fatalf("Degraded out-param = %q, want %q", degraded, "embed_error")
	}
}
