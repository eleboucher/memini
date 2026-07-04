package service

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/eleboucher/memini/internal/embed"
	"github.com/eleboucher/memini/internal/embed/embedtest"
	"github.com/eleboucher/memini/internal/llm"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/store"
	"github.com/eleboucher/memini/internal/store/sqlitevec"
)

const testDims = 64

// recordingConsolidator records how many times it was consulted.
type recordingConsolidator struct {
	called int
	dec    llm.Decision
}

func (r *recordingConsolidator) Consolidate(_ context.Context, in llm.Input) (llm.Decision, error) {
	r.called++
	return r.dec, nil
}

type countingMetrics struct {
	results map[string]int
	depth   int
}

func newCountingMetrics() *countingMetrics                     { return &countingMetrics{results: map[string]int{}} }
func (m *countingMetrics) ConsolidateResult(r string)          { m.results[r]++ }
func (m *countingMetrics) ConsolidateQueueDepth(d int)         { m.depth = d }
func (m *countingMetrics) RememberResult(string, string)       {}
func (m *countingMetrics) RecallResult(string, string, string) {}
func (m *countingMetrics) ForgetResult(string)                 {}
func (m *countingMetrics) SupersedeResult(string)              {}
func (m *countingMetrics) PromoteResult(string, int)           {}
func (m *countingMetrics) FsckResult(string)                   {}
func (m *countingMetrics) OpDuration(string, time.Duration)    {}
func (m *countingMetrics) AnswerResult(string)                 {}
func (m *countingMetrics) RerankResult(string, string)         {}
func (m *countingMetrics) RecallDegraded(string)               {}
func (m *countingMetrics) WriteSanitized(string)               {}
func (m *countingMetrics) ReinforceResult(r string)            { m.results["reinforce:"+r]++ }
func (m *countingMetrics) DedupTombstoned(int)                 {}
func (m *countingMetrics) CorroborateResult(string)            {}
func (m *countingMetrics) ContradictResult(string)             {}
func (m *countingMetrics) TierClassified(string)               {}

func newAsyncSvc(t *testing.T, fc llm.Consolidator, minScore float64, mx Metrics) (*Service, store.Store) {
	t.Helper()
	st, err := sqlitevec.Open(context.Background(), filepath.Join(t.TempDir(), "a.db"), testDims)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	opts := []Option{
		WithConsolidator(fc),
		WithConsolidateMode(ConsolidateAsync),
		WithConsolidateMinScore(minScore),
		WithSyncReinforce(),
		WithClock(func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }),
	}
	if mx != nil {
		opts = append(opts, WithMetrics(mx))
	}
	return New(st, embedtest.New(testDims), opts...), st
}

func put(t *testing.T, st store.Store, e embed.Embedder, id, content string) *memory.Memory {
	t.Helper()
	vec, err := embed.EmbedOne(context.Background(), e, content)
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	now := time.Unix(1_700_000_000, 0).UTC()
	m := &memory.Memory{
		ID: id, Namespace: "ns", Tier: memory.TierSemantic, Content: content,
		CreatedAt: now, UpdatedAt: now, LastAccessedAt: now, Embedding: vec,
	}
	if err := st.Upsert(context.Background(), m); err != nil {
		t.Fatalf("upsert %s: %v", id, err)
	}
	return m
}

func TestGateSkipsLLMBelowThreshold(t *testing.T) {
	fc := &recordingConsolidator{}
	// "the sky is green" vs "the sky is blue" score ≈ 0.586 with the fake
	// embedder; a 0.7 gate must skip the LLM.
	svc, st := newAsyncSvc(t, fc, 0.7, nil)
	e := embedtest.New(testDims)
	put(t, st, e, "a", "the sky is green")
	put(t, st, e, "b", "the sky is blue")

	svc.consolidateOne(context.Background(), consolidateJob{namespace: "ns", id: "b"})
	if fc.called != 0 {
		t.Fatalf("gate should have skipped the LLM, but it was called %d times", fc.called)
	}
}

