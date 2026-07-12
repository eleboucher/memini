package service_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/eleboucher/memini/internal/embed/embedtest"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/service"
	"github.com/eleboucher/memini/internal/store"
	"github.com/eleboucher/memini/internal/store/sqlitevec"
)

// newEventSvc builds a service with the activity log on and synchronous, so
// assertions see the log without racing the background writer.
func newEventSvc(t *testing.T) *service.Service {
	t.Helper()
	ctx := context.Background()
	st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "events.db"), dims)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return service.New(st, embedtest.New(dims),
		service.WithSyncReinforce(),
		service.WithEventLog(true),
		service.WithSyncEventLog(),
	)
}

// TestRecallLogsQueryRankAndScore is the point of the whole activity log: a
// recall must record which query served each memory, at what rank, and with
// what score — the detail access_count rolls away.
func TestRecallLogsQueryRankAndScore(t *testing.T) {
	ctx := context.Background()
	svc := newEventSvc(t)

	for _, c := range []string{"the database is postgres", "the cache is redis"} {
		if _, err := svc.Remember(ctx, service.RememberInput{
			Namespace: "n", Content: c, Tier: memory.TierSemantic,
		}); err != nil {
			t.Fatalf("remember %q: %v", c, err)
		}
	}
	if _, err := svc.Recall(ctx, service.RecallInput{Namespace: "n", Query: "which database", Limit: 5}); err != nil {
		t.Fatalf("recall: %v", err)
	}

	page, err := svc.Events(ctx, service.EventsInput{
		Namespace: "n", Kinds: []store.EventKind{store.EventRecall},
	})
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	if len(page.Events) != 1 {
		t.Fatalf("got %d recall events, want 1", len(page.Events))
	}
	ev := page.Events[0]
	if ev.Query != "which database" {
		t.Errorf("query = %q, want %q", ev.Query, "which database")
	}
	if len(ev.Memories) == 0 {
		t.Fatal("recall event served no memories")
	}
	// Memories come back rank-ordered even though the store hands an
	// operation's rows back in reverse (they share a created_at, so the id
	// tiebreak runs backwards) — the grouped reader re-sorts them.
	for i, m := range ev.Memories {
		if m.Rank != i+1 {
			t.Errorf("memory %d has rank %d, want %d", i, m.Rank, i+1)
		}
		if m.Score == nil {
			t.Errorf("memory %d (%s) has no score", i, m.ID)
		}
		if m.Summary == "" {
			t.Errorf("memory %d (%s) has no summary snapshot", i, m.ID)
		}
	}
}

// TestRecallWithNoHitsIsLogged pins the sentinel row: "this query found
// nothing" is exactly what an activity feed exists to surface.
func TestRecallWithNoHitsIsLogged(t *testing.T) {
	ctx := context.Background()
	svc := newEventSvc(t)

	if _, err := svc.Recall(ctx, service.RecallInput{Namespace: "empty", Query: "anything at all", Limit: 5}); err != nil {
		t.Fatalf("recall: %v", err)
	}
	page, err := svc.Events(ctx, service.EventsInput{Namespace: "empty"})
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	if len(page.Events) != 1 {
		t.Fatalf("got %d events, want 1 (the zero-hit sentinel)", len(page.Events))
	}
	ev := page.Events[0]
	if ev.Kind != store.EventRecall || ev.Query != "anything at all" {
		t.Fatalf("event = %+v, want a recall of the query", ev)
	}
	if len(ev.Memories) != 0 {
		t.Fatalf("zero-hit recall listed %d memories, want none", len(ev.Memories))
	}
}

