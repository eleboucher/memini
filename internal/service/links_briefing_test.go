package service_test

import (
	"context"
	"testing"

	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/service"
)

// TestLinkDurableSurfacesSemanticNotEpisodic: A links to B with the default
// ("durable") tier mode. B's semantic fact surfaces in A's briefing and
// recall; B's episodic memory does not — end-to-end through the public
// Briefing/Recall calls, not just the resolveReadSet unit tests.
func TestLinkDurableSurfacesSemanticNotEpisodic(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()

	if err := svc.LinkNamespaces(ctx, "A", "B", "durable"); err != nil {
		t.Fatalf("LinkNamespaces: %v", err)
	}
	if _, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "B", Content: "B: uses Go modules", Tier: memory.TierSemantic,
	}); err != nil {
		t.Fatalf("remember B semantic: %v", err)
	}
	if _, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "B", Content: "B: chatter about lunch plans", Tier: memory.TierEpisodic,
	}); err != nil {
		t.Fatalf("remember B episodic: %v", err)
	}

	b, err := svc.Briefing(ctx, "A", service.BriefingOpts{})
	if err != nil {
		t.Fatalf("briefing: %v", err)
	}
	if !hasFact(b.Facts, "B: uses Go modules") {
		t.Fatal("briefing facts should include the durable link target's semantic fact")
	}
	for _, m := range b.Recent {
		if m.Content == "B: chatter about lunch plans" {
			t.Fatal("a durable-tiers link must not surface the target's episodic memories")
		}
	}

	got, err := svc.Recall(ctx, service.RecallInput{
		Namespace: "A", Query: "Go modules", Limit: 10,
	})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	found := false
	for _, s := range got {
		if s.Memory.Content == "B: uses Go modules" {
			found = true
		}
		if s.Memory.Content == "B: chatter about lunch plans" {
			t.Fatal("a durable-tiers link must not surface the target's episodic memories in recall")
		}
	}
	if !found {
		t.Fatalf("recall in A should include the durable link target's semantic fact, got %+v", got)
	}
}

// TestLinkAllSurfacesEpisodicToo: an "all"-tiers link also surfaces the
// target's episodic memories, unlike the default "durable" mode.
func TestLinkAllSurfacesEpisodicToo(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()

	if err := svc.LinkNamespaces(ctx, "A", "B", "all"); err != nil {
		t.Fatalf("LinkNamespaces: %v", err)
	}
	if _, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "B", Content: "B: deployed the staging release", Tier: memory.TierEpisodic,
	}); err != nil {
		t.Fatalf("remember B episodic: %v", err)
	}

	got, err := svc.Recall(ctx, service.RecallInput{
		Namespace: "A", Query: "staging release", Limit: 10,
	})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	found := false
	for _, s := range got {
		if s.Memory.Content == "B: deployed the staging release" {
			found = true
		}
	}
	if !found {
		t.Fatalf("recall in A with an all-tiers link should include B's episodic memory, got %+v", got)
	}
}

// TestLinkOneHopRecall: A links to B, B links to C. Recall in A must never
// surface C's memories — links are 1-hop, non-transitive by construction.
func TestLinkOneHopRecall(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()

	if err := svc.LinkNamespaces(ctx, "A", "B", "durable"); err != nil {
		t.Fatalf("LinkNamespaces A->B: %v", err)
	}
	if err := svc.LinkNamespaces(ctx, "B", "C", "durable"); err != nil {
		t.Fatalf("LinkNamespaces B->C: %v", err)
	}
	if _, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "C", Content: "C: only reachable from B directly", Tier: memory.TierSemantic,
	}); err != nil {
		t.Fatalf("remember C: %v", err)
	}

	got, err := svc.Recall(ctx, service.RecallInput{
		Namespace: "A", Query: "reachable", Limit: 10,
	})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	for _, s := range got {
		if s.Memory.Content == "C: only reachable from B directly" {
			t.Fatal("recall in A followed B's link to C — links must be 1-hop, non-transitive")
		}
	}
}

// TestLinkIgnoredOnExplicitNamespacesRecall: a per-call namespaces list
// replaces the default read set, so A's link to B must not sneak B's
// memories back in when the caller passes an explicit namespaces list that
// excludes B.
func TestLinkIgnoredOnExplicitNamespacesRecall(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()

	if err := svc.LinkNamespaces(ctx, "A", "B", "durable"); err != nil {
		t.Fatalf("LinkNamespaces: %v", err)
	}
	if _, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "B", Content: "B: linked but excluded explicitly", Tier: memory.TierSemantic,
	}); err != nil {
		t.Fatalf("remember B: %v", err)
	}

	got, err := svc.Recall(ctx, service.RecallInput{
		Namespace: "A", Query: "excluded", Limit: 10, Namespaces: []string{"A"},
	})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	for _, s := range got {
		if s.Memory.Content == "B: linked but excluded explicitly" {
			t.Fatal("explicit per-call namespaces must replace the default read set, not merge in A's links")
		}
	}
}
