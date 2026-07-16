package service

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/eleboucher/memini/internal/embed/embedtest"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/store"
	"github.com/eleboucher/memini/internal/store/sqlitevec"
)

// rotationNow is the fixed clock the exploration-slot tests thread, so the
// 7-day "recently served" window is deterministic (a served event stamped
// rotationNow-1d is inside it, rotationNow-8d outside).
var rotationNow = time.Unix(1_700_000_000, 0).UTC()

// rotationNS is the single namespace the exploration-slot tests operate in.
const rotationNS = "acme"

// newRotationSvc builds a Service over a real sqlite-vec store with the
// activity log on and synchronous, so the reserved exploration slot reads the
// briefing events the tests inject without racing a background writer.
func newRotationSvc(t *testing.T) (*Service, *sqlitevec.Store) {
	t.Helper()
	st, err := sqlitevec.Open(context.Background(), filepath.Join(t.TempDir(), "rotation.db"), readsetTestDims)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	svc := New(st, embedtest.New(readsetTestDims),
		WithClock(func() time.Time { return rotationNow }),
		WithEventLog(true), WithSyncEventLog())
	return svc, st
}

// putRankedMem upserts a durable memory into rotationNS whose Importance fixes
// its DurableScore ordering: with confidence untracked and access-count zero,
// DurableScore reduces to Salience, strictly increasing in Importance, so a
// descending Importance list yields a deterministic top-N.
func putRankedMem(t *testing.T, st *sqlitevec.Store, id string, tier memory.Tier, importance float64) {
	t.Helper()
	m := &memory.Memory{
		ID: id, Namespace: rotationNS, Tier: tier, Content: "content of " + id,
		Importance:     importance,
		CreatedAt:      rotationNow,
		UpdatedAt:      rotationNow,
		LastAccessedAt: rotationNow,
	}
	if err := st.Upsert(context.Background(), m); err != nil {
		t.Fatalf("upsert %s/%s: %v", rotationNS, id, err)
	}
}

// putBriefingEvent records that a briefing in rotationNS served memID, inside
// the 7-day exploration window, so the reserved slot sees it as recently shown.
func putBriefingEvent(t *testing.T, st *sqlitevec.Store, memID string) {
	t.Helper()
	e := store.Event{
		OpID:       "brief-" + memID,
		Kind:       store.EventBriefing,
		Namespace:  rotationNS,
		MemoryID:   memID,
		MemoryNS:   rotationNS,
		MemoryTier: memory.TierSemantic,
		CreatedAt:  rotationNow.Add(-24 * time.Hour),
	}
	if err := st.AppendEvents(context.Background(), []store.Event{e}); err != nil {
		t.Fatalf("append briefing event %s: %v", memID, err)
	}
}

// seedRankedFacts writes ids[0]..ids[n-1] into rotationNS as semantic facts with
// strictly descending DurableScore, so ids is the exact pure-DurableScore order.
func seedRankedFacts(t *testing.T, st *sqlitevec.Store, ids ...string) {
	t.Helper()
	for i, id := range ids {
		putRankedMem(t, st, id, memory.TierSemantic, 0.9-0.1*float64(i))
	}
}

// eqIDs reports whether idList(got) equals want in order.
func eqIDs(got []*memory.Memory, want ...string) bool {
	g := idList(got)
	if len(g) != len(want) {
		return false
	}
	for i := range want {
		if g[i] != want[i] {
			return false
		}
	}
	return true
}

// TestBriefingReservesLastSlotForUnservedItem: when the whole top-N was served
// in the last week, the section's last slot swaps in the highest-DurableScore
// item not recently served, and the first N-1 slots are untouched.
func TestBriefingReservesLastSlotForUnservedItem(t *testing.T) {
	svc, st := newRotationSvc(t)
	ctx := context.Background()
	seedRankedFacts(t, st, "f1", "f2", "f3", "f4", "f5", "f6", "f7")
	// The top 5 (f1..f5) were all served recently; f6/f7 sit below the cutoff
	// and were never served — f6 is the highest-scored of them.
	for _, id := range []string{"f1", "f2", "f3", "f4", "f5"} {
		putBriefingEvent(t, st, id)
	}

	b, err := svc.Briefing(ctx, "acme", BriefingOpts{})
	if err != nil {
		t.Fatalf("briefing: %v", err)
	}
	if !eqIDs(b.Facts, "f1", "f2", "f3", "f4", "f6") {
		t.Fatalf("facts = %v, want [f1 f2 f3 f4 f6] (last slot reserved for the highest unserved item, first 4 unchanged)", idList(b.Facts))
	}
}

