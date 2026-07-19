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

// TestRecallSourceLandsInDetail covers the recall "why": the source the caller
// declared is recorded verbatim in the event detail, including the zero-hit
// sentinel — "who searched for what, and found nothing" is answerable.
func TestRecallSourceLandsInDetail(t *testing.T) {
	ctx := context.Background()
	svc := newEventSvc(t)

	if _, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "n", Content: "the cache is redis", Tier: memory.TierSemantic,
	}); err != nil {
		t.Fatalf("remember: %v", err)
	}
	if _, err := svc.Recall(ctx, service.RecallInput{
		Namespace: "n", Query: "cache", Limit: 5, Source: "pretool",
	}); err != nil {
		t.Fatalf("recall: %v", err)
	}
	// A zero-hit recall still carries the source on its sentinel row.
	if _, err := svc.Recall(ctx, service.RecallInput{
		Namespace: "empty", Query: "nothing here", Limit: 5, Source: "ui",
	}); err != nil {
		t.Fatalf("zero-hit recall: %v", err)
	}

	hit, err := svc.Events(ctx, service.EventsInput{
		Namespace: "n", Kinds: []store.EventKind{store.EventRecall},
	})
	if err != nil {
		t.Fatalf("events n: %v", err)
	}
	if len(hit.Events) != 1 || hit.Events[0].Detail["source"] != "pretool" {
		t.Fatalf("recall detail = %+v, want source=pretool", hit.Events)
	}
	sentinel, err := svc.Events(ctx, service.EventsInput{Namespace: "empty"})
	if err != nil {
		t.Fatalf("events empty: %v", err)
	}
	if len(sentinel.Events) != 1 || sentinel.Events[0].Detail["source"] != "ui" {
		t.Fatalf("sentinel detail = %+v, want source=ui", sentinel.Events)
	}
}

// TestRecallExcludedCountLandsInDetail covers the observability signal for the
// inject-cooldown work: when a recall carries exclude_ids (the in-cooldown ids a
// surface asked the server to drop), the count lands in the event detail on
// every row — including the zero-hit sentinel — so the feed can show how many
// candidates a dedupe pass suppressed. A recall with no exclude_ids omits the
// key entirely.
func TestRecallExcludedCountLandsInDetail(t *testing.T) {
	ctx := context.Background()
	svc := newEventSvc(t)

	for _, c := range []string{"the database is postgres", "the cache is redis"} {
		if _, err := svc.Remember(ctx, service.RememberInput{
			Namespace: "n", Content: c, Tier: memory.TierSemantic,
		}); err != nil {
			t.Fatalf("remember %q: %v", c, err)
		}
	}
	// A multi-hit recall that excludes ids: the count is len(ExcludeIDs) — it is
	// recorded whether or not those ids happened to match, and it must ride every
	// served row (checked via the grouped detail, shared across the op's rows).
	if _, err := svc.Recall(ctx, service.RecallInput{
		Namespace: "n", Query: "database", Limit: 5,
		ExcludeIDs: []string{"ghost-a", "ghost-b", "ghost-c"},
	}); err != nil {
		t.Fatalf("recall with excludes: %v", err)
	}
	// A zero-hit recall that excludes ids: the sentinel row still carries the count.
	if _, err := svc.Recall(ctx, service.RecallInput{
		Namespace: "empty", Query: "nothing here", Limit: 5,
		ExcludeIDs: []string{"ghost-x"},
	}); err != nil {
		t.Fatalf("zero-hit recall with excludes: %v", err)
	}
	// A recall with no excludes: the key is absent, not zero.
	if _, err := svc.Recall(ctx, service.RecallInput{
		Namespace: "n", Query: "cache", Limit: 5,
	}); err != nil {
		t.Fatalf("recall without excludes: %v", err)
	}

	hits, err := svc.Events(ctx, service.EventsInput{
		Namespace: "n", Kinds: []store.EventKind{store.EventRecall},
	})
	if err != nil {
		t.Fatalf("events n: %v", err)
	}
	var withExcl, withoutExcl *service.ActivityEvent
	for i := range hits.Events {
		ev := &hits.Events[i]
		switch ev.Query {
		case "database":
			withExcl = ev
		case "cache":
			withoutExcl = ev
		}
	}
	if withExcl == nil || withoutExcl == nil {
		t.Fatalf("expected both a 'database' and a 'cache' recall event, got %+v", hits.Events)
	}
	if len(withExcl.Memories) == 0 {
		t.Fatal("the excluding recall served no memories — cannot prove the count rides a served row")
	}
	if got := withExcl.Detail["excluded_count"]; !eqInt(got, 3) {
		t.Errorf("excluding recall detail excluded_count = %v (%T), want 3", got, got)
	}
	if got, ok := withoutExcl.Detail["excluded_count"]; ok {
		t.Errorf("non-excluding recall carries excluded_count = %v, want the key absent", got)
	}

	sentinel, err := svc.Events(ctx, service.EventsInput{Namespace: "empty"})
	if err != nil {
		t.Fatalf("events empty: %v", err)
	}
	if len(sentinel.Events) != 1 {
		t.Fatalf("got %d events for the zero-hit recall, want 1 sentinel", len(sentinel.Events))
	}
	if len(sentinel.Events[0].Memories) != 0 {
		t.Fatalf("zero-hit recall listed memories, want none: %+v", sentinel.Events[0])
	}
	if got := sentinel.Events[0].Detail["excluded_count"]; !eqInt(got, 1) {
		t.Errorf("zero-hit sentinel detail excluded_count = %v (%T), want 1", got, got)
	}
}

