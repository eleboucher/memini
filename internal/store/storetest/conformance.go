package storetest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/store"
)

// Run executes the full conformance suite against st. dims is the store's
// embedding dimensionality (>= 4). Subtests use distinct namespaces so they can
// share a single backing store without interfering.
func Run(t *testing.T, st store.Store, dims int) {
	t.Helper()
	if dims < 4 {
		t.Fatalf("storetest needs dims >= 4, got %d", dims)
	}
	t.Run("UpsertGetDelete", func(t *testing.T) { testUpsertGetDelete(t, st, dims) })
	t.Run("UpdateInPlace", func(t *testing.T) { testUpdateInPlace(t, st, dims) })
	t.Run("CrossNamespaceUpsert", func(t *testing.T) { testCrossNamespaceUpsert(t, st, dims) })
	t.Run("VectorRanking", func(t *testing.T) { testVectorRanking(t, st, dims) })
	t.Run("KeywordSearch", func(t *testing.T) { testKeyword(t, st, dims) })
	t.Run("Filters", func(t *testing.T) { testFilters(t, st, dims) })
	t.Run("TagMetadataFilter", func(t *testing.T) { testTagMetadataFilter(t, st, dims) })
	t.Run("ExcludeMetadataFilter", func(t *testing.T) { testExcludeMetadataFilter(t, st, dims) })
	t.Run("ExcludeIDsFilter", func(t *testing.T) { testExcludeIDsFilter(t, st, dims) })
	t.Run("SetSuperseded", func(t *testing.T) { testSetSuperseded(t, st, dims) })
	t.Run("PredecessorIDs", func(t *testing.T) { testPredecessorIDs(t, st, dims) })
	t.Run("Restore", func(t *testing.T) { testRestore(t, st, dims) })
	t.Run("Reinforce", func(t *testing.T) { testReinforce(t, st, dims) })
	t.Run("DeleteIfExpiredBefore", func(t *testing.T) { testDeleteIfExpiredBefore(t, st, dims) })
	t.Run("KeywordSearchHostileQueries", func(t *testing.T) { testKeywordHostileQueries(t, st, dims) })
	t.Run("FilterNow", func(t *testing.T) { testFilterNow(t, st, dims) })
	t.Run("ConcurrentAccess", func(t *testing.T) { testConcurrentAccess(t, st, dims) })
	t.Run("Reassign", func(t *testing.T) { testReassign(t, st, dims) })
	t.Run("Retier", func(t *testing.T) { testRetier(t, st, dims) })
	t.Run("DeleteNamespace", func(t *testing.T) { testDeleteNamespace(t, st, dims) })
	t.Run("ListNamespaces", func(t *testing.T) { testListNamespaces(t, st, dims) })
	t.Run("TemporalAsOf", func(t *testing.T) { testTemporalAsOf(t, st, dims) })
	t.Run("SetConfidence", func(t *testing.T) { testSetConfidence(t, st, dims) })
	t.Run("MarkContradicted", func(t *testing.T) { testMarkContradicted(t, st, dims) })
	t.Run("GetByFingerprint", func(t *testing.T) { testGetByFingerprint(t, st, dims) })
	t.Run("LevelFilter", func(t *testing.T) { testLevelFilter(t, st, dims) })
	t.Run("VectorlessRow", func(t *testing.T) { testVectorlessRow(t, st, dims) })
	t.Run("GetEmbedding", func(t *testing.T) { testGetEmbedding(t, st, dims) })
	t.Run("NamespaceLinks", func(t *testing.T) { testNamespaceLinks(t, st, dims) })
	t.Run("NamespaceActivity", func(t *testing.T) { testNamespaceActivity(t, st, dims) })
	t.Run("APIKeys", func(t *testing.T) { testAPIKeys(t, st, dims) })
	t.Run("Pins", func(t *testing.T) { testPin(t, st, dims) })
	t.Run("ClientSettings", func(t *testing.T) { testClientSettings(t, st, dims) })
	t.Run("ListSort", func(t *testing.T) { testListSort(t, st, dims) })
	t.Run("MemoryTypeFilter", func(t *testing.T) { testMemoryTypeFilter(t, st, dims) })
	t.Run("ListRecencyWindow", func(t *testing.T) { testListRecencyWindow(t, st, dims) })
	t.Run("EventLog", func(t *testing.T) { testEventLog(t, st, dims) })
	t.Run("Chunks", func(t *testing.T) { testChunks(t, st, dims) })
	t.Run("Repair", func(t *testing.T) { testRepair(t, st, dims) })
	t.Run("IDsByPrefix", func(t *testing.T) { testIDsByPrefix(t, st, dims) })
}

// testIDsByPrefix pins the indexed id-prefix scan backing short-id
// resolution: ascending order, the limit bound, literal (never wildcard)
// matching of LIKE metacharacters, and namespace scoping.
func testIDsByPrefix(t *testing.T, st store.Store, dims int) {
	ctx := context.Background()
	ns := t.Name()
	other := ns + "-other"
	// IDs are stored verbatim (no id() scoping): the scan matches on the
	// leading bytes, so the fixture controls them exactly. UUID-shaped so the
	// fixture mirrors production ids.
	const (
		twinA  = "aabbccdd-1111-4000-8000-000000000001"
		twinB  = "aabbccdd-2222-4000-8000-000000000002"
		unique = "aabbccff-3333-4000-8000-000000000003"
	)
	for _, memID := range []string{twinA, twinB, unique} {
		m := mem(ns, "x", "prefix fixture "+memID, vec(dims, 1))
		m.ID = memID
		mustUpsert(t, st, m)
	}
	foreign := mem(other, "x", "same prefix, different namespace", vec(dims, 1))
	foreign.ID = "aabbccdd-9999-4000-8000-000000000009"
	mustUpsert(t, st, foreign)

	// A prefix unique in the namespace resolves to exactly its row.
	got, err := st.IDsByPrefix(ctx, ns, "aabbccff", 2)
	if err != nil {
		t.Fatalf("unique prefix: %v", err)
	}
	if want := []string{unique}; !slices.Equal(got, want) {
		t.Fatalf("unique prefix = %v, want %v", got, want)
	}

	// An ambiguous prefix returns every collision up to the bound, ascending —
	// the foreign namespace's aabbccdd row must NOT leak in.
	got, err = st.IDsByPrefix(ctx, ns, "aabbccdd", 5)
	if err != nil {
		t.Fatalf("ambiguous prefix: %v", err)
	}
	if want := []string{twinA, twinB}; !slices.Equal(got, want) {
		t.Fatalf("ambiguous prefix = %v, want %v", got, want)
	}

	// The limit bounds the scan (the resolver asks for 2: enough to tell
	// "unique" from "ambiguous" without walking every collision).
	if got, err = st.IDsByPrefix(ctx, ns, "aabbcc", 1); err != nil || len(got) != 1 {
		t.Fatalf("limit 1 = (%v, %v), want exactly 1 row", got, err)
	}

	// No match is an empty result, not an error.
	if got, err = st.IDsByPrefix(ctx, ns, "ffffffff", 2); err != nil || len(got) != 0 {
		t.Fatalf("no match = (%v, %v), want empty", got, err)
	}

	// A full id is its own prefix.
	if got, err = st.IDsByPrefix(ctx, ns, twinA, 2); err != nil || !slices.Equal(got, []string{twinA}) {
		t.Fatalf("full id = (%v, %v), want itself", got, err)
	}

	// LIKE metacharacters match literally: "aabbcc%" is not a wildcard and
	// matches nothing here; same for "_" and a lone "%".
	for _, hostile := range []string{"aabbcc%", "aabbcc_d", "%", "_"} {
		if got, err = st.IDsByPrefix(ctx, ns, hostile, 5); err != nil || len(got) != 0 {
			t.Fatalf("hostile prefix %q = (%v, %v), want empty", hostile, got, err)
		}
	}

	// Empty prefix and non-positive limit return nothing — the scan resolves
	// handles, it does not enumerate.
	if got, err = st.IDsByPrefix(ctx, ns, "", 2); err != nil || len(got) != 0 {
		t.Fatalf("empty prefix = (%v, %v), want empty", got, err)
	}
	if got, err = st.IDsByPrefix(ctx, ns, "aabbcc", 0); err != nil || len(got) != 0 {
		t.Fatalf("limit 0 = (%v, %v), want empty", got, err)
	}
}

// Fixture labels shared by the sort/filter subtests below. They are constants
// because the same three memories are asserted on by every ordering case.
const (
	memOld = "old"
	memMid = "mid"
	memNew = "new"

	typeDecision   = "decision"
	typePreference = "preference"

	opRecall   = "op-recall"
	eventQuery = "why sqlite"
)

// testListSort pins List's ordering contract: results come back ordered by
// Filter.Sort, the zero value means created_at descending, and a limit takes
// the top N of that order rather than an arbitrary N. Both drivers must agree
// byte for byte, since the all-namespaces aggregate merges their outputs with
// an equivalent Go comparator.
func testListSort(t *testing.T, st store.Store, dims int) {
	ctx := context.Background()
	ns := t.Name()
	base := time.Now().UTC().Truncate(time.Millisecond).Add(-72 * time.Hour)

	// Three memories whose orderings differ per key, so a wrong ORDER BY column
	// cannot accidentally produce the expected sequence.
	for i, spec := range []struct {
		short       string
		createdOff  time.Duration // from base
		updatedOff  time.Duration
		accessedOff time.Duration
		accessCount int
		importance  float64
	}{
		{memOld, 0, 2 * time.Hour, 1 * time.Hour, 7, 0.1},
		{memMid, 1 * time.Hour, 0, 2 * time.Hour, 1, 0.9},
		{memNew, 2 * time.Hour, 1 * time.Hour, 0, 4, 0.5},
	} {
		m := mem(ns, spec.short, "sortable "+spec.short, vec(dims, float32(i+1)))
		m.CreatedAt = base.Add(spec.createdOff)
		m.UpdatedAt = base.Add(spec.updatedOff)
		m.LastAccessedAt = base.Add(spec.accessedOff)
		m.AccessCount = spec.accessCount
		m.Importance = spec.importance
		mustUpsert(t, st, m)
	}

	shorts := func(ms []*memory.Memory) []string {
		out := make([]string, len(ms))
		for i, m := range ms {
			out[i] = strings.TrimPrefix(m.ID, ns+"/")
		}
		return out
	}

	for _, tc := range []struct {
		name string
		sort store.Sort
		want []string
	}{
		{"default is created desc", store.Sort{}, []string{memNew, memMid, memOld}},
		{"created asc", store.Sort{Key: store.SortCreatedAt, Asc: true}, []string{memOld, memMid, memNew}},
		{"updated desc", store.Sort{Key: store.SortUpdatedAt}, []string{memOld, memNew, memMid}},
		{"accessed desc", store.Sort{Key: store.SortLastAccessedAt}, []string{memMid, memOld, memNew}},
		{"access count desc", store.Sort{Key: store.SortAccessCount}, []string{memOld, memNew, memMid}},
		{"access count asc", store.Sort{Key: store.SortAccessCount, Asc: true}, []string{memMid, memNew, memOld}},
		{"importance desc", store.Sort{Key: store.SortImportance}, []string{memMid, memNew, memOld}},
		{"importance asc", store.Sort{Key: store.SortImportance, Asc: true}, []string{memOld, memNew, memMid}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := shorts(mustList(t, st, ns, store.Filter{Sort: tc.sort}))
			if !slices.Equal(got, tc.want) {
				t.Fatalf("order = %v, want %v", got, tc.want)
			}
		})
	}

	// A limit must take the top of the sorted order, not an arbitrary subset:
	// this is what makes server-side sort + limit meaningful for the browser.
	top, err := st.List(ctx, ns, store.Filter{Sort: store.Sort{Key: store.SortImportance}}, 1)
	if err != nil {
		t.Fatalf("list with limit: %v", err)
	}
	if got := shorts(top); !slices.Equal(got, []string{memMid}) {
		t.Fatalf("limit 1 by importance desc = %v, want [mid]", got)
	}
}

// testMemoryTypeFilter covers the OR-semantics metadata.memory_type filter the
// UI's multi-select needs — distinct from Filter.Metadata, which ANDs one value
// per key. Memories carrying no memory_type must never match.
func testMemoryTypeFilter(t *testing.T, st store.Store, dims int) {
	ns := t.Name()

	decision := mem(ns, "dec", "we chose sqlite", vec(dims, 1))
	decision.Metadata = map[string]any{"memory_type": typeDecision}
	mustUpsert(t, st, decision)

	preference := mem(ns, "pref", "user prefers tabs", vec(dims, 2))
	preference.Metadata = map[string]any{"memory_type": typePreference}
	mustUpsert(t, st, preference)

	untyped := mem(ns, "untyped", "no memory type at all", vec(dims, 3))
	mustUpsert(t, st, untyped)

	for _, tc := range []struct {
		name  string
		types []string
		want  []string
	}{
		{"empty matches all", nil, []string{id(ns, "dec"), id(ns, "pref"), id(ns, "untyped")}},
		{"single type", []string{typeDecision}, []string{id(ns, "dec")}},
		{"two types OR", []string{typeDecision, typePreference}, []string{id(ns, "dec"), id(ns, "pref")}},
		{"unknown type matches none", []string{"nonesuch"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := memIDs(mustList(t, st, ns, store.Filter{MemoryTypes: tc.types}))
			slices.Sort(got)
			want := slices.Clone(tc.want)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Fatalf("memory_type=%v matched %v, want %v", tc.types, got, want)
			}
		})
	}
}

// testListRecencyWindow covers the CreatedAfter/AccessedAfter window filters,
// which are inclusive at the boundary (>=).
func testListRecencyWindow(t *testing.T, st store.Store, dims int) {
	ns := t.Name()
	now := time.Now().UTC().Truncate(time.Millisecond)
	cutoff := now.Add(-24 * time.Hour)

	old := mem(ns, "old", "created long ago, accessed long ago", vec(dims, 1))
	old.CreatedAt = now.Add(-48 * time.Hour)
	old.LastAccessedAt = now.Add(-48 * time.Hour)
	mustUpsert(t, st, old)

	// Straddles the window: created before the cutoff but accessed after it, so
	// each filter selects a different memory — a filter wired to the wrong
	// column would still pass with a simpler fixture.
	revisited := mem(ns, "revisited", "created long ago, accessed just now", vec(dims, 2))
	revisited.CreatedAt = now.Add(-48 * time.Hour)
	revisited.LastAccessedAt = now
	mustUpsert(t, st, revisited)

	fresh := mem(ns, "fresh", "created at the cutoff exactly", vec(dims, 3))
	fresh.CreatedAt = cutoff
	fresh.LastAccessedAt = now.Add(-48 * time.Hour)
	mustUpsert(t, st, fresh)

	got := memIDs(mustList(t, st, ns, store.Filter{CreatedAfter: cutoff}))
	if want := []string{id(ns, "fresh")}; !slices.Equal(got, want) {
		t.Fatalf("CreatedAfter matched %v, want %v (boundary is inclusive)", got, want)
	}

	got = memIDs(mustList(t, st, ns, store.Filter{AccessedAfter: cutoff}))
	if want := []string{id(ns, "revisited")}; !slices.Equal(got, want) {
		t.Fatalf("AccessedAfter matched %v, want %v", got, want)
	}
}

// testEventLog exercises the optional EventLogStore capability. Split into
// focused subtests over one shared fixture: an append/list round trip, the
// filters, the keyset cursor, and pruning. Stores that do not implement
// EventLogStore skip.
func testEventLog(t *testing.T, st store.Store, _ int) {
	els, ok := st.(store.EventLogStore)
	if !ok {
		t.Skip("store does not implement store.EventLogStore")
	}
	ns := t.Name()
	other := ns + "-other"
	base := time.Now().UTC().Truncate(time.Millisecond).Add(-time.Hour)

	seedEvents(t, els, ns, other, base)

	// Ordered: the prune subtests destroy the fixture the read subtests rely on,
	// so they run last.
	t.Run("RoundTripAndOrder", func(t *testing.T) { testEventRoundTrip(t, els, ns, base) })
	t.Run("Filters", func(t *testing.T) { testEventFilters(t, els, ns, other, base) })
	t.Run("Cursor", func(t *testing.T) { testEventCursor(t, els, ns) })
	t.Run("ServedSnapshots", func(t *testing.T) { testServedSnapshots(t, els, ns, base) })
	t.Run("Prune", func(t *testing.T) { testEventPrune(t, els, ns, base) })
}

// eventScore is the composite score the seeded recall's top hit was served with.
const eventScore = 0.75

// eventActor / eventActorKind are the named key the seeded recall is attributed
// to, and its kind (see store.Event.ActorKind).
const (
	eventActor     = "alice"
	eventActorKind = "key"
)

// seedEvents writes the fixture the event-log subtests read: one recall serving
// two memories of different tiers (two rows, one op_id, one timestamp), a later
// write in the same namespace, and one event in another namespace.
func seedEvents(t *testing.T, els store.EventLogStore, ns, other string, base time.Time) {
	t.Helper()
	ctx := context.Background()
	score := eventScore

	if err := els.AppendEvents(ctx, []store.Event{
		{
			OpID: opRecall, Kind: store.EventRecall, Namespace: ns, Query: eventQuery,
			MemoryID: "m1", MemoryNS: ns, MemoryTier: memory.TierSemantic,
			MemorySummary: "we chose sqlite", Rank: 1, Score: &score,
			Detail: map[string]any{"degraded": "vector"},
			Actor:  eventActor, ActorKind: eventActorKind, CreatedAt: base,
		},
		{
			OpID: opRecall, Kind: store.EventRecall, Namespace: ns, Query: eventQuery,
			MemoryID: "m2", MemoryNS: ns, MemoryTier: memory.TierEpisodic,
			MemorySummary: "sqlite benchmark", Rank: 2,
			Actor: eventActor, ActorKind: eventActorKind, CreatedAt: base,
		},
	}); err != nil {
		t.Fatalf("append recall: %v", err)
	}
	// The admin env key carries a kind but no name.
	if err := els.AppendEvents(ctx, []store.Event{{
		OpID: "op-remember", Kind: store.EventRemember, Namespace: ns,
		MemoryID: "m3", MemoryNS: ns, MemoryTier: memory.TierSemantic,
		MemorySummary: "a new fact", ActorKind: "env", CreatedAt: base.Add(time.Minute),
	}}); err != nil {
		t.Fatalf("append remember: %v", err)
	}
	// A legacy row predating attribution: both actor fields empty.
	if err := els.AppendEvents(ctx, []store.Event{{
		OpID: "op-elsewhere", Kind: store.EventGet, Namespace: other,
		MemoryID: "m4", MemoryNS: other, CreatedAt: base.Add(2 * time.Minute),
	}}); err != nil {
		t.Fatalf("append other-namespace event: %v", err)
	}
}

