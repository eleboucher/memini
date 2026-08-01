package service_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/eleboucher/memini/internal/embed/embedtest"
	"github.com/eleboucher/memini/internal/llm"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/service"
	"github.com/eleboucher/memini/internal/store"
)

// cancelUpsertStore cancels the request context on entry to Upsert and then
// asserts the context the store actually receives is still live. It is the most
// direct expression of the property under test: by the time we have decided to
// store a memory, the caller going away must not be able to undo it.
type cancelUpsertStore struct {
	store.Store
	cancel context.CancelFunc
	t      *testing.T
}

func (c *cancelUpsertStore) Upsert(ctx context.Context, m *memory.Memory) error {
	c.cancel()
	// Give the cancellation a moment to propagate to anything derived from the
	// request context.
	time.Sleep(10 * time.Millisecond)
	if err := ctx.Err(); err != nil {
		c.t.Errorf("Upsert received a cancelled context (%v); an accepted write must not be "+
			"rolled back because the client hung up", err)
	}
	return c.Store.Upsert(ctx, m)
}

// TestRememberUpsertRunsOnAnUncancellableContext pins the durable-write
// contract. Every driver's Upsert is a transaction, and both database/sql and
// pgx roll an in-flight transaction back when its context is cancelled — so
// without store.DurableCtx a client disconnect landing between "we decided to
// store this" and the commit silently loses an accepted memory and reports a
// 500 for it.
func TestRememberUpsertRunsOnAnUncancellableContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	base := openTestStore(t)
	st := &cancelUpsertStore{Store: base, cancel: cancel, t: t}
	svc := service.New(st, embedtest.New(dims), service.WithSyncReinforce())

	got, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "alice", Content: "a write the client abandoned mid-flight",
		Tier: memory.TierSemantic,
	})
	if err != nil {
		t.Fatalf("remember: %v", err)
	}

	// And it really landed: read it back on a fresh context.
	stored, err := base.Get(context.Background(), "alice", got.ID)
	if err != nil {
		t.Fatalf("the accepted write was lost to the client's disconnect: %v", err)
	}
	if stored.Content != "a write the client abandoned mid-flight" {
		t.Fatalf("stored content = %q, want the written content", stored.Content)
	}
}

// errConsolidator fails every consolidation decision.
type mergeConsolidator struct{ target string }

func (m mergeConsolidator) Consolidate(context.Context, llm.Input) (llm.Decision, error) {
	return llm.Decision{Action: llm.ActionUpdate, Target: m.target, Content: "merged content"}, nil
}

// flakyEmbedder succeeds n times, then fails. It reproduces the specific shape
// that used to hard-fail a write: an embedder healthy enough to answer the
// write embed and flaky enough to fail the merge re-embed that follows.
type flakyEmbedder struct {
	dims int
	ok   *int
}

func (f flakyEmbedder) Dims() int { return f.dims }

func (f flakyEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if *f.ok <= 0 {
		return nil, errors.New("flakyEmbedder: exhausted")
	}
	*f.ok--
	return embedtest.New(f.dims).Embed(ctx, texts)
}

// TestConsolidateSyncEmbedFailureStoresRawInsteadOfFailing pins P0-3. The
// re-embed inside applyUpdate bypassed the write path's degrade contract
// entirely: it called the embedder unbounded and returned a fatal error, so a
// transiently flaky embedder turned a perfectly storable write into a 500 —
// a hundred lines from the code that carefully degrades the write embed.
func TestConsolidateSyncEmbedFailureStoresRawInsteadOfFailing(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	seed := service.New(st, embedtest.New(dims), service.WithSyncReinforce())
	target, err := seed.Remember(ctx, service.RememberInput{
		Namespace: "alice", Content: "the original durable fact", Tier: memory.TierSemantic,
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// One successful embed (the write), then failure (the merge re-embed).
	budget := 1
	svc := service.New(st, flakyEmbedder{dims: dims, ok: &budget},
		service.WithSyncReinforce(),
		service.WithConsolidator(mergeConsolidator{target: target.ID}),
		service.WithConsolidateMode(service.ConsolidateSync),
		service.WithConsolidateMinScore(0),
		service.WithWriteEmbedTimeout(time.Second))

	got, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "alice", Content: "a related durable fact worth merging", Tier: memory.TierSemantic,
	})
	if err != nil {
		t.Fatalf("a failed merge re-embed should fall through to a normal insert, not fail: %v", err)
	}
	if got == nil {
		t.Fatal("remember returned no memory")
	}
	if _, err := st.Get(ctx, "alice", got.ID); err != nil {
		t.Fatalf("the write was not stored after the merge failed: %v", err)
	}
}

