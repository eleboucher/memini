package service_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/eleboucher/memini/internal/embed/embedtest"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/service"
	"github.com/eleboucher/memini/internal/store"
)

// TestRememberEmbedErrorStoresVectorless confirms that with a write embed
// budget set, an embedder error degrades Remember to storing the memory
// without a vector (keyword-searchable only) instead of failing the write:
// the result carries metadata pending_embed="true", RememberDegraded("embed_error")
// is recorded, and the memory is still findable via keyword recall.
func TestRememberEmbedErrorStoresVectorless(t *testing.T) {
	st := openTestStore(t)
	m := &countingMetrics{}
	svc := service.New(st, errEmbedder{dims: dims}, service.WithSyncReinforce(),
		service.WithWriteEmbedTimeout(time.Second), service.WithRecallEmbedTimeout(time.Second),
		service.WithMetrics(m))

	got, err := svc.Remember(context.Background(), service.RememberInput{
		Namespace: "alice", Content: "the deploy key rotates every 90 days", Tier: memory.TierSemantic,
	})
	if err != nil {
		t.Fatalf("remember should degrade, not error: %v", err)
	}
	if got == nil {
		t.Fatal("remember returned a nil memory on a degraded write")
	}
	if len(got.Embedding) != 0 {
		t.Fatalf("Embedding = %v, want empty on a degraded write", got.Embedding)
	}
	if got.Metadata["pending_embed"] != "true" {
		t.Fatalf("Metadata[pending_embed] = %v, want \"true\"", got.Metadata["pending_embed"])
	}
	if m.rememberDegraded["embed_error"] != 1 {
		t.Fatalf("RememberDegraded(embed_error) = %d, want 1", m.rememberDegraded["embed_error"])
	}

	res, err := svc.Recall(context.Background(), service.RecallInput{
		Namespace: "alice", Query: "deploy key rotates", Limit: 5,
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
		t.Fatal("keyword recall did not find the vectorless write")
	}
}

// TestRememberEmbedTimeoutStoresVectorless mirrors the above with a slow
// embedder that exceeds the write embed budget, confirming the timeout path
// records reason "embed_timeout".
func TestRememberEmbedTimeoutStoresVectorless(t *testing.T) {
	st := openTestStore(t)
	m := &countingMetrics{}
	svc := service.New(st, slowEmbedder{d: 2 * time.Second}, service.WithSyncReinforce(),
		service.WithWriteEmbedTimeout(50*time.Millisecond), service.WithMetrics(m))

	got, err := svc.Remember(context.Background(), service.RememberInput{
		Namespace: "alice", Content: "hello world", Tier: memory.TierSemantic,
	})
	if err != nil {
		t.Fatalf("remember should degrade, not error: %v", err)
	}
	if got.Metadata["pending_embed"] != "true" {
		t.Fatalf("Metadata[pending_embed] = %v, want \"true\"", got.Metadata["pending_embed"])
	}
	if m.rememberDegraded["embed_timeout"] != 1 {
		t.Fatalf("RememberDegraded(embed_timeout) = %d, want 1", m.rememberDegraded["embed_timeout"])
	}
}

// TestRememberEmbedErrorFailsFastWithoutBudget confirms that WriteEmbedTimeout
// == 0 (the zero value / not configured) preserves the original fail-fast
// behavior: an embedder error still hard-fails Remember.
func TestRememberEmbedErrorFailsFastWithoutBudget(t *testing.T) {
	st := openTestStore(t)
	svc := service.New(st, errEmbedder{dims: dims}, service.WithSyncReinforce())

	_, err := svc.Remember(context.Background(), service.RememberInput{
		Namespace: "alice", Content: "hello world", Tier: memory.TierSemantic,
	})
	if err == nil || !strings.Contains(err.Error(), "remember: embed:") {
		t.Fatalf("want a wrapped remember: embed error, got %v", err)
	}
}

// panicOnVectorSearchStore wraps a store and panics if VectorSearch is ever
// called, so a test can prove a vectorless write never reaches a
// vector-dependent similarity job.
type panicOnVectorSearchStore struct {
	store.Store
}

func (p *panicOnVectorSearchStore) VectorSearch(context.Context, string, []float32, store.Filter, int) ([]store.Scored, error) {
	panic("VectorSearch must not be called for a vectorless write")
}

// TestRememberVectorlessSkipsSimilarityJobs confirms that a write which
// degrades to vectorless storage (embedder down, write embed budget set) never
// invokes any of the write-time similarity jobs that need a query vector:
// dedup, corroborate, contradict, and sync consolidation. Each is configured
// to fire aggressively (near-zero thresholds); panicOnVectorSearchStore proves
// none of them ever calls VectorSearch.
func TestRememberVectorlessSkipsSimilarityJobs(t *testing.T) {
	t.Run("dedup", func(t *testing.T) {
		base := openTestStore(t)
		st := &panicOnVectorSearchStore{Store: base}
		svc := service.New(st, errEmbedder{dims: dims}, service.WithSyncReinforce(),
			service.WithWriteEmbedTimeout(time.Second),
			service.WithWriteDedup(0.01, service.WriteDedupSupersede))

		if _, err := svc.Remember(context.Background(), service.RememberInput{
			Namespace: "alice", Content: "hello world", Tier: memory.TierSemantic,
		}); err != nil {
			t.Fatalf("remember: %v", err)
		}
	})

	t.Run("contradict", func(t *testing.T) {
		base := openTestStore(t)
		st := &panicOnVectorSearchStore{Store: base}
		svc := service.New(st, errEmbedder{dims: dims}, service.WithSyncReinforce(),
			service.WithWriteEmbedTimeout(time.Second),
			service.WithContradictionDownrank(0.01))

		if _, err := svc.Remember(context.Background(), service.RememberInput{
			Namespace: "alice", Content: "hello world", Tier: memory.TierSemantic,
		}); err != nil {
			t.Fatalf("remember: %v", err)
		}
	})

	t.Run("corroborate", func(t *testing.T) {
		base := openTestStore(t)
		st := &panicOnVectorSearchStore{Store: base}
		svc := service.New(st, errEmbedder{dims: dims}, service.WithSyncReinforce(),
			service.WithWriteEmbedTimeout(time.Second),
			service.WithCorroboration(0.01))

		if _, err := svc.Remember(context.Background(), service.RememberInput{
			Namespace: "alice", Content: "hello world", Tier: memory.TierEpisodic,
		}); err != nil {
			t.Fatalf("remember: %v", err)
		}
	})

	t.Run("consolidate_sync", func(t *testing.T) {
		base := openTestStore(t)
		st := &panicOnVectorSearchStore{Store: base}
		fc := &fakeConsolidator{}
		svc := service.New(st, errEmbedder{dims: dims}, service.WithSyncReinforce(),
			service.WithWriteEmbedTimeout(time.Second),
			service.WithConsolidator(fc), service.WithConsolidateMode(service.ConsolidateSync),
			service.WithConsolidateMinScore(0))

		if _, err := svc.Remember(context.Background(), service.RememberInput{
			Namespace: "alice", Content: "hello world", Tier: memory.TierSemantic,
		}); err != nil {
			t.Fatalf("remember: %v", err)
		}
	})
}

// TestRememberUpdatePreservesPendingEmbed confirms the memory_update path
// (Remember with in.ID set) applies the same fallback: updating a memory while
// the embedder is down stores it vectorless and marked pending_embed, rather
// than failing or silently dropping the flag.
func TestRememberUpdatePreservesPendingEmbed(t *testing.T) {
	st := openTestStore(t)
	seed := service.New(st, embedtest.New(dims), service.WithSyncReinforce())
	created, err := seed.Remember(context.Background(), service.RememberInput{
		Namespace: "alice", Content: "original content", Tier: memory.TierSemantic,
	})
	if err != nil {
		t.Fatalf("seed remember: %v", err)
	}

	m := &countingMetrics{}
	svc := service.New(st, errEmbedder{dims: dims}, service.WithSyncReinforce(),
		service.WithWriteEmbedTimeout(time.Second), service.WithMetrics(m))

	updated, err := svc.Remember(context.Background(), service.RememberInput{
		Namespace: "alice", ID: created.ID, Content: "updated content", Tier: memory.TierSemantic,
	})
	if err != nil {
		t.Fatalf("update should degrade, not error: %v", err)
	}
	if updated.Metadata["pending_embed"] != "true" {
		t.Fatalf("Metadata[pending_embed] = %v, want \"true\" on the updated row", updated.Metadata["pending_embed"])
	}
	if len(updated.Embedding) != 0 {
		t.Fatalf("Embedding = %v, want empty on a degraded update", updated.Embedding)
	}
	if m.rememberDegraded["embed_error"] != 1 {
		t.Fatalf("RememberDegraded(embed_error) = %d, want 1", m.rememberDegraded["embed_error"])
	}
}