func TestGateAllowsLLMAboveThreshold(t *testing.T) {
	fc := &recordingConsolidator{dec: llm.Decision{Action: llm.ActionNew}}
	svc, st := newAsyncSvc(t, fc, 0.5, nil) // 0.586 > 0.5 → not gated
	e := embedtest.New(testDims)
	put(t, st, e, "a", "the sky is green")
	put(t, st, e, "b", "the sky is blue")

	svc.consolidateOne(context.Background(), consolidateJob{namespace: "ns", id: "b"})
	if fc.called != 1 {
		t.Fatalf("LLM should have been consulted once, got %d", fc.called)
	}
}

func TestConsolidateOneExcludesSelf(t *testing.T) {
	fc := &recordingConsolidator{}
	// Gate disabled (0): only an empty candidate set should skip the LLM. With
	// just the memory itself in the store, self-exclusion must leave no candidates.
	svc, st := newAsyncSvc(t, fc, 0, nil)
	e := embedtest.New(testDims)
	put(t, st, e, "solo", "a unique durable fact")

	svc.consolidateOne(context.Background(), consolidateJob{namespace: "ns", id: "solo"})
	if fc.called != 0 {
		t.Fatalf("self should be excluded, leaving no candidates; LLM called %d times", fc.called)
	}
}

func TestConsolidateOneSupersede(t *testing.T) {
	fc := &recordingConsolidator{}
	svc, st := newAsyncSvc(t, fc, 0.5, nil)
	e := embedtest.New(testDims)
	a := put(t, st, e, "a", "the sky is green")
	b := put(t, st, e, "b", "the sky is blue")
	fc.dec = llm.Decision{Action: llm.ActionSupersede, Target: a.ID}

	svc.consolidateOne(context.Background(), consolidateJob{namespace: "ns", id: b.ID})

	got, err := st.Get(context.Background(), "ns", a.ID)
	if err != nil {
		t.Fatalf("get a: %v", err)
	}
	if got.SupersededBy == nil || *got.SupersededBy != b.ID {
		t.Fatalf("a.SupersededBy = %v, want %s", got.SupersededBy, b.ID)
	}
}

func TestConsolidateOneUpdateTombstonesSource(t *testing.T) {
	fc := &recordingConsolidator{}
	svc, st := newAsyncSvc(t, fc, 0.5, nil)
	e := embedtest.New(testDims)
	a := put(t, st, e, "a", "the sky is green")
	b := put(t, st, e, "b", "the sky is blue")
	fc.dec = llm.Decision{Action: llm.ActionUpdate, Target: a.ID, Content: "the sky is azure"}

	svc.consolidateOne(context.Background(), consolidateJob{namespace: "ns", id: b.ID})

	got, err := st.Get(context.Background(), "ns", a.ID)
	if err != nil {
		t.Fatalf("get a: %v", err)
	}
	if got.Content != "the sky is azure" {
		t.Fatalf("merged content not applied to target: %q", got.Content)
	}
	// The source b is tombstoned onto a (not hard-deleted), so any prior
	// supersede chain pointing at b stays resolvable. It still exists but is
	// superseded and therefore excluded from recall.
	src, err := st.Get(context.Background(), "ns", b.ID)
	if err != nil {
		t.Fatalf("source should still exist as a tombstone, got %v", err)
	}
	if src.SupersededBy == nil || *src.SupersededBy != a.ID {
		t.Fatalf("source SupersededBy = %v, want %s", src.SupersededBy, a.ID)
	}
}

func TestConsolidateOneSkipsDeleted(t *testing.T) {
	fc := &recordingConsolidator{}
	svc, _ := newAsyncSvc(t, fc, 0.5, nil)
	// Job for an ID that was never stored: must be a safe no-op.
	svc.consolidateOne(context.Background(), consolidateJob{namespace: "ns", id: "ghost"})
	if fc.called != 0 {
		t.Fatalf("deleted memory should not reach the LLM, called %d times", fc.called)
	}
}

func TestEnqueueDropsWhenFull(t *testing.T) {
	mx := newCountingMetrics()
	fc := &recordingConsolidator{}
	svc, _ := newAsyncSvc(t, fc, 0.5, mx)
	// Worker not started, so the queue never drains. Fill it to capacity, then
	// one more must be dropped.
	for range defaultConsolidateQueueCap {
		svc.enqueueConsolidate("ns", "id")
	}
	svc.enqueueConsolidate("ns", "overflow")
	if mx.results["dropped"] != 1 {
		t.Fatalf("expected exactly 1 dropped job, got %d", mx.results["dropped"])
	}
}

