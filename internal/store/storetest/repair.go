package storetest

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/store"
)

// testRepair is the conformance suite for store.RepairStore — the durable
// deferred-repair queue that lives in columns on `memories` rather than a job
// table. Both backends must behave identically here even though their claim
// statements are structurally different: sqlite leans on single-writer
// serialization and an UPDATE ... RETURNING, Postgres on FOR UPDATE SKIP LOCKED
// and a server-side clock.
func testRepair(t *testing.T, st store.Store, dims int) {
	rs, ok := st.(store.RepairStore)
	if !ok {
		t.Skip("store does not implement store.RepairStore")
	}
	t.Run("FreshWriteOwesNothing", func(t *testing.T) { testRepairFreshWrite(t, st, rs, dims) })
	t.Run("MarkClaimComplete", func(t *testing.T) { testRepairRoundTrip(t, st, rs, dims) })
	t.Run("MarkSkipsRowsAlreadyRepairing", func(t *testing.T) { testRepairMarkSkips(t, st, rs, dims) })
	t.Run("ClaimRespectsNextRunAt", func(t *testing.T) { testRepairClaimDue(t, st, rs, dims) })
	t.Run("ClaimIncrementsAttemptsAndLeases", func(t *testing.T) { testRepairClaimLeases(t, st, rs, dims) })
	t.Run("ClaimFiltersByState", func(t *testing.T) { testRepairClaimState(t, st, rs, dims) })
	t.Run("ClaimIsExclusiveUnderConcurrency", func(t *testing.T) { testRepairClaimExclusive(t, st, rs, dims) })
	t.Run("FingerprintGuardRejectsMovedRow", func(t *testing.T) { testRepairFingerprintGuard(t, st, rs, dims) })
	t.Run("RepairDoesNotBumpUpdatedAt", func(t *testing.T) { testRepairKeepsUpdatedAt(t, st, rs, dims) })
	t.Run("SetRepairStateKeepsVector", func(t *testing.T) { testRepairKeepsVector(t, st, rs, dims) })
	t.Run("FailDoesNotTouchAttempts", func(t *testing.T) { testRepairFail(t, st, rs, dims) })
	t.Run("ParkExcludesFromClaim", func(t *testing.T) { testRepairPark(t, st, rs, dims) })
	t.Run("RearmReturnsParkedRows", func(t *testing.T) { testRepairRearm(t, st, rs, dims) })
	t.Run("Stats", func(t *testing.T) { testRepairStats(t, st, rs, dims) })
	t.Run("CompletionClearsTheLegacyMarker", func(t *testing.T) { testRepairClearsMarker(t, st, rs, dims) })
	t.Run("StateOfMissingRowErrors", func(t *testing.T) { testRepairStateOfMissing(t, rs) })
}

// repairSeed writes a vectorless memory and marks it pending, the state a
// degraded write leaves behind.
func repairSeed(t *testing.T, st store.Store, rs store.RepairStore, ns, short, content string) *memory.Memory {
	t.Helper()
	ctx := context.Background()
	m := mem(ns, short, content, nil)
	mustUpsert(t, st, m)
	n, err := rs.MarkRepairNeeded(ctx, ns, []string{m.ID}, store.RepairPending)
	if err != nil {
		t.Fatalf("mark repair: %v", err)
	}
	if n != 1 {
		t.Fatalf("MarkRepairNeeded moved %d rows, want 1", n)
	}
	return m
}

func testRepairFreshWrite(t *testing.T, st store.Store, rs store.RepairStore, dims int) {
	ctx := context.Background()
	ns := t.Name()
	m := mem(ns, "a", "a healthy write", vec(dims, 1))
	mustUpsert(t, st, m)

	state, attempts, lastErr, err := rs.RepairStateOf(ctx, ns, m.ID)
	if err != nil {
		t.Fatalf("state of: %v", err)
	}
	if state != store.RepairNone || attempts != 0 || lastErr != "" {
		t.Fatalf("fresh write = (%q, %d, %q), want (RepairNone, 0, \"\")", state, attempts, lastErr)
	}
	// A healthy row must never be claimable, or the worker would re-embed the
	// entire corpus on its first tick.
	got, err := rs.ClaimRepairs(ctx, store.RepairPending, time.Now(), time.Minute, 10)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	for _, r := range got {
		if r.ID == m.ID {
			t.Fatal("a healthy memory was claimed for repair")
		}
	}
}

