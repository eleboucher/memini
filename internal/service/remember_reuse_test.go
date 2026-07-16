package service_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/eleboucher/memini/internal/embed/embedtest"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/service"
)

// TestRememberUpdateReusesVectorWhenContentUnchanged pins the point of the
// reuse path: an update that touches only metadata/tags/importance costs no
// embedder call.
//
// The Recall assertion is not incidental. Reusing existing.Embedding (always
// empty — Get omits vectors) would hand Upsert a zero-length vector, which it
// reads as "drop the vector", silently evicting the row from semantic recall.
// That bug spends zero embedder calls too, so a count-only test would pass
// while the memory quietly stopped being findable.
func TestRememberUpdateReusesVectorWhenContentUnchanged(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	rec := &recordingEmbedder{Embedder: embedtest.New(dims)}
	svc := service.New(st, rec, service.WithSyncReinforce())

	const content = "the deploy runbook lives in the ops wiki"
	created, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "alice", Content: content, Tier: memory.TierSemantic,
		Tags: []string{"ops"},
	})
	if err != nil {
		t.Fatalf("seed remember: %v", err)
	}
	seedCalls := len(rec.texts)

	updated, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "alice", ID: created.ID, Content: content, Tier: memory.TierSemantic,
		Tags: []string{"ops", "runbook"}, Metadata: map[string]any{"reviewed": "yes"},
	})
	if err != nil {
		t.Fatalf("metadata-only update: %v", err)
	}

	if len(rec.texts) != seedCalls {
		t.Fatalf("embedder called %d extra time(s) for a content-unchanged update, want 0 (texts: %v)",
			len(rec.texts)-seedCalls, rec.texts[seedCalls:])
	}
	if len(updated.Embedding) == 0 {
		t.Fatal("Embedding empty after a reused-vector update, want the stored vector carried forward")
	}
	if !slices.Contains(updated.Tags, "runbook") {
		t.Fatalf("Tags = %v, want the update applied", updated.Tags)
	}

	// The vector must still be in the index, not merely on the returned struct.
	hits, err := svc.Recall(ctx, service.RecallInput{Namespace: "alice", Query: content, Limit: 5})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	var found bool
	for _, h := range hits {
		if h.Memory.ID == created.ID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("memory %s is no longer recallable after a metadata-only update — its vector was dropped", created.ID)
	}
}

// TestRememberUpdateReEmbedsVectorlessRow is the guard on the reuse condition.
// A row left vectorless by a degraded write has content that matches the update
// byte-for-byte but nothing to reuse, so it must still embed for real — this is
// the documented recovery path (memory_update once the embedder is back).
//
// Written as reuse-on-content-match alone, without the has-a-vector check, this
// fails: the row would keep its empty vector and stay degraded forever. The two
// existing pending_embed tests both change content, so neither reaches this
// branch.
func TestRememberUpdateReEmbedsVectorlessRow(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	const content = "the office wifi password is on the whiteboard"
	degraded := service.New(st, errEmbedder{dims: dims}, service.WithSyncReinforce(),
		service.WithWriteEmbedTimeout(time.Second))
	created, err := degraded.Remember(ctx, service.RememberInput{
		Namespace: "alice", Content: content, Tier: memory.TierSemantic,
	})
	if err != nil {
		t.Fatalf("seed degraded remember: %v", err)
	}
	if created.Metadata["pending_embed"] != "true" {
		t.Fatalf("seed Metadata[pending_embed] = %v, want \"true\"", created.Metadata["pending_embed"])
	}

	// Same content, healthy embedder, metadata-only edit.
	rec := &recordingEmbedder{Embedder: embedtest.New(dims)}
	healthy := service.New(st, rec, service.WithSyncReinforce())
	updated, err := healthy.Remember(ctx, service.RememberInput{
		Namespace: "alice", ID: created.ID, Content: content, Tier: memory.TierSemantic,
		Metadata: created.Metadata,
	})
	if err != nil {
		t.Fatalf("recovery update: %v", err)
	}

	if len(rec.texts) != 1 {
		t.Fatalf("embedder calls = %d, want 1: a vectorless row has nothing to reuse and must re-embed", len(rec.texts))
	}
	if _, ok := updated.Metadata["pending_embed"]; ok {
		t.Fatal("Metadata[pending_embed] still set, want cleared once the row gained a vector")
	}
	if len(updated.Embedding) == 0 {
		t.Fatal("Embedding empty, want a vector after the recovery re-embed")
	}
}

// TestRememberUpdateReEmbedsWhenContentChanges is the negative control: a real
// content edit must still pay exactly one embedder call, or the reuse condition
// is too eager and the stored vector would point at replaced content.
func TestRememberUpdateReEmbedsWhenContentChanges(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	rec := &recordingEmbedder{Embedder: embedtest.New(dims)}
	svc := service.New(st, rec, service.WithSyncReinforce())

	created, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "alice", Content: "the deploy runbook lives in the ops wiki", Tier: memory.TierSemantic,
	})
	if err != nil {
		t.Fatalf("seed remember: %v", err)
	}
	seedCalls := len(rec.texts)

	const replacement = "the deploy runbook moved to the platform handbook"
	if _, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "alice", ID: created.ID, Content: replacement, Tier: memory.TierSemantic,
	}); err != nil {
		t.Fatalf("content update: %v", err)
	}

	if got := len(rec.texts) - seedCalls; got != 1 {
		t.Fatalf("embedder calls for a content change = %d, want 1", got)
	}
	if last := rec.texts[len(rec.texts)-1]; last != replacement {
		t.Fatalf("embedded %q, want the replacement content %q", last, replacement)
	}
}