// TestBriefingReservesLastSlotForProcedures: the reservation applies to the
// procedures section too, not only facts.
func TestBriefingReservesLastSlotForProcedures(t *testing.T) {
	svc, st := newRotationSvc(t)
	ctx := context.Background()
	for i, id := range []string{"p1", "p2", "p3", "p4", "p5", "p6"} {
		putRankedMem(t, st, id, memory.TierProcedural, 0.9-0.1*float64(i))
	}
	for _, id := range []string{"p1", "p2", "p3", "p4", "p5"} {
		putBriefingEvent(t, st, id)
	}

	b, err := svc.Briefing(ctx, "acme", BriefingOpts{})
	if err != nil {
		t.Fatalf("briefing: %v", err)
	}
	if !eqIDs(b.Procedures, "p1", "p2", "p3", "p4", "p6") {
		t.Fatalf("procedures = %v, want [p1 p2 p3 p4 p6]", idList(b.Procedures))
	}
}

// TestBriefingAllServedFallsBackToPureOrder: with every candidate served in the
// window there is nothing staler to surface, so the section stays in pure
// DurableScore order.
func TestBriefingAllServedFallsBackToPureOrder(t *testing.T) {
	svc, st := newRotationSvc(t)
	ctx := context.Background()
	seedRankedFacts(t, st, "f1", "f2", "f3", "f4", "f5", "f6", "f7")
	for _, id := range []string{"f1", "f2", "f3", "f4", "f5", "f6", "f7"} {
		putBriefingEvent(t, st, id)
	}

	b, err := svc.Briefing(ctx, "acme", BriefingOpts{})
	if err != nil {
		t.Fatalf("briefing: %v", err)
	}
	if !eqIDs(b.Facts, "f1", "f2", "f3", "f4", "f5") {
		t.Fatalf("facts = %v, want pure order [f1 f2 f3 f4 f5]", idList(b.Facts))
	}
}

// TestBriefingUnservedInsideTopNNoSwap: an unserved item already inside the
// top-N means the reservation is already satisfied — no swap, pure order.
func TestBriefingUnservedInsideTopNNoSwap(t *testing.T) {
	svc, st := newRotationSvc(t)
	ctx := context.Background()
	seedRankedFacts(t, st, "f1", "f2", "f3", "f4", "f5", "f6", "f7")
	// Everything served EXCEPT f3, which sits inside the top-5.
	for _, id := range []string{"f1", "f2", "f4", "f5", "f6", "f7"} {
		putBriefingEvent(t, st, id)
	}

	b, err := svc.Briefing(ctx, "acme", BriefingOpts{})
	if err != nil {
		t.Fatalf("briefing: %v", err)
	}
	if !eqIDs(b.Facts, "f1", "f2", "f3", "f4", "f5") {
		t.Fatalf("facts = %v, want pure order [f1 f2 f3 f4 f5] (unserved f3 already in top-N)", idList(b.Facts))
	}
}

// TestBriefingFewerThanCapNoRotation: with N or fewer candidates the whole
// section fits, so there is no last slot to reserve — pure order, even when
// every candidate was recently served.
func TestBriefingFewerThanCapNoRotation(t *testing.T) {
	svc, st := newRotationSvc(t)
	ctx := context.Background()
	seedRankedFacts(t, st, "f1", "f2", "f3")
	for _, id := range []string{"f1", "f2", "f3"} {
		putBriefingEvent(t, st, id)
	}

	b, err := svc.Briefing(ctx, "acme", BriefingOpts{})
	if err != nil {
		t.Fatalf("briefing: %v", err)
	}
	if !eqIDs(b.Facts, "f1", "f2", "f3") {
		t.Fatalf("facts = %v, want [f1 f2 f3] unchanged", idList(b.Facts))
	}
}