// TestBriefingAndGetLogButDoNotReinforce protects the invariant the whole
// log-vs-reinforce split rests on: reads that are not relevance signals must
// appear in the feed without touching access_count, which gates promotion and
// durable ranking.
func TestBriefingAndGetLogButDoNotReinforce(t *testing.T) {
	ctx := context.Background()
	svc := newEventSvc(t)

	m, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "n", Content: "the database is postgres", Tier: memory.TierSemantic,
	})
	if err != nil {
		t.Fatalf("remember: %v", err)
	}
	before, err := svc.Get(ctx, "n", m.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	baseline := before.AccessCount

	if _, err := svc.Briefing(ctx, "n", service.BriefingOpts{}); err != nil {
		t.Fatalf("briefing: %v", err)
	}
	if _, err := svc.Get(ctx, "n", m.ID); err != nil {
		t.Fatalf("get after briefing: %v", err)
	}

	after, err := svc.Get(ctx, "n", m.ID)
	if err != nil {
		t.Fatalf("final get: %v", err)
	}
	if after.AccessCount != baseline {
		t.Errorf("access_count = %d after briefing+gets, want %d unchanged: "+
			"briefing/get must not reinforce, or every session start would inflate the counter",
			after.AccessCount, baseline)
	}

	// But both are in the feed.
	page, err := svc.Events(ctx, service.EventsInput{Namespace: "n"})
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	kinds := map[store.EventKind]int{}
	for _, ev := range page.Events {
		kinds[ev.Kind]++
	}
	if kinds[store.EventBriefing] != 1 {
		t.Errorf("briefing events = %d, want 1", kinds[store.EventBriefing])
	}
	if kinds[store.EventGet] == 0 {
		t.Error("no get events logged")
	}

	// A recall, by contrast, does reinforce.
	if _, err := svc.Recall(ctx, service.RecallInput{Namespace: "n", Query: "database", Limit: 5}); err != nil {
		t.Fatalf("recall: %v", err)
	}
	recalled, err := svc.Get(ctx, "n", m.ID)
	if err != nil {
		t.Fatalf("get after recall: %v", err)
	}
	if recalled.AccessCount <= baseline {
		t.Errorf("access_count = %d after recall, want > %d: recall must still reinforce",
			recalled.AccessCount, baseline)
	}
}

// TestBriefingEventCarriesSections checks the "why" for a briefing row: which
// section of the briefing the memory was served under.
func TestBriefingEventCarriesSections(t *testing.T) {
	ctx := context.Background()
	svc := newEventSvc(t)

	if _, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "n", Content: "deploys go through CI", Tier: memory.TierSemantic,
	}); err != nil {
		t.Fatalf("remember fact: %v", err)
	}
	if _, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "n", Content: "run mise test to verify", Tier: memory.TierProcedural,
	}); err != nil {
		t.Fatalf("remember procedure: %v", err)
	}
	if _, err := svc.Briefing(ctx, "n", service.BriefingOpts{}); err != nil {
		t.Fatalf("briefing: %v", err)
	}

	page, err := svc.Events(ctx, service.EventsInput{
		Namespace: "n", Kinds: []store.EventKind{store.EventBriefing},
	})
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	if len(page.Events) != 1 {
		t.Fatalf("got %d briefing events, want 1", len(page.Events))
	}
	sections := map[string]bool{}
	for _, m := range page.Events[0].Memories {
		sections[m.Section] = true
	}
	for _, want := range []string{"facts", "procedures"} {
		if !sections[want] {
			t.Errorf("no briefing memory tagged section %q (got %v)", want, sections)
		}
	}
}

// TestWriteEventsDistinguishRememberFromUpdate covers the one Upsert that backs
// two verbs, plus forget and supersede snapshotting.
func TestWriteEventsDistinguishRememberFromUpdate(t *testing.T) {
	ctx := context.Background()
	svc := newEventSvc(t)

	m, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "n", Content: "original content", Tier: memory.TierSemantic,
	})
	if err != nil {
		t.Fatalf("remember: %v", err)
	}
	// Re-writing the same ID is an update, not a new memory.
	if _, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "n", ID: m.ID, Content: "revised content", Tier: memory.TierSemantic,
	}); err != nil {
		t.Fatalf("update: %v", err)
	}

	page, err := svc.Events(ctx, service.EventsInput{
		Namespace: "n", Kinds: []store.EventKind{store.EventRemember, store.EventUpdate},
	})
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	kinds := map[store.EventKind]int{}
	for _, ev := range page.Events {
		kinds[ev.Kind]++
	}
	if kinds[store.EventRemember] != 1 || kinds[store.EventUpdate] != 1 {
		t.Fatalf("kinds = %v, want exactly one remember and one update", kinds)
	}
}

// TestForgetEventKeepsSnapshot is why the log denormalizes the memory: after
// the row is deleted, the snapshot is the only thing left that can describe
// what was forgotten.
func TestForgetEventKeepsSnapshot(t *testing.T) {
	ctx := context.Background()
	svc := newEventSvc(t)

	m, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "n", Content: "a fact that will be deleted", Tier: memory.TierSemantic,
	})
	if err != nil {
		t.Fatalf("remember: %v", err)
	}
	if err := svc.Forget(ctx, "n", m.ID); err != nil {
		t.Fatalf("forget: %v", err)
	}

	page, err := svc.Events(ctx, service.EventsInput{
		Namespace: "n", Kinds: []store.EventKind{store.EventForget},
	})
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	if len(page.Events) != 1 || len(page.Events[0].Memories) != 1 {
		t.Fatalf("forget event = %+v, want one event describing one memory", page.Events)
	}
	got := page.Events[0].Memories[0]
	if got.ID != m.ID {
		t.Errorf("forget event memory id = %q, want %q", got.ID, m.ID)
	}
	if got.Summary != "a fact that will be deleted" {
		t.Errorf("forget event lost its snapshot: summary = %q", got.Summary)
	}
	// And the memory really is gone.
	if _, err := svc.Get(ctx, "n", m.ID); err == nil {
		t.Error("memory still readable after forget")
	}
}