// eqInt compares a detail value to an int tolerantly: an int written into a
// free-form map[string]any may round-trip through JSON as a float64, so accept
// either numeric shape.
func eqInt(got any, want int) bool {
	switch v := got.(type) {
	case int:
		return v == want
	case int64:
		return v == int64(want)
	case float64:
		return v == float64(want)
	default:
		return false
	}
}

// TestActorThreadsThroughEveryKind is the point of attribution: the actor
// stamped on the request context (service.WithActor) must land on every event
// kind — recall, remember, forget, briefing, and a config event — since they
// all funnel through logEvents. A request with no actor stamps the legacy
// empty kind, which renders as "unknown".
func TestActorThreadsThroughEveryKind(t *testing.T) {
	ctx := context.Background()
	svc := newEventSvc(t)
	named := service.WithActor(ctx, "alice", "key")

	m, err := svc.Remember(named, service.RememberInput{
		Namespace: "n", Content: "the database is postgres", Tier: memory.TierSemantic,
	})
	if err != nil {
		t.Fatalf("remember: %v", err)
	}
	if _, err := svc.Recall(named, service.RecallInput{Namespace: "n", Query: "database", Limit: 5}); err != nil {
		t.Fatalf("recall: %v", err)
	}
	if _, err := svc.Briefing(named, "n", service.BriefingOpts{}); err != nil {
		t.Fatalf("briefing: %v", err)
	}
	svc.LogConfigEvent(named, store.EventPin, "n", map[string]any{"keys": []string{"k"}})
	if err := svc.Forget(named, "n", m.ID); err != nil {
		t.Fatalf("forget: %v", err)
	}

	page, err := svc.Events(ctx, service.EventsInput{Namespace: "n"})
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	seen := map[store.EventKind]bool{}
	for _, ev := range page.Events {
		seen[ev.Kind] = true
		if ev.Actor != "alice" || ev.ActorKind != "key" {
			t.Errorf("%s event actor = (%q, %q), want (alice, key)", ev.Kind, ev.Actor, ev.ActorKind)
		}
	}
	for _, want := range []store.EventKind{
		store.EventRemember, store.EventRecall, store.EventBriefing, store.EventPin, store.EventForget,
	} {
		if !seen[want] {
			t.Errorf("no %s event logged", want)
		}
	}

	// A request with no actor stamped (background work, dev mode) logs the
	// empty legacy attribution rather than inheriting a previous request's.
	if _, err := svc.Recall(ctx, service.RecallInput{Namespace: "other", Query: "q", Limit: 5}); err != nil {
		t.Fatalf("unattributed recall: %v", err)
	}
	anon, err := svc.Events(ctx, service.EventsInput{Namespace: "other"})
	if err != nil {
		t.Fatalf("events other: %v", err)
	}
	if len(anon.Events) != 1 || anon.Events[0].Actor != "" || anon.Events[0].ActorKind != "" {
		t.Fatalf("unattributed event = %+v, want empty actor", anon.Events)
	}
}