// deadlineCapturingConsolidator reports whether the context it was handed has a
// deadline, so a test can confirm the worker bounds each job.
type deadlineCapturingConsolidator struct {
	gotDeadline chan bool
}

func (d *deadlineCapturingConsolidator) Consolidate(ctx context.Context, _ llm.Input) (llm.Decision, error) {
	_, ok := ctx.Deadline()
	d.gotDeadline <- ok
	return llm.Decision{Action: llm.ActionNew}, nil
}

func TestConsolidatorWorkerBoundsJobContext(t *testing.T) {
	fc := &deadlineCapturingConsolidator{gotDeadline: make(chan bool, 1)}
	svc, st := newAsyncSvc(t, fc, 0, nil) // gate 0 → any candidate reaches the LLM
	e := embedtest.New(testDims)
	put(t, st, e, "a", "the sky is blue")
	put(t, st, e, "b", "the sky is azure")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go svc.StartConsolidator(ctx)
	svc.enqueueConsolidate("ns", "b")

	select {
	case ok := <-fc.gotDeadline:
		if !ok {
			t.Error("consolidation job ctx had no deadline; the worker must bound each job")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("consolidator was not called within 2s")
	}
}

// fakeDistiller returns scripted facts (or err) and records the episodes it was
// given.
type fakeDistiller struct {
	facts []llm.Fact
	err   error
	calls int
	last  llm.DistillInput
}

func (d *fakeDistiller) Distill(_ context.Context, in llm.DistillInput) ([]llm.Fact, error) {
	d.calls++
	d.last = in
	return d.facts, d.err
}

func newPromoterSvc(t *testing.T, fd llm.Distiller) (*Service, store.Store) {
	t.Helper()
	st, err := sqlitevec.Open(context.Background(), filepath.Join(t.TempDir(), "p.db"), testDims)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return New(st, embedtest.New(testDims),
		WithDistiller(fd),
		WithPromoteMinAccess(3),
		// Store distilled facts plainly so the test asserts promotion, not dedup.
		WithConsolidateMode(ConsolidateOff),
		WithSyncReinforce(),
		WithClock(func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }),
	), st
}

func putEpisodic(t *testing.T, st store.Store, e embed.Embedder, id, content string, access int) {
	t.Helper()
	vec, err := embed.EmbedOne(context.Background(), e, content)
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	now := time.Unix(1_700_000_000, 0).UTC()
	m := &memory.Memory{
		ID: id, Namespace: "ns", Tier: memory.TierEpisodic, Content: content,
		AccessCount: access, CreatedAt: now, UpdatedAt: now, LastAccessedAt: now, Embedding: vec,
	}
	if err := st.Upsert(context.Background(), m); err != nil {
		t.Fatalf("upsert %s: %v", id, err)
	}
}

func TestPromoteWritesFactsAndStampsSources(t *testing.T) {
	fd := &fakeDistiller{facts: []llm.Fact{{Content: "user prefers Go", Summary: "lang pref"}}}
	svc, st := newPromoterSvc(t, fd)
	e := embedtest.New(testDims)
	// Two eligible (access >= 3), one below threshold.
	putEpisodic(t, st, e, "hot1", "wrote a lot of go code", 5)
	putEpisodic(t, st, e, "hot2", "more go work today", 3)
	putEpisodic(t, st, e, "cold", "a one-off note", 1)

	n, err := svc.Promote(context.Background())
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 fact written, got %d", n)
	}
	if fd.calls != 1 || len(fd.last.Episodes) != 2 {
		t.Fatalf("expected 1 distill call with 2 eligible episodes, got calls=%d episodes=%d",
			fd.calls, len(fd.last.Episodes))
	}

	// The distilled fact should now be a durable semantic memory.
	res, err := svc.Recall(context.Background(), RecallInput{
		Namespace: "ns", Query: "go preference", Tiers: []memory.Tier{memory.TierSemantic}, Limit: 5,
	})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(res) == 0 {
		t.Fatal("expected the promoted semantic fact to be recallable")
	}

	// Sources must be stamped so a second run reprocesses nothing.
	hot1, err := st.Get(context.Background(), "ns", "hot1")
	if err != nil {
		t.Fatalf("get hot1: %v", err)
	}
	if hot1.Metadata["promoted_at"] == nil {
		t.Fatalf("source should be stamped promoted_at, got %v", hot1.Metadata)
	}

	fd.calls = 0
	if _, err := svc.Promote(context.Background()); err != nil {
		t.Fatalf("promote 2: %v", err)
	}
	if fd.calls != 0 {
		t.Fatalf("second run should reprocess nothing, but distiller was called %d times", fd.calls)
	}
}