func testRepairRoundTrip(t *testing.T, st store.Store, rs store.RepairStore, dims int) {
	ctx := context.Background()
	ns := t.Name()
	m := repairSeed(t, st, rs, ns, "a", "the deploy key rotates every 90 days")

	claimed, err := rs.ClaimRepairs(ctx, store.RepairPending, time.Now(), time.Minute, 10)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	var row *store.RepairRow
	for i := range claimed {
		if claimed[i].ID == m.ID {
			row = &claimed[i]
		}
	}
	if row == nil {
		t.Fatal("pending memory was not claimed")
	}
	if row.Content != m.Content || row.Namespace != ns || row.Fingerprint != memory.Fingerprint(m.Content) {
		t.Fatalf("claimed row = %+v, want content/ns/fingerprint of %s", row, m.ID)
	}
	if row.Tier != memory.TierSemantic {
		t.Fatalf("claimed Tier = %q, want %q", row.Tier, memory.TierSemantic)
	}

	// pending -> enrich, attaching the vector.
	ok, err := rs.SetEmbeddingIfUnchanged(ctx, ns, m.ID, row.Fingerprint, vec(dims, 3), store.RepairEnrich)
	if err != nil {
		t.Fatalf("set embedding: %v", err)
	}
	if !ok {
		t.Fatal("SetEmbeddingIfUnchanged reported no rows for an unchanged memory")
	}
	got, err := st.GetEmbedding(ctx, ns, m.ID)
	if err != nil {
		t.Fatalf("get embedding: %v", err)
	}
	if len(got) != dims {
		t.Fatalf("embedding len = %d, want %d", len(got), dims)
	}
	// The repaired row must now be reachable by vector search, which is the
	// whole point of the repair.
	res, err := st.VectorSearch(ctx, ns, vec(dims, 3), store.Filter{}, 5)
	if err != nil {
		t.Fatalf("vector search: %v", err)
	}
	if len(res) == 0 || res[0].Memory.ID != m.ID {
		t.Fatalf("repaired memory not found by vector search: %+v", res)
	}

	// enrich -> healthy.
	ok, err = rs.SetRepairState(ctx, ns, m.ID, row.Fingerprint, store.RepairNone)
	if err != nil {
		t.Fatalf("set repair state: %v", err)
	}
	if !ok {
		t.Fatal("SetRepairState reported no rows")
	}
	state, attempts, _, err := rs.RepairStateOf(ctx, ns, m.ID)
	if err != nil {
		t.Fatalf("state of: %v", err)
	}
	if state != store.RepairNone || attempts != 0 {
		t.Fatalf("after completion = (%q, %d), want (RepairNone, 0)", state, attempts)
	}
}

func testRepairMarkSkips(t *testing.T, st store.Store, rs store.RepairStore, _ int) {
	ctx := context.Background()
	ns := t.Name()
	m := repairSeed(t, st, rs, ns, "a", "already repairing")
	if _, err := rs.ClaimRepairs(ctx, store.RepairPending, time.Now(), time.Hour, 10); err != nil {
		t.Fatalf("claim: %v", err)
	}
	_, attemptsBefore, _, err := rs.RepairStateOf(ctx, ns, m.ID)
	if err != nil {
		t.Fatalf("state of: %v", err)
	}
	// A sweep must not reset the backoff of a row already mid-repair.
	n, err := rs.MarkRepairNeeded(ctx, ns, []string{m.ID}, store.RepairPending)
	if err != nil {
		t.Fatalf("mark repair: %v", err)
	}
	if n != 0 {
		t.Fatalf("MarkRepairNeeded moved %d rows already repairing, want 0", n)
	}
	_, attemptsAfter, _, err := rs.RepairStateOf(ctx, ns, m.ID)
	if err != nil {
		t.Fatalf("state of: %v", err)
	}
	if attemptsAfter != attemptsBefore {
		t.Fatalf("attempts = %d after a redundant mark, want %d", attemptsAfter, attemptsBefore)
	}
}