// TestEventsPaginationAndKindFilter covers the reader: newest-first ordering,
// the operation-counted limit, and a cursor that neither repeats nor skips.
func TestEventsPaginationAndKindFilter(t *testing.T) {
	ctx := context.Background()
	svc := newEventSvc(t)

	const n = 5
	for i := range n {
		if _, err := svc.Remember(ctx, service.RememberInput{
			Namespace: "n", Content: "fact number " + string(rune('a'+i)), Tier: memory.TierSemantic,
		}); err != nil {
			t.Fatalf("remember %d: %v", i, err)
		}
	}

	page1, err := svc.Events(ctx, service.EventsInput{
		Namespace: "n", Kinds: []store.EventKind{store.EventRemember}, Limit: 2,
	})
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	if len(page1.Events) != 2 || !page1.HasMore {
		t.Fatalf("page 1: %d events, hasMore=%v; want 2 and true", len(page1.Events), page1.HasMore)
	}
	page2, err := svc.Events(ctx, service.EventsInput{
		Namespace: "n", Kinds: []store.EventKind{store.EventRemember}, Limit: 2,
		Before: page1.NextBefore, BeforeID: page1.NextBeforeID,
	})
	if err != nil {
		t.Fatalf("page 2: %v", err)
	}
	if len(page2.Events) != 2 {
		t.Fatalf("page 2 returned %d events, want 2", len(page2.Events))
	}
	seen := map[string]bool{}
	for _, ev := range append(append([]service.ActivityEvent{}, page1.Events...), page2.Events...) {
		if seen[ev.OpID] {
			t.Fatalf("op %s returned on both pages: the cursor repeats rows", ev.OpID)
		}
		seen[ev.OpID] = true
	}
}

// TestEventLogOffRecordsNothing checks the config toggle actually gates writes.
func TestEventLogOffRecordsNothing(t *testing.T) {
	ctx := context.Background()
	st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "off.db"), dims)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	svc := service.New(st, embedtest.New(dims), service.WithSyncReinforce(), service.WithEventLog(false))

	if _, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "n", Content: "unlogged", Tier: memory.TierSemantic,
	}); err != nil {
		t.Fatalf("remember: %v", err)
	}
	if _, err := svc.Recall(ctx, service.RecallInput{Namespace: "n", Query: "unlogged", Limit: 5}); err != nil {
		t.Fatalf("recall: %v", err)
	}
	page, err := svc.Events(ctx, service.EventsInput{Namespace: "n"})
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	if len(page.Events) != 0 {
		t.Fatalf("event log disabled but %d events recorded", len(page.Events))
	}
}

// TestPruneEvents covers the retention path the sweeper drives.
func TestPruneEvents(t *testing.T) {
	ctx := context.Background()
	svc := newEventSvc(t)

	for i := range 3 {
		if _, err := svc.Remember(ctx, service.RememberInput{
			Namespace: "n", Content: "fact " + string(rune('a'+i)), Tier: memory.TierSemantic,
		}); err != nil {
			t.Fatalf("remember %d: %v", i, err)
		}
	}
	// Cap the log at one row; the newest survives.
	if _, err := svc.PruneEvents(ctx, time.Time{}, 1); err != nil {
		t.Fatalf("prune: %v", err)
	}
	page, err := svc.Events(ctx, service.EventsInput{Namespace: "n"})
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	if len(page.Events) != 1 {
		t.Fatalf("after cap prune, %d events remain, want 1", len(page.Events))
	}
	// Both bounds unset means keep forever — a prune must not empty the log.
	if _, err := svc.PruneEvents(ctx, time.Time{}, 0); err != nil {
		t.Fatalf("no-op prune: %v", err)
	}
	page, err = svc.Events(ctx, service.EventsInput{Namespace: "n"})
	if err != nil {
		t.Fatalf("events after no-op prune: %v", err)
	}
	if len(page.Events) != 1 {
		t.Fatalf("no-op prune deleted rows: %d events remain, want 1", len(page.Events))
	}
}
