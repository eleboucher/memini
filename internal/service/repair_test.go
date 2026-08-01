package service_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eleboucher/memini/internal/embed/embedtest"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/service"
	"github.com/eleboucher/memini/internal/store"
	"github.com/eleboucher/memini/internal/store/sqlitevec"
)

// openRepairStore opens a real sqlite store at a fixed path. Repair tests must
// NOT use the shared wrapper doubles: Go promotes the embedded interface's
// method set, not the dynamic value's, so a wrapper embedding store.Store does
// not satisfy store.RepairStore — a repair test built on one would silently
// exercise the no-repair fallback while looking like it tested the queue.
func openRepairStore(t *testing.T, path string) *sqlitevec.Store {
	t.Helper()
	st, err := sqlitevec.Open(context.Background(), path, dims)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// TestDegradedWriteIsClaimableImmediately pins the property the whole design is
// for: a write that could not reach the embedder commits its own "needs repair"
// state in the same transaction, so there is no enqueue that could be lost and
// the row is claimable the instant it lands.
func TestDegradedWriteIsClaimableImmediately(t *testing.T) {
	ctx := context.Background()
	st := openRepairStore(t, filepath.Join(t.TempDir(), "m.db"))
	svc := service.New(st, errEmbedder{dims: dims}, service.WithSyncReinforce(),
		service.WithWriteEmbedTimeout(time.Second))

	got, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "alice", Content: "the deploy key rotates every 90 days", Tier: memory.TierSemantic,
	})
	if err != nil {
		t.Fatalf("remember should degrade, not error: %v", err)
	}
	if got.EmbedState != string(store.RepairPending) {
		t.Fatalf("EmbedState = %q, want %q", got.EmbedState, store.RepairPending)
	}

	state, _, _, err := st.RepairStateOf(ctx, "alice", got.ID)
	if err != nil {
		t.Fatalf("repair state: %v", err)
	}
	if state != store.RepairPending {
		t.Fatalf("stored repair state = %q, want pending", state)
	}
	claimed, err := st.ClaimRepairs(ctx, store.RepairPending, time.Now(), time.Minute, 10)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(claimed) != 1 || claimed[0].ID != got.ID {
		t.Fatalf("claimed %+v, want exactly the degraded write", claimed)
	}
}

// TestHealthyWriteOwesNoRepair guards the other direction: a normal write must
// not enter the queue, or the worker would re-embed the whole corpus.
func TestHealthyWriteOwesNoRepair(t *testing.T) {
	ctx := context.Background()
	st := openRepairStore(t, filepath.Join(t.TempDir(), "m.db"))
	svc := service.New(st, embedtest.New(dims), service.WithSyncReinforce(),
		service.WithWriteEmbedTimeout(time.Second))

	got, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "alice", Content: "a perfectly healthy fact", Tier: memory.TierSemantic,
	})
	if err != nil {
		t.Fatalf("remember: %v", err)
	}
	if got.EmbedState != "" {
		t.Fatalf("EmbedState = %q on a healthy write, want empty", got.EmbedState)
	}
	claimed, err := st.ClaimRepairs(ctx, store.RepairPending, time.Now(), time.Minute, 10)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(claimed) != 0 {
		t.Fatalf("claimed %d rows after a healthy write, want 0", len(claimed))
	}
}