func mustListEvents(t *testing.T, els store.EventLogStore, f store.EventFilter) []store.Event {
	t.Helper()
	got, err := els.ListEvents(context.Background(), f)
	if err != nil {
		t.Fatalf("list events %+v: %v", f, err)
	}
	return got
}

// testEventRoundTrip covers newest-first ordering, an operation's rows staying
// adjacent, and every field surviving the trip through the driver.
func testEventRoundTrip(t *testing.T, els store.EventLogStore, ns string, base time.Time) {
	all := mustListEvents(t, els, store.EventFilter{})
	if len(all) < 4 {
		t.Fatalf("expected at least 4 rows, got %d", len(all))
	}
	for i := 1; i < len(all); i++ {
		if all[i-1].CreatedAt.Before(all[i].CreatedAt) {
			t.Fatalf("rows not newest-first: row %d (%s) older than row %d (%s)",
				i-1, all[i-1].CreatedAt, i, all[i].CreatedAt)
		}
	}

	mine := mustListEvents(t, els, store.EventFilter{Namespace: ns})
	if len(mine) != 3 {
		t.Fatalf("namespace filter returned %d rows, want 3", len(mine))
	}
	if mine[0].Kind != store.EventRemember || mine[0].MemoryID != "m3" {
		t.Fatalf("newest row = %+v, want the remember of m3", mine[0])
	}
	// The recall's two rows must be adjacent — the grouped reader regroups a
	// flat page by walking consecutive rows sharing an op_id, so a driver that
	// interleaved operations here would silently split events in the UI.
	if mine[1].OpID != opRecall || mine[2].OpID != opRecall {
		t.Fatalf("recall rows not adjacent: got op_ids %q, %q", mine[1].OpID, mine[2].OpID)
	}
	// Rows of one operation share a created_at, so the id-DESC tiebreak hands
	// them back in reverse insertion order — rank 2 before rank 1. Adjacency is
	// the store's contract; ordering *within* a group is the reader's job (it
	// sorts by rank), so find the rows by rank rather than by position.
	byRank := map[int]store.Event{mine[1].Rank: mine[1], mine[2].Rank: mine[2]}
	top, ok := byRank[1]
	if !ok {
		t.Fatalf("no rank-1 row among the recall rows: %+v", mine[1:3])
	}
	if top.Score == nil || *top.Score != eventScore {
		t.Fatalf("score round trip failed: %v", top.Score)
	}
	if top.Query != eventQuery || top.MemoryTier != memory.TierSemantic ||
		top.MemorySummary != "we chose sqlite" || top.MemoryNS != ns {
		t.Fatalf("recall row round trip failed: %+v", top)
	}
	if got := top.Detail["degraded"]; got != "vector" {
		t.Fatalf("detail round trip: degraded = %v, want %q", got, "vector")
	}
	// Attribution round-trips: a named key carries name+kind.
	if top.Actor != eventActor || top.ActorKind != eventActorKind {
		t.Fatalf("actor round trip: got (%q, %q), want (alice, key)", top.Actor, top.ActorKind)
	}
	// A legacy row (seeded with both actor fields empty) round-trips as the ''
	// default, not null — "unknown", cleanly distinguishable from a named actor.
	legacy := mustListEvents(t, els, store.EventFilter{Namespaces: []string{ns + "-other"}})
	if len(legacy) != 1 {
		t.Fatalf("expected 1 legacy row, got %d", len(legacy))
	}
	if legacy[0].Actor != "" || legacy[0].ActorKind != "" {
		t.Fatalf("legacy row actor = (%q, %q), want both empty", legacy[0].Actor, legacy[0].ActorKind)
	}
	if !top.CreatedAt.Equal(base) {
		t.Fatalf("created_at round trip: got %s, want %s", top.CreatedAt, base)
	}
	// A nil score must stay nil, not become 0 — "not applicable" and "scored
	// zero" are different facts.
	if second := byRank[2]; second.Score != nil {
		t.Fatalf("rank-2 row score = %v, want nil", *second.Score)
	}
}

// testEventFilters covers kind, tier, text, since and namespace narrowing.
// Tier and text select whole operations, not rows.
func testEventFilters(t *testing.T, els store.EventLogStore, ns, other string, base time.Time) {
	if got := mustListEvents(t, els, store.EventFilter{
		Namespace: ns, Kinds: []store.EventKind{store.EventRecall},
	}); len(got) != 2 {
		t.Fatalf("kind filter returned %d rows, want 2", len(got))
	}

	// The recall touched one semantic and one episodic memory, so filtering on
	// either tier must return BOTH of its rows. Returning only the matching row
	// would make the event misreport what it served.
	for _, tier := range []memory.Tier{memory.TierSemantic, memory.TierEpisodic} {
		got := mustListEvents(t, els, store.EventFilter{
			Namespace: ns, Kinds: []store.EventKind{store.EventRecall}, Tiers: []memory.Tier{tier},
		})
		if len(got) != 2 {
			t.Fatalf("tier=%s returned %d rows, want the recall's 2 rows intact "+
				"(the operation matched, so all of its memories come back)", tier, len(got))
		}
	}
	if got := mustListEvents(t, els, store.EventFilter{
		Namespace: ns, Tiers: []memory.Tier{memory.TierWorking},
	}); len(got) != 0 {
		t.Fatalf("tier=working matched %d rows, want none", len(got))
	}

	for _, tc := range []struct {
		name string
		text string
		want int
	}{
		{"matches a served memory's summary", "benchmark", 2},
		{"matches the recall query", "why SQLite", 2}, // case-insensitive
		{"matches a write's summary", "a new fact", 1},
		{"matches nothing", "kangaroo", 0},
		{"wildcards are literal, not patterns", "%", 0},
	} {
		if got := mustListEvents(t, els, store.EventFilter{Namespace: ns, Text: tc.text}); len(got) != tc.want {
			t.Errorf("text=%q (%s) returned %d rows, want %d", tc.text, tc.name, len(got), tc.want)
		}
	}

	since := mustListEvents(t, els, store.EventFilter{Namespace: ns, Since: base.Add(30 * time.Second)})
	if len(since) != 1 || since[0].MemoryID != "m3" {
		t.Fatalf("since filter returned %+v, want only the later remember", since)
	}

	narrowed := mustListEvents(t, els, store.EventFilter{Namespaces: []string{other}})
	if len(narrowed) != 1 || narrowed[0].Namespace != other {
		t.Fatalf("namespaces=[%s] returned %+v, want only that namespace's event", other, narrowed)
	}

	// The actor filter selects only the named key's rows — both recall rows,
	// none of the env/legacy ones. It matches the name exactly, so the
	// nameless env and legacy rows are never selected.
	byActor := mustListEvents(t, els, store.EventFilter{Actor: eventActor})
	if len(byActor) != 2 {
		t.Fatalf("actor filter returned %d rows, want the 2 recall rows", len(byActor))
	}
	for _, e := range byActor {
		if e.Actor != eventActor || e.Kind != store.EventRecall {
			t.Fatalf("actor filter leaked a non-alice row: %+v", e)
		}
	}
	if got := mustListEvents(t, els, store.EventFilter{Actor: "nobody"}); len(got) != 0 {
		t.Fatalf("actor=nobody matched %d rows, want none", len(got))
	}
}

// testEventCursor covers the keyset cursor: paging must neither repeat a row
// nor skip one.
func testEventCursor(t *testing.T, els store.EventLogStore, ns string) {
	page1 := mustListEvents(t, els, store.EventFilter{Namespace: ns, Limit: 1})
	if len(page1) != 1 || page1[0].MemoryID != "m3" {
		t.Fatalf("page 1 = %+v, want the single newest row (m3)", page1)
	}
	page2 := mustListEvents(t, els, store.EventFilter{
		Namespace: ns, Limit: 2,
		Before:   page1[0].CreatedAt,
		BeforeID: page1[0].ID,
	})
	if len(page2) != 2 {
		t.Fatalf("page 2 returned %d rows, want 2", len(page2))
	}
	for _, e := range page2 {
		if e.ID == page1[0].ID {
			t.Fatal("cursor re-returned the row it was taken from")
		}
	}
}

// testEventPrune covers pruning by age and by row cap. It destroys the fixture,
// so it runs last.
// testServedSnapshots pins the lookup backing inject-event hydration. It seeds
// its own namespace at timestamps after the shared fixture's, so it stays
// invisible to the read subtests that already ran and leaves testEventPrune's
// counts untouched.
//
// The cases that matter are the ones a naive implementation gets wrong: an
// ancestor-namespace serve must report the MEMORY's namespace and not the
// request's, a bare inject row must not shadow the real snapshot beneath it,
// and a non-serve kind must not be a donor.
func testServedSnapshots(t *testing.T, els store.EventLogStore, ns string, base time.Time) {
	ctx := context.Background()
	snapNS := ns + "-snap"
	parent := ns + "-parent"
	at := base.Add(3 * time.Minute)

	// One id per case the lookup has to get right.
	const (
		idTwice     = "s-twice"     // served twice: newest snapshot wins
		idAncestor  = "s-ancestor"  // served to a child, owned by the parent
		idShadowed  = "s-shadowed"  // a bare inject row sits on top of the serve
		idWritten   = "s-written"   // written, never served: not a donor
		idElsewhere = "s-elsewhere" // served, but to another namespace
		idUnknown   = "s-unknown"   // never seen at all
	)

	seed := []store.Event{
		// Served twice: the newer snapshot must win.
		{
			OpID: "op-s1", Kind: store.EventRecall, Namespace: snapNS,
			MemoryID: idTwice, MemoryNS: snapNS, MemoryTier: memory.TierSemantic,
			MemorySummary: "stale text", CreatedAt: at,
		},
		{
			OpID: "op-s2", Kind: store.EventRecall, Namespace: snapNS,
			MemoryID: idTwice, MemoryNS: snapNS, MemoryTier: memory.TierProcedural,
			MemorySummary: "current text", CreatedAt: at.Add(time.Minute),
		},
		// A cascading recall: served against snapNS, but the memory lives in an
		// ancestor. The serve row is the only record of that fact.
		{
			OpID: "op-s3", Kind: store.EventBriefing, Namespace: snapNS,
			MemoryID: idAncestor, MemoryNS: parent, MemoryTier: memory.TierSemantic,
			MemorySummary: "inherited fact", CreatedAt: at,
		},
		// A real serve, then a bare inject row on top of it. The bare row is
		// newer, so a lookup that forgets to skip snapshot-less rows returns
		// empty strings here instead of the snapshot underneath.
		{
			OpID: "op-s4", Kind: store.EventRecall, Namespace: snapNS,
			MemoryID: idShadowed, MemoryNS: snapNS, MemoryTier: memory.TierEpisodic,
			MemorySummary: "still findable", CreatedAt: at,
		},
		{
			OpID: "op-s5", Kind: store.EventInject, Namespace: snapNS,
			MemoryID: idShadowed, Rank: 1, CreatedAt: at.Add(time.Minute),
		},
		// Written but never served: not a donor, so a beacon cannot use a write
		// it did not see to learn the memory's text.
		{
			OpID: "op-s6", Kind: store.EventRemember, Namespace: snapNS,
			MemoryID: idWritten, MemoryNS: snapNS, MemoryTier: memory.TierSemantic,
			MemorySummary: "never served", CreatedAt: at,
		},
		// Served, but to a different namespace.
		{
			OpID: "op-s7", Kind: store.EventRecall, Namespace: parent,
			MemoryID: idElsewhere, MemoryNS: parent, MemoryTier: memory.TierSemantic,
			MemorySummary: "someone else's", CreatedAt: at,
		},
	}
	if err := els.AppendEvents(ctx, seed); err != nil {
		t.Fatalf("append snapshot fixture: %v", err)
	}

	ids := []string{idTwice, idAncestor, idShadowed, idWritten, idElsewhere, idUnknown}
	got, err := els.ServedSnapshots(ctx, snapNS, ids, time.Time{})
	if err != nil {
		t.Fatalf("served snapshots: %v", err)
	}

	want := map[string]store.MemorySnapshot{
		idTwice:    {Namespace: snapNS, Tier: memory.TierProcedural, Summary: "current text"},
		idAncestor: {Namespace: parent, Tier: memory.TierSemantic, Summary: "inherited fact"},
		idShadowed: {Namespace: snapNS, Tier: memory.TierEpisodic, Summary: "still findable"},
	}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("snapshot[%s] = %+v, want %+v", id, got[id], w)
		}
	}
	for _, id := range []string{idWritten, idElsewhere, idUnknown} {
		if snap, ok := got[id]; ok {
			t.Errorf("snapshot[%s] = %+v, want absent", id, snap)
		}
	}

	// The since bound excludes a serve older than the window.
	fresh, err := els.ServedSnapshots(ctx, snapNS, []string{idAncestor}, at.Add(time.Hour))
	if err != nil {
		t.Fatalf("served snapshots since: %v", err)
	}
	if snap, ok := fresh[idAncestor]; ok {
		t.Errorf("snapshot outside the window = %+v, want absent", snap)
	}

	// No ids is not an error — the caller has nothing to hydrate.
	empty, err := els.ServedSnapshots(ctx, snapNS, nil, time.Time{})
	if err != nil {
		t.Fatalf("served snapshots with no ids: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("served snapshots with no ids = %+v, want empty", empty)
	}
}

func testEventPrune(t *testing.T, els store.EventLogStore, ns string, base time.Time) {
	ctx := context.Background()

	// By age: drop the recall's rows (at base), keep everything later.
	deleted, err := els.PruneEvents(ctx, base.Add(30*time.Second), 0)
	if err != nil {
		t.Fatalf("prune by age: %v", err)
	}
	if deleted != 2 {
		t.Fatalf("prune by age deleted %d rows, want 2", deleted)
	}
	left := mustListEvents(t, els, store.EventFilter{Namespace: ns})
	if len(left) != 1 || left[0].MemoryID != "m3" {
		t.Fatalf("after age prune, remaining = %+v, want only m3", left)
	}

	// By cap: keep only the newest row across the whole log.
	if _, err := els.PruneEvents(ctx, time.Time{}, 1); err != nil {
		t.Fatalf("prune by cap: %v", err)
	}
	if remaining := mustListEvents(t, els, store.EventFilter{}); len(remaining) != 1 {
		t.Fatalf("after cap prune, %d rows remain, want 1", len(remaining))
	}
}

// testNamespaceActivity verifies the optional ActivityStore aggregate: one
// row per namespace holding live memories, with Total counting only live rows
// (expired and superseded rows excluded) and LastWrite the max created_at
// among those live rows — a tombstoned row must neither count nor advance the
// clock. A namespace holding only non-live rows yields no row at all. Stores
// that do not implement ActivityStore skip.
func testNamespaceActivity(t *testing.T, st store.Store, dims int) {
	as, ok := st.(store.ActivityStore)
	if !ok {
		t.Skip("store does not implement store.ActivityStore")
	}
	ctx := context.Background()
	ns := t.Name()
	nsA, nsB, nsDead := ns+"-a", ns+"-b", ns+"-dead"
	base := time.Now().UTC().Truncate(time.Millisecond)

	older := mem(nsA, "older", "older live fact", vec(dims, 1))
	older.CreatedAt = base.Add(-2 * time.Hour)
	mustUpsert(t, st, older)
	newest := mem(nsA, "newest", "newest live fact", vec(dims, 2))
	newest.CreatedAt = base.Add(-1 * time.Hour)
	mustUpsert(t, st, newest)
	// An expired row CREATED AFTER the newest live one: it must not count
	// toward Total and must not advance LastWrite past the live max.
	expired := mem(nsA, "expired", "expired fact", vec(dims, 3))
	expired.CreatedAt = base.Add(-30 * time.Minute)
	exp := base.Add(-10 * time.Minute)
	expired.ExpiresAt = &exp
	mustUpsert(t, st, expired)
	// Same for a superseded row created after the newest live one.
	sup := mem(nsA, "sup", "superseded fact", vec(dims, 4))
	sup.CreatedAt = base.Add(-20 * time.Minute)
	by := id(nsA, "newest")
	sup.SupersededBy = &by
	mustUpsert(t, st, sup)

	bOnly := mem(nsB, "only", "b live fact", vec(dims, 5))
	bOnly.CreatedAt = base.Add(-3 * time.Hour)
	mustUpsert(t, st, bOnly)

	// A namespace whose every row is expired must yield no activity row.
	dead := mem(nsDead, "gone", "expired-only namespace", vec(dims, 6))
	dead.CreatedAt = base.Add(-2 * time.Hour)
	deadExp := base.Add(-1 * time.Hour)
	dead.ExpiresAt = &deadExp
	mustUpsert(t, st, dead)

	acts, err := as.NamespaceActivity(ctx, base)
	if err != nil {
		t.Fatalf("namespace activity: %v", err)
	}
	// The store is shared across conformance subtests, so assert only on this
	// test's namespaces rather than on the full row set.
	byNS := map[string]store.NamespaceActivity{}
	for _, a := range acts {
		byNS[a.NS] = a
	}
	a, ok := byNS[nsA]
	if !ok {
		t.Fatalf("no activity row for %s (rows: %v)", nsA, acts)
	}
	if a.Total != 2 {
		t.Errorf("%s total = %d, want 2 (live rows only — expired/superseded excluded)", nsA, a.Total)
	}
	if !a.LastWrite.Equal(newest.CreatedAt) {
		t.Errorf("%s last write = %v, want %v (max created_at among LIVE rows; the newer tombstoned rows must not advance it)",
			nsA, a.LastWrite, newest.CreatedAt)
	}
	b, ok := byNS[nsB]
	if !ok {
		t.Fatalf("no activity row for %s (rows: %v)", nsB, acts)
	}
	if b.Total != 1 || !b.LastWrite.Equal(bOnly.CreatedAt) {
		t.Errorf("%s = {total %d, last %v}, want {total 1, last %v}", nsB, b.Total, b.LastWrite, bOnly.CreatedAt)
	}
	if _, ok := byNS[nsDead]; ok {
		t.Errorf("%s holds only expired rows and must yield no activity row, got %+v", nsDead, byNS[nsDead])
	}
}