func testRepairClaimDue(t *testing.T, st store.Store, rs store.RepairStore, _ int) {
	ctx := context.Background()
	ns := t.Name()
	m := repairSeed(t, st, rs, ns, "a", "not due yet")

	// A long lease pushes it well into the future.
	if _, err := rs.ClaimRepairs(ctx, store.RepairPending, time.Now(), time.Hour, 10); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	again, err := rs.ClaimRepairs(ctx, store.RepairPending, time.Now(), time.Hour, 10)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	for _, r := range again {
		if r.ID == m.ID {
			t.Fatal("a leased row was re-claimed before its lease expired")
		}
	}
	// Expire the lease. ClaimRepairs' now argument cannot do this portably:
	// postgres deliberately ignores it and leases against the database clock
	// (see store.RepairStore), so a client-side fake clock is invisible there.
	// FailRepair writes the caller's next-run time on every backend and leaves
	// the attempt count alone, which is precisely the row a worker that died
	// mid-repair leaves behind once its lease runs out.
	if err := rs.FailRepair(ctx, ns, m.ID, "", time.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("expire lease: %v", err)
	}
	// Past the lease it becomes claimable again, with the attempt already
	// charged — the crash-recovery contract.
	later, err := rs.ClaimRepairs(ctx, store.RepairPending, time.Now(), time.Minute, 10)
	if err != nil {
		t.Fatalf("post-lease claim: %v", err)
	}
	found := false
	for _, r := range later {
		if r.ID == m.ID {
			found = true
			if r.Attempts != 2 {
				t.Fatalf("Attempts = %d after a lease expiry, want 2", r.Attempts)
			}
		}
	}
	if !found {
		t.Fatal("row was not re-claimable after its lease expired")
	}
}

func testRepairClaimLeases(t *testing.T, st store.Store, rs store.RepairStore, _ int) {
	ctx := context.Background()
	ns := t.Name()
	m := repairSeed(t, st, rs, ns, "a", "counts its attempts")

	claimed, err := rs.ClaimRepairs(ctx, store.RepairPending, time.Now(), time.Hour, 10)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	for _, r := range claimed {
		if r.ID == m.ID && r.Attempts != 1 {
			t.Fatalf("Attempts = %d on the first claim, want 1 (the claim charges its own run)", r.Attempts)
		}
	}
	state, attempts, _, err := rs.RepairStateOf(ctx, ns, m.ID)
	if err != nil {
		t.Fatalf("state of: %v", err)
	}
	if state != store.RepairPending {
		t.Fatalf("state = %q after a claim, want still pending", state)
	}
	if attempts != 1 {
		t.Fatalf("stored attempts = %d, want 1", attempts)
	}
}

func testRepairClaimState(t *testing.T, st store.Store, rs store.RepairStore, _ int) {
	ctx := context.Background()
	ns := t.Name()
	pending := repairSeed(t, st, rs, ns, "a", "needs a vector")
	enrich := repairSeed(t, st, rs, ns, "b", "needs enrichment")
	if ok, err := rs.SetRepairState(ctx, ns, enrich.ID, memory.Fingerprint(enrich.Content), store.RepairEnrich); err != nil || !ok {
		t.Fatalf("move to enrich: ok=%v err=%v", ok, err)
	}

	got, err := rs.ClaimRepairs(ctx, store.RepairEnrich, time.Now(), time.Minute, 10)
	if err != nil {
		t.Fatalf("claim enrich: %v", err)
	}
	for _, r := range got {
		if r.ID == pending.ID {
			t.Fatal("a pending row was returned by an enrich claim")
		}
	}
	found := false
	for _, r := range got {
		if r.ID == enrich.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("enrich claim did not return the enrich row")
	}
}

