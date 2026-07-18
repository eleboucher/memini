package service_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/eleboucher/memini/internal/embed"
	"github.com/eleboucher/memini/internal/embed/embedtest"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/rerank"
	"github.com/eleboucher/memini/internal/service"
	"github.com/eleboucher/memini/internal/store"
)

// seedTwoRanked stores a strongly- and a weakly-relevant memory for the query
// "postgres database" and returns a service. Both clear the server-wide fused
// gate, so a floor-free recall returns both — the composite floor under test
// then separates them by their final ranked score.
func seedTwoRanked(t *testing.T, opts ...service.Option) *service.Service {
	t.Helper()
	ctx := context.Background()
	st := openTestStore(t)
	svc := service.New(st, embedtest.New(dims), append([]service.Option{service.WithSyncReinforce()}, opts...)...)
	for _, c := range []string{
		"the production postgres database runs on port 5432",
		"the office kitchen has fresh coffee every morning",
	} {
		if _, err := svc.Remember(ctx, service.RememberInput{
			Namespace: "n", Content: c, Tier: memory.TierSemantic,
		}); err != nil {
			t.Fatalf("remember %q: %v", c, err)
		}
	}
	return svc
}

func TestRecallMinRankScoreFiltersFinalResults(t *testing.T) {
	ctx := context.Background()
	svc := seedTwoRanked(t)

	// Baseline: no floor returns both, best-first, with their composite scores.
	base, err := svc.Recall(ctx, service.RecallInput{Namespace: "n", Query: "postgres database", Limit: 5})
	if err != nil {
		t.Fatalf("baseline recall: %v", err)
	}
	if len(base) != 2 {
		t.Fatalf("baseline should return both, got %d", len(base))
	}
	if base[0].Score <= base[1].Score {
		t.Fatalf("expected distinct composite scores best-first, got %v then %v", base[0].Score, base[1].Score)
	}
	// A floor strictly between the two composite scores keeps only the strong hit.
	floor := (base[0].Score + base[1].Score) / 2

	got, err := svc.Recall(ctx, service.RecallInput{
		Namespace: "n", Query: "postgres database", Limit: 5, MinRankScore: floor,
	})
	if err != nil {
		t.Fatalf("floored recall: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("floor between the two scores should keep exactly one, got %d", len(got))
	}
	if got[0].Memory.ID != base[0].Memory.ID {
		t.Fatalf("floor kept the wrong memory: want %s, got %s", base[0].Memory.ID, got[0].Memory.ID)
	}
	for _, r := range got {
		if r.Score < floor {
			t.Fatalf("returned a hit below the floor: score %v < floor %v", r.Score, floor)
		}
	}
}

func TestRecallMinRankScoreZeroIsNoop(t *testing.T) {
	ctx := context.Background()
	svc := seedTwoRanked(t)

	absent, err := svc.Recall(ctx, service.RecallInput{Namespace: "n", Query: "postgres database", Limit: 5})
	if err != nil {
		t.Fatalf("recall (field absent): %v", err)
	}
	zero, err := svc.Recall(ctx, service.RecallInput{
		Namespace: "n", Query: "postgres database", Limit: 5, MinRankScore: 0,
	})
	if err != nil {
		t.Fatalf("recall (MinRankScore 0): %v", err)
	}
	if len(absent) != len(zero) {
		t.Fatalf("MinRankScore 0 changed the result count: absent %d, zero %d", len(absent), len(zero))
	}
	for i := range absent {
		if absent[i].Memory.ID != zero[i].Memory.ID || absent[i].Score != zero[i].Score {
			t.Fatalf("MinRankScore 0 changed result %d: %+v vs %+v", i, absent[i], zero[i])
		}
	}
}

func TestRecallMinRankScoreRejectsOutOfRange(t *testing.T) {
	ctx := context.Background()
	svc := seedTwoRanked(t)

	for _, bad := range []float64{-0.1, 1.0} {
		_, err := svc.Recall(ctx, service.RecallInput{
			Namespace: "n", Query: "postgres database", MinRankScore: bad,
		})
		if !errors.Is(err, service.ErrInvalidInput) {
			t.Fatalf("MinRankScore %v should be rejected as invalid input, got %v", bad, err)
		}
	}
}

// recordingReranker records the candidate IDs it was handed and returns them in
// reverse order, so the survivors' order is the reranker's, not the composite
// ranker's — proving the floor honors the reranked order and runs after it.
type recordingReranker struct {
	seen []string
}

func (r *recordingReranker) Rerank(_ context.Context, _ string, c []rerank.Candidate) ([]string, error) {
	r.seen = make([]string, len(c))
	out := make([]string, len(c))
	for i := range c {
		r.seen[i] = c[i].ID
		out[len(c)-1-i] = c[i].ID
	}
	return out, nil
}

func TestRecallMinRankScoreAppliedAfterRerank(t *testing.T) {
	ctx := context.Background()
	rr := &recordingReranker{}
	svc := seedTwoRanked(t, service.WithReranker(rr, "test"))

	// Baseline (floored off): the reranker reverses, so results come back in
	// reverse composite order with their composite scores re-attached.
	base, err := svc.Recall(ctx, service.RecallInput{Namespace: "n", Query: "postgres database", Limit: 5})
	if err != nil {
		t.Fatalf("baseline recall: %v", err)
	}
	if len(base) != 2 {
		t.Fatalf("baseline should return both, got %d", len(base))
	}
	// base[0] is the weaker hit (reranker reversed the composite order); floor
	// just above it drops it, leaving the stronger hit — a hole at the front of
	// the reranker's order.
	weak, strong := base[0], base[1]
	if weak.Score >= strong.Score {
		t.Fatalf("expected the reranker to reverse composite order, got %v then %v", weak.Score, strong.Score)
	}
	floor := (weak.Score + strong.Score) / 2

	rr.seen = nil
	got, err := svc.Recall(ctx, service.RecallInput{
		Namespace: "n", Query: "postgres database", Limit: 5, MinRankScore: floor,
	})
	if err != nil {
		t.Fatalf("floored recall: %v", err)
	}
	// The reranker saw the full pre-floor pool, including the memory the floor
	// then dropped.
	if len(rr.seen) < 2 {
		t.Fatalf("reranker saw %d candidates, want the full pre-floor pool", len(rr.seen))
	}
	sawWeak := false
	for _, id := range rr.seen {
		if id == weak.Memory.ID {
			sawWeak = true
		}
	}
	if !sawWeak {
		t.Fatal("reranker did not see the floored memory — floor ran before rerank, not after")
	}
	if len(got) != 1 || got[0].Memory.ID != strong.Memory.ID {
		t.Fatalf("survivors should keep reranker order with holes, got %+v", got)
	}
	// The survivor keeps the composite score finalizeRecall re-attached.
	if got[0].Score != strong.Score {
		t.Fatalf("survivor score = %v, want the re-attached composite %v", got[0].Score, strong.Score)
	}
}

func TestRecallMinRankScoreEmptyVerdictStaysEmpty(t *testing.T) {
	ctx := context.Background()
	rr := &emptyReranker{}
	svc := seedTwoRanked(t, service.WithReranker(rr, "test"), service.WithRerankEmptyVerdict())

	got, err := svc.Recall(ctx, service.RecallInput{
		Namespace: "n", Query: "postgres database", Limit: 5, MinRankScore: 0.5,
	})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if !rr.called {
		t.Fatal("reranker not invoked")
	}
	if len(got) != 0 {
		t.Fatalf("a gated-empty verdict must stay empty under a floor, got %v", got)
	}
}

func TestRecallMinRankScoreExemptsLinkedExpansion(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	e := embedtest.New(dims)
	now := time.Unix(1_700_000_000, 0).UTC()

	embedAndPut := func(id, ns, content string, linked ...string) {
		vec, err := embed.EmbedOne(ctx, e, content)
		if err != nil {
			t.Fatalf("embed: %v", err)
		}
		m := &memory.Memory{
			ID: id, Namespace: ns, Tier: memory.TierSemantic, Content: content,
			CreatedAt: now, UpdatedAt: now, LastAccessedAt: now, Embedding: vec,
			LinkedMemoryIDs: linked,
		}
		if err := st.Upsert(ctx, m); err != nil {
			t.Fatalf("upsert %s: %v", id, err)
		}
	}
	// b-link shares no vocabulary with the query, so it only ever reaches results
	// via the LinkedMemoryIDs expansion — the exemption under test.
	embedAndPut("b-hit", "B", "widget rollout plan", "b-link")
	embedAndPut("b-link", "B", "unrelated archived notes about kitchen inventory")
	embedAndPut("a-filler", "A", "unconnected filler text")

	svc := service.New(st, e, service.WithClock(func() time.Time { return now }), service.WithSyncReinforce())

	// Baseline: the fused gate isolates b-hit as the only direct hit; the linked
	// b-link comes in at 0.5 × the min direct score.
	base, err := svc.Recall(ctx, service.RecallInput{
		Namespace: "A", Query: "widget rollout plan", Limit: 10,
		Namespaces: []string{"A", "B"}, IncludeLinked: true, MinScore: 0.5,
	})
	if err != nil {
		t.Fatalf("baseline recall: %v", err)
	}
	var linkScore, hitScore float64
	for _, r := range base {
		switch r.Memory.ID {
		case "b-link":
			linkScore = r.Score
		case "b-hit":
			hitScore = r.Score
		}
	}
	if linkScore == 0 || hitScore == 0 {
		t.Fatalf("baseline must include both b-hit and b-link, got %+v", base)
	}
	// A floor above the linked score but below the direct hit: the floor runs
	// before expansion, so b-hit survives and its linked b-link is added exempt.
	floor := (hitScore + linkScore) / 2
	if !(linkScore < floor && floor < hitScore) {
		t.Fatalf("test floor %v is not between link %v and hit %v", floor, linkScore, hitScore)
	}

	got, err := svc.Recall(ctx, service.RecallInput{
		Namespace: "A", Query: "widget rollout plan", Limit: 10,
		Namespaces: []string{"A", "B"}, IncludeLinked: true, MinScore: 0.5,
		MinRankScore: floor,
	})
	if err != nil {
		t.Fatalf("floored recall: %v", err)
	}
	foundLink := false
	for _, r := range got {
		if r.Memory.ID == "b-link" {
			foundLink = true
			if r.Score >= floor {
				t.Fatalf("b-link should have survived as an exempt sub-floor link, score %v >= floor %v", r.Score, floor)
			}
		}
	}
	if !foundLink {
		t.Fatal("include_linked expansion must be exempt from MinRankScore, but b-link was dropped")
	}
}

func TestRecallMinRankScoreLogsFlooredButExcludesFromResponse(t *testing.T) {
	ctx := context.Background()
	strong := "the production postgres database runs on port 5432"
	weak := "the office kitchen has fresh coffee every morning"
	query := "postgres database"

	// Probe a throwaway store to learn the composite split without reinforcing
	// the store under test (a floor-free recall would reinforce both memories,
	// masking the "floored is never reinforced" assertion below). Deterministic
	// embeddings on identical content give the same composite scores here as in
	// the service under test.
	probe := service.New(openTestStore(t), embedtest.New(dims), service.WithSyncReinforce())
	for _, c := range []string{strong, weak} {
		if _, err := probe.Remember(ctx, service.RememberInput{Namespace: "n", Content: c, Tier: memory.TierSemantic}); err != nil {
			t.Fatalf("probe remember: %v", err)
		}
	}
	base, err := probe.Recall(ctx, service.RecallInput{Namespace: "n", Query: query, Limit: 5})
	if err != nil {
		t.Fatalf("probe recall: %v", err)
	}
	if len(base) != 2 || base[0].Score <= base[1].Score {
		t.Fatalf("probe should return both, best-first with distinct scores, got %+v", base)
	}
	floor := (base[0].Score + base[1].Score) / 2

	// Store under test, with the activity log on and synchronous.
	svc := service.New(openTestStore(t), embedtest.New(dims),
		service.WithSyncReinforce(), service.WithEventLog(true), service.WithSyncEventLog())
	for _, c := range []string{strong, weak} {
		if _, err := svc.Remember(ctx, service.RememberInput{Namespace: "n", Content: c, Tier: memory.TierSemantic}); err != nil {
			t.Fatalf("remember: %v", err)
		}
	}
	got, err := svc.Recall(ctx, service.RecallInput{Namespace: "n", Query: query, Limit: 5, MinRankScore: floor})
	if err != nil {
		t.Fatalf("floored recall: %v", err)
	}
	// The response omits the floored hit.
	if len(got) != 1 || got[0].Memory.Content != strong {
		t.Fatalf("response should exclude the floored hit, got %+v", got)
	}
	servedID := got[0].Memory.ID

	// The activity feed still records the floored hit, marked, with its score.
	page, err := svc.Events(ctx, service.EventsInput{Namespace: "n", Kinds: []store.EventKind{store.EventRecall}})
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	if len(page.Events) != 1 {
		t.Fatalf("want 1 recall event, got %d", len(page.Events))
	}
	ev := page.Events[0]
	if len(ev.Memories) != 2 {
		t.Fatalf("recall event should log served + floored = 2 rows, got %d", len(ev.Memories))
	}
	var flooredID string
	var served, flooredCount int
	for _, m := range ev.Memories {
		if m.Filtered == "rank_floor" {
			flooredCount++
			flooredID = m.ID
			if m.Summary != weak {
				t.Errorf("floored row summary = %q, want the weak hit %q", m.Summary, weak)
			}
			if m.Score == nil {
				t.Error("floored row must carry its true composite score, got nil")
			}
			if m.Rank == 0 {
				t.Error("floored row must carry a rank")
			}
		} else {
			served++
			if m.Filtered != "" {
				t.Errorf("served row must not be marked filtered, got %q", m.Filtered)
			}
		}
	}
	if served != 1 || flooredCount != 1 {
		t.Fatalf("want 1 served + 1 floored row, got %d served, %d floored", served, flooredCount)
	}
	// The operation-level detail (source/etc.) must not leak the per-row marker.
	if _, ok := ev.Detail["filtered"]; ok {
		t.Error("event-level detail must not carry the per-row filtered marker")
	}

	// Reinforcement rolls usage into access_count; the floored hit is never
	// reinforced, the served hit is.
	servedMem, err := svc.Get(ctx, "n", servedID)
	if err != nil {
		t.Fatalf("get served: %v", err)
	}
	flooredMem, err := svc.Get(ctx, "n", flooredID)
	if err != nil {
		t.Fatalf("get floored: %v", err)
	}
	if servedMem.AccessCount == 0 {
		t.Error("served hit should have been reinforced (access_count > 0)")
	}
	if flooredMem.AccessCount != 0 {
		t.Errorf("floored hit must not be reinforced, got access_count=%d", flooredMem.AccessCount)
	}
}

// TestRecallMinRankScoreFlooredRowKeepsTrueRank pins the reviewer's concrete
// case: the finalized (pre-floor) list is [weak, strong] — the recording
// reranker reverses composite order — so weak sits at rank 1 and strong at
// rank 2. A floor between their scores drops weak. The floored row must log at
// rank 1 (its true position, marked), and the served row at rank 2 — the served
// rank shows the hole the floor bit, instead of collapsing to a contiguous 1.
func TestRecallMinRankScoreFlooredRowKeepsTrueRank(t *testing.T) {
	ctx := context.Background()
	strong := "the production postgres database runs on port 5432"
	weak := "the office kitchen has fresh coffee every morning"
	query := "postgres database"
	seed := func(svc *service.Service) {
		for _, c := range []string{strong, weak} {
			if _, err := svc.Remember(ctx, service.RememberInput{Namespace: "n", Content: c, Tier: memory.TierSemantic}); err != nil {
				t.Fatalf("remember: %v", err)
			}
		}
	}

	// Probe a throwaway store to learn the composite split. The recording
	// reranker reverses composite order, so the finalized pre-floor list is
	// [weak (rank 1), strong (rank 2)].
	probe := service.New(openTestStore(t), embedtest.New(dims),
		service.WithSyncReinforce(), service.WithReranker(&recordingReranker{}, "test"))
	seed(probe)
	base, err := probe.Recall(ctx, service.RecallInput{Namespace: "n", Query: query, Limit: 5})
	if err != nil {
		t.Fatalf("probe recall: %v", err)
	}
	if len(base) != 2 || base[0].Memory.Content != weak || base[1].Memory.Content != strong {
		t.Fatalf("probe should finalize as [weak, strong], got %+v", base)
	}
	if base[0].Score >= base[1].Score {
		t.Fatalf("weak should score below strong, got %v then %v", base[0].Score, base[1].Score)
	}
	floor := (base[0].Score + base[1].Score) / 2

	svc := service.New(openTestStore(t), embedtest.New(dims),
		service.WithSyncReinforce(), service.WithReranker(&recordingReranker{}, "test"),
		service.WithEventLog(true), service.WithSyncEventLog())
	seed(svc)
	got, err := svc.Recall(ctx, service.RecallInput{Namespace: "n", Query: query, Limit: 5, MinRankScore: floor})
	if err != nil {
		t.Fatalf("floored recall: %v", err)
	}
	if len(got) != 1 || got[0].Memory.Content != strong {
		t.Fatalf("response should be only the strong hit, got %+v", got)
	}

	page, err := svc.Events(ctx, service.EventsInput{Namespace: "n", Kinds: []store.EventKind{store.EventRecall}})
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	if len(page.Events) != 1 {
		t.Fatalf("want 1 recall event, got %d", len(page.Events))
	}
	byRank := map[int]service.ActivityMemory{}
	for _, m := range page.Events[0].Memories {
		byRank[m.Rank] = m
	}
	r1, ok1 := byRank[1]
	r2, ok2 := byRank[2]
	if !ok1 || !ok2 {
		t.Fatalf("want logged rows at rank 1 and rank 2, got ranks %v", byRank)
	}
	// Rank 1 is the floored weak hit — its true pre-floor position, marked.
	if r1.Filtered != "rank_floor" || r1.Summary != weak {
		t.Errorf("rank 1 should be the floored weak hit, got filtered=%q summary=%q", r1.Filtered, r1.Summary)
	}
	// Rank 2 is the served strong hit — it keeps the hole the floor bit.
	if r2.Filtered != "" || r2.Summary != strong {
		t.Errorf("rank 2 should be the served strong hit, got filtered=%q summary=%q", r2.Filtered, r2.Summary)
	}
}