// TestBriefingWithoutEventLogFallsBackToPureOrder: against a store the service
// cannot see as an EventLogStore, the reservation degrades to pure DurableScore
// order — even though the served events exist in the underlying store — exactly
// how childRollup degrades without ActivityStore.
func TestBriefingWithoutEventLogFallsBackToPureOrder(t *testing.T) {
	st, err := sqlitevec.Open(context.Background(), filepath.Join(t.TempDir(), "no-eventlog.db"), readsetTestDims)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	seedRankedFacts(t, st, "f1", "f2", "f3", "f4", "f5", "f6", "f7")
	// Prove the underlying store DOES hold served events, so the fallback below
	// is attributable to the wrapper hiding EventLogStore, not to missing data.
	for _, id := range []string{"f1", "f2", "f3", "f4", "f5"} {
		putBriefingEvent(t, st, id)
	}
	// countingListStore embeds the store.Store interface, so no optional
	// capability (EventLogStore, ActivityStore) is promoted from the wrapped
	// driver — the "store predates the event log" fake.
	svc := New(&countingListStore{Store: st}, embedtest.New(readsetTestDims),
		WithClock(func() time.Time { return rotationNow }),
		WithEventLog(true), WithSyncEventLog())

	b, err := svc.Briefing(context.Background(), "acme", BriefingOpts{})
	if err != nil {
		t.Fatalf("briefing: %v", err)
	}
	if !eqIDs(b.Facts, "f1", "f2", "f3", "f4", "f5") {
		t.Fatalf("facts = %v, want pure order [f1 f2 f3 f4 f5] — no event log to rotate against", idList(b.Facts))
	}
}

// TestReserveExplorationSlot exercises the pure ranking helper directly, so the
// swap/no-swap decision is pinned independent of the store and section plumbing.
func TestReserveExplorationSlot(t *testing.T) {
	mems := func(ids ...string) []*memory.Memory {
		out := make([]*memory.Memory, len(ids))
		for i, id := range ids {
			out[i] = &memory.Memory{ID: id}
		}
		return out
	}
	set := func(ids ...string) map[string]bool {
		m := map[string]bool{}
		for _, id := range ids {
			m[id] = true
		}
		return m
	}
	tests := []struct {
		name   string
		ids    []string
		n      int
		served []string
		want   []string
	}{
		{
			// e (the served slot-5 item) is displaced by f, the highest unserved
			// item below the cutoff, and slides down after it.
			name:   "top-n all served, unserved below the cutoff swaps into the last slot",
			ids:    []string{"a", "b", "c", "d", "e", "f", "g"},
			n:      5,
			served: []string{"a", "b", "c", "d", "e"},
			want:   []string{"a", "b", "c", "d", "f", "e", "g"},
		},
		{
			name:   "highest-scored unserved below the cutoff wins, not a lower one",
			ids:    []string{"a", "b", "c", "d", "e", "f", "g"},
			n:      5,
			served: []string{"a", "b", "c", "d", "e", "f"}, // f served too, g is the only unserved below
			want:   []string{"a", "b", "c", "d", "g", "e", "f"},
		},
		{
			name:   "unserved item already in the top-n: no swap",
			ids:    []string{"a", "b", "c", "d", "e", "f"},
			n:      5,
			served: []string{"a", "b", "d", "e", "f"}, // c (in top-5) unserved
			want:   []string{"a", "b", "c", "d", "e", "f"},
		},
		{
			name:   "every candidate served: nothing staler to surface",
			ids:    []string{"a", "b", "c", "d", "e", "f"},
			n:      5,
			served: []string{"a", "b", "c", "d", "e", "f"},
			want:   []string{"a", "b", "c", "d", "e", "f"},
		},
		{
			name:   "n or fewer candidates: unchanged",
			ids:    []string{"a", "b", "c"},
			n:      5,
			served: []string{"a", "b", "c"},
			want:   []string{"a", "b", "c"},
		},
		{
			name:   "disabled section (n<=0): unchanged",
			ids:    []string{"a", "b"},
			n:      0,
			served: []string{"a", "b"},
			want:   []string{"a", "b"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := reserveExplorationSlot(mems(tt.ids...), tt.n, set(tt.served...))
			if !eqIDs(got, tt.want...) {
				t.Fatalf("reserveExplorationSlot = %v, want %v", idList(got), tt.want)
			}
		})
	}
}
