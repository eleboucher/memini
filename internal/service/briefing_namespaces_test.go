package service_test

import (
	"context"
	"errors"
	"testing"

	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/service"
)

func hasFact(ms []*memory.Memory, content string) bool {
	for _, m := range ms {
		if m.Content == content {
			return true
		}
	}
	return false
}

// TestBriefingSubtreeSurfacesChildDurableFacts: opts.Subtree pulls durable
// facts from nested namespaces into the parent's briefing — a capability
// briefing never had before this refactor (subtree used to be recall-only).
func TestBriefingSubtreeSurfacesChildDurableFacts(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()

	if _, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "proj", Content: "parent: uses Go modules", Tier: memory.TierSemantic,
	}); err != nil {
		t.Fatalf("remember parent: %v", err)
	}
	if _, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "proj/agent-a", Content: "child: agent-a prefers table tests", Tier: memory.TierSemantic,
	}); err != nil {
		t.Fatalf("remember child: %v", err)
	}

	exact, err := svc.Briefing(ctx, "proj", service.BriefingOpts{})
	if err != nil {
		t.Fatalf("exact briefing: %v", err)
	}
	if hasFact(exact.Facts, "child: agent-a prefers table tests") {
		t.Fatal("exact-scope briefing should not see the nested namespace's facts")
	}

	sub, err := svc.Briefing(ctx, "proj", service.BriefingOpts{Subtree: true})
	if err != nil {
		t.Fatalf("subtree briefing: %v", err)
	}
	if !hasFact(sub.Facts, "parent: uses Go modules") || !hasFact(sub.Facts, "child: agent-a prefers table tests") {
		t.Fatalf("subtree briefing should include parent and child facts, got %+v", sub.Facts)
	}
}

// TestBriefingExplicitNamespaces: opts.Namespaces replaces the default read
// set (namespace, subtree, ancestors, home, and links), same replace
// semantics as Recall — even when Home is also set.
func TestBriefingExplicitNamespaces(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()

	if _, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "A", Content: "A fact", Tier: memory.TierSemantic,
	}); err != nil {
		t.Fatalf("remember A: %v", err)
	}
	if _, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "B", Content: "B fact", Tier: memory.TierSemantic,
	}); err != nil {
		t.Fatalf("remember B: %v", err)
	}
	if _, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "personal/kit", Content: "home fact", Tier: memory.TierSemantic,
	}); err != nil {
		t.Fatalf("remember home: %v", err)
	}

	b, err := svc.Briefing(ctx, "A", service.BriefingOpts{Namespaces: []string{"A", "B"}, Home: "personal/kit"})
	if err != nil {
		t.Fatalf("briefing: %v", err)
	}
	if !hasFact(b.Facts, "A fact") || !hasFact(b.Facts, "B fact") {
		t.Fatalf("explicit-namespace briefing should include A and B facts, got %+v", b.Facts)
	}
	if hasFact(b.Facts, "home fact") {
		t.Fatal("explicit Namespaces must replace the default read set, not extend it with home")
	}
}

// TestBriefingAncestorMergesDurableOnly: a nested namespace's briefing
// implicitly merges its ancestor's ("work", the interior node — the design's
// replacement for the deleted work/_shared convention) durable facts, never
// its episodic ones — end-to-end through the public Briefing() call, no
// config required.
func TestBriefingAncestorMergesDurableOnly(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()

	if _, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "work", Content: "shared: always use conventional commits", Tier: memory.TierSemantic,
	}); err != nil {
		t.Fatalf("remember work semantic: %v", err)
	}
	if _, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "work", Content: "shared: chatter about lunch plans", Tier: memory.TierEpisodic,
	}); err != nil {
		t.Fatalf("remember work episodic: %v", err)
	}
	if _, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "work/memini", Content: "work/memini: deploys with helm charts", Tier: memory.TierSemantic,
	}); err != nil {
		t.Fatalf("remember work/memini: %v", err)
	}

	b, err := svc.Briefing(ctx, "work/memini", service.BriefingOpts{})
	if err != nil {
		t.Fatalf("briefing: %v", err)
	}
	if !hasFact(b.Facts, "shared: always use conventional commits") {
		t.Fatal("briefing facts should include the ancestor durable fact")
	}
	for _, m := range b.Recent {
		if m.Content == "shared: chatter about lunch plans" {
			t.Fatal("ancestor episodic memories must not appear in briefing recent")
		}
	}
}

// TestBriefingScopeEverywhereAddsSubtree: Scope "everywhere" is equivalent to
// Subtree: true (plus the ancestor/home/link cascade) — a child namespace's
// durable facts surface in the parent's briefing.
func TestBriefingScopeEverywhereAddsSubtree(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	if _, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "proj/agent-a", Content: "child: agent-a prefers table tests", Tier: memory.TierSemantic,
	}); err != nil {
		t.Fatalf("remember child: %v", err)
	}

	b, err := svc.Briefing(ctx, "proj", service.BriefingOpts{Scope: "everywhere"})
	if err != nil {
		t.Fatalf("briefing: %v", err)
	}
	if !hasFact(b.Facts, "child: agent-a prefers table tests") {
		t.Fatalf(`Scope "everywhere" should include the child namespace's facts, got %+v`, b.Facts)
	}
}

// TestBriefingScopeInvalidErrors: an unrecognized Scope value is a caller
// input error.
func TestBriefingScopeInvalidErrors(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	_, err := svc.Briefing(ctx, "A", service.BriefingOpts{Scope: "bogus"})
	if err == nil || !errors.Is(err, service.ErrInvalidInput) {
		t.Fatalf("error = %v, want ErrInvalidInput for an unrecognized Scope", err)
	}
}