// testVectorlessRow verifies stores accept memories with no embedding
// (len(m.Embedding) == 0): the row is stored and keyword-searchable but never
// surfaces from VectorSearch. This is the write path memini falls back to when
// the embedding provider is unavailable (Task 11). Re-upserting with a real
// vector must make it vector-searchable; re-upserting a vectored row without a
// vector must remove the now-stale vector-index entry, not just leave it
// unreachable through the new write.
// testGetEmbedding pins the three-way contract callers depend on to tell "reuse
// this vector" from "go embed": a stored vector round-trips exactly, a
// vectorless row reports (nil, nil) rather than an error, and a missing memory
// is ErrNotFound. Conflating the middle case with either neighbour is what
// makes a skip-the-embed write either lose the vector or fail closed.
func testGetEmbedding(t *testing.T, st store.Store, dims int) {
	ctx := context.Background()
	ns := t.Name()

	// (a) a stored vector round-trips byte-for-byte.
	want := vec(dims, 0.25, -0.5, 0.75)
	mustUpsert(t, st, mem(ns, "vectored", "content with a vector", want))
	got, err := st.GetEmbedding(ctx, ns, id(ns, "vectored"))
	if err != nil {
		t.Fatalf("get embedding: %v", err)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("embedding round-trip mismatch:\n got %v\nwant %v", got, want)
	}

	// (b) a vectorless row is (nil, nil) — present, but nothing to reuse.
	mustUpsert(t, st, mem(ns, "vectorless", "content with no vector", nil))
	got, err = st.GetEmbedding(ctx, ns, id(ns, "vectorless"))
	if err != nil {
		t.Fatalf("get embedding on a vectorless row: %v", err)
	}
	if got != nil {
		t.Fatalf("vectorless row should report a nil embedding, got %v", got)
	}

	// (c) a missing memory is ErrNotFound, distinct from (b).
	if _, err := st.GetEmbedding(ctx, ns, id(ns, "absent")); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("get embedding on a missing memory: want ErrNotFound, got %v", err)
	}

	// (d) a row that loses its vector reports (nil, nil) again — the degraded
	// re-upsert path, which must not keep serving the old vector.
	mustUpsert(t, st, mem(ns, "vectored", "content with a vector", nil))
	got, err = st.GetEmbedding(ctx, ns, id(ns, "vectored"))
	if err != nil {
		t.Fatalf("get embedding after dropping the vector: %v", err)
	}
	if got != nil {
		t.Fatalf("embedding should be gone after a vectorless re-upsert, got %v", got)
	}
}

func testVectorlessRow(t *testing.T, st store.Store, dims int) {
	ctx := context.Background()
	ns := t.Name()
	const content = "the office wifi password is on the whiteboard"

	// (a) upsert with nil embedding succeeds.
	mustUpsert(t, st, mem(ns, "row", content, nil))
	got, err := st.Get(ctx, ns, id(ns, "row"))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Content != content {
		t.Fatalf("content mismatch: %q", got.Content)
	}

	// (b) KeywordSearch finds it.
	kres, err := st.KeywordSearch(ctx, ns, "whiteboard", store.Filter{}, 10)
	if err != nil {
		t.Fatalf("keyword search: %v", err)
	}
	if !slices.Contains(idsOf(kres), id(ns, "row")) {
		t.Fatalf("vectorless memory should be keyword-searchable, got %v", idsOf(kres))
	}

	// (c) VectorSearch does not.
	vres, err := st.VectorSearch(ctx, ns, vec(dims, 1), store.Filter{}, 10)
	if err != nil {
		t.Fatalf("vector search: %v", err)
	}
	if slices.Contains(idsOf(vres), id(ns, "row")) {
		t.Fatalf("vectorless memory should not be vector-searchable, got %v", idsOf(vres))
	}

	// (d) re-upsert the same id WITH a vector: VectorSearch now finds it.
	mustUpsert(t, st, mem(ns, "row", content, vec(dims, 1)))
	vres, err = st.VectorSearch(ctx, ns, vec(dims, 1), store.Filter{}, 10)
	if err != nil {
		t.Fatalf("vector search after gaining an embedding: %v", err)
	}
	if !slices.Contains(idsOf(vres), id(ns, "row")) {
		t.Fatalf("memory should be vector-searchable after gaining an embedding, got %v", idsOf(vres))
	}

	// (e) re-upsert again WITHOUT a vector: the stale vec-index entry must be
	// removed (VectorSearch stops finding it) while KeywordSearch still does.
	mustUpsert(t, st, mem(ns, "row", content, nil))
	vres, err = st.VectorSearch(ctx, ns, vec(dims, 1), store.Filter{}, 10)
	if err != nil {
		t.Fatalf("vector search after losing the embedding: %v", err)
	}
	if slices.Contains(idsOf(vres), id(ns, "row")) {
		t.Fatalf("stale vector-index entry not removed on re-upsert without an embedding, got %v", idsOf(vres))
	}
	kres, err = st.KeywordSearch(ctx, ns, "whiteboard", store.Filter{}, 10)
	if err != nil {
		t.Fatalf("keyword search after losing the embedding: %v", err)
	}
	if !slices.Contains(idsOf(kres), id(ns, "row")) {
		t.Fatalf("keyword search should still find the memory after the vector is removed, got %v", idsOf(kres))
	}

	// (f) a wrong non-zero embedding length must still error.
	if err := st.Upsert(ctx, mem(ns, "bad", "wrong dims", vec(dims-1, 1))); err == nil {
		t.Fatalf("expected an error for a wrong non-zero embedding length")
	}

	// A vectorless memory must still be movable by Reassign: a driver whose
	// lookup inner-joins the vector index would otherwise silently skip it
	// (no vec-index row to join against), reporting 0 moved instead of moving it.
	toNS := ns + "-moved"
	n, err := st.Reassign(ctx, ns, []string{id(ns, "row")}, toNS)
	if err != nil {
		t.Fatalf("reassign vectorless memory: %v", err)
	}
	if n != 1 {
		t.Fatalf("reassign vectorless memory moved %d, want 1", n)
	}
	if _, err := st.Get(ctx, toNS, id(ns, "row")); err != nil {
		t.Fatalf("vectorless memory not found after reassign: %v", err)
	}
}

// testNamespaceLinks exercises the optional LinkStore capability: put/list
// round-trip (including tiers and note), upsert-overwrites on a (src,dst)
// conflict, DeleteLink's existed-bool return, ListLinks/ListAllLinks scoping,
// the DeleteNamespace cascade (gap G5), and RenameLinkEndpoints rewriting both
// sides of a link. Stores that do not implement LinkStore skip. Split into
// sub-tests (rather than one long function) to keep each check focused.
func testNamespaceLinks(t *testing.T, st store.Store, dims int) {
	_ = dims // links carry no embedding; kept for signature parity with the other subtests
	ls, ok := st.(store.LinkStore)
	if !ok {
		t.Skip("store does not implement store.LinkStore")
	}
	ns := t.Name()
	t.Run("PutListRoundTripAndUpsert", func(t *testing.T) { testLinkRoundTripAndUpsert(t, ls, ns) })
	t.Run("ScopingAndDelete", func(t *testing.T) { testLinkScopingAndDelete(t, ls, ns) })
	t.Run("DeleteNamespaceCascade", func(t *testing.T) { testLinkDeleteNamespaceCascade(t, st, ls, ns) })
	t.Run("RenameEndpoints", func(t *testing.T) { testLinkRenameEndpoints(t, ls, ns) })
	t.Run("RenameCollisionKeepsExisting", func(t *testing.T) { testLinkRenameCollisionKeepsExisting(t, ls, ns) })
	t.Run("RenameReciprocalPair", func(t *testing.T) { testLinkRenameReciprocalPair(t, ls, ns) })
}

// testLinkRoundTripAndUpsert covers PutLink/ListLinks round-tripping every
// field (including tiers and note), and that a second PutLink for the same
// (src,dst) overwrites in place rather than duplicating the row.
func testLinkRoundTripAndUpsert(t *testing.T, ls store.LinkStore, ns string) {
	ctx := context.Background()
	src := ns + "-rt-src"
	dst := ns + "-rt-dst"
	now := time.Now().UTC().Truncate(time.Millisecond)

	link := store.NamespaceLink{
		Src: src, Dst: dst,
		Tiers:     []memory.Tier{memory.TierSemantic, memory.TierProcedural},
		Note:      "shared golang helpers",
		CreatedAt: now,
	}
	if err := ls.PutLink(ctx, link); err != nil {
		t.Fatalf("put link: %v", err)
	}
	got, err := ls.ListLinks(ctx, src)
	if err != nil {
		t.Fatalf("list links: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("list links = %d, want 1", len(got))
	}
	if got[0].Src != src || got[0].Dst != dst || got[0].Note != link.Note {
		t.Fatalf("round-trip mismatch: %+v", got[0])
	}
	if !slices.Equal(got[0].Tiers, link.Tiers) {
		t.Fatalf("tiers = %v, want %v", got[0].Tiers, link.Tiers)
	}
	if !got[0].CreatedAt.Equal(now) {
		t.Fatalf("created_at = %v, want %v", got[0].CreatedAt, now)
	}

	// Upsert overwrites in place: same (src,dst), different tiers/note. Must
	// not duplicate the row.
	overwrite := store.NamespaceLink{
		Src: src, Dst: dst,
		Tiers:     []memory.Tier{memory.TierSemantic},
		Note:      "updated note",
		CreatedAt: now.Add(time.Minute),
	}
	if err := ls.PutLink(ctx, overwrite); err != nil {
		t.Fatalf("put link (overwrite): %v", err)
	}
	got, err = ls.ListLinks(ctx, src)
	if err != nil {
		t.Fatalf("list links after overwrite: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("list links after overwrite = %d, want 1 (upsert must not duplicate)", len(got))
	}
	if got[0].Note != "updated note" || !slices.Equal(got[0].Tiers, overwrite.Tiers) {
		t.Fatalf("overwrite not applied: %+v", got[0])
	}
}

// testLinkScopingAndDelete covers ListLinks scoping to a single Src,
// ListAllLinks returning everything, an unknown Src yielding an empty (not
// error) list, and DeleteLink's existed-bool return.
func testLinkScopingAndDelete(t *testing.T, ls store.LinkStore, ns string) {
	ctx := context.Background()
	src := ns + "-sc-src"
	dst := ns + "-sc-dst"
	other := ns + "-sc-other"
	now := time.Now().UTC().Truncate(time.Millisecond)

	if err := ls.PutLink(ctx, store.NamespaceLink{Src: src, Dst: dst, CreatedAt: now}); err != nil {
		t.Fatalf("put link1: %v", err)
	}
	if err := ls.PutLink(ctx, store.NamespaceLink{Src: src, Dst: other, CreatedAt: now}); err != nil {
		t.Fatalf("put link2: %v", err)
	}
	if err := ls.PutLink(ctx, store.NamespaceLink{Src: other, Dst: dst, CreatedAt: now}); err != nil {
		t.Fatalf("put link3: %v", err)
	}

	got, err := ls.ListLinks(ctx, src)
	if err != nil {
		t.Fatalf("list links (src): %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("list links (src) = %d, want 2", len(got))
	}

	all, err := ls.ListAllLinks(ctx)
	if err != nil {
		t.Fatalf("list all links: %v", err)
	}
	if len(all) < 3 {
		t.Fatalf("list all links = %d, want >= 3", len(all))
	}

	// Empty/unknown src -> empty list, no error.
	empty, err := ls.ListLinks(ctx, ns+"-unknown")
	if err != nil {
		t.Fatalf("list links (unknown src): %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("list links (unknown src) = %d, want 0", len(empty))
	}

	// DeleteLink returns existed=true, then existed=false on the second call.
	existed, err := ls.DeleteLink(ctx, src, other)
	if err != nil {
		t.Fatalf("delete link: %v", err)
	}
	if !existed {
		t.Fatalf("delete link: existed = false, want true")
	}
	existed, err = ls.DeleteLink(ctx, src, other)
	if err != nil {
		t.Fatalf("delete link (again): %v", err)
	}
	if existed {
		t.Fatalf("delete link (again): existed = true, want false")
	}
}

// testLinkDeleteNamespaceCascade covers gap G5: DeleteNamespace must also
// drop namespace_links rows referencing the namespace on either side.
func testLinkDeleteNamespaceCascade(t *testing.T, st store.Store, ls store.LinkStore, ns string) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	cascSrc := ns + "-casc-src"
	cascDst := ns + "-casc-dst"
	if err := ls.PutLink(ctx, store.NamespaceLink{Src: cascSrc, Dst: cascDst, CreatedAt: now}); err != nil {
		t.Fatalf("put cascade link (as src): %v", err)
	}
	if err := ls.PutLink(ctx, store.NamespaceLink{Src: cascDst, Dst: cascSrc, CreatedAt: now}); err != nil {
		t.Fatalf("put cascade link (as dst): %v", err)
	}
	if _, err := st.DeleteNamespace(ctx, cascSrc); err != nil {
		t.Fatalf("delete namespace: %v", err)
	}
	remaining, err := ls.ListAllLinks(ctx)
	if err != nil {
		t.Fatalf("list all links after delete namespace: %v", err)
	}
	for _, l := range remaining {
		if l.Src == cascSrc || l.Dst == cascSrc {
			t.Fatalf("link referencing deleted namespace %q survived: %+v", cascSrc, l)
		}
	}
}

// testLinkRenameEndpoints covers gap G5: RenameLinkEndpoints rewrites a
// namespace wherever it appears, as either Src or Dst, and is a no-op when
// from == to.
func testLinkRenameEndpoints(t *testing.T, ls store.LinkStore, ns string) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	from := ns + "-rename-from"
	to := ns + "-rename-to"
	third := ns + "-rename-third"
	if err := ls.PutLink(ctx, store.NamespaceLink{Src: from, Dst: third, Note: "from as src", CreatedAt: now}); err != nil {
		t.Fatalf("put rename link (as src): %v", err)
	}
	if err := ls.PutLink(ctx, store.NamespaceLink{Src: third, Dst: from, Note: "from as dst", CreatedAt: now}); err != nil {
		t.Fatalf("put rename link (as dst): %v", err)
	}
	if err := ls.RenameLinkEndpoints(ctx, from, to); err != nil {
		t.Fatalf("rename link endpoints: %v", err)
	}
	toLinks, err := ls.ListLinks(ctx, to)
	if err != nil {
		t.Fatalf("list links (to): %v", err)
	}
	if len(toLinks) != 1 || toLinks[0].Dst != third || toLinks[0].Note != "from as src" {
		t.Fatalf("rename did not rewrite src side: %+v", toLinks)
	}
	thirdLinks, err := ls.ListLinks(ctx, third)
	if err != nil {
		t.Fatalf("list links (third): %v", err)
	}
	if len(thirdLinks) != 1 || thirdLinks[0].Dst != to || thirdLinks[0].Note != "from as dst" {
		t.Fatalf("rename did not rewrite dst side: %+v", thirdLinks)
	}
	fromLinks, err := ls.ListLinks(ctx, from)
	if err != nil {
		t.Fatalf("list links (from, after rename): %v", err)
	}
	if len(fromLinks) != 0 {
		t.Fatalf("old src namespace %q still has links after rename: %v", from, fromLinks)
	}

	// Rename is a no-op when from == to.
	if err := ls.RenameLinkEndpoints(ctx, to, to); err != nil {
		t.Fatalf("rename link endpoints (noop): %v", err)
	}
}

// testLinkRenameCollisionKeepsExisting pins the collision semantics of
// RenameLinkEndpoints: when a rewritten link lands on a key where the target
// namespace already has its own link, the pre-existing link survives
// untouched and the renamed one is dropped — a rename must never silently
// widen or narrow tier access the target had explicitly configured.
func testLinkRenameCollisionKeepsExisting(t *testing.T, ls store.LinkStore, ns string) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	a := ns + "-coll-a"
	b := ns + "-coll-b"
	c := ns + "-coll-c"

	// B's own, pre-existing grant to C: narrow tiers, distinct note.
	existing := store.NamespaceLink{
		Src: b, Dst: c,
		Tiers:     []memory.Tier{memory.TierSemantic},
		Note:      "b's own grant",
		CreatedAt: now,
	}
	if err := ls.PutLink(ctx, existing); err != nil {
		t.Fatalf("put pre-existing link (b,c): %v", err)
	}
	// A's link to C: wider tiers. Renaming A->B makes it collide with (b,c).
	inherited := store.NamespaceLink{
		Src: a, Dst: c,
		Tiers:     []memory.Tier{memory.TierSemantic, memory.TierProcedural},
		Note:      "inherited from a",
		CreatedAt: now.Add(time.Minute),
	}
	if err := ls.PutLink(ctx, inherited); err != nil {
		t.Fatalf("put colliding link (a,c): %v", err)
	}

	if err := ls.RenameLinkEndpoints(ctx, a, b); err != nil {
		t.Fatalf("rename link endpoints: %v", err)
	}

	got, err := ls.ListLinks(ctx, b)
	if err != nil {
		t.Fatalf("list links (b): %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("links from %q after collision = %d, want exactly 1", b, len(got))
	}
	if got[0].Note != existing.Note || !slices.Equal(got[0].Tiers, existing.Tiers) {
		t.Fatalf("pre-existing link was clobbered by the rename: %+v, want note=%q tiers=%v",
			got[0], existing.Note, existing.Tiers)
	}
	if !got[0].CreatedAt.Equal(existing.CreatedAt) {
		t.Fatalf("pre-existing link created_at was rewritten: %v, want %v", got[0].CreatedAt, existing.CreatedAt)
	}
	// The renamed source has nothing left.
	aLinks, err := ls.ListLinks(ctx, a)
	if err != nil {
		t.Fatalf("list links (a): %v", err)
	}
	if len(aLinks) != 0 {
		t.Fatalf("renamed namespace %q still has links: %v", a, aLinks)
	}
}

// testLinkRenameReciprocalPair pins the multi-way collision: link(from,to)
// and link(to,from) both collapse onto the self-link (to,to) when from is
// renamed to to. Exactly one row must survive, and — with the rename's
// key-ordered scan — deterministically the first in (src,dst) key order,
// which is link(from,to).
func testLinkRenameReciprocalPair(t *testing.T, ls store.LinkStore, ns string) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	from := ns + "-recip-from"
	to := ns + "-recip-to"

	if err := ls.PutLink(ctx, store.NamespaceLink{Src: from, Dst: to, Note: "from to to", CreatedAt: now}); err != nil {
		t.Fatalf("put link (from,to): %v", err)
	}
	if err := ls.PutLink(ctx, store.NamespaceLink{Src: to, Dst: from, Note: "to to from", CreatedAt: now}); err != nil {
		t.Fatalf("put link (to,from): %v", err)
	}

	if err := ls.RenameLinkEndpoints(ctx, from, to); err != nil {
		t.Fatalf("rename link endpoints (reciprocal pair): %v", err)
	}

	// Nothing may reference the old namespace anymore, and the pair must have
	// collapsed to exactly one self-link — not zero (both dropped) and not an
	// error (unique-key violation).
	toLinks, err := ls.ListLinks(ctx, to)
	if err != nil {
		t.Fatalf("list links (to): %v", err)
	}
	if len(toLinks) != 1 || toLinks[0].Dst != to {
		t.Fatalf("reciprocal pair should collapse to one self-link (to,to), got %+v", toLinks)
	}
	// Deterministic winner: (from,to) sorts before (to,from), so its rewrite
	// is inserted first and the second is dropped on conflict.
	if toLinks[0].Note != "from to to" {
		t.Fatalf("self-link note = %q, want %q (first row in key order wins)", toLinks[0].Note, "from to to")
	}
	fromLinks, err := ls.ListLinks(ctx, from)
	if err != nil {
		t.Fatalf("list links (from): %v", err)
	}
	if len(fromLinks) != 0 {
		t.Fatalf("old namespace %q still has links after rename: %v", from, fromLinks)
	}
}

func testGetByFingerprint(t *testing.T, st store.Store, dims int) {
	ctx := context.Background()
	ns := t.Name()
	now := time.Now().UTC()
	m := mem(ns, "fact", "the user likes coffee", vec(dims, 1)) // semantic
	mustUpsert(t, st, m)

	// A normalized restatement (case/whitespace) shares the fingerprint.
	fp := memory.Fingerprint("  The user   likes COFFEE ")
	got, err := st.GetByFingerprint(ctx, ns, memory.TierSemantic, fp, now)
	if err != nil {
		t.Fatalf("get by fingerprint: %v", err)
	}
	if got.ID != m.ID {
		t.Fatalf("fingerprint matched %q, want %q", got.ID, m.ID)
	}

	// Wrong tier, unknown content, and empty fingerprint all miss.
	if _, err := st.GetByFingerprint(ctx, ns, memory.TierWorking, fp, now); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("tier mismatch: want ErrNotFound, got %v", err)
	}
	if _, err := st.GetByFingerprint(ctx, ns, memory.TierSemantic, memory.Fingerprint("unrelated"), now); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown content: want ErrNotFound, got %v", err)
	}
	if _, err := st.GetByFingerprint(ctx, ns, memory.TierSemantic, "", now); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("empty fingerprint: want ErrNotFound, got %v", err)
	}

	// A superseded match is excluded so a dead duplicate never absorbs a write.
	repl := mem(ns, "repl", "the user prefers tea", vec(dims, 0, 1))
	mustUpsert(t, st, repl)
	if err := st.SetSuperseded(ctx, ns, m.ID, repl.ID); err != nil {
		t.Fatalf("supersede: %v", err)
	}
	if _, err := st.GetByFingerprint(ctx, ns, memory.TierSemantic, fp, now); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("superseded match: want ErrNotFound, got %v", err)
	}

	// A validity-closed (contradicted) match is excluded too: re-asserting a
	// contradicted fact must store a live row, not corroborate the dead one.
	closed := mem(ns, "closed", "the office is in Berlin", vec(dims, 0, 0, 1))
	seed := 0.4
	closed.Confidence = &seed
	mustUpsert(t, st, closed)
	if err := st.MarkContradicted(ctx, ns, closed.ID, repl.ID, 0.2, now.Add(-time.Minute)); err != nil {
		t.Fatalf("mark contradicted: %v", err)
	}
	closedFP := memory.Fingerprint(closed.Content)
	if _, err := st.GetByFingerprint(ctx, ns, memory.TierSemantic, closedFP, now); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("validity-closed match: want ErrNotFound, got %v", err)
	}
}

