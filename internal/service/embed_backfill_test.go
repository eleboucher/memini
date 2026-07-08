package service_test

import (
	"context"
	"testing"
	"time"

	"github.com/eleboucher/memini/internal/embed"
	"github.com/eleboucher/memini/internal/embed/embedtest"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/service"
	"github.com/eleboucher/memini/internal/store"
)

// seedPendingEmbed writes n vectorless memories (embedder down, write embed
// budget set) each stamped metadata pending_embed="true" -- the same shape
// embedForRemember produces on a degraded write -- and returns their ids.
func seedPendingEmbed(t *testing.T, st store.Store, n int, contents ...string) []string {
	t.Helper()
	degraded := service.New(st, errEmbedder{dims: dims}, service.WithSyncReinforce(),
		service.WithWriteEmbedTimeout(time.Second))
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		content := contents[i]
		got, err := degraded.Remember(context.Background(), service.RememberInput{
			Namespace: "alice", Content: content, Tier: memory.TierSemantic,
		})
		if err != nil {
			t.Fatalf("seed pending row %d: %v", i, err)
		}
		if got.Metadata["pending_embed"] != "true" {
			t.Fatalf("seed row %d: pending_embed not set", i)
		}
		ids = append(ids, got.ID)
	}
	return ids
}

// TestBackfillEmbeddingsHealthyEmbedderClearsQueue confirms one
// BackfillEmbeddings tick, run with a healthy embedder, re-embeds every
// pending row: the vector is set, metadata.pending_embed is stripped, the row
// is now findable by vector search, and the pending gauge lands on 0.
func TestBackfillEmbeddingsHealthyEmbedderClearsQueue(t *testing.T) {
	st := openTestStore(t)
	ids := seedPendingEmbed(t, st, 2,
		"the deploy key rotates every 90 days",
		"the staging database lives in us-east-1")

	m := &countingMetrics{}
	svc := service.New(st, embedtest.New(dims), service.WithSyncReinforce(), service.WithMetrics(m))

	n, err := svc.BackfillEmbeddings(context.Background())
	if err != nil {
		t.Fatalf("BackfillEmbeddings: %v", err)
	}
	if n != 2 {
		t.Fatalf("BackfillEmbeddings backfilled = %d, want 2", n)
	}

	mems, err := st.List(context.Background(), "alice", store.Filter{}, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	// List omits embeddings by design (see promote.go's stampPromoted), so the
	// vector itself is verified below via VectorSearch; here we only check the
	// pending_embed flag was cleared.
	seen := map[string]bool{}
	for _, m := range mems {
		seen[m.ID] = true
		if id0or1(ids, m.ID) {
			if _, ok := m.Metadata["pending_embed"]; ok {
				t.Fatalf("row %s: pending_embed still set after backfill", m.ID)
			}
		}
	}
	for _, id := range ids {
		if !seen[id] {
			t.Fatalf("row %s missing after backfill", id)
		}
	}

	// Vector search now finds the backfilled rows: before backfill a
	// vectorless row cannot surface from a vector query at all.
	qvec, err := embedtest.New(dims).Embed(context.Background(), []string{"deploy key rotates"})
	if err != nil {
		t.Fatalf("embed query: %v", err)
	}
	hits, err := st.VectorSearch(context.Background(), "alice", qvec[0], store.Filter{}, 5)
	if err != nil {
		t.Fatalf("vector search: %v", err)
	}
	found := false
	for _, h := range hits {
		if h.Memory.ID == ids[0] {
			found = true
		}
	}
	if !found {
		t.Fatal("vector search did not find the backfilled row")
	}

	if m.embedBackfillPending != 0 {
		t.Fatalf("EmbedBackfillPending = %d, want 0", m.embedBackfillPending)
	}
	if m.embedBackfillCalls != 1 {
		t.Fatalf("EmbedBackfillPending called %d times, want 1", m.embedBackfillCalls)
	}
}

func id0or1(ids []string, id string) bool {
	for _, want := range ids {
		if want == id {
			return true
		}
	}
	return false
}

// TestBackfillEmbeddingsEmbedderDownLeavesRowsPending confirms that when the
// embedder is still down, a tick aborts after the first failed row (no error
// spam probing every pending row against a dead backend), leaves every row
// pending, returns no error, and reports the full backlog on the gauge.
func TestBackfillEmbeddingsEmbedderDownLeavesRowsPending(t *testing.T) {
	st := openTestStore(t)
	ids := seedPendingEmbed(t, st, 2,
		"the deploy key rotates every 90 days",
		"the staging database lives in us-east-1")

	m := &countingMetrics{}
	svc := service.New(st, errEmbedder{dims: dims}, service.WithSyncReinforce(), service.WithMetrics(m))

	n, err := svc.BackfillEmbeddings(context.Background())
	if err != nil {
		t.Fatalf("BackfillEmbeddings should not hard-fail on a down embedder: %v", err)
	}
	if n != 0 {
		t.Fatalf("BackfillEmbeddings backfilled = %d, want 0", n)
	}

	mems, err := st.List(context.Background(), "alice", store.Filter{}, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, mm := range mems {
		if !id0or1(ids, mm.ID) {
			continue
		}
		if mm.Metadata["pending_embed"] != "true" {
			t.Fatalf("row %s: pending_embed cleared despite embedder being down", mm.ID)
		}
	}

	if m.embedBackfillPending != 2 {
		t.Fatalf("EmbedBackfillPending = %d, want 2 (full backlog)", m.embedBackfillPending)
	}
}

// TestBackfillEmbeddingsBoundsEmbedWithWriteTimeout confirms each row's embed
// is bounded by the write-embed timeout: a slow-but-not-erroring embedder (a
// network stall, exactly the degraded scenario backfill exists to recover
// from) must not hang the tick. With WithWriteEmbedTimeout(50ms) and an
// embedder that blocks for 10s unless its ctx is cancelled, one tick must
// return promptly, leave the row pending, and report the backlog.
func TestBackfillEmbeddingsBoundsEmbedWithWriteTimeout(t *testing.T) {
	st := openTestStore(t)
	ids := seedPendingEmbed(t, st, 1, "the deploy key rotates every 90 days")

	m := &countingMetrics{}
	svc := service.New(st, slowEmbedder{d: 10 * time.Second}, service.WithSyncReinforce(),
		service.WithWriteEmbedTimeout(50*time.Millisecond), service.WithMetrics(m))

	start := time.Now()
	n, err := svc.BackfillEmbeddings(context.Background())
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("BackfillEmbeddings should not hard-fail on a stalled embedder: %v", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("BackfillEmbeddings took %v; the row embed is not bounded by the write-embed timeout", elapsed)
	}
	if n != 0 {
		t.Fatalf("BackfillEmbeddings backfilled = %d, want 0", n)
	}

	mems, err := st.List(context.Background(), "alice", store.Filter{}, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, mm := range mems {
		if mm.ID == ids[0] && mm.Metadata["pending_embed"] != "true" {
			t.Fatalf("row %s: pending_embed cleared despite the embed timing out", mm.ID)
		}
	}
	if m.embedBackfillPending != 1 {
		t.Fatalf("EmbedBackfillPending = %d, want 1 (backlog)", m.embedBackfillPending)
	}
}

// hookEmbedder calls fn synchronously before delegating to the wrapped
// embedder, so a test can perform a concurrent mutation exactly while a
// backfill row's embed is in flight.
type hookEmbedder struct {
	embed.Embedder
	fn func()
}

func (h hookEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	h.fn()
	return h.Embedder.Embed(ctx, texts)
}

// TestBackfillEmbeddingsSkipsRowUpdatedConcurrently confirms the re-Get/
// UpdatedAt guard: if a memory_update lands on a pending row while its
// backfill embed is in flight (up to writeEmbedTimeout), the backfill must
// not clobber the concurrent write with a vector for the stale content --
// it detects the UpdatedAt mismatch and skips the row entirely, leaving the
// updated content and (if the concurrent write had a healthy embedder) its
// own fresh vector intact.
//
// A shared, monotonically-increasing fake clock (rather than wall time)
// guarantees the seed write, the concurrent update, and the backfill's own
// stamps land at distinct instants, so the UpdatedAt comparison can't
// coincidentally collide at millisecond storage precision.
func TestBackfillEmbeddingsSkipsRowUpdatedConcurrently(t *testing.T) {
	st := openTestStore(t)

	var tick int64
	clock := func() time.Time {
		tick++
		return time.Unix(0, tick*int64(time.Millisecond))
	}

	degraded := service.New(st, errEmbedder{dims: dims}, service.WithSyncReinforce(),
		service.WithWriteEmbedTimeout(time.Second), service.WithClock(clock))
	seeded, err := degraded.Remember(context.Background(), service.RememberInput{
		Namespace: "alice", Content: "the deploy key rotates every 90 days", Tier: memory.TierSemantic,
	})
	if err != nil {
		t.Fatalf("seed pending row: %v", err)
	}
	if seeded.Metadata["pending_embed"] != "true" {
		t.Fatal("seed row: pending_embed not set")
	}
	id := seeded.ID

	// The concurrent writer: a healthy-embedder service that updates the row
	// mid-backfill-embed, changing its content and clearing pending_embed
	// (per Fix 1) via its own successful embed.
	updater := service.New(st, embedtest.New(dims), service.WithSyncReinforce(), service.WithClock(clock))
	fired := false
	he := hookEmbedder{Embedder: embedtest.New(dims), fn: func() {
		if fired {
			return
		}
		fired = true
		if _, err := updater.Remember(context.Background(), service.RememberInput{
			Namespace: "alice", ID: id, Content: "the deploy key now rotates every 30 days",
			Tier: memory.TierSemantic,
		}); err != nil {
			t.Errorf("concurrent update: %v", err)
		}
	}}

	m := &countingMetrics{}
	svc := service.New(st, he, service.WithSyncReinforce(), service.WithMetrics(m), service.WithClock(clock))

	n, err := svc.BackfillEmbeddings(context.Background())
	if err != nil {
		t.Fatalf("BackfillEmbeddings: %v", err)
	}
	if n != 0 {
		t.Fatalf("BackfillEmbeddings backfilled = %d, want 0 (the only row changed concurrently and must be skipped)", n)
	}

	got, err := st.Get(context.Background(), "alice", id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Content != "the deploy key now rotates every 30 days" {
		t.Fatalf("content = %q, want the concurrent update's content preserved (no stale overwrite)", got.Content)
	}
	if _, ok := got.Metadata["pending_embed"]; ok {
		t.Fatalf("pending_embed still set = %v, want cleared by the concurrent update's own healthy embed", got.Metadata["pending_embed"])
	}

	// Get omits the embedding by design; confirm the concurrent update's own
	// vector (not a backfill-produced one for stale content) is live via
	// VectorSearch, the same way TestBackfillEmbeddingsHealthyEmbedderClearsQueue
	// verifies a successful backfill.
	qvec, err := embedtest.New(dims).Embed(context.Background(), []string{"deploy key now rotates every 30 days"})
	if err != nil {
		t.Fatalf("embed query: %v", err)
	}
	hits, err := st.VectorSearch(context.Background(), "alice", qvec[0], store.Filter{}, 5)
	if err != nil {
		t.Fatalf("vector search: %v", err)
	}
	found := false
	for _, h := range hits {
		if h.Memory.ID == id {
			found = true
		}
	}
	if !found {
		t.Fatal("vector search did not find the row; want the concurrent update's own vector preserved")
	}
}

// TestRunEmbedBackfillNoIntervalReturnsImmediately confirms RunEmbedBackfill
// is a no-op with interval<=0: it must return promptly instead of blocking
// forever on a ticker that never fires.
func TestRunEmbedBackfillNoIntervalReturnsImmediately(t *testing.T) {
	st := openTestStore(t)
	svc := service.New(st, embedtest.New(dims), service.WithSyncReinforce())

	done := make(chan struct{})
	go func() {
		svc.RunEmbedBackfill(context.Background(), 0)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunEmbedBackfill(interval<=0) did not return immediately")
	}
}