// testRepairClaimExclusive is the subtest that actually pins the claim's
// concurrency contract: sqlite's UPDATE ... RETURNING and Postgres's
// FOR UPDATE SKIP LOCKED must both hand every row to exactly one claimant.
func testRepairClaimExclusive(t *testing.T, st store.Store, rs store.RepairStore, _ int) {
	ctx := context.Background()
	ns := t.Name()
	const rows = 40
	want := map[string]bool{}
	for i := range rows {
		m := repairSeed(t, st, rs, ns, fmt.Sprintf("m%02d", i), fmt.Sprintf("row %d needs a vector", i))
		want[m.ID] = true
	}

	var mu sync.Mutex
	seen := map[string]int{}
	var wg sync.WaitGroup
	for range 6 {
		wg.Go(func() {
			for {
				// A long lease means a row claimed once must never come back.
				got, err := rs.ClaimRepairs(ctx, store.RepairPending, time.Now(), time.Hour, 3)
				if err != nil {
					t.Errorf("claim: %v", err)
					return
				}
				if len(got) == 0 {
					return
				}
				mu.Lock()
				for _, r := range got {
					seen[r.ID]++
				}
				mu.Unlock()
			}
		})
	}
	wg.Wait()

	for id := range want {
		switch seen[id] {
		case 1:
		case 0:
			t.Fatalf("row %s was never claimed", id)
		default:
			t.Fatalf("row %s was claimed %d times, want exactly 1", id, seen[id])
		}
	}
}