func testDeleteNamespace(t *testing.T, st store.Store, dims int) {
	ctx := context.Background()
	nsDel := t.Name() + "-del"
	nsKeep := t.Name() + "-keep"
	mustUpsert(t, st, mem(nsDel, "a", "first to delete", vec(dims, 1)))
	mustUpsert(t, st, mem(nsDel, "b", "second to delete", vec(dims, 0, 1)))
	mustUpsert(t, st, mem(nsKeep, "c", "survivor", vec(dims, 1)))

	n, err := st.DeleteNamespace(ctx, nsDel)
	if err != nil {
		t.Fatalf("delete namespace: %v", err)
	}
	if n != 2 {
		t.Fatalf("DeleteNamespace returned %d, want 2", n)
	}
	// The rows are gone, including the vector index entries.
	if _, err := st.Get(ctx, nsDel, id(nsDel, "a")); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("deleted memory still present: %v", err)
	}
	res, err := st.VectorSearch(ctx, nsDel, vec(dims, 1), store.Filter{}, 5)
	if err != nil {
		t.Fatalf("vector search after delete: %v", err)
	}
	if len(res) != 0 {
		t.Errorf("vector index not cleared for the deleted namespace: %d hits", len(res))
	}
	// A sibling namespace is untouched.
	if _, err := st.Get(ctx, nsKeep, id(nsKeep, "c")); err != nil {
		t.Errorf("sibling namespace was affected by the delete: %v", err)
	}
	// Deleting an empty/unknown namespace returns 0 with no error.
	n, err = st.DeleteNamespace(ctx, t.Name()+"-empty")
	if err != nil {
		t.Fatalf("delete empty namespace: %v", err)
	}
	if n != 0 {
		t.Errorf("DeleteNamespace on an empty namespace returned %d, want 0", n)
	}
}

func testListNamespaces(t *testing.T, st store.Store, dims int) {
	ctx := context.Background()
	nsA := t.Name() + "-a"
	nsB := t.Name() + "-b"
	mustUpsert(t, st, mem(nsA, "a", "in a", vec(dims, 1)))
	mustUpsert(t, st, mem(nsB, "b", "in b", vec(dims, 0, 1)))

	got, err := st.ListNamespaces(ctx)
	if err != nil {
		t.Fatalf("list namespaces: %v", err)
	}
	// The conformance store is shared across subtests, so assert containment,
	// not equality.
	if !slices.Contains(got, nsA) || !slices.Contains(got, nsB) {
		t.Fatalf("ListNamespaces missing seeded namespaces: got %v, want to contain %q and %q", got, nsA, nsB)
	}
	// A namespace with multiple memories must appear exactly once (distinct).
	mustUpsert(t, st, mem(nsA, "a2", "also in a", vec(dims, 0, 0, 1)))
	got, err = st.ListNamespaces(ctx)
	if err != nil {
		t.Fatalf("list namespaces (after second insert): %v", err)
	}
	count := 0
	for _, ns := range got {
		if ns == nsA {
			count++
		}
	}
	if count != 1 {
		t.Errorf("namespace %q appears %d times, want 1 (must be distinct)", nsA, count)
	}
}

func testReassign(t *testing.T, st store.Store, dims int) {
	ctx := context.Background()
	from, to := t.Name()+"-from", t.Name()+"-to"

	moved := mem(from, "moved", "a memory to relocate", vec(dims, 1))
	stay := mem(from, "stay", "a memory that stays put", vec(dims, 0, 1))
	mustUpsert(t, st, moved)
	mustUpsert(t, st, stay)

	// Move only "moved" (plus a bogus ID, which must be skipped, not error).
	n, err := st.Reassign(ctx, from, []string{moved.ID, "does-not-exist"}, to)
	if err != nil {
		t.Fatalf("reassign: %v", err)
	}
	if n != 1 {
		t.Fatalf("reassign moved %d, want 1", n)
	}

	// The moved memory is now readable in the target namespace and gone from
	// the source.
	if _, err := st.Get(ctx, to, moved.ID); err != nil {
		t.Errorf("moved memory not in target namespace: %v", err)
	}
	if _, err := st.Get(ctx, from, moved.ID); err != store.ErrNotFound {
		t.Errorf("moved memory still in source namespace: %v", err)
	}
	// The untouched memory stays.
	if _, err := st.Get(ctx, from, stay.ID); err != nil {
		t.Errorf("stay memory should remain in source: %v", err)
	}

	// The vector index must follow the move: a search in the target namespace
	// finds it; a search in the source does not.
	res, err := st.VectorSearch(ctx, to, vec(dims, 1), store.Filter{}, 5)
	if err != nil {
		t.Fatalf("vector search target: %v", err)
	}
	if !containsScored(res, moved.ID) {
		t.Errorf("moved memory not vector-searchable in target namespace")
	}
	res, err = st.VectorSearch(ctx, from, vec(dims, 1), store.Filter{}, 5)
	if err != nil {
		t.Fatalf("vector search source: %v", err)
	}
	if containsScored(res, moved.ID) {
		t.Errorf("moved memory still vector-searchable in source namespace")
	}
}

func testRetier(t *testing.T, st store.Store, dims int) {
	ctx := context.Background()
	ns := t.Name()
	m := mem(ns, "m", "a durable fact", vec(dims, 1))
	m.Tier = memory.TierSemantic
	mustUpsert(t, st, m)

	exp := time.Now().UTC().Add(90 * 24 * time.Hour).Truncate(time.Millisecond)
	if err := st.Retier(ctx, ns, m.ID, memory.TierEpisodic, &exp); err != nil {
		t.Fatalf("retier: %v", err)
	}
	got, err := st.Get(ctx, ns, m.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Tier != memory.TierEpisodic {
		t.Errorf("tier = %q, want episodic", got.Tier)
	}
	if got.ExpiresAt == nil || !got.ExpiresAt.Equal(exp) {
		t.Errorf("expiry = %v, want %v", got.ExpiresAt, exp)
	}
	if err := st.Retier(ctx, ns, "missing", memory.TierEpisodic, &exp); err != store.ErrNotFound {
		t.Errorf("retier missing: want ErrNotFound, got %v", err)
	}
}

// testTemporalAsOf verifies time-travel recall: a superseded fact is excluded
// from default recall but reappears for an as_of within its validity window.
func testTemporalAsOf(t *testing.T, st store.Store, dims int) {
	ctx := context.Background()
	ns := t.Name()
	old := mem(ns, "old", "the capital is Bonn", vec(dims, 1))
	cur := mem(ns, "cur", "the capital is Berlin", vec(dims, 1))
	mustUpsert(t, st, old)
	mustUpsert(t, st, cur)

	// Supersede "old" with "cur": old gets superseded_by + valid_to=now.
	if err := st.SetSuperseded(ctx, ns, old.ID, cur.ID); err != nil {
		t.Fatalf("supersede: %v", err)
	}

	q := vec(dims, 1)
	// Default recall hides the superseded fact.
	now, err := st.VectorSearch(ctx, ns, q, store.Filter{}, 5)
	if err != nil {
		t.Fatalf("recall now: %v", err)
	}
	if containsScored(now, old.ID) {
		t.Errorf("superseded 'old' must not appear in default recall")
	}
	// Time-travel to before the supersession surfaces the then-valid fact.
	past := time.Now().Add(-time.Hour).UTC()
	asof, err := st.VectorSearch(ctx, ns, q, store.Filter{AsOf: past}, 5)
	if err != nil {
		t.Fatalf("recall as_of: %v", err)
	}
	if !containsScored(asof, old.ID) {
		t.Errorf("as_of recall before supersession should surface 'old'")
	}
}

func testSetConfidence(t *testing.T, st store.Store, dims int) {
	ctx := context.Background()
	ns := t.Name()
	m := mem(ns, "m", "a durable fact", vec(dims, 1))
	m.Tier = memory.TierSemantic
	seed := 0.4
	m.Confidence = &seed
	mustUpsert(t, st, m)

	now := time.Now().UTC().Truncate(time.Millisecond)
	if err := st.SetConfidence(ctx, ns, m.ID, 0.46, now); err != nil {
		t.Fatalf("set confidence: %v", err)
	}
	got, err := st.Get(ctx, ns, m.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Confidence == nil || *got.Confidence != 0.46 {
		t.Errorf("confidence = %v, want 0.46", got.Confidence)
	}
	if !got.UpdatedAt.Equal(now) {
		t.Errorf("updated_at = %v, want bumped to %v (decay baseline reset)", got.UpdatedAt, now)
	}
	if err := st.SetConfidence(ctx, ns, "missing", 0.5, now); err != store.ErrNotFound {
		t.Errorf("set confidence on missing: want ErrNotFound, got %v", err)
	}

	// A validity-closed (contradicted) row is not touched: corroboration must
	// never regrow an invalidated fact, even when the invalidation raced in
	// between the caller's read and its write.
	if err := st.MarkContradicted(ctx, ns, m.ID, "other", 0.2, now); err != nil {
		t.Fatalf("mark contradicted: %v", err)
	}
	if err := st.SetConfidence(ctx, ns, m.ID, 0.9, now.Add(time.Second)); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("set confidence on validity-closed: want ErrNotFound, got %v", err)
	}
	got, err = st.Get(ctx, ns, m.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Confidence == nil || *got.Confidence != 0.2 {
		t.Errorf("confidence after refused regrow = %v, want 0.2", got.Confidence)
	}
}

func testMarkContradicted(t *testing.T, st store.Store, dims int) {
	ctx := context.Background()
	ns := t.Name()
	m := mem(ns, "old", "the sky is green", vec(dims, 1))
	m.Tier = memory.TierSemantic
	seed := 0.7
	m.Confidence = &seed
	m.AccessCount = 3
	mustUpsert(t, st, m)
	mustUpsert(t, st, mem(ns, "new", "the sky is blue", vec(dims, 1)))

	now := time.Now().UTC().Truncate(time.Millisecond)
	if err := st.MarkContradicted(ctx, ns, id(ns, "old"), id(ns, "new"), 0.13, now); err != nil {
		t.Fatalf("mark contradicted: %v", err)
	}
	got, err := st.Get(ctx, ns, id(ns, "old"))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Confidence == nil || *got.Confidence != 0.13 {
		t.Errorf("confidence = %v, want 0.13", got.Confidence)
	}
	if got.ValidTo == nil || !got.ValidTo.Equal(now) {
		t.Errorf("valid_to = %v, want stamped to %v", got.ValidTo, now)
	}
	if got.Metadata["contradicted_by"] != id(ns, "new") {
		t.Errorf("contradicted_by = %v, want %q", got.Metadata["contradicted_by"], id(ns, "new"))
	}
	// The pre-update confidence (0.7) is snapshotted for audit and reversal.
	if prev, ok := got.Metadata["contradicted_prev_confidence"].(float64); !ok || prev != 0.7 {
		t.Errorf("contradicted_prev_confidence = %v, want 0.7", got.Metadata["contradicted_prev_confidence"])
	}

	// Default recall excludes the invalidated fact (valid_to in the past)...
	res, err := st.VectorSearch(ctx, ns, vec(dims, 1), store.Filter{Now: now.Add(time.Second)}, 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if slices.Contains(idsOf(res), id(ns, "old")) {
		t.Fatalf("contradicted memory should be excluded from live recall, got %v", idsOf(res))
	}
	// ...but AsOf time-travel before the stamp still surfaces it (history kept).
	asof, err := st.VectorSearch(ctx, ns, vec(dims, 1), store.Filter{AsOf: now.Add(-time.Hour)}, 10)
	if err != nil {
		t.Fatalf("asof search: %v", err)
	}
	if !slices.Contains(idsOf(asof), id(ns, "old")) {
		t.Fatalf("AsOf before valid_to should still surface the fact, got %v", idsOf(asof))
	}

	if err := st.MarkContradicted(ctx, ns, id(ns, "missing"), id(ns, "new"), 0.1, now); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("mark contradicted on missing: want ErrNotFound, got %v", err)
	}
}

func containsScored(res []store.Scored, id string) bool {
	for _, r := range res {
		if r.Memory.ID == id {
			return true
		}
	}
	return false
}

func vec(dims int, head ...float32) []float32 {
	v := make([]float32, dims)
	copy(v, head)
	return v
}

// id creates a globally-unique memory ID by scoping a short label to the
// namespace. This avoids cross-subtest collisions when subtests run against
// the same backing store (IDs are globally unique within the store).
func id(ns, short string) string { return ns + "/" + short }

func mem(ns, short, content string, v []float32) *memory.Memory {
	now := time.Now().UTC().Truncate(time.Millisecond)
	return &memory.Memory{
		ID: id(ns, short), Namespace: ns, Tier: memory.TierSemantic, Content: content,
		Importance: 0.5, CreatedAt: now, UpdatedAt: now, LastAccessedAt: now, Embedding: v,
	}
}

func mustUpsert(t *testing.T, st store.Store, m *memory.Memory) {
	t.Helper()
	if err := st.Upsert(context.Background(), m); err != nil {
		t.Fatalf("upsert %s: %v", m.ID, err)
	}
}

func testUpsertGetDelete(t *testing.T, st store.Store, dims int) {
	ctx := context.Background()
	ns := t.Name()
	m := mem(ns, "a", "the cat sat on the mat", vec(dims, 1))
	m.Tags = []string{"animals", "cat"}
	m.Metadata = map[string]any{"source": "test"}
	mustUpsert(t, st, m)

	got, err := st.Get(ctx, ns, id(ns, "a"))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Content != m.Content || got.Tier != memory.TierSemantic {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}
	if len(got.Tags) != 2 || got.Tags[0] != "animals" {
		t.Fatalf("tags not preserved: %v", got.Tags)
	}
	if got.Metadata["source"] != "test" {
		t.Fatalf("metadata not preserved: %v", got.Metadata)
	}

	if _, err := st.Get(ctx, ns+"-other", id(ns, "a")); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cross-namespace get: want ErrNotFound, got %v", err)
	}
	if err := st.Delete(ctx, ns, id(ns, "a")); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := st.Get(ctx, ns, id(ns, "a")); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("get after delete: want ErrNotFound, got %v", err)
	}
	if err := st.Delete(ctx, ns, id(ns, "a")); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("double delete: want ErrNotFound, got %v", err)
	}
}

func testUpdateInPlace(t *testing.T, st store.Store, dims int) {
	ctx := context.Background()
	ns := t.Name()

	original := mem(ns, "a", "original text", vec(dims, 1))
	createdAt := original.CreatedAt
	mustUpsert(t, st, original)

	// An update-by-ID carries a fresh CreatedAt/UpdatedAt (the service rebuilds
	// the Memory with now). The store must keep the original created_at but
	// advance updated_at.
	update := mem(ns, "a", "updated text", vec(dims, 0, 1))
	update.CreatedAt = createdAt.Add(time.Hour) // a (wrong) newer creation time
	update.UpdatedAt = createdAt.Add(time.Hour)
	mustUpsert(t, st, update)

	got, err := st.Get(ctx, ns, id(ns, "a"))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Content != "updated text" {
		t.Fatalf("update not applied: %q", got.Content)
	}
	if !got.CreatedAt.Equal(createdAt) {
		t.Errorf("created_at mutated on update: got %v, want %v (immutable)", got.CreatedAt, createdAt)
	}
	if !got.UpdatedAt.After(createdAt) {
		t.Errorf("updated_at not advanced on update: got %v, want > %v", got.UpdatedAt, createdAt)
	}
	res, err := st.VectorSearch(ctx, ns, vec(dims, 0, 1), store.Filter{}, 5)
	if err != nil {
		t.Fatalf("vector search: %v", err)
	}
	if len(res) != 1 {
		t.Fatalf("expected exactly one row after in-place update, got %d", len(res))
	}
}