func TestTierForCategory(t *testing.T) {
	cases := map[string]memory.Tier{
		"procedure":  memory.TierProcedural,
		"PROCEDURE ": memory.TierProcedural,
		"preference": memory.TierSemantic,
		"fact":       memory.TierSemantic,
		"":           memory.TierSemantic,
		"bogus":      memory.TierSemantic,
	}
	for in, want := range cases {
		if got := tierForCategory(in); got != want {
			t.Errorf("tierForCategory(%q) = %q, want %q", in, got, want)
		}
	}
}

// A distilled "procedure" (incl. error→recovery) is stored in the procedural
// tier; a "preference" stays semantic. Both carry a distill_category tag.
func TestPromoteRoutesCategoryToTier(t *testing.T) {
	fd := &fakeDistiller{facts: []llm.Fact{
		{Content: "when go test fails on a dirty cache, run go clean -testcache", Category: "procedure"},
		{Content: "the user prefers tabs over spaces", Category: "preference"},
	}}
	svc, st := newPromoterSvc(t, fd)
	putEpisodic(t, st, embedtest.New(testDims), "hot1", "ran go clean -testcache to fix the tests", 5)

	if _, err := svc.Promote(context.Background()); err != nil {
		t.Fatalf("promote: %v", err)
	}

	proc, err := svc.List(context.Background(), ListInput{Namespace: "ns", Tiers: []memory.Tier{memory.TierProcedural}})
	if err != nil {
		t.Fatalf("list procedural: %v", err)
	}
	if len(proc) != 1 || proc[0].Metadata["distill_category"] != "procedure" {
		t.Fatalf("procedure fact should land in procedural tier with category tag, got %+v", proc)
	}

	sem, err := svc.List(context.Background(), ListInput{Namespace: "ns", Tiers: []memory.Tier{memory.TierSemantic}})
	if err != nil {
		t.Fatalf("list semantic: %v", err)
	}
	if len(sem) != 1 || sem[0].Metadata["distill_category"] != "preference" {
		t.Fatalf("preference fact should land in semantic tier, got %+v", sem)
	}
}