// TestActorEnvKindHasNoName pins the env/none cases: the admin env key and
// dev mode carry a kind but no name, and that distinction round-trips.
func TestActorEnvKindHasNoName(t *testing.T) {
	ctx := context.Background()
	svc := newEventSvc(t)

	if _, err := svc.Recall(service.WithActor(ctx, "", "env"), service.RecallInput{
		Namespace: "env-ns", Query: "q", Limit: 5,
	}); err != nil {
		t.Fatalf("env recall: %v", err)
	}
	page, err := svc.Events(ctx, service.EventsInput{Namespace: "env-ns"})
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	if len(page.Events) != 1 || page.Events[0].Actor != "" || page.Events[0].ActorKind != "env" {
		t.Fatalf("env event = %+v, want actor='' kind='env'", page.Events)
	}
}

// TestWriteEventRecordsOutcomeDetail covers the write-outcome enrichment: a
// plain write records its tier; an auto-superseding write also records the id
// it replaced.
func TestWriteEventRecordsOutcomeDetail(t *testing.T) {
	ctx := context.Background()
	svc := newEventSvc(t)

	if _, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "n", Content: "a plain durable fact", Tier: memory.TierSemantic,
	}); err != nil {
		t.Fatalf("remember: %v", err)
	}
	page, err := svc.Events(ctx, service.EventsInput{
		Namespace: "n", Kinds: []store.EventKind{store.EventRemember},
	})
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	if len(page.Events) != 1 {
		t.Fatalf("got %d remember events, want 1", len(page.Events))
	}
	if got := page.Events[0].Detail["tier"]; got != string(memory.TierSemantic) {
		t.Fatalf("write detail tier = %v, want %q", got, memory.TierSemantic)
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

// TestRecordInjectedWritesEvent covers the injection-telemetry beacon's write
// path: one report becomes one inject event carrying the session, surface,
// size estimates and suppression counts in its detail, with the injected ids
// as the event's memory refs — unknown ids included, best-effort by design.
func TestRecordInjectedWritesEvent(t *testing.T) {
	ctx := context.Background()
	svc := newEventSvc(t)

	m, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "n", Content: "the database is postgres", Tier: memory.TierSemantic,
	})
	if err != nil {
		t.Fatalf("remember: %v", err)
	}
	tokens, chars := 412, 1650
	svc.RecordInjected(ctx, "n", service.InjectedReport{
		SessionID:   "s1",
		Surface:     "prompt",
		Source:      "claude-code",
		InjectedIDs: []string{m.ID, "ghost-id"},
		TokensEst:   &tokens,
		Chars:       &chars,
		Suppressed:  service.InjectedSuppressed{Seen: 2, Cooldown: 1, Score: 3},
	})

	page, err := svc.Events(ctx, service.EventsInput{
		Namespace: "n", Kinds: []store.EventKind{store.EventInject},
	})
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	if len(page.Events) != 1 {
		t.Fatalf("got %d inject events, want 1", len(page.Events))
	}
	ev := page.Events[0]
	if ev.Kind != store.EventInject {
		t.Fatalf("kind = %q, want inject", ev.Kind)
	}
	if ev.Detail["surface"] != "prompt" || ev.Detail["session_id"] != "s1" || ev.Detail["source"] != "claude-code" {
		t.Errorf("detail = %+v, want surface=prompt session_id=s1 source=claude-code", ev.Detail)
	}
	if got := ev.Detail["injected_tokens_est"]; !eqInt(got, 412) {
		t.Errorf("injected_tokens_est = %v (%T), want 412", got, got)
	}
	if got := ev.Detail["injected_chars"]; !eqInt(got, 1650) {
		t.Errorf("injected_chars = %v (%T), want 1650", got, got)
	}
	sup, ok := ev.Detail["suppressed"].(map[string]any)
	if !ok {
		t.Fatalf("detail suppressed = %v (%T), want a map", ev.Detail["suppressed"], ev.Detail["suppressed"])
	}
	if !eqInt(sup["seen"], 2) || !eqInt(sup["cooldown"], 1) || !eqInt(sup["score"], 3) {
		t.Errorf("suppressed = %+v, want seen=2 cooldown=1 score=3", sup)
	}
	if _, ok := sup["budget"]; ok {
		t.Errorf("suppressed carries budget = %v, want zero reasons omitted", sup["budget"])
	}
	if len(ev.Memories) != 2 {
		t.Fatalf("inject event carries %d memory refs, want 2 (unknown ids are kept)", len(ev.Memories))
	}
	if ev.Memories[0].ID != m.ID || ev.Memories[1].ID != "ghost-id" {
		t.Errorf("memory refs = [%q, %q], want [%q, ghost-id]", ev.Memories[0].ID, ev.Memories[1].ID, m.ID)
	}

	// A suppression-only report (nothing injected) still records: one sentinel
	// event with no memory refs, mirroring the zero-hit recall.
	svc.RecordInjected(ctx, "quiet", service.InjectedReport{
		SessionID: "s2", Surface: "pretool",
		Suppressed: service.InjectedSuppressed{Budget: 4},
	})
	only, err := svc.Events(ctx, service.EventsInput{Namespace: "quiet"})
	if err != nil {
		t.Fatalf("events quiet: %v", err)
	}
	if len(only.Events) != 1 || len(only.Events[0].Memories) != 0 {
		t.Fatalf("suppression-only report = %+v, want one memory-less event", only.Events)
	}
	if sup, _ := only.Events[0].Detail["suppressed"].(map[string]any); !eqInt(sup["budget"], 4) {
		t.Errorf("suppression-only detail = %+v, want suppressed.budget=4", only.Events[0].Detail)
	}
}