func testCrossNamespaceUpsert(t *testing.T, st store.Store, dims int) {
	ctx := context.Background()
	nsA := t.Name() + "-a"
	nsB := t.Name() + "-b"

	// Use an ID scoped to nsA; nsB should not be allowed to claim it.
	sharedID := id(nsA, "x")
	m := mem(nsA, "x", "original", vec(dims, 1))
	mustUpsert(t, st, m)

	// Upserting the same ID under a different namespace must be rejected.
	attacker := &memory.Memory{
		ID: sharedID, Namespace: nsB, Tier: memory.TierSemantic, Content: "attacker",
		Importance: 0.5, CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt,
		LastAccessedAt: m.LastAccessedAt, Embedding: vec(dims, 0, 1),
	}
	if err := st.Upsert(ctx, attacker); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("cross-namespace upsert: want ErrConflict, got %v", err)
	}

	// The original memory must be untouched.
	got, err := st.Get(ctx, nsA, sharedID)
	if err != nil {
		t.Fatalf("get after failed cross-ns upsert: %v", err)
	}
	if got.Content != "original" {
		t.Fatalf("original memory was modified: %q", got.Content)
	}

	// The attacker namespace must not have a copy.
	if _, err := st.Get(ctx, nsB, sharedID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("attacker namespace should not have the memory, got %v", err)
	}
}

func testVectorRanking(t *testing.T, st store.Store, dims int) {
	ctx := context.Background()
	ns := t.Name()
	mustUpsert(t, st, mem(ns, "a", "the cat sat on the mat", vec(dims, 1)))
	mustUpsert(t, st, mem(ns, "b", "dogs are loyal animals", vec(dims, 0, 1)))
	mustUpsert(t, st, mem(ns, "c", "felines love naps", vec(dims, 0.9, 0.1)))
	mustUpsert(t, st, mem(ns+"-other", "z", "secret", vec(dims, 1)))

	res, err := st.VectorSearch(ctx, ns, vec(dims, 1), store.Filter{}, 2)
	if err != nil {
		t.Fatalf("vector search: %v", err)
	}
	if len(res) != 2 {
		t.Fatalf("want 2 results, got %d", len(res))
	}
	if res[0].Memory.ID != id(ns, "a") || res[1].Memory.ID != id(ns, "c") {
		t.Fatalf("ranking wrong: %v", idsOf(res))
	}
	for _, r := range res {
		if r.Memory.Namespace != ns {
			t.Fatalf("namespace leak: %s", r.Memory.Namespace)
		}
	}
}

func testKeyword(t *testing.T, st store.Store, dims int) {
	ctx := context.Background()
	ns := t.Name()
	mustUpsert(t, st, mem(ns, "a", "the cat sat on the mat", vec(dims, 1)))
	mustUpsert(t, st, mem(ns, "b", "dogs are loyal animals", vec(dims, 0, 1)))
	mustUpsert(t, st, mem(ns, "c", "felines and cats love naps", vec(dims, 0.9, 0.1)))

	res, err := st.KeywordSearch(ctx, ns, "cats", store.Filter{}, 10)
	if err != nil {
		t.Fatalf("keyword search: %v", err)
	}
	// Backends differ on stemming, but "c" must match and "dogs" (b) must not.
	got := idsOf(res)
	if !slices.Contains(got, id(ns, "c")) {
		t.Fatalf("expected %q in keyword results, got %v", id(ns, "c"), got)
	}
	if slices.Contains(got, id(ns, "b")) {
		t.Fatalf("did not expect %q (dogs) in results for 'cats', got %v", id(ns, "b"), got)
	}
}

func testFilters(t *testing.T, st store.Store, dims int) {
	ctx := context.Background()
	ns := t.Name()

	past := time.Now().Add(-time.Hour)
	expired := mem(ns, "exp", "stale fact", vec(dims, 1))
	expired.ExpiresAt = &past
	mustUpsert(t, st, expired)

	live := mem(ns, "live", "fresh fact", vec(dims, 1))
	mustUpsert(t, st, live)

	target := id(ns, "live")
	superseded := mem(ns, "old", "outdated fact", vec(dims, 1))
	superseded.SupersededBy = &target
	mustUpsert(t, st, superseded)

	res, err := st.VectorSearch(ctx, ns, vec(dims, 1), store.Filter{}, 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if got := idsOf(res); len(got) != 1 || got[0] != id(ns, "live") {
		t.Fatalf("default filter should yield only %q, got %v", id(ns, "live"), got)
	}

	res, err = st.VectorSearch(ctx, ns, vec(dims, 1),
		store.Filter{IncludeExpired: true, IncludeSuperseded: true}, 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(res) != 3 {
		t.Fatalf("inclusive filter should yield 3, got %v", idsOf(res))
	}

	exp, err := st.ListExpired(ctx, time.Now(), 100)
	if err != nil {
		t.Fatalf("list expired: %v", err)
	}
	if !containsMem(exp, id(ns, "exp")) {
		t.Fatalf("ListExpired should include %q, got %v", id(ns, "exp"), memIDs(exp))
	}
}

// scoredIDs returns memory IDs from a Scored slice.
func scoredIDs(res []store.Scored) []string {
	ids := make([]string, len(res))
	for i, r := range res {
		ids[i] = r.Memory.ID
	}
	return ids
}

// testLevelFilter verifies that Filter.Levels restricts results to memories
// whose derivation level matches one of the listed values; empty means no
// constraint.
func testLevelFilter(t *testing.T, st store.Store, dims int) {
	ctx := context.Background()
	ns := t.Name()

	// Insert three memories with different levels.
	explicit := mem(ns, "exp", "user stated this directly", vec(dims, 1))
	explicit.Level = memory.LevelExplicit
	mustUpsert(t, st, explicit)

	deduced := mem(ns, "ded", "LLM distilled this fact", vec(dims, 2))
	deduced.Level = memory.LevelDeduced
	mustUpsert(t, st, deduced)

	unnamed := mem(ns, "unl", "no level set (legacy)", vec(dims, 3))
	// Level is empty string (zero value).
	mustUpsert(t, st, unnamed)

	// Empty level filter matches all three (no constraint).
	all := mustList(t, st, ns, store.Filter{Levels: []memory.Level{}})
	if len(all) != 3 {
		t.Fatalf("empty levels filter should yield 3, got %d", len(all))
	}

	// Filter to explicit only.
	expOnly := mustSearch(t, st, ns, vec(dims, 1), store.Filter{Levels: []memory.Level{memory.LevelExplicit}}, 10)
	if len(expOnly) != 1 || scoredIDs(expOnly)[0] != id(ns, "exp") {
		t.Fatalf("level=explicit should yield exp only, got %v", scoredIDs(expOnly))
	}

	// Filter to deduced only.
	dedOnly := mustSearch(t, st, ns, vec(dims, 2), store.Filter{Levels: []memory.Level{memory.LevelDeduced}}, 10)
	if len(dedOnly) != 1 || scoredIDs(dedOnly)[0] != id(ns, "ded") {
		t.Fatalf("level=deduced should yield ded only, got %v", scoredIDs(dedOnly))
	}

	// Multi-level filter: explicit + deduced (still excludes unnamed).
	multi := mustSearch(t, st, ns, vec(dims, 1), store.Filter{Levels: []memory.Level{memory.LevelExplicit, memory.LevelDeduced}}, 10)
	if len(multi) != 2 {
		t.Fatalf("level=explicit+deduced should yield 2, got %d", len(multi))
	}

	// VectorSearch with level filter.
	vRes, err := st.VectorSearch(ctx, ns, vec(dims, 1), store.Filter{Levels: []memory.Level{memory.LevelExplicit}}, 10)
	if err != nil {
		t.Fatalf("VectorSearch level filter: %v", err)
	}
	if len(vRes) != 1 || scoredIDs(vRes)[0] != id(ns, "exp") {
		t.Fatalf("VectorSearch level=explicit should yield exp only, got %v", scoredIDs(vRes))
	}

	// KeywordSearch with level filter.
	kRes, err := st.KeywordSearch(ctx, ns, "distilled", store.Filter{Levels: []memory.Level{memory.LevelDeduced}}, 10)
	if err != nil {
		t.Fatalf("KeywordSearch level filter: %v", err)
	}
	if len(kRes) != 1 || scoredIDs(kRes)[0] != id(ns, "ded") {
		t.Fatalf("KeywordSearch level=deduced should yield ded only, got %v", scoredIDs(kRes))
	}
}

// mustSearch is a helper for testLevelFilter that calls VectorSearch and
// fatals on error.
func mustSearch(t *testing.T, st store.Store, ns string, vec []float32, f store.Filter, k int) []store.Scored {
	t.Helper()
	res, err := st.VectorSearch(context.Background(), ns, vec, f, k)
	if err != nil {
		t.Fatalf("VectorSearch: %v", err)
	}
	return res
}

// testTagMetadataFilter verifies that Filter.Tags (AND semantics) and
// Filter.Metadata (top-level key=value) narrow List, VectorSearch and
// KeywordSearch across backends.
func testTagMetadataFilter(t *testing.T, st store.Store, dims int) {
	ctx := context.Background()
	ns := t.Name()
	// Tokens reused as both memory ids and tags; consts keep goconst quiet.
	const (
		tagAuth = "auth"
		bug     = "bug"
		perf    = "perf"
		keyCat  = "category"
		catBug  = "bug_fixes"
	)

	bm := mem(ns, bug, "fixed the auth race condition", vec(dims, 1))
	bm.Tags = []string{bug, tagAuth}
	bm.Metadata = map[string]any{keyCat: catBug}
	mustUpsert(t, st, bm)

	pm := mem(ns, perf, "auth handler latency tuning", vec(dims, 1))
	pm.Tags = []string{perf, tagAuth}
	pm.Metadata = map[string]any{keyCat: "performance_findings"}
	mustUpsert(t, st, pm)

	plain := mem(ns, "plain", "unrelated note about auth", vec(dims, 1))
	mustUpsert(t, st, plain)

	// Single tag matches every memory carrying it.
	byTag := mustList(t, st, ns, store.Filter{Tags: []string{tagAuth}})
	if got := memIDs(byTag); len(got) != 2 || !containsMem(byTag, id(ns, bug)) {
		t.Fatalf("tag=auth should yield bug+perf, got %v", got)
	}

	// Multiple tags are ANDed.
	got := memIDs(mustList(t, st, ns, store.Filter{Tags: []string{tagAuth, bug}}))
	if len(got) != 1 || got[0] != id(ns, bug) {
		t.Fatalf("tags=auth+bug should yield only bug, got %v", got)
	}

	// Metadata key=value narrows to the matching category.
	got = memIDs(mustList(t, st, ns, store.Filter{Metadata: map[string]string{keyCat: "bug_fixes"}}))
	if len(got) != 1 || got[0] != id(ns, bug) {
		t.Fatalf("category=bug_fixes should yield only bug, got %v", got)
	}

	// ExcludeMetadata drops the matching category, keeping the rest.
	excluded := mustList(t, st, ns, store.Filter{ExcludeMetadata: map[string]string{keyCat: "bug_fixes"}})
	if len(excluded) != 2 || containsMem(excluded, id(ns, bug)) {
		t.Fatalf("exclude category=bug_fixes should drop only bug, got %v", memIDs(excluded))
	}

	// Tag + metadata filters compose on search legs too.
	f := store.Filter{Tags: []string{perf}, Metadata: map[string]string{keyCat: "performance_findings"}}
	vres, err := st.VectorSearch(ctx, ns, vec(dims, 1), f, 10)
	if err != nil {
		t.Fatalf("vector search: %v", err)
	}
	if ids := idsOf(vres); len(ids) != 1 || ids[0] != id(ns, perf) {
		t.Fatalf("filtered vector search should yield only perf, got %v", ids)
	}
	kres, err := st.KeywordSearch(ctx, ns, tagAuth, store.Filter{Tags: []string{bug}}, 10)
	if err != nil {
		t.Fatalf("keyword search: %v", err)
	}
	if ids := idsOf(kres); len(ids) != 1 || ids[0] != id(ns, bug) {
		t.Fatalf("filtered keyword search should yield only bug, got %v", ids)
	}
}

// testExcludeMetadataFilter verifies Filter.ExcludeMetadata drops memories
// carrying any listed key=value pair (the inverse of Metadata) across List,
// VectorSearch and KeywordSearch — the mechanism the OpenClaw plugin uses to
// keep a session from recalling its own just-captured turns.
func testExcludeMetadataFilter(t *testing.T, st store.Store, dims int) {
	ctx := context.Background()
	ns := t.Name()
	const keySession = "session"

	mine := mem(ns, "mine", "deploy notes for the auth service", vec(dims, 1))
	mine.Metadata = map[string]any{keySession: "s1"}
	mustUpsert(t, st, mine)

	other := mem(ns, "other", "deploy notes for the auth service", vec(dims, 1))
	other.Metadata = map[string]any{keySession: "s2"}
	mustUpsert(t, st, other)

	untagged := mem(ns, "untagged", "deploy notes for the auth service", vec(dims, 1))
	mustUpsert(t, st, untagged)

	// Excluding session s1 drops only the s1 capture; s2 and untagged remain.
	exclude := store.Filter{ExcludeMetadata: map[string]string{keySession: "s1"}}
	got := memIDs(mustList(t, st, ns, exclude))
	if len(got) != 2 || slices.Contains(got, id(ns, "mine")) {
		t.Fatalf("exclude session=s1 should yield other+untagged, got %v", got)
	}

	vres, err := st.VectorSearch(ctx, ns, vec(dims, 1), exclude, 10)
	if err != nil {
		t.Fatalf("vector search: %v", err)
	}
	if ids := idsOf(vres); slices.Contains(ids, id(ns, "mine")) || len(ids) != 2 {
		t.Fatalf("filtered vector search should drop the s1 capture, got %v", ids)
	}

	kres, err := st.KeywordSearch(ctx, ns, "deploy", exclude, 10)
	if err != nil {
		t.Fatalf("keyword search: %v", err)
	}
	if ids := idsOf(kres); slices.Contains(ids, id(ns, "mine")) || len(ids) != 2 {
		t.Fatalf("filtered keyword search should drop the s1 capture, got %v", ids)
	}
}

// testExcludeIDsFilter verifies Filter.ExcludeIDs drops the listed ids across
// List, VectorSearch and KeywordSearch — and that the exclusion happens before
// the limit, so an excluded hit frees its result slot for the next-best match
// (the property a client-side post-filter cannot have).
func testExcludeIDsFilter(t *testing.T, st store.Store, dims int) {
	ctx := context.Background()
	ns := t.Name()

	mustUpsert(t, st, mem(ns, "a", "the cat sat on the mat", vec(dims, 1)))
	mustUpsert(t, st, mem(ns, "b", "dogs are loyal animals", vec(dims, 0, 1)))
	mustUpsert(t, st, mem(ns, "c", "felines love naps", vec(dims, 0.9, 0.1)))

	exclude := store.Filter{ExcludeIDs: []string{id(ns, "a"), id(ns, "b")}}
	got := memIDs(mustList(t, st, ns, exclude))
	if len(got) != 1 || got[0] != id(ns, "c") {
		t.Fatalf("exclude a+b should yield only c, got %v", got)
	}

	// "a" is the top-ranked hit for this query; excluding it must surface the
	// next-best ("c") within the same k=1 budget rather than returning nothing.
	vres, err := st.VectorSearch(ctx, ns, vec(dims, 1), store.Filter{ExcludeIDs: []string{id(ns, "a")}}, 1)
	if err != nil {
		t.Fatalf("vector search: %v", err)
	}
	if ids := idsOf(vres); len(ids) != 1 || ids[0] != id(ns, "c") {
		t.Fatalf("excluding the top hit should free its slot for c, got %v", ids)
	}

	kres, err := st.KeywordSearch(ctx, ns, "cat", store.Filter{ExcludeIDs: []string{id(ns, "a")}}, 10)
	if err != nil {
		t.Fatalf("keyword search: %v", err)
	}
	if ids := idsOf(kres); slices.Contains(ids, id(ns, "a")) {
		t.Fatalf("filtered keyword search should drop a, got %v", ids)
	}

	// An empty list is a no-op, not an exclude-everything.
	all := memIDs(mustList(t, st, ns, store.Filter{ExcludeIDs: []string{}}))
	if len(all) != 3 {
		t.Fatalf("empty ExcludeIDs should match all 3 memories, got %v", all)
	}
}

func mustList(t *testing.T, st store.Store, ns string, f store.Filter) []*memory.Memory {
	t.Helper()
	ms, err := st.List(context.Background(), ns, f, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	return ms
}

func testSetSuperseded(t *testing.T, st store.Store, dims int) {
	ctx := context.Background()
	ns := t.Name()
	mustUpsert(t, st, mem(ns, "old", "the sky is green", vec(dims, 1)))
	mustUpsert(t, st, mem(ns, "new", "the sky is blue", vec(dims, 1)))

	if err := st.SetSuperseded(ctx, ns, id(ns, "old"), id(ns, "new")); err != nil {
		t.Fatalf("set superseded: %v", err)
	}
	got, err := st.Get(ctx, ns, id(ns, "old"))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.SupersededBy == nil || *got.SupersededBy != id(ns, "new") {
		t.Fatalf("superseded_by not set: %v", got.SupersededBy)
	}

	res, err := st.VectorSearch(ctx, ns, vec(dims, 1), store.Filter{}, 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if slices.Contains(idsOf(res), id(ns, "old")) {
		t.Fatalf("superseded memory should be excluded by default, got %v", idsOf(res))
	}

	if err := st.SetSuperseded(ctx, ns, id(ns, "missing"), id(ns, "new")); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("set superseded on missing: want ErrNotFound, got %v", err)
	}
}

func testPredecessorIDs(t *testing.T, st store.Store, dims int) {
	ctx := context.Background()
	ns := t.Name()
	mustUpsert(t, st, mem(ns, "v1", "draft one", vec(dims, 1)))
	mustUpsert(t, st, mem(ns, "v2", "draft two", vec(dims, 1)))
	mustUpsert(t, st, mem(ns, "v3", "draft three", vec(dims, 1)))
	// A merge: both v1 and v2 are superseded by v3.
	if err := st.SetSuperseded(ctx, ns, id(ns, "v1"), id(ns, "v3")); err != nil {
		t.Fatalf("supersede v1: %v", err)
	}
	if err := st.SetSuperseded(ctx, ns, id(ns, "v2"), id(ns, "v3")); err != nil {
		t.Fatalf("supersede v2: %v", err)
	}

	preds, err := st.PredecessorIDs(ctx, ns, id(ns, "v3"))
	if err != nil {
		t.Fatalf("predecessor ids: %v", err)
	}
	slices.Sort(preds)
	want := []string{id(ns, "v1"), id(ns, "v2")}
	slices.Sort(want)
	if !slices.Equal(preds, want) {
		t.Fatalf("predecessors of v3 = %v, want %v", preds, want)
	}

	// A leaf nothing supersedes has no predecessors.
	if got, err := st.PredecessorIDs(ctx, ns, id(ns, "v1")); err != nil || len(got) != 0 {
		t.Fatalf("predecessors of v1 = %v, %v; want empty", got, err)
	}
}

func testRestore(t *testing.T, st store.Store, dims int) {
	ctx := context.Background()
	ns := t.Name()
	mustUpsert(t, st, mem(ns, "old", "the sky is green", vec(dims, 1)))
	mustUpsert(t, st, mem(ns, "new", "the sky is blue", vec(dims, 1)))
	if err := st.SetSuperseded(ctx, ns, id(ns, "old"), id(ns, "new")); err != nil {
		t.Fatalf("set superseded: %v", err)
	}

	if err := st.Restore(ctx, ns, id(ns, "old")); err != nil {
		t.Fatalf("restore: %v", err)
	}
	got, err := st.Get(ctx, ns, id(ns, "old"))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.SupersededBy != nil {
		t.Fatalf("superseded_by not cleared: %v", got.SupersededBy)
	}
	if got.ValidTo != nil {
		t.Fatalf("valid_to not cleared: %v", got.ValidTo)
	}

	res, err := st.VectorSearch(ctx, ns, vec(dims, 1), store.Filter{}, 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if !slices.Contains(idsOf(res), id(ns, "old")) {
		t.Fatalf("restored memory should be searchable again, got %v", idsOf(res))
	}

	if err := st.Restore(ctx, ns, id(ns, "missing")); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("restore on missing: want ErrNotFound, got %v", err)
	}
}

func testReinforce(t *testing.T, st store.Store, dims int) {
	ctx := context.Background()
	ns := t.Name()
	now := time.Now().UTC().Truncate(time.Millisecond)
	exp := now.Add(time.Hour)

	short := mem(ns, "short", "transient", vec(dims, 1))
	short.Tier = memory.TierWorking
	short.ExpiresAt = &exp
	mustUpsert(t, st, short)

	long := mem(ns, "long", "durable", vec(dims, 1)) // semantic, no expiry
	mustUpsert(t, st, long)

	accessed := now.Add(30 * time.Minute)
	slid := accessed.Add(time.Hour)
	if err := st.Reinforce(ctx, ns, []string{id(ns, "short"), id(ns, "long")}, accessed, &slid); err != nil {
		t.Fatalf("reinforce: %v", err)
	}

	gotShort, _ := st.Get(ctx, ns, id(ns, "short"))
	gotLong, _ := st.Get(ctx, ns, id(ns, "long"))
	if gotShort.AccessCount != 1 || gotLong.AccessCount != 1 {
		t.Fatalf("access_count not bumped: short=%d long=%d", gotShort.AccessCount, gotLong.AccessCount)
	}
	if !gotShort.LastAccessedAt.Equal(accessed) {
		t.Fatalf("last_accessed_at = %v, want %v", gotShort.LastAccessedAt, accessed)
	}
	if gotShort.ExpiresAt == nil || !gotShort.ExpiresAt.Equal(slid) {
		t.Fatalf("short-term TTL not slid: %v, want %v", gotShort.ExpiresAt, slid)
	}
	if gotLong.ExpiresAt != nil {
		t.Fatalf("durable memory must not gain an expiry, got %v", gotLong.ExpiresAt)
	}
	if err := st.Reinforce(ctx, ns, []string{id(ns, "missing")}, accessed, nil); err != nil {
		t.Fatalf("reinforce of missing id should be a no-op, got %v", err)
	}
}

func testDeleteIfExpiredBefore(t *testing.T, st store.Store, dims int) {
	ctx := context.Background()
	ns := t.Name()
	now := time.Now().UTC()

	past := now.Add(-time.Hour)
	future := now.Add(time.Hour)

	expired := mem(ns, "exp", "stale", vec(dims, 1))
	expired.ExpiresAt = &past
	mustUpsert(t, st, expired)

	live := mem(ns, "live", "fresh", vec(dims, 0, 1))
	mustUpsert(t, st, live)

	// Cutoff older than the expiry: memory is not yet considered expired at that time.
	if err := st.DeleteIfExpiredBefore(ctx, ns, id(ns, "exp"), past.Add(-time.Minute)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expired-before older cutoff: want ErrNotFound, got %v", err)
	}

	// A memory without an expiry must not be deleted.
	if err := st.DeleteIfExpiredBefore(ctx, ns, id(ns, "live"), future); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("durable memory: want ErrNotFound, got %v", err)
	}

	// Actually-expired memory is deleted.
	if err := st.DeleteIfExpiredBefore(ctx, ns, id(ns, "exp"), now); err != nil {
		t.Fatalf("delete expired: %v", err)
	}
	if _, err := st.Get(ctx, ns, id(ns, "exp")); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("memory should be gone, got %v", err)
	}

	// Idempotent: deleting again returns ErrNotFound.
	if err := st.DeleteIfExpiredBefore(ctx, ns, id(ns, "exp"), now); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("double delete: want ErrNotFound, got %v", err)
	}
}