// failingSearchStore fails the vector search consolidation uses to find merge
// candidates.
type failingSearchStore struct{ store.Store }

func (failingSearchStore) VectorSearch(
	context.Context, string, []float32, store.Filter, int,
) ([]store.Scored, error) {
	return nil, errors.New("search boom")
}

// TestConsolidateSyncSearchFailureStoresRaw pins P0-4: the candidate search is
// how consolidation finds something to merge into, and failing to find one is
// not a reason to reject the memory.
func TestConsolidateSyncSearchFailureStoresRaw(t *testing.T) {
	ctx := context.Background()
	st := &failingSearchStore{Store: openTestStore(t)}
	svc := service.New(st, embedtest.New(dims), service.WithSyncReinforce(),
		service.WithConsolidator(mergeConsolidator{target: "whatever"}),
		service.WithConsolidateMode(service.ConsolidateSync),
		service.WithConsolidateMinScore(0))

	got, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "alice", Content: "stored despite a failed candidate search", Tier: memory.TierSemantic,
	})
	if err != nil {
		t.Fatalf("a failed consolidate search should degrade, not fail the write: %v", err)
	}
	if got == nil {
		t.Fatal("remember returned no memory")
	}
}

// TestBriefingDropsAnUnavailableSecondaryLeg confirms one unreachable ancestor
// does not take down a briefing the primary namespace can already answer, and
// that the dropped namespace is named rather than silently omitted — a briefing
// that quietly loses an ancestor's durable facts is precisely the case where an
// agent cannot know what it is missing.
func TestBriefingDropsAnUnavailableSecondaryLeg(t *testing.T) {
	ctx := context.Background()
	base := openTestStore(t)
	seed := service.New(base, embedtest.New(dims), service.WithSyncReinforce())
	if _, err := seed.Remember(ctx, service.RememberInput{
		Namespace: "acme/phoenix", Content: "the phoenix service deploys via flux",
		Tier: memory.TierSemantic,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	svc := service.New(failingListStore{Store: base, failNS: "acme"}, embedtest.New(dims),
		service.WithSyncReinforce())
	b, err := svc.Briefing(ctx, "acme/phoenix", service.BriefingOpts{})
	if err != nil {
		t.Fatalf("a failing ancestor leg should degrade the briefing, not fail it: %v", err)
	}
	if len(b.Facts) == 0 {
		t.Fatal("no facts; the primary namespace should still have contributed")
	}
	if len(b.Degraded) == 0 || b.Degraded[0] != "acme" {
		t.Fatalf("Briefing.Degraded = %v, want it to name the unreachable ancestor", b.Degraded)
	}
}

// TestBriefingFailsWhenThePrimaryLegFails is the deliberate non-degradation: a
// briefing without the project's own context is not a briefing.
func TestBriefingFailsWhenThePrimaryLegFails(t *testing.T) {
	st := failingListStore{Store: openTestStore(t), failNS: "acme/phoenix"}
	svc := service.New(st, embedtest.New(dims), service.WithSyncReinforce())

	if _, err := svc.Briefing(context.Background(), "acme/phoenix", service.BriefingOpts{}); err == nil {
		t.Fatal("a failing primary leg must fail the briefing, not return an empty one")
	}
}

// failingListStore fails List for one namespace and serves every other.
type failingListStore struct {
	store.Store
	failNS string
}

func (f failingListStore) List(
	ctx context.Context, ns string, filter store.Filter, limit int,
) ([]*memory.Memory, error) {
	if ns == f.failNS {
		return nil, errors.New("list boom")
	}
	return f.Store.List(ctx, ns, filter, limit)
}

// TestDegradedWireRendersEachCombination pins the shared renderer both
// transports now use, so a dropped-namespace report cannot end up surfaced on
// one surface and not the other.
func TestDegradedWireRendersEachCombination(t *testing.T) {
	if v, n := service.DegradedWire("", nil); v != "" || n != "" {
		t.Fatalf("healthy recall rendered (%q, %q), want empty", v, n)
	}
	v, n := service.DegradedWire("embed_timeout", nil)
	if v != "keyword_only" || n == "" {
		t.Fatalf("embed-only degradation rendered (%q, %q), want keyword_only with a note", v, n)
	}
	v, n = service.DegradedWire("", []string{"acme"})
	if v != "partial" || n == "" {
		t.Fatalf("dropped-namespace degradation rendered (%q, %q), want partial with a note", v, n)
	}
	v, n = service.DegradedWire("embed_error", []string{"acme", "acme/phoenix"})
	if v != "partial" || n == "" {
		t.Fatalf("combined degradation rendered (%q, %q), want partial with a note", v, n)
	}
}