// TestDrainRepairsRestoresVectorAndClearsState walks a degraded write all the
// way back to healthy against a recovered embedder.
func TestDrainRepairsRestoresVectorAndClearsState(t *testing.T) {
	ctx := context.Background()
	st := openRepairStore(t, filepath.Join(t.TempDir(), "m.db"))

	degraded := service.New(st, errEmbedder{dims: dims}, service.WithSyncReinforce(),
		service.WithWriteEmbedTimeout(time.Second))
	got, err := degraded.Remember(ctx, service.RememberInput{
		Namespace: "alice", Content: "kubernetes upgrades go through the tuppr controller",
		Tier: memory.TierSemantic,
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	healthy := service.New(st, embedtest.New(dims), service.WithSyncReinforce(),
		service.WithWriteEmbedTimeout(time.Second))
	if _, err := healthy.DrainRepairs(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}
	healthy.WaitBackground()

	vec, err := st.GetEmbedding(ctx, "alice", got.ID)
	if err != nil {
		t.Fatalf("get embedding: %v", err)
	}
	if len(vec) != dims {
		t.Fatalf("embedding len = %d after repair, want %d", len(vec), dims)
	}
	state, _, _, err := st.RepairStateOf(ctx, "alice", got.ID)
	if err != nil {
		t.Fatalf("repair state: %v", err)
	}
	if state != store.RepairNone {
		t.Fatalf("repair state = %q after a full drain, want cleared", state)
	}

	// The repaired memory must now be reachable semantically, which is the
	// user-visible point of the whole exercise.
	res, err := healthy.Recall(ctx, service.RecallInput{
		Namespace: "alice", Query: "tuppr controller upgrades", Limit: 5,
	})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	found := false
	for _, r := range res {
		if r.Memory.ID == got.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("repaired memory not returned by recall")
	}
}

// TestRepairSurvivesStoreRestart is the test that distinguishes this design from
// the in-memory consolidate queue: the repair state is a committed column, so
// killing the process between the degraded write and the repair loses nothing.
func TestRepairSurvivesStoreRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "restart.db")

	first, err := sqlitevec.Open(ctx, path, dims)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	degraded := service.New(first, errEmbedder{dims: dims}, service.WithSyncReinforce(),
		service.WithWriteEmbedTimeout(time.Second))
	got, err := degraded.Remember(ctx, service.RememberInput{
		Namespace: "alice", Content: "a fact written while the embedder was down",
		Tier: memory.TierSemantic,
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Hard stop, as if the process had been killed mid-repair.
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	second := openRepairStore(t, path)
	recovered := service.New(second, embedtest.New(dims), service.WithSyncReinforce(),
		service.WithWriteEmbedTimeout(time.Second))
	if _, err := recovered.DrainRepairs(ctx); err != nil {
		t.Fatalf("drain after restart: %v", err)
	}
	recovered.WaitBackground()

	vec, err := second.GetEmbedding(ctx, "alice", got.ID)
	if err != nil {
		t.Fatalf("get embedding: %v", err)
	}
	if len(vec) != dims {
		t.Fatalf("embedding len = %d after restart+repair, want %d", len(vec), dims)
	}
}

// TestRepairKickWakesWorkerWithoutWaitingForTheTick proves the latency comes
// from the kick rather than the poll interval: with an hour-long tick, a
// degraded write must still be repaired within seconds.
func TestRepairKickWakesWorkerWithoutWaitingForTheTick(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st := openRepairStore(t, filepath.Join(t.TempDir(), "m.db"))

	svc := service.New(st, embedtest.New(dims), service.WithSyncReinforce(),
		service.WithWriteEmbedTimeout(time.Nanosecond))
	go svc.RunRepairWorker(ctx, time.Hour)
	t.Cleanup(func() { cancel(); svc.WaitBackground() })

	// A one-nanosecond write budget guarantees the inline embed misses.
	got, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "alice", Content: "repaired by the kick, not the tick", Tier: memory.TierSemantic,
	})
	if err != nil {
		t.Fatalf("remember: %v", err)
	}
	if got.EmbedState == "" {
		t.Skip("write did not degrade; the embed beat a 1ns budget")
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		vec, err := st.GetEmbedding(ctx, "alice", got.ID)
		if err != nil {
			t.Fatalf("get embedding: %v", err)
		}
		if len(vec) == dims {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("degraded write was not repaired within 10s despite the kick (poll interval is 1h)")
}

// TestRepairSkipsRowThatAlreadyHasAVector guards against wasting an embedder
// call on a phantom marker. Several write paths (import, reembed, dedup merge,
// promotion) hand a row a good vector without clearing the legacy pending flag,
// and without this check every one of them would cost a pointless round trip.
func TestRepairSkipsRowThatAlreadyHasAVector(t *testing.T) {
	ctx := context.Background()
	st := openRepairStore(t, filepath.Join(t.TempDir(), "m.db"))
	svc := service.New(st, embedtest.New(dims), service.WithSyncReinforce())

	healthy, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "alice", Content: "has a vector but is wrongly marked", Tier: memory.TierSemantic,
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := st.MarkRepairNeeded(ctx, "alice", []string{healthy.ID}, store.RepairPending); err != nil {
		t.Fatalf("mark: %v", err)
	}

	// A dead embedder: if the repair tried to embed, the row would fail and
	// stay pending instead of advancing.
	repairer := service.New(st, errEmbedder{dims: dims}, service.WithSyncReinforce())
	if _, err := repairer.DrainRepairs(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}
	repairer.WaitBackground()

	state, _, _, err := st.RepairStateOf(ctx, "alice", healthy.ID)
	if err != nil {
		t.Fatalf("repair state: %v", err)
	}
	if state != store.RepairNone {
		t.Fatalf("repair state = %q, want cleared without an embedder call", state)
	}
	vec, err := st.GetEmbedding(ctx, "alice", healthy.ID)
	if err != nil || len(vec) != dims {
		t.Fatalf("vector len = %d (err %v), want the original %d preserved", len(vec), err, dims)
	}
}

// TestRepairParksAfterTheAttemptCeiling pins the poison-pill contract: a row
// that keeps failing is retained with its error and excluded from claims rather
// than either dropped or retried forever.
func TestRepairParksAfterTheAttemptCeiling(t *testing.T) {
	ctx := context.Background()
	st := openRepairStore(t, filepath.Join(t.TempDir(), "m.db"))
	svc := service.New(st, errEmbedder{dims: dims}, service.WithSyncReinforce(),
		service.WithWriteEmbedTimeout(time.Second))

	got, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "alice", Content: "content the provider will never accept", Tier: memory.TierSemantic,
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Drive it past the ceiling. Each pass claims (which charges an attempt) and
	// then fails; the claim clock advances every pass so the previous pass's
	// lease has expired and the row is due again.
	clock := time.Now()
	for range 20 {
		clock = clock.Add(time.Hour)
		rows, err := st.ClaimRepairs(ctx, store.RepairPending, clock, time.Minute, 10)
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		if len(rows) == 0 {
			break
		}
		for _, r := range rows {
			if r.Attempts >= 12 {
				if perr := st.ParkRepair(ctx, r.Namespace, r.ID, "provider rejected the content", clock); perr != nil {
					t.Fatalf("park: %v", perr)
				}
				continue
			}
			if ferr := st.FailRepair(ctx, r.Namespace, r.ID, "provider rejected the content", clock); ferr != nil {
				t.Fatalf("fail: %v", ferr)
			}
		}
	}

	state, _, lastErr, err := st.RepairStateOf(ctx, "alice", got.ID)
	if err != nil {
		t.Fatalf("repair state: %v", err)
	}
	if state != store.RepairFailed {
		t.Fatalf("repair state = %q after exceeding the ceiling, want failed", state)
	}
	if lastErr == "" {
		t.Fatal("parked row kept no error; retaining the diagnosis is the point of parking")
	}

	// And the sweeper's circuit breaker re-arms it after its rest.
	n, err := st.RearmRepairs(ctx, clock.Add(time.Hour), time.Now())
	if err != nil {
		t.Fatalf("rearm: %v", err)
	}
	if n != 1 {
		t.Fatalf("re-armed %d rows, want 1", n)
	}
}

// TestSweepAdoptsLegacyPendingMarker is the upgrade path: a database written by
// a release predating the repair columns carries only the metadata flag, and
// only a sweep can find it.
func TestSweepAdoptsLegacyPendingMarker(t *testing.T) {
	ctx := context.Background()
	st := openRepairStore(t, filepath.Join(t.TempDir(), "m.db"))

	// Write straight through the store, as an older binary would have.
	legacy := &memory.Memory{
		ID: "legacy-1", Namespace: "alice", Tier: memory.TierSemantic,
		Content: "written before the repair columns existed",
		Metadata: map[string]any{
			memory.PendingEmbedKey: memory.PendingEmbedValue,
		},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(), LastAccessedAt: time.Now().UTC(),
	}
	if err := st.Upsert(ctx, legacy); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}
	if state, _, _, err := st.RepairStateOf(ctx, "alice", legacy.ID); err != nil || state != store.RepairNone {
		t.Fatalf("legacy row starts with state %q (err %v), want empty", state, err)
	}

	svc := service.New(st, embedtest.New(dims), service.WithSyncReinforce())
	if err := svc.SweepRepairs(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	state, _, _, err := st.RepairStateOf(ctx, "alice", legacy.ID)
	if err != nil {
		t.Fatalf("repair state: %v", err)
	}
	if state != store.RepairPending {
		t.Fatalf("legacy row state = %q after a sweep, want adopted as pending", state)
	}

	if _, err := svc.DrainRepairs(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}
	svc.WaitBackground()
	vec, err := st.GetEmbedding(ctx, "alice", legacy.ID)
	if err != nil || len(vec) != dims {
		t.Fatalf("legacy row vector len = %d (err %v), want %d", len(vec), err, dims)
	}
}

// TestRepairDoesNotBlockOnAPoisonRow is the head-of-line-blocking regression
// guard. The loop this replaces aborted the entire tick when the FIRST row
// failed, so one permanently-unembeddable memory wedged every other repair
// forever. Here a failing row must not stop its neighbours from being repaired.
func TestRepairDoesNotBlockOnAPoisonRow(t *testing.T) {
	ctx := context.Background()
	st := openRepairStore(t, filepath.Join(t.TempDir(), "m.db"))

	degraded := service.New(st, errEmbedder{dims: dims}, service.WithSyncReinforce(),
		service.WithWriteEmbedTimeout(time.Second))
	contents := []string{"poison row sorts first", "healthy row two", "healthy row three"}
	ids := make([]string, 0, len(contents))
	for _, c := range contents {
		m, err := degraded.Remember(ctx, service.RememberInput{
			Namespace: "alice", Content: c, Tier: memory.TierSemantic,
		})
		if err != nil {
			t.Fatalf("seed %q: %v", c, err)
		}
		ids = append(ids, m.ID)
	}

	// An embedder that fails only the poison row's content.
	svc := service.New(st, selectiveEmbedder{dims: dims, failOn: "poison row sorts first"},
		service.WithSyncReinforce())
	if _, err := svc.DrainRepairs(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}
	svc.WaitBackground()

	for _, id := range ids[1:] {
		vec, err := st.GetEmbedding(ctx, "alice", id)
		if err != nil {
			t.Fatalf("get embedding %s: %v", id, err)
		}
		if len(vec) != dims {
			t.Fatalf("row %s was not repaired (len %d); one poison row must not block its neighbours",
				id, len(vec))
		}
	}
}

// selectiveEmbedder fails only for one exact input, so a test can seed a
// "poison" row alongside healthy ones and prove the poison row does not block
// them.
type selectiveEmbedder struct {
	dims   int
	failOn string
}

func (e selectiveEmbedder) Dims() int { return e.dims }

func (e selectiveEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, 0, len(texts))
	for _, txt := range texts {
		if strings.Contains(txt, e.failOn) {
			return nil, errors.New("selectiveEmbedder: refusing this content")
		}
		v := make([]float32, e.dims)
		for i := range v {
			v[i] = float32(len(txt)%7) + 0.1
		}
		out = append(out, v)
	}
	return out, nil
}