func idsOf(res []store.Scored) []string {
	out := make([]string, len(res))
	for i, r := range res {
		out[i] = r.Memory.ID
	}
	return out
}

func memIDs(ms []*memory.Memory) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.ID
	}
	return out
}

func containsMem(ms []*memory.Memory, id string) bool {
	for _, m := range ms {
		if m.ID == id {
			return true
		}
	}
	return false
}

// testKeywordHostileQueries pins that keyword search treats user queries as
// data: FTS/tsquery operators, quotes, and non-ASCII input must never produce
// a syntax error from the underlying engine. Hit counts are backend-specific
// (the sqlite tokenizer drops non-ASCII; postgres stems it), so only the
// no-error contract is asserted here.
func testKeywordHostileQueries(t *testing.T, st store.Store, dims int) {
	ctx := context.Background()
	const ns = "hostile-queries"
	mustUpsert(t, st, mem(ns, "a", "plain ascii content about cats", vec(dims, 1)))

	queries := []string{
		`NEAR(foo bar)`,
		`"quoted phrase"`,
		`col:value AND x* OR (y)`,
		`cat's -toy`,
		"東京タワー",
		`café naïve`,
		`); DROP TABLE memories; --`,
	}
	for _, q := range queries {
		if _, err := st.KeywordSearch(ctx, ns, q, store.Filter{}, 5); err != nil {
			t.Errorf("KeywordSearch(%q) errored: %v", q, err)
		}
	}

	// A sane query must still match — guards against a sanitizer that starts
	// neutralizing everything into zero results.
	res, err := st.KeywordSearch(ctx, ns, "cats", store.Filter{}, 5)
	if err != nil {
		t.Fatalf("KeywordSearch(cats): %v", err)
	}
	if !slices.Contains(idsOf(res), id(ns, "a")) {
		t.Fatalf("plain query no longer matches after hostile queries: %v", idsOf(res))
	}
}

// testFilterNow pins that expiry filtering honors Filter.Now (the caller's
// injected clock) instead of the wall clock, and that the zero value falls
// back to time.Now(). The expiry clause is duplicated per backend, so this
// runs through the conformance suite to keep them in lockstep.
func testFilterNow(t *testing.T, st store.Store, dims int) {
	ctx := context.Background()
	const ns = "filter-now"
	base := time.Now().UTC().Truncate(time.Millisecond)
	expiry := base.Add(time.Hour)

	m := mem(ns, "ttl", "fact with a one hour ttl", vec(dims, 1))
	m.ExpiresAt = &expiry
	mustUpsert(t, st, m)

	list := func(f store.Filter) []*memory.Memory {
		t.Helper()
		mems, err := st.List(ctx, ns, f, 0)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		return mems
	}

	if got := list(store.Filter{Now: base}); len(got) != 1 {
		t.Fatalf("before expiry (Now=base): want 1 memory, got %d", len(got))
	}
	if got := list(store.Filter{Now: base.Add(2 * time.Hour)}); len(got) != 0 {
		t.Fatalf("after expiry (Now=base+2h): want 0 memories, got %d", len(got))
	}
	if got := list(store.Filter{Now: base.Add(2 * time.Hour), IncludeExpired: true}); len(got) != 1 {
		t.Fatalf("IncludeExpired after expiry: want 1 memory, got %d", len(got))
	}
	// Zero Now falls back to the wall clock, which is well before the expiry.
	if got := list(store.Filter{}); len(got) != 1 {
		t.Fatalf("zero Now (wall clock, before expiry): want 1 memory, got %d", len(got))
	}

	// Search legs honor it too.
	res, err := st.VectorSearch(ctx, ns, vec(dims, 1), store.Filter{Now: base.Add(2 * time.Hour)}, 5)
	if err != nil {
		t.Fatalf("vector search: %v", err)
	}
	if len(res) != 0 {
		t.Fatalf("vector search after expiry: want 0, got %d", len(res))
	}
}