// TestEventsAnnotatesInjectedMemories is the served→injected join: a recall
// event's memory is marked injected=true when a later inject report names it,
// injected=false when a report exists but omits it (the client suppressed it),
// and stays unannotated — absent, not false — when no report covered the
// serve, so old data and non-reporting integrations render unchanged.
func TestEventsAnnotatesInjectedMemories(t *testing.T) {
	ctx := context.Background()
	svc := newEventSvc(t)

	seed := func(ns, content string) string {
		t.Helper()
		m, err := svc.Remember(ctx, service.RememberInput{
			Namespace: ns, Content: content, Tier: memory.TierSemantic,
		})
		if err != nil {
			t.Fatalf("remember %q: %v", content, err)
		}
		return m.ID
	}
	injectedID := seed("n", "the primary database is postgres")
	suppressedID := seed("n", "the backup database is mysql")

	res, err := svc.Recall(ctx, service.RecallInput{Namespace: "n", Query: "database", Limit: 5})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(res) != 2 {
		t.Fatalf("recall served %d memories, want both seeds", len(res))
	}
	// The client injected one and suppressed the other.
	svc.RecordInjected(ctx, "n", service.InjectedReport{
		SessionID: "s1", Surface: "prompt", InjectedIDs: []string{injectedID},
		Suppressed: service.InjectedSuppressed{Seen: 1},
	})

	page, err := svc.Events(ctx, service.EventsInput{
		Namespace: "n", Kinds: []store.EventKind{store.EventRecall, store.EventInject},
	})
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	var recall *service.ActivityEvent
	for i := range page.Events {
		if page.Events[i].Kind == store.EventRecall {
			recall = &page.Events[i]
		}
	}
	if recall == nil {
		t.Fatalf("no recall event in %+v", page.Events)
	}
	flags := map[string]*bool{}
	for _, m := range recall.Memories {
		flags[m.ID] = m.Injected
	}
	if got := flags[injectedID]; got == nil || !*got {
		t.Errorf("injected memory flag = %v, want true", got)
	}
	if got := flags[suppressedID]; got == nil || *got {
		t.Errorf("suppressed memory flag = %v, want false (reported-suppressed, not unknown)", got)
	}

	// A recall no report ever covered stays unannotated: absent means unknown.
	seed("bare", "the cache is redis")
	if _, err := svc.Recall(ctx, service.RecallInput{Namespace: "bare", Query: "cache", Limit: 5}); err != nil {
		t.Fatalf("bare recall: %v", err)
	}
	bare, err := svc.Events(ctx, service.EventsInput{
		Namespace: "bare", Kinds: []store.EventKind{store.EventRecall},
	})
	if err != nil {
		t.Fatalf("events bare: %v", err)
	}
	if len(bare.Events) != 1 || len(bare.Events[0].Memories) == 0 {
		t.Fatalf("bare recall = %+v, want one event with memories", bare.Events)
	}
	for _, m := range bare.Events[0].Memories {
		if m.Injected != nil {
			t.Errorf("unreported memory %s flag = %v, want absent (nil)", m.ID, *m.Injected)
		}
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