// A source episodic captured from a failed turn (metadata.failed) reaches the
// distiller with a "[failed]" content prefix so it can be mined into recovery.
func TestPromoteMarksFailedEpisodes(t *testing.T) {
	fd := &fakeDistiller{facts: []llm.Fact{{Content: "x", Category: "procedure"}}}
	svc, st := newPromoterSvc(t, fd)
	vec, err := embed.EmbedOne(context.Background(), embedtest.New(testDims), "go test errored")
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	now := time.Unix(1_700_000_000, 0).UTC()
	if err := st.Upsert(context.Background(), &memory.Memory{
		ID: "f1", Namespace: "ns", Tier: memory.TierEpisodic, Content: "go test ./... errored",
		AccessCount: 5, CreatedAt: now, UpdatedAt: now, LastAccessedAt: now, Embedding: vec,
		Metadata: map[string]any{"failed": true},
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	if _, err := svc.Promote(context.Background()); err != nil {
		t.Fatalf("promote: %v", err)
	}
	if len(fd.last.Episodes) != 1 {
		t.Fatalf("want 1 distilled episode, got %d", len(fd.last.Episodes))
	}
	if got := fd.last.Episodes[0].Content; got != "[failed] go test ./... errored" {
		t.Fatalf("failed episode content = %q, want it prefixed with [failed]", got)
	}
}

func TestPromoteStampsSourcesBeforeDistilling(t *testing.T) {
	// Distillation fails, so no facts are written. The source must still be
	// stamped (stamping happens before distilling), so the next tick does NOT
	// re-distill it — which, distillation being non-deterministic, would emit a
	// differently worded duplicate fact.
	fd := &fakeDistiller{err: errors.New("distill boom")}
	svc, st := newPromoterSvc(t, fd)
	e := embedtest.New(testDims)
	putEpisodic(t, st, e, "hot1", "wrote a lot of go code", 5)

	// Promote swallows per-batch errors (logs + continues), so it returns 0,nil.
	n, err := svc.Promote(context.Background())
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	if n != 0 {
		t.Fatalf("distill failed, want 0 facts written, got %d", n)
	}
	if fd.calls != 1 {
		t.Fatalf("distiller should have been called once, got %d", fd.calls)
	}

	got, err := st.Get(context.Background(), "ns", "hot1")
	if err != nil {
		t.Fatalf("get hot1: %v", err)
	}
	if got.Metadata["promoted_at"] == nil {
		t.Fatal("source must be stamped before distillation so a distill failure cannot cause re-distillation")
	}

	// Confirm the idempotency guarantee: a second run re-distills nothing.
	fd.calls = 0
	if _, err := svc.Promote(context.Background()); err != nil {
		t.Fatalf("promote 2: %v", err)
	}
	if fd.calls != 0 {
		t.Fatalf("stamped source must not be reprocessed; distiller called %d times", fd.calls)
	}
}

func TestPromoteGroundsEpisodeDates(t *testing.T) {
	fd := &fakeDistiller{facts: []llm.Fact{{Content: "switched to Go on 2023-11-13", Summary: "lang"}}}
	svc, st := newPromoterSvc(t, fd)
	putEpisodic(t, st, embedtest.New(testDims), "hot1", "yesterday we switched to Go", 5)

	if _, err := svc.Promote(context.Background()); err != nil {
		t.Fatalf("promote: %v", err)
	}
	// The distiller receives the reference date and each episode's record date
	// (fixed test clock: Unix 1_700_000_000 == 2023-11-14) so it can ground
	// "yesterday" to an absolute date.
	const wantDate = "2023-11-14"
	if fd.last.Now != wantDate {
		t.Fatalf("DistillInput.Now = %q, want %q", fd.last.Now, wantDate)
	}
	if len(fd.last.Episodes) != 1 || fd.last.Episodes[0].Date != wantDate {
		t.Fatalf("episode date not grounded: %+v", fd.last.Episodes)
	}
	if fd.last.Episodes[0].Content != "yesterday we switched to Go" {
		t.Fatalf("episode content = %q", fd.last.Episodes[0].Content)
	}
}

func TestPromoteNoDistillerIsNoop(t *testing.T) {
	st, err := sqlitevec.Open(context.Background(), filepath.Join(t.TempDir(), "p.db"), testDims)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	svc := New(st, embedtest.New(testDims), WithSyncReinforce())
	if n, err := svc.Promote(context.Background()); err != nil || n != 0 {
		t.Fatalf("no-distiller Promote should be a 0,nil no-op, got %d,%v", n, err)
	}
}

func TestRememberAsyncReturnsBeforeConsolidation(t *testing.T) {
	fc := &recordingConsolidator{dec: llm.Decision{Action: llm.ActionNew}}
	svc, _ := newAsyncSvc(t, fc, 0.5, nil)
	// In async mode the write returns immediately and is enqueued, not consolidated
	// inline, so the LLM has not been called by the time Remember returns.
	_, err := svc.Remember(context.Background(), RememberInput{
		Namespace: "ns", Content: "a durable fact", Tier: memory.TierSemantic,
	})
	if err != nil {
		t.Fatalf("remember: %v", err)
	}
	if fc.called != 0 {
		t.Fatalf("async Remember must not consolidate inline, LLM called %d times", fc.called)
	}
	if len(svc.consolidateQueue) != 1 {
		t.Fatalf("expected 1 queued job, got %d", len(svc.consolidateQueue))
	}
}