// testConcurrentAccess hammers one namespace from several goroutines with
// mixed reads, writes, reinforcement, and deletes. It asserts no operation
// fails (beyond ErrNotFound on a racing delete) and exists chiefly so the
// -race runs in CI exercise real store concurrency: sqlite's single-writer
// handling (busy_timeout) and the pgx pool both get contention here.
func testConcurrentAccess(t *testing.T, st store.Store, dims int) {
	ctx := context.Background()
	const ns = "concurrent"
	const workers, iters = 8, 25

	var wg sync.WaitGroup
	// Each iteration can emit up to 5 errors; size the channel for the worst
	// case so a pathologically failing store fails loudly instead of blocking
	// the senders and timing the test out.
	errs := make(chan error, workers*iters*5)
	for w := range workers {
		wg.Go(func() {
			for i := range iters {
				short := fmt.Sprintf("w%d-i%d", w, i)
				m := mem(ns, short, fmt.Sprintf("memory %s about shared topic", short), vec(dims, float32(w), float32(i)))
				if err := st.Upsert(ctx, m); err != nil {
					errs <- fmt.Errorf("upsert %s: %w", short, err)
					continue
				}
				if _, err := st.VectorSearch(ctx, ns, vec(dims, float32(w)), store.Filter{}, 5); err != nil {
					errs <- fmt.Errorf("vector search: %w", err)
				}
				if _, err := st.KeywordSearch(ctx, ns, "shared topic", store.Filter{}, 5); err != nil {
					errs <- fmt.Errorf("keyword search: %w", err)
				}
				if err := st.Reinforce(ctx, ns, []string{m.ID}, time.Now().UTC(), nil); err != nil {
					errs <- fmt.Errorf("reinforce: %w", err)
				}
				if _, err := st.Get(ctx, ns, m.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
					errs <- fmt.Errorf("get: %w", err)
				}
				// Delete every third memory so reads race tombstoned rows too.
				if i%3 == 0 {
					if err := st.Delete(ctx, ns, m.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
						errs <- fmt.Errorf("delete: %w", err)
					}
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	// The store must still be coherent: a full list succeeds and contains
	// exactly the non-deleted writes.
	mems, err := st.List(ctx, ns, store.Filter{}, 0)
	if err != nil {
		t.Fatalf("list after hammer: %v", err)
	}
	deletedPerWorker := (iters + 2) / 3 // i%3==0 for i in [0,iters)
	want := workers * (iters - deletedPerWorker)
	if len(mems) != want {
		t.Fatalf("list = %d memories, want %d", len(mems), want)
	}
}

// testAPIKeys exercises the optional APIKeyStore capability: put/get-by-hash
// round-trip (including HomeNS and Disabled), upsert-by-name preserving
// CreatedAt when the incoming CreatedAt is zero (NOTE: this deliberately
// differs from namespace_links' PutLink, which resets created_at on every
// upsert — see store.APIKeyStore.PutAPIKey's doc for why), DeleteAPIKey's
// existed-bool return, GetAPIKeyByHash absent -> nil,nil, ListAPIKeys
// ordered by name, and a duplicate hash under a different name surfacing as
// an error (the unique constraint on key_hash). Stores that do not
// implement APIKeyStore skip.
func testAPIKeys(t *testing.T, st store.Store, dims int) {
	_ = dims // keys carry no embedding; kept for signature parity with the other subtests
	ks, ok := st.(store.APIKeyStore)
	if !ok {
		t.Skip("store does not implement store.APIKeyStore")
	}
	ns := t.Name()
	t.Run("PutGetByHashRoundTrip", func(t *testing.T) { testAPIKeyRoundTrip(t, ks, ns) })
	t.Run("UpsertPreservesCreatedAt", func(t *testing.T) { testAPIKeyUpsertPreservesCreatedAt(t, ks, ns) })
	t.Run("DeleteExistedBool", func(t *testing.T) { testAPIKeyDelete(t, ks, ns) })
	t.Run("GetByHashAbsent", func(t *testing.T) { testAPIKeyGetByHashAbsent(t, ks, ns) })
	t.Run("ListOrderedByName", func(t *testing.T) { testAPIKeyListOrdered(t, ks, ns) })
	t.Run("DuplicateHashDifferentNameErrors", func(t *testing.T) { testAPIKeyDuplicateHash(t, ks, ns) })
	t.Run("DefaultNSRoundTrip", func(t *testing.T) { testAPIKeyDefaultNSRoundTrip(t, ks, ns) })
	t.Run("RenameNamespaces", func(t *testing.T) { testAPIKeyRenameNamespaces(t, ks, ns) })
	for _, f := range apiKeyFlags {
		t.Run(f.Label+"RoundTrip", func(t *testing.T) { testAPIKeyFlagRoundTrip(t, ks, ns, f) })
		t.Run(f.Label+"SurvivesRenameNamespaces", func(t *testing.T) { testAPIKeyFlagSurvivesRename(t, ks, ns, f) })
		t.Run(f.Label+"PreservedOnUpsertWithZeroCreatedAt", func(t *testing.T) { testAPIKeyFlagPreservedOnUpsert(t, ks, ns, f) })
	}
	t.Run("ReadOnlyIndependentOfAdmin", func(t *testing.T) { testAPIKeyReadOnlyIndependentOfAdmin(t, ks, ns) })
}

// apiKeyFlag describes one boolean capability column on store.APIKey. The
// storage contract for every such bit is identical — round-trip through
// Put/Get/List, survive a namespace rename, survive a zero-CreatedAt upsert
// (rotation) in both directions — so the coverage is written once here and run
// per flag rather than copy-pasted per flag. A future third bit gets the full
// suite by appending one entry.
type apiKeyFlag struct {
	// Label names the field in failure messages and prefixes the sub-test
	// name, so changing it renames the sub-test.
	Label string
	// Slug is a filename-safe token mixed into generated key names so two
	// flags' fixtures cannot collide within one namespace.
	Slug string
	Set  func(*store.APIKey, bool)
	Get  func(store.APIKey) bool
}

// apiKeyFlags is every boolean capability the store must persist. Admin gates
// the /v1/keys and /v1/settings/defaults surfaces; ReadOnly denies mutation
// across both HTTP surfaces. They are independent — see
// testAPIKeyReadOnlyIndependentOfAdmin.
var apiKeyFlags = []apiKeyFlag{
	{
		Label: "Admin", Slug: "admin",
		Set: func(k *store.APIKey, v bool) { k.Admin = v },
		Get: func(k store.APIKey) bool { return k.Admin },
	},
	{
		Label: "ReadOnly", Slug: "readonly",
		Set: func(k *store.APIKey, v bool) { k.ReadOnly = v },
		Get: func(k store.APIKey) bool { return k.ReadOnly },
	},
}

// testAPIKeyFlagRoundTrip covers f round-tripping through
// PutAPIKey/GetAPIKeyByHash/ListAPIKeys for both values — true and false (the
// default, the sibling case to Disabled's own false-by-default round trip).
func testAPIKeyFlagRoundTrip(t *testing.T, ks store.APIKeyStore, ns string, f apiKeyFlag) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)

	names := map[bool]string{true: ns + "-" + f.Slug + "-true", false: ns + "-" + f.Slug + "-false"}
	for _, want := range []bool{true, false} {
		k := store.APIKey{Name: names[want], Hash: apiKeyHash(names[want]), CreatedAt: now}
		f.Set(&k, want)
		if err := ks.PutAPIKey(ctx, k); err != nil {
			t.Fatalf("put %s=%v: %v", f.Label, want, err)
		}
		got, err := ks.GetAPIKeyByHash(ctx, k.Hash)
		if err != nil {
			t.Fatalf("get by hash (%s=%v): %v", f.Label, want, err)
		}
		if got == nil || f.Get(*got) != want {
			t.Fatalf("%s=%v round-trip = %+v, want %s=%v", f.Label, want, got, f.Label, want)
		}
	}

	// ListAPIKeys carries it too.
	all, err := ks.ListAPIKeys(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	seen := map[bool]bool{}
	for _, k := range all {
		for _, want := range []bool{true, false} {
			if k.Name != names[want] {
				continue
			}
			seen[want] = true
			if f.Get(k) != want {
				t.Fatalf("list: %q %s = %v, want %v", k.Name, f.Label, f.Get(k), want)
			}
		}
	}
	if !seen[true] || !seen[false] {
		t.Fatalf("list missing one of the %s round-trip keys: %+v", f.Label, all)
	}
}

// testAPIKeyFlagSurvivesRename covers f surviving RenameAPIKeyNamespaces
// (which only touches HomeNS/DefaultNS) — the same precedent as
// testAPIKeySettingsSurviveRename for the per-key Settings override. A rename
// that silently cleared ReadOnly would hand a CI credential write access.
func testAPIKeyFlagSurvivesRename(t *testing.T, ks store.APIKeyStore, ns string, f apiKeyFlag) {
	ctx := context.Background()
	name := ns + "-" + f.Slug + "-rename"
	from := name + "-old"
	to := name + "-new"
	k := store.APIKey{
		Name:      name,
		Hash:      apiKeyHash(name),
		HomeNS:    from,
		CreatedAt: time.Now().UTC().Truncate(time.Millisecond),
	}
	f.Set(&k, true)
	if err := ks.PutAPIKey(ctx, k); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := ks.RenameAPIKeyNamespaces(ctx, from, to); err != nil {
		t.Fatalf("rename: %v", err)
	}
	got, err := ks.GetAPIKeyByHash(ctx, k.Hash)
	if err != nil {
		t.Fatalf("get by hash after rename: %v", err)
	}
	if got == nil {
		t.Fatalf("key vanished after rename")
	}
	if got.HomeNS != to {
		t.Fatalf("home_ns after rename = %q, want %q", got.HomeNS, to)
	}
	if !f.Get(*got) {
		t.Fatalf("%s did not survive rename: %+v, want %s=true", f.Label, got, f.Label)
	}
}

// testAPIKeyFlagPreservedOnUpsert covers f round-tripping through the
// upsert-with-zero-CreatedAt path (rotation): the manual
// read-then-conditional-write CreatedAt-preserve logic must not accidentally
// drop the column from the INSERT/ON CONFLICT list, in either direction, while
// CreatedAt itself is preserved from the original row. A rotation that dropped
// ReadOnly would silently promote a read-only credential to read-write.
func testAPIKeyFlagPreservedOnUpsert(t *testing.T, ks store.APIKeyStore, ns string, f apiKeyFlag) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)

	for _, tc := range []struct {
		suffix   string
		from, to bool
	}{
		{"a", false, true},
		{"b", true, false},
	} {
		name := ns + "-" + f.Slug + "-upsert-" + tc.suffix
		original := store.APIKey{Name: name, Hash: apiKeyHash(name + "-v1"), CreatedAt: now}
		f.Set(&original, tc.from)
		if err := ks.PutAPIKey(ctx, original); err != nil {
			t.Fatalf("put original (%s): %v", tc.suffix, err)
		}
		// Zero CreatedAt is what rotation writes: the driver must look the
		// existing row's timestamp up rather than stamping a new one.
		rotated := store.APIKey{Name: name, Hash: apiKeyHash(name + "-v2")}
		f.Set(&rotated, tc.to)
		if err := ks.PutAPIKey(ctx, rotated); err != nil {
			t.Fatalf("put rotated (%s): %v", tc.suffix, err)
		}
		got, err := ks.GetAPIKeyByHash(ctx, rotated.Hash)
		if err != nil {
			t.Fatalf("get by hash after rotation (%s): %v", tc.suffix, err)
		}
		if got == nil || f.Get(*got) != tc.to {
			t.Fatalf("%s after %v->%v upsert = %+v, want %s=%v", f.Label, tc.from, tc.to, got, f.Label, tc.to)
		}
		if !got.CreatedAt.Equal(now) {
			t.Fatalf("created_at after rotation (%s) = %v, want preserved %v", tc.suffix, got.CreatedAt, now)
		}
	}
}

// testAPIKeyReadOnlyIndependentOfAdmin pins that ReadOnly and Admin are two
// independent bits rather than one collapsed capability: admin+read_only is a
// legitimate combination (a read-only auditor who can still enumerate keys),
// so a column mix-up that aliased one onto the other must fail here.
func testAPIKeyReadOnlyIndependentOfAdmin(t *testing.T, ks store.APIKeyStore, ns string) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	for _, tc := range []struct {
		label           string
		admin, readOnly bool
	}{
		{"admin-rw", true, false},
		{"admin-ro", true, true},
		{"named-rw", false, false},
		{"named-ro", false, true},
	} {
		name := ns + "-combo-" + tc.label
		k := store.APIKey{
			Name: name, Hash: apiKeyHash(name), CreatedAt: now,
			Admin: tc.admin, ReadOnly: tc.readOnly,
		}
		if err := ks.PutAPIKey(ctx, k); err != nil {
			t.Fatalf("%s: put: %v", tc.label, err)
		}
		got, err := ks.GetAPIKeyByHash(ctx, k.Hash)
		if err != nil {
			t.Fatalf("%s: get by hash: %v", tc.label, err)
		}
		if got == nil {
			t.Fatalf("%s: key not found after put", tc.label)
		}
		if got.Admin != tc.admin || got.ReadOnly != tc.readOnly {
			t.Fatalf("%s: got Admin=%v ReadOnly=%v, want Admin=%v ReadOnly=%v",
				tc.label, got.Admin, got.ReadOnly, tc.admin, tc.readOnly)
		}
	}
}

// testAPIKeyDefaultNSRoundTrip covers PutAPIKey/GetAPIKeyByHash/ListAPIKeys
// round-tripping DefaultNS (the per-key default namespace applied when a
// request carries no X-Memini-Namespace header; an explicit header wins),
// including updating and clearing it on upsert.
func testAPIKeyDefaultNSRoundTrip(t *testing.T, ks store.APIKeyStore, ns string) {
	ctx := context.Background()
	name := ns + "-defns"
	now := time.Now().UTC().Truncate(time.Millisecond)
	k := store.APIKey{
		Name:      name,
		Hash:      apiKeyHash(name),
		HomeNS:    ns + "-home",
		DefaultNS: ns + "-default",
		CreatedAt: now,
	}
	if err := ks.PutAPIKey(ctx, k); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := ks.GetAPIKeyByHash(ctx, k.Hash)
	if err != nil {
		t.Fatalf("get by hash: %v", err)
	}
	if got == nil || got.DefaultNS != k.DefaultNS {
		t.Fatalf("default_ns round-trip = %+v, want DefaultNS %q", got, k.DefaultNS)
	}
	// ListAPIKeys carries it too.
	all, err := ks.ListAPIKeys(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	found := false
	for _, l := range all {
		if l.Name == name {
			found = true
			if l.DefaultNS != k.DefaultNS {
				t.Fatalf("list default_ns = %q, want %q", l.DefaultNS, k.DefaultNS)
			}
		}
	}
	if !found {
		t.Fatalf("key %q missing from list", name)
	}
	// An upsert can clear it back to "" (unbound) — not coalesced away.
	k.DefaultNS = ""
	if err := ks.PutAPIKey(ctx, k); err != nil {
		t.Fatalf("put (clear default_ns): %v", err)
	}
	got, err = ks.GetAPIKeyByHash(ctx, k.Hash)
	if err != nil {
		t.Fatalf("get by hash after clear: %v", err)
	}
	if got == nil || got.DefaultNS != "" {
		t.Fatalf("default_ns after clearing upsert = %+v, want empty", got)
	}
}

// testAPIKeyRenameNamespaces covers RenameAPIKeyNamespaces: every key whose
// HomeNS or DefaultNS equals from is rewritten to to — both columns in one
// call, since a namespace move (maintenance.Move, the RenameLinkEndpoints
// caller) must not leave either binding dangling. Non-matching keys and
// non-matching columns are untouched, CreatedAt survives, and from == to is
// a no-op.
func testAPIKeyRenameNamespaces(t *testing.T, ks store.APIKeyStore, ns string) {
	ctx := context.Background()
	from := ns + "-ren-old"
	to := ns + "-ren-new"
	other := ns + "-ren-other"
	now := time.Now().UTC().Truncate(time.Millisecond)

	seed := []store.APIKey{
		{Name: ns + "-ren-home", Hash: apiKeyHash(ns + "-ren-home"), HomeNS: from, DefaultNS: other, CreatedAt: now},
		{Name: ns + "-ren-def", Hash: apiKeyHash(ns + "-ren-def"), HomeNS: other, DefaultNS: from, CreatedAt: now},
		{Name: ns + "-ren-both", Hash: apiKeyHash(ns + "-ren-both"), HomeNS: from, DefaultNS: from, CreatedAt: now},
		{Name: ns + "-ren-none", Hash: apiKeyHash(ns + "-ren-none"), HomeNS: other, DefaultNS: "", CreatedAt: now},
	}
	for _, k := range seed {
		if err := ks.PutAPIKey(ctx, k); err != nil {
			t.Fatalf("put %s: %v", k.Name, err)
		}
	}

	if err := ks.RenameAPIKeyNamespaces(ctx, from, to); err != nil {
		t.Fatalf("rename api key namespaces: %v", err)
	}

	want := map[string][2]string{ // name -> {HomeNS, DefaultNS}
		ns + "-ren-home": {to, other},
		ns + "-ren-def":  {other, to},
		ns + "-ren-both": {to, to},
		ns + "-ren-none": {other, ""},
	}
	for _, k := range seed {
		got, err := ks.GetAPIKeyByHash(ctx, k.Hash)
		if err != nil {
			t.Fatalf("get %s by hash: %v", k.Name, err)
		}
		if got == nil {
			t.Fatalf("key %s vanished after rename", k.Name)
		}
		w := want[k.Name]
		if got.HomeNS != w[0] || got.DefaultNS != w[1] {
			t.Errorf("%s after rename = {home %q, default %q}, want {home %q, default %q}",
				k.Name, got.HomeNS, got.DefaultNS, w[0], w[1])
		}
		// CreatedAt untouched by a rename.
		if !got.CreatedAt.Equal(now) {
			t.Errorf("%s created_at mutated by rename: %v, want %v", k.Name, got.CreatedAt, now)
		}
	}

	// from == to is a no-op, not an error.
	if err := ks.RenameAPIKeyNamespaces(ctx, to, to); err != nil {
		t.Fatalf("rename api key namespaces (noop): %v", err)
	}
}

// testAPIKeyRoundTrip covers PutAPIKey/GetAPIKeyByHash round-tripping every
// field, including HomeNS and Disabled.
func testAPIKeyRoundTrip(t *testing.T, ks store.APIKeyStore, ns string) {
	ctx := context.Background()
	name := ns + "-rt"
	now := time.Now().UTC().Truncate(time.Millisecond)
	k := store.APIKey{
		Name:      name,
		Hash:      apiKeyHash(name),
		HomeNS:    ns + "-home",
		DefaultNS: ns + "-default",
		CreatedAt: now,
		Disabled:  true,
		Admin:     true,
	}
	if err := ks.PutAPIKey(ctx, k); err != nil {
		t.Fatalf("put api key: %v", err)
	}
	got, err := ks.GetAPIKeyByHash(ctx, k.Hash)
	if err != nil {
		t.Fatalf("get by hash: %v", err)
	}
	if got == nil {
		t.Fatalf("get by hash: got nil, want a key")
	}
	if got.Name != k.Name || got.Hash != k.Hash || got.HomeNS != k.HomeNS ||
		got.DefaultNS != k.DefaultNS || got.Disabled != k.Disabled || got.Admin != k.Admin {
		t.Fatalf("round-trip mismatch: %+v, want %+v", got, k)
	}
	if !got.CreatedAt.Equal(k.CreatedAt) {
		t.Fatalf("created_at = %v, want %v", got.CreatedAt, k.CreatedAt)
	}
}

// testAPIKeyUpsertPreservesCreatedAt pins the deliberate semantic choice
// documented on store.APIKeyStore.PutAPIKey: an upsert with a zero
// CreatedAt preserves the row's original CreatedAt (key rotation must not
// reset "when was this key first created"), while an upsert with an
// explicit non-zero CreatedAt (e.g. import restore) overwrites it. This is
// the opposite default from namespace_links' PutLink, which always
// overwrites created_at because links carry no recency semantics.
func testAPIKeyUpsertPreservesCreatedAt(t *testing.T, ks store.APIKeyStore, ns string) {
	ctx := context.Background()
	name := ns + "-upsert"
	now := time.Now().UTC().Truncate(time.Millisecond)
	original := store.APIKey{
		Name:      name,
		Hash:      apiKeyHash(name + "-v1"),
		HomeNS:    ns + "-home-a",
		CreatedAt: now,
	}
	if err := ks.PutAPIKey(ctx, original); err != nil {
		t.Fatalf("put original: %v", err)
	}

	// Rotation: new hash/home, zero CreatedAt.
	rotated := store.APIKey{
		Name:   name,
		Hash:   apiKeyHash(name + "-v2"),
		HomeNS: ns + "-home-b",
	}
	if err := ks.PutAPIKey(ctx, rotated); err != nil {
		t.Fatalf("put rotated: %v", err)
	}
	got, err := ks.GetAPIKeyByHash(ctx, rotated.Hash)
	if err != nil {
		t.Fatalf("get by hash after rotation: %v", err)
	}
	if got == nil {
		t.Fatalf("get by hash after rotation: got nil")
	}
	if got.HomeNS != rotated.HomeNS {
		t.Fatalf("home_ns not updated by rotation: got %q, want %q", got.HomeNS, rotated.HomeNS)
	}
	if !got.CreatedAt.Equal(original.CreatedAt) {
		t.Fatalf("created_at = %v, want preserved %v", got.CreatedAt, original.CreatedAt)
	}
	// The old hash must no longer resolve — it belongs to no key now.
	stale, err := ks.GetAPIKeyByHash(ctx, original.Hash)
	if err != nil {
		t.Fatalf("get by stale hash: %v", err)
	}
	if stale != nil {
		t.Fatalf("stale hash still resolves: %+v", stale)
	}

	// Passing a non-zero CreatedAt (e.g. import restore replaying an original
	// timestamp) does overwrite it.
	replayed := store.APIKey{
		Name:      name,
		Hash:      apiKeyHash(name + "-v3"),
		HomeNS:    ns + "-home-c",
		CreatedAt: now.Add(-24 * time.Hour),
	}
	if err := ks.PutAPIKey(ctx, replayed); err != nil {
		t.Fatalf("put replayed: %v", err)
	}
	got, err = ks.GetAPIKeyByHash(ctx, replayed.Hash)
	if err != nil {
		t.Fatalf("get by hash after replay: %v", err)
	}
	if got == nil || !got.CreatedAt.Equal(replayed.CreatedAt) {
		t.Fatalf("created_at after explicit replay = %+v, want %v", got, replayed.CreatedAt)
	}
}

// testAPIKeyDelete covers DeleteAPIKey's existed-bool return and that a
// deleted key no longer resolves by hash.
func testAPIKeyDelete(t *testing.T, ks store.APIKeyStore, ns string) {
	ctx := context.Background()
	name := ns + "-del"
	k := store.APIKey{Name: name, Hash: apiKeyHash(name), CreatedAt: time.Now().UTC().Truncate(time.Millisecond)}
	if err := ks.PutAPIKey(ctx, k); err != nil {
		t.Fatalf("put: %v", err)
	}
	existed, err := ks.DeleteAPIKey(ctx, name)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if !existed {
		t.Fatalf("delete: existed = false, want true")
	}
	existed, err = ks.DeleteAPIKey(ctx, name)
	if err != nil {
		t.Fatalf("delete (again): %v", err)
	}
	if existed {
		t.Fatalf("delete (again): existed = true, want false")
	}
	got, err := ks.GetAPIKeyByHash(ctx, k.Hash)
	if err != nil {
		t.Fatalf("get by hash after delete: %v", err)
	}
	if got != nil {
		t.Fatalf("deleted key still resolvable by hash: %+v", got)
	}
}

// testAPIKeyGetByHashAbsent covers the nil,nil contract for an unknown hash.
func testAPIKeyGetByHashAbsent(t *testing.T, ks store.APIKeyStore, ns string) {
	ctx := context.Background()
	got, err := ks.GetAPIKeyByHash(ctx, apiKeyHash(ns+"-never-existed"))
	if err != nil {
		t.Fatalf("get by hash (absent): %v", err)
	}
	if got != nil {
		t.Fatalf("get by hash (absent) = %+v, want nil", got)
	}
}

// testAPIKeyListOrdered covers ListAPIKeys returning rows ordered by name.
func testAPIKeyListOrdered(t *testing.T, ks store.APIKeyStore, ns string) {
	ctx := context.Background()
	names := []string{ns + "-list-c", ns + "-list-a", ns + "-list-b"}
	for _, n := range names {
		k := store.APIKey{Name: n, Hash: apiKeyHash(n), CreatedAt: time.Now().UTC().Truncate(time.Millisecond)}
		if err := ks.PutAPIKey(ctx, k); err != nil {
			t.Fatalf("put %s: %v", n, err)
		}
	}
	all, err := ks.ListAPIKeys(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	// The store is shared across subtests, so restrict the assertion to this
	// subtest's own rows.
	var got []string
	for _, k := range all {
		if strings.HasPrefix(k.Name, ns+"-list-") {
			got = append(got, k.Name)
		}
	}
	want := []string{ns + "-list-a", ns + "-list-b", ns + "-list-c"}
	if !slices.Equal(got, want) {
		t.Fatalf("list order = %v, want %v", got, want)
	}
}

// testAPIKeyDuplicateHash covers the unique constraint on key_hash: two
// different names must not be able to share a hash.
func testAPIKeyDuplicateHash(t *testing.T, ks store.APIKeyStore, ns string) {
	ctx := context.Background()
	hash := apiKeyHash(ns + "-dup-shared")
	first := ns + "-dup-first"
	second := ns + "-dup-second"
	now := time.Now().UTC().Truncate(time.Millisecond)
	if err := ks.PutAPIKey(ctx, store.APIKey{Name: first, Hash: hash, CreatedAt: now}); err != nil {
		t.Fatalf("put first: %v", err)
	}
	if err := ks.PutAPIKey(ctx, store.APIKey{Name: second, Hash: hash, CreatedAt: now}); err == nil {
		t.Fatalf("put second key with duplicate hash under a different name: want an error (unique constraint), got nil")
	}
}

// apiKeyHash returns a deterministic hex-encoded SHA-256 digest for seed, so
// conformance tests can produce distinct, realistic-looking key hashes
// without depending on a real secret-hashing implementation.
func apiKeyHash(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:])
}

// testPin covers store.PinStore, the config-handshake
// redesign's project→namespace pin table. It is an optional capability
// interface (the APIKeyStore precedent), so a driver that predates it skips
// cleanly.
func testPin(t *testing.T, st store.Store, dims int) {
	_ = dims // pins carry no embedding; kept for signature parity with the other subtests
	pm, ok := st.(store.PinStore)
	if !ok {
		t.Skip("store does not implement store.PinStore")
	}
	ns := t.Name()
	t.Run("UpsertRoundTripBothKeyKinds", func(t *testing.T) { testPinRoundTrip(t, pm, ns) })
	t.Run("UpdatePreservesCreatedAtAndCreatedBy", func(t *testing.T) { testPinUpdatePreservesCreated(t, pm, ns) })
	t.Run("DeleteReturnsAccurateCount", func(t *testing.T) { testPinDelete(t, pm, ns) })
	t.Run("ListStableOrder", func(t *testing.T) { testPinListOrder(t, pm, ns) })
	t.Run("RenameExactMatchOnly", func(t *testing.T) { testPinRename(t, pm, ns) })
	t.Run("MultiEntryPutIsAtomic", func(t *testing.T) { testPinAtomicPut(t, pm, ns) })
}

// testPinRoundTrip covers PutPins/GetPins
// round-tripping both key shapes ("remote:<canonical>" and
// "path:<absolute-toplevel>") in a single call.
func testPinRoundTrip(t *testing.T, pm store.PinStore, ns string) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	remoteKey := "remote:" + ns + "-rt-github.com-acme-widgets"
	pathKey := "path:/srv/" + ns + "-rt-bare-repo"
	entries := []store.Pin{
		{Key: remoteKey, Namespace: ns + "/widgets", Note: "pinned by ops", CreatedBy: "ci-bot", CreatedAt: now, UpdatedAt: now},
		{Key: pathKey, Namespace: ns + "/bare", Note: "", CreatedBy: "", CreatedAt: now, UpdatedAt: now},
	}
	if err := pm.PutPins(ctx, entries); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := pm.GetPins(ctx, []string{remoteKey, pathKey})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("get = %d entries, want 2", len(got))
	}
	byKey := make(map[string]store.Pin, len(got))
	for _, e := range got {
		byKey[e.Key] = e
	}
	for _, want := range entries {
		g, ok := byKey[want.Key]
		if !ok {
			t.Fatalf("missing entry %q", want.Key)
		}
		if g.Namespace != want.Namespace || g.Note != want.Note || g.CreatedBy != want.CreatedBy {
			t.Fatalf("round-trip mismatch for %q: got %+v, want %+v", want.Key, g, want)
		}
		if !g.CreatedAt.Equal(want.CreatedAt) {
			t.Fatalf("created_at for %q = %v, want %v", want.Key, g.CreatedAt, want.CreatedAt)
		}
		if !g.UpdatedAt.Equal(want.UpdatedAt) {
			t.Fatalf("updated_at for %q = %v, want %v", want.Key, g.UpdatedAt, want.UpdatedAt)
		}
	}
}

