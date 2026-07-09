package service_test

import (
	"context"
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
// set (namespace, subtree/links/env-configured namespaces, and the global
// namespace), same replace semantics as Recall.
func TestBriefingExplicitNamespaces(t *testing.T) {
	svc := newService(t, service.WithGlobalNamespace("global"))
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
		Namespace: "global", Content: "global fact", Tier: memory.TierSemantic,
	}); err != nil {
		t.Fatalf("remember global: %v", err)
	}

	b, err := svc.Briefing(ctx, "A", service.BriefingOpts{Namespaces: []string{"A", "B"}})
	if err != nil {
		t.Fatalf("briefing: %v", err)
	}
	if !hasFact(b.Facts, "A fact") || !hasFact(b.Facts, "B fact") {
		t.Fatalf("explicit-namespace briefing should include A and B facts, got %+v", b.Facts)
	}
	if hasFact(b.Facts, "global fact") {
		t.Fatal("explicit Namespaces must replace the default read set, not extend it with global")
	}
}

// TestBriefingReadNamespacesMergesDurableOnly: WithReadNamespaces merges into
// Briefing the same way as WithGlobalNamespace — durable facts only, never
// episodic — end-to-end through the public Briefing() call, not just the
// resolveReadSet unit tests.
func TestBriefingReadNamespacesMergesDurableOnly(t *testing.T) {
	svc := newService(t, service.WithReadNamespaces([]string{"shared"}))
	ctx := context.Background()

	if _, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "shared", Content: "shared: always use conventional commits", Tier: memory.TierSemantic,
	}); err != nil {
		t.Fatalf("remember shared semantic: %v", err)
	}
	if _, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "shared", Content: "shared: chatter about lunch plans", Tier: memory.TierEpisodic,
	}); err != nil {
		t.Fatalf("remember shared episodic: %v", err)
	}
	if _, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "proj", Content: "proj: deploys with helm charts", Tier: memory.TierSemantic,
	}); err != nil {
		t.Fatalf("remember proj: %v", err)
	}

	b, err := svc.Briefing(ctx, "proj", service.BriefingOpts{})
	if err != nil {
		t.Fatalf("briefing: %v", err)
	}
	if !hasFact(b.Facts, "shared: always use conventional commits") {
		t.Fatal("briefing facts should include the read-namespace's durable fact")
	}
	for _, m := range b.Recent {
		if m.Content == "shared: chatter about lunch plans" {
			t.Fatal("read-namespace episodic memories must not appear in briefing recent")
		}
	}
}