func testRepairFingerprintGuard(t *testing.T, st store.Store, rs store.RepairStore, dims int) {
	ctx := context.Background()
	ns := t.Name()
	m := repairSeed(t, st, rs, ns, "a", "original content")
	staleFP := memory.Fingerprint(m.Content)

	// Simulate a content edit landing while the repair's embed was in flight.
	edited, err := st.Get(ctx, ns, m.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	edited.Content = "rewritten content"
	edited.Embedding = vec(dims, 7)
	mustUpsert(t, st, edited)

	ok, err := rs.SetEmbeddingIfUnchanged(ctx, ns, m.ID, staleFP, vec(dims, 2), store.RepairEnrich)
	if err != nil {
		t.Fatalf("set embedding: %v", err)
	}
	if ok {
		t.Fatal("a stale-fingerprint repair was applied; it must be discarded")
	}
	// The concurrent writer's vector must survive untouched.
	got, err := st.GetEmbedding(ctx, ns, m.ID)
	if err != nil {
		t.Fatalf("get embedding: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("the concurrent writer's vector was destroyed by a rejected repair")
	}
	if ok, err := rs.SetRepairState(ctx, ns, m.ID, staleFP, store.RepairNone); err != nil || ok {
		t.Fatalf("SetRepairState with a stale fingerprint: ok=%v err=%v, want ok=false", ok, err)
	}
}

// testRepairKeepsUpdatedAt pins that a repair is index maintenance, not a
// logical edit: bumping updated_at would make a system re-embed win every
// "prefer the most recent" recency decision in recall and answer.
func testRepairKeepsUpdatedAt(t *testing.T, st store.Store, rs store.RepairStore, dims int) {
	ctx := context.Background()
	ns := t.Name()
	m := repairSeed(t, st, rs, ns, "a", "content whose updated_at must not move")

	before, err := st.Get(ctx, ns, m.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	fp := memory.Fingerprint(m.Content)
	if ok, err := rs.SetEmbeddingIfUnchanged(ctx, ns, m.ID, fp, vec(dims, 4), store.RepairEnrich); err != nil || !ok {
		t.Fatalf("set embedding: ok=%v err=%v", ok, err)
	}
	after, err := st.Get(ctx, ns, m.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("UpdatedAt moved from %v to %v across a repair; a system re-embed is not a logical edit",
			before.UpdatedAt, after.UpdatedAt)
	}
	if !after.CreatedAt.Equal(before.CreatedAt) {
		t.Fatalf("CreatedAt moved from %v to %v across a repair", before.CreatedAt, after.CreatedAt)
	}
}

// testRepairKeepsVector guards the trap that makes a Get-then-Upsert round trip
// lossy: advancing repair state must never be expressible as "drop the vector".
func testRepairKeepsVector(t *testing.T, st store.Store, rs store.RepairStore, dims int) {
	ctx := context.Background()
	ns := t.Name()
	m := mem(ns, "a", "already has a vector", vec(dims, 5))
	mustUpsert(t, st, m)
	if _, err := rs.MarkRepairNeeded(ctx, ns, []string{m.ID}, store.RepairEnrich); err != nil {
		t.Fatalf("mark: %v", err)
	}

	if ok, err := rs.SetRepairState(ctx, ns, m.ID, memory.Fingerprint(m.Content), store.RepairNone); err != nil || !ok {
		t.Fatalf("set repair state: ok=%v err=%v", ok, err)
	}
	got, err := st.GetEmbedding(ctx, ns, m.ID)
	if err != nil {
		t.Fatalf("get embedding: %v", err)
	}
	if len(got) != dims {
		t.Fatalf("embedding len = %d after a state-only transition, want %d preserved", len(got), dims)
	}
}

func testRepairFail(t *testing.T, st store.Store, rs store.RepairStore, _ int) {
	ctx := context.Background()
	ns := t.Name()
	m := repairSeed(t, st, rs, ns, "a", "fails to embed")
	if _, err := rs.ClaimRepairs(ctx, store.RepairPending, time.Now(), time.Minute, 10); err != nil {
		t.Fatalf("claim: %v", err)
	}

	retryAt := time.Now().Add(30 * time.Second)
	if err := rs.FailRepair(ctx, ns, m.ID, "provider returned 429", retryAt); err != nil {
		t.Fatalf("fail repair: %v", err)
	}
	state, attempts, lastErr, err := rs.RepairStateOf(ctx, ns, m.ID)
	if err != nil {
		t.Fatalf("state of: %v", err)
	}
	if state != store.RepairPending {
		t.Fatalf("state = %q after a recoverable failure, want still pending", state)
	}
	// The attempt was charged at claim time; failing must not charge it twice,
	// or a crashed run and a failed run would cost differently.
	if attempts != 1 {
		t.Fatalf("attempts = %d after FailRepair, want 1 (charged at claim, not at failure)", attempts)
	}
	if lastErr != "provider returned 429" {
		t.Fatalf("lastErr = %q, want the recorded provider error", lastErr)
	}
	notYet, err := rs.ClaimRepairs(ctx, store.RepairPending, time.Now(), time.Minute, 10)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	for _, r := range notYet {
		if r.ID == m.ID {
			t.Fatal("a backed-off row was claimed before its retry time")
		}
	}
}

func testRepairPark(t *testing.T, st store.Store, rs store.RepairStore, _ int) {
	ctx := context.Background()
	ns := t.Name()
	m := repairSeed(t, st, rs, ns, "a", "poison content")

	if err := rs.ParkRepair(ctx, ns, m.ID, "content rejected by the provider", time.Now()); err != nil {
		t.Fatalf("park: %v", err)
	}
	state, _, lastErr, err := rs.RepairStateOf(ctx, ns, m.ID)
	if err != nil {
		t.Fatalf("state of: %v", err)
	}
	if state != store.RepairFailed {
		t.Fatalf("state = %q after parking, want %q", state, store.RepairFailed)
	}
	if lastErr == "" {
		t.Fatal("parking discarded the last error; retaining it is the point of parking")
	}
	// Far in the future, so this is about state exclusion rather than backoff.
	got, err := rs.ClaimRepairs(ctx, store.RepairPending, time.Now().Add(365*24*time.Hour), time.Minute, 10)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	for _, r := range got {
		if r.ID == m.ID {
			t.Fatal("a parked row was claimed; parking must exclude it until rearm")
		}
	}
}

func testRepairRearm(t *testing.T, st store.Store, rs store.RepairStore, _ int) {
	ctx := context.Background()
	ns := t.Name()
	m := repairSeed(t, st, rs, ns, "a", "parked then rearmed")
	parkedAt := time.Now().Add(-2 * time.Hour)
	if err := rs.ParkRepair(ctx, ns, m.ID, "embedder down", parkedAt); err != nil {
		t.Fatalf("park: %v", err)
	}

	// Too recent to rearm.
	n, err := rs.RearmRepairs(ctx, time.Now().Add(-3*time.Hour), time.Now())
	if err != nil {
		t.Fatalf("rearm: %v", err)
	}
	if n != 0 {
		t.Fatalf("rearmed %d rows parked more recently than the window, want 0", n)
	}

	n, err = rs.RearmRepairs(ctx, time.Now().Add(-time.Hour), time.Now())
	if err != nil {
		t.Fatalf("rearm: %v", err)
	}
	if n < 1 {
		t.Fatalf("rearmed %d rows, want at least 1", n)
	}
	state, attempts, _, err := rs.RepairStateOf(ctx, ns, m.ID)
	if err != nil {
		t.Fatalf("state of: %v", err)
	}
	if state != store.RepairPending {
		t.Fatalf("state = %q after rearm, want pending", state)
	}
	if attempts != 0 {
		t.Fatalf("attempts = %d after rearm, want 0 (a rearm is a fresh budget)", attempts)
	}
}

func testRepairStats(t *testing.T, st store.Store, rs store.RepairStore, _ int) {
	ctx := context.Background()
	ns := t.Name()
	repairSeed(t, st, rs, ns, "a", "pending one")
	repairSeed(t, st, rs, ns, "b", "pending two")
	parked := repairSeed(t, st, rs, ns, "c", "parked one")
	if err := rs.ParkRepair(ctx, ns, parked.ID, "boom", time.Now()); err != nil {
		t.Fatalf("park: %v", err)
	}

	stats, err := rs.RepairStats(ctx)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	byState := map[store.RepairState]store.RepairStat{}
	for _, s := range stats {
		byState[s.State] = s
	}
	if got := byState[store.RepairPending].Count; got < 2 {
		t.Fatalf("pending count = %d, want at least 2", got)
	}
	if got := byState[store.RepairFailed].Count; got < 1 {
		t.Fatalf("failed count = %d, want at least 1", got)
	}
	if byState[store.RepairFailed].LastError == "" {
		t.Fatal("failed bucket carries no LastError; operators need it to diagnose a stuck repair")
	}
	if byState[store.RepairPending].OldestAt.IsZero() {
		t.Fatal("pending bucket carries no OldestAt; it is the backlog-is-growing signal")
	}
	if _, ok := byState[store.RepairNone]; ok {
		t.Fatal("stats reported a bucket for healthy rows; only outstanding work belongs here")
	}
}

// testRepairClearsMarker pins that finishing a repair also strips the legacy
// metadata.pending_embed marker.
//
// Leaving it is not cosmetic. Memory.PendingEmbed reads that marker, so stats,
// doctor and the UI badge would keep reporting a fully repaired memory as
// degraded — and the sweeper's compat scan re-adopts any row carrying it, so the
// row would be re-claimed and re-embedded every tick, forever.
func testRepairClearsMarker(t *testing.T, st store.Store, rs store.RepairStore, dims int) {
	ctx := context.Background()
	ns := t.Name()
	m := mem(ns, "a", "carries the legacy marker", nil)
	m.Metadata = map[string]any{memory.PendingEmbedKey: memory.PendingEmbedValue}
	mustUpsert(t, st, m)
	if _, err := rs.MarkRepairNeeded(ctx, ns, []string{m.ID}, store.RepairPending); err != nil {
		t.Fatalf("mark: %v", err)
	}
	fp := memory.Fingerprint(m.Content)

	// pending -> enrich keeps the marker: the row still owes work.
	if ok, err := rs.SetEmbeddingIfUnchanged(ctx, ns, m.ID, fp, vec(dims, 2), store.RepairEnrich); err != nil || !ok {
		t.Fatalf("set embedding: ok=%v err=%v", ok, err)
	}
	mid, err := st.Get(ctx, ns, m.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !mid.PendingEmbed() {
		t.Fatal("marker cleared at the enrich stage; the row still owes its enrichment")
	}

	// enrich -> healthy must clear it.
	if ok, err := rs.SetRepairState(ctx, ns, m.ID, fp, store.RepairNone); err != nil || !ok {
		t.Fatalf("set repair state: ok=%v err=%v", ok, err)
	}
	done, err := st.Get(ctx, ns, m.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if done.PendingEmbed() {
		t.Fatalf("a fully repaired memory still reports PendingEmbed (metadata=%v); stats, doctor "+
			"and the UI would show it as degraded forever, and the sweeper would re-adopt it every tick",
			done.Metadata)
	}
	if _, ok := done.Metadata[memory.PendingEmbedKey]; ok {
		t.Fatalf("legacy marker survived the repair: %v", done.Metadata)
	}
}

func testRepairStateOfMissing(t *testing.T, rs store.RepairStore) {
	_, _, _, err := rs.RepairStateOf(context.Background(), t.Name(), "no-such-memory")
	if err == nil {
		t.Fatal("RepairStateOf on a missing memory returned no error, want ErrNotFound")
	}
}
