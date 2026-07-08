package service_test

import (
	"context"
	"testing"
	"time"

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