// testPinUpdatePreservesCreated pins the deliberate semantic choice
// documented on store.PinStore.PutPins: a second Put for
// the same Key updates Namespace/Note/UpdatedAt but preserves the row's
// original CreatedAt/CreatedBy even when the second call passes different
// values for them — a pin's provenance is fixed at creation.
func testPinUpdatePreservesCreated(t *testing.T, pm store.PinStore, ns string) {
	ctx := context.Background()
	key := "remote:" + ns + "-upd-github.com-acme-widgets"
	created := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Millisecond)
	firstUpdate := created.Add(time.Hour)
	if err := pm.PutPins(ctx, []store.Pin{
		{Key: key, Namespace: ns + "/orig", Note: "first", CreatedBy: "alice", CreatedAt: created, UpdatedAt: firstUpdate},
	}); err != nil {
		t.Fatalf("put (insert): %v", err)
	}

	secondUpdate := firstUpdate.Add(time.Hour)
	// Deliberately different CreatedAt/CreatedBy on the update, to prove the
	// store ignores them once the row exists.
	if err := pm.PutPins(ctx, []store.Pin{
		{
			Key: key, Namespace: ns + "/updated", Note: "second", CreatedBy: "mallory",
			CreatedAt: time.Now().UTC(), UpdatedAt: secondUpdate,
		},
	}); err != nil {
		t.Fatalf("put (update): %v", err)
	}

	got, err := pm.GetPins(ctx, []string{key})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("get after update = %d entries, want 1", len(got))
	}
	e := got[0]
	if e.Namespace != ns+"/updated" || e.Note != "second" {
		t.Fatalf("namespace/note not updated: %+v", e)
	}
	if !e.UpdatedAt.Equal(secondUpdate) {
		t.Fatalf("updated_at = %v, want %v", e.UpdatedAt, secondUpdate)
	}
	if !e.CreatedAt.Equal(created) {
		t.Fatalf("created_at mutated by update: got %v, want preserved %v", e.CreatedAt, created)
	}
	if e.CreatedBy != "alice" {
		t.Fatalf("created_by mutated by update: got %q, want preserved %q", e.CreatedBy, "alice")
	}
}

// testPinDelete covers DeletePins' accurate-count return,
// including 0 for a batch of entirely-missing keys.
func testPinDelete(t *testing.T, pm store.PinStore, ns string) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	k1 := "path:/srv/" + ns + "-del-1"
	k2 := "path:/srv/" + ns + "-del-2"
	missing := "path:/srv/" + ns + "-del-missing"
	if err := pm.PutPins(ctx, []store.Pin{
		{Key: k1, Namespace: ns + "/d1", CreatedAt: now, UpdatedAt: now},
		{Key: k2, Namespace: ns + "/d2", CreatedAt: now, UpdatedAt: now},
	}); err != nil {
		t.Fatalf("put: %v", err)
	}

	n, err := pm.DeletePins(ctx, []string{k1, missing})
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if n != 1 {
		t.Fatalf("delete count = %d, want 1 (one real key, one already-missing)", n)
	}

	n, err = pm.DeletePins(ctx, []string{missing})
	if err != nil {
		t.Fatalf("delete (all missing): %v", err)
	}
	if n != 0 {
		t.Fatalf("delete count (all missing) = %d, want 0", n)
	}

	got, err := pm.GetPins(ctx, []string{k1, k2})
	if err != nil {
		t.Fatalf("get after delete: %v", err)
	}
	if len(got) != 1 || got[0].Key != k2 {
		t.Fatalf("get after delete = %+v, want only %q", got, k2)
	}
}

// testPinListOrder covers ListPins returning a stable
// order (by Key) across both key shapes.
func testPinListOrder(t *testing.T, pm store.PinStore, ns string) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	keys := []string{
		"remote:" + ns + "-list-c",
		"remote:" + ns + "-list-a",
		"path:/srv/" + ns + "-list-b",
	}
	entries := make([]store.Pin, 0, len(keys))
	for _, k := range keys {
		entries = append(entries, store.Pin{Key: k, Namespace: ns + "/x", CreatedAt: now, UpdatedAt: now})
	}
	if err := pm.PutPins(ctx, entries); err != nil {
		t.Fatalf("put: %v", err)
	}
	all, err := pm.ListPins(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	// The store is shared across subtests, so restrict the assertion to this
	// subtest's own rows (mirroring testAPIKeyListOrdered's prefix filter).
	prefix := ns + "-list-"
	var got []string
	for _, e := range all {
		_, rest, found := strings.Cut(e.Key, ":")
		if found && strings.Contains(rest, prefix) {
			got = append(got, e.Key)
		}
	}
	want := []string{
		"path:/srv/" + ns + "-list-b",
		"remote:" + ns + "-list-a",
		"remote:" + ns + "-list-c",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("list order = %v, want %v (lexicographic by key)", got, want)
	}
}

// testPinRename covers RenamePinNamespaces' exact-match
// semantics: a namespace that merely looks alike (e.g. "memini2" against
// from="memini") must not be touched.
func testPinRename(t *testing.T, pm store.PinStore, ns string) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	from := ns + "-ren-memini"
	similar := ns + "-ren-memini2" // must NOT match an exact-match rename of `from`
	to := ns + "-ren-memini-new"

	k1 := "remote:" + ns + "-ren-a"
	k2 := "path:/srv/" + ns + "-ren-b"
	if err := pm.PutPins(ctx, []store.Pin{
		{Key: k1, Namespace: from, CreatedAt: now, UpdatedAt: now},
		{Key: k2, Namespace: similar, CreatedAt: now, UpdatedAt: now},
	}); err != nil {
		t.Fatalf("put: %v", err)
	}

	if err := pm.RenamePinNamespaces(ctx, from, to); err != nil {
		t.Fatalf("rename: %v", err)
	}

	got, err := pm.GetPins(ctx, []string{k1, k2})
	if err != nil {
		t.Fatalf("get after rename: %v", err)
	}
	byKey := make(map[string]store.Pin, len(got))
	for _, e := range got {
		byKey[e.Key] = e
	}
	if byKey[k1].Namespace != to {
		t.Fatalf("exact-match namespace not renamed: %+v, want %q", byKey[k1], to)
	}
	if byKey[k2].Namespace != similar {
		t.Fatalf("look-alike namespace %q wrongly renamed: %+v", similar, byKey[k2])
	}
}

// testPinAtomicPut covers PutPins writing a multi-entry
// batch in a single transaction: both rows must land together.
func testPinAtomicPut(t *testing.T, pm store.PinStore, ns string) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	k1 := "remote:" + ns + "-atomic-1"
	k2 := "path:/srv/" + ns + "-atomic-2"
	if err := pm.PutPins(ctx, []store.Pin{
		{Key: k1, Namespace: ns + "/a1", CreatedAt: now, UpdatedAt: now},
		{Key: k2, Namespace: ns + "/a2", CreatedAt: now, UpdatedAt: now},
	}); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := pm.GetPins(ctx, []string{k1, k2})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("get = %d entries, want 2 (both rows from the single Put must land)", len(got))
	}
}

// testClientSettings covers store.ClientSettingsStore (the global-defaults
// blob) and the per-key ClientSettings override riding along on
// store.APIKeyStore. Both are optional capability interfaces (the
// EmbedModelStore/APIKeyStore precedent), so a driver that predates either
// skips cleanly.
//
// GlobalRoundTripAndZeroWhenUnset runs first and deliberately: it is the only
// subtest allowed to observe the global-settings key before anything has
// written to it, since SetGlobalClientSettings is a full replace (not a
// merge) of one store-wide row — every later subtest that calls it would
// otherwise clobber the "unset" precondition.
func testClientSettings(t *testing.T, st store.Store, dims int) {
	_ = dims
	cs, ok := st.(store.ClientSettingsStore)
	if !ok {
		t.Skip("store does not implement store.ClientSettingsStore")
	}
	ns := t.Name()
	t.Run("GlobalRoundTripAndZeroWhenUnset", func(t *testing.T) { testClientSettingsGlobalRoundTrip(t, cs) })
	t.Run("OnlySetFieldsPersisted", func(t *testing.T) { testClientSettingsOnlySetPersisted(t, cs) })

	ks, ok := st.(store.APIKeyStore)
	if !ok {
		t.Skip("store does not implement store.APIKeyStore (per-key settings subtests)")
	}
	t.Run("PerKeySettingsSurviveAPIKeyOps", func(t *testing.T) { testAPIKeySettingsRoundTrip(t, ks, ns) })
	t.Run("PerKeySettingsSurviveRename", func(t *testing.T) { testAPIKeySettingsSurviveRename(t, ks, ns) })
}

// testClientSettingsGlobalRoundTrip covers GlobalClientSettings returning the
// zero value before anything has been set, SetGlobalClientSettings/
// GlobalClientSettings round-tripping a partial settings value (only the set
// fields non-nil), and a second Set replacing wholesale rather than merging.
func testClientSettingsGlobalRoundTrip(t *testing.T, cs store.ClientSettingsStore) {
	ctx := context.Background()

	got, err := cs.GlobalClientSettings(ctx)
	if err != nil {
		t.Fatalf("global client settings (unset): %v", err)
	}
	if got != (store.ClientSettings{}) {
		t.Fatalf("global client settings before any Set = %+v, want the zero value", got)
	}

	pinned := 7
	recall := false
	if err := cs.SetGlobalClientSettings(ctx, store.ClientSettings{
		InjectBriefingPinned: &pinned,
		Recall:               &recall,
	}); err != nil {
		t.Fatalf("set global client settings: %v", err)
	}
	got, err = cs.GlobalClientSettings(ctx)
	if err != nil {
		t.Fatalf("get global client settings: %v", err)
	}
	if got.InjectBriefingPinned == nil || *got.InjectBriefingPinned != pinned {
		t.Fatalf("inject_briefing_pinned round-trip = %v, want %d", got.InjectBriefingPinned, pinned)
	}
	if got.Recall == nil || *got.Recall != recall {
		t.Fatalf("recall round-trip = %v, want %v", got.Recall, recall)
	}
	if got.CaptureTurns != nil {
		t.Fatalf("capture_turns = %v, want nil (never set)", *got.CaptureTurns)
	}

	// A second Set replaces wholesale: the first Set's fields must not survive.
	interval := 42
	if err := cs.SetGlobalClientSettings(ctx, store.ClientSettings{AutoSaveInterval: &interval}); err != nil {
		t.Fatalf("set global client settings (replace): %v", err)
	}
	got, err = cs.GlobalClientSettings(ctx)
	if err != nil {
		t.Fatalf("get global client settings (after replace): %v", err)
	}
	if got.AutoSaveInterval == nil || *got.AutoSaveInterval != interval {
		t.Fatalf("auto_save_interval after replace = %v, want %d", got.AutoSaveInterval, interval)
	}
	if got.InjectBriefingPinned != nil {
		t.Fatalf("inject_briefing_pinned survived a full replace: %v, want nil", *got.InjectBriefingPinned)
	}
}

// testClientSettingsOnlySetPersisted covers the "only explicitly-set fields
// are persisted" rule: storing a settings value with a single field set must
// read back with every other field nil, never coalesced to a default or a
// zero value.
//
// gocyclo is silenced: the assertion is a flat per-field nil enumeration, not
// genuinely complex control flow.
func testClientSettingsOnlySetPersisted(t *testing.T, cs store.ClientSettingsStore) { //nolint:gocyclo
	ctx := context.Background()
	autoSave := false
	if err := cs.SetGlobalClientSettings(ctx, store.ClientSettings{AutoSave: &autoSave}); err != nil {
		t.Fatalf("set: %v", err)
	}
	got, err := cs.GlobalClientSettings(ctx)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.AutoSave == nil || *got.AutoSave {
		t.Fatalf("auto_save = %v, want false", got.AutoSave)
	}
	if got.CaptureTurns != nil || got.SessionDigest != nil || got.InlineExtract != nil ||
		got.AutoSaveInterval != nil || got.InjectBriefingPinned != nil || got.InjectBriefingFacts != nil ||
		got.InjectBriefingProcedures != nil || got.InjectBriefingRecent != nil || got.InjectBriefingMaxTok != nil ||
		got.InjectPretoolItems != nil || got.InjectPretoolMaxTok != nil || got.InjectPretoolMinScore != nil ||
		got.InjectPretoolTools != nil || got.InjectPretoolGateMs != nil || got.InjectDedupe != nil ||
		got.InjectCooldownMs != nil || got.InjectCooldownPrompts != nil ||
		got.InjectLabels != nil || got.Recall != nil || got.Capture != nil ||
		got.RecallLimit != nil || got.InjectRecallMaxTok != nil || got.InjectRecallMinScore != nil ||
		got.MinCaptureChars != nil || got.RequestTimeoutMs != nil ||
		got.NamespaceScope != nil || got.NamespacePrefix != nil {
		t.Fatalf("only-set-field: unexpected non-nil field(s) in %+v", got)
	}
}

// testAPIKeySettingsRoundTrip covers APIKey.Settings surviving
// PutAPIKey/GetAPIKeyByHash/ListAPIKeys, including a slice-typed field, and a
// key with no override persisting as the zero ClientSettings rather than an
// error or a coalesced default.
func testAPIKeySettingsRoundTrip(t *testing.T, ks store.APIKeyStore, ns string) {
	ctx := context.Background()
	name := ns + "-settings"
	pinned := 2
	autoSave := false
	tools := []string{"Read", "Grep"}
	k := store.APIKey{
		Name:      name,
		Hash:      apiKeyHash(name),
		CreatedAt: time.Now().UTC().Truncate(time.Millisecond),
		Settings: store.ClientSettings{
			InjectBriefingPinned: &pinned,
			AutoSave:             &autoSave,
			InjectPretoolTools:   &tools,
		},
	}
	if err := ks.PutAPIKey(ctx, k); err != nil {
		t.Fatalf("put: %v", err)
	}

	byHash, err := ks.GetAPIKeyByHash(ctx, k.Hash)
	if err != nil {
		t.Fatalf("get by hash: %v", err)
	}
	if byHash == nil {
		t.Fatalf("get by hash: got nil")
	}
	checkAPIKeySettings(t, "get by hash", byHash.Settings, pinned, autoSave, tools)

	all, err := ks.ListAPIKeys(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	found := false
	for _, l := range all {
		if l.Name == name {
			found = true
			checkAPIKeySettings(t, "list", l.Settings, pinned, autoSave, tools)
		}
	}
	if !found {
		t.Fatalf("key %q missing from list", name)
	}

	// A key with no override at all persists as the zero ClientSettings.
	plainName := ns + "-settings-none"
	if err := ks.PutAPIKey(ctx, store.APIKey{
		Name: plainName, Hash: apiKeyHash(plainName), CreatedAt: time.Now().UTC().Truncate(time.Millisecond),
	}); err != nil {
		t.Fatalf("put (no settings): %v", err)
	}
	got, err := ks.GetAPIKeyByHash(ctx, apiKeyHash(plainName))
	if err != nil {
		t.Fatalf("get by hash (no settings): %v", err)
	}
	if got == nil {
		t.Fatalf("get by hash (no settings): got nil")
	}
	if got.Settings != (store.ClientSettings{}) {
		t.Fatalf("settings for a key with no override = %+v, want the zero value", got.Settings)
	}
}

// checkAPIKeySettings asserts the three fields testAPIKeySettingsRoundTrip
// sets, and that every other field stayed nil (no coalescing to a default).
func checkAPIKeySettings(t *testing.T, label string, got store.ClientSettings, wantPinned int, wantAutoSave bool, wantTools []string) {
	t.Helper()
	if got.InjectBriefingPinned == nil || *got.InjectBriefingPinned != wantPinned {
		t.Fatalf("%s: inject_briefing_pinned = %v, want %d", label, got.InjectBriefingPinned, wantPinned)
	}
	if got.AutoSave == nil || *got.AutoSave != wantAutoSave {
		t.Fatalf("%s: auto_save = %v, want %v", label, got.AutoSave, wantAutoSave)
	}
	if got.InjectPretoolTools == nil || !slices.Equal(*got.InjectPretoolTools, wantTools) {
		t.Fatalf("%s: inject_pretool_tools = %v, want %v", label, got.InjectPretoolTools, wantTools)
	}
	if got.Recall != nil {
		t.Fatalf("%s: recall = %v, want nil (never set)", label, *got.Recall)
	}
}

// testAPIKeySettingsSurviveRename covers APIKey.Settings surviving
// RenameAPIKeyNamespaces (which only touches HomeNS/DefaultNS).
func testAPIKeySettingsSurviveRename(t *testing.T, ks store.APIKeyStore, ns string) {
	ctx := context.Background()
	name := ns + "-settings-rename"
	from := ns + "-settings-rename-old"
	to := ns + "-settings-rename-new"
	pinned := 9
	k := store.APIKey{
		Name:      name,
		Hash:      apiKeyHash(name),
		HomeNS:    from,
		CreatedAt: time.Now().UTC().Truncate(time.Millisecond),
		Settings:  store.ClientSettings{InjectBriefingPinned: &pinned},
	}
	if err := ks.PutAPIKey(ctx, k); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := ks.RenameAPIKeyNamespaces(ctx, from, to); err != nil {
		t.Fatalf("rename: %v", err)
	}
	got, err := ks.GetAPIKeyByHash(ctx, k.Hash)
	if err != nil {
		t.Fatalf("get by hash after rename: %v", err)
	}
	if got == nil {
		t.Fatalf("key vanished after rename")
	}
	if got.HomeNS != to {
		t.Fatalf("home_ns after rename = %q, want %q", got.HomeNS, to)
	}
	if got.Settings.InjectBriefingPinned == nil || *got.Settings.InjectBriefingPinned != pinned {
		t.Fatalf("settings did not survive rename: %+v, want inject_briefing_pinned=%d", got.Settings, pinned)
	}
}
