package service_test

import (
	"context"
	"slices"
	"testing"

	"github.com/eleboucher/memini/internal/service"
)

// TestRecallSubtreeScope: a subtree recall on the parent namespace reads the
// parent plus nested per-agent namespaces; the default exact scope does not.
func TestRecallSubtreeScope(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()

	if _, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "proj", Tier: "semantic", Content: "shared: the service is written in Go",
	}); err != nil {
		t.Fatalf("remember shared: %v", err)
	}
	if _, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "proj/reviewer", Tier: "semantic", Content: "private: the reviewer prefers table tests in Go",
	}); err != nil {
		t.Fatalf("remember private: %v", err)
	}

	// Exact scope on the parent sees only the parent's memory.
	exact, err := svc.Recall(ctx, service.RecallInput{Namespace: "proj", Query: "Go service", Limit: 10})
	if err != nil {
		t.Fatalf("exact recall: %v", err)
	}
	for _, r := range exact {
		if r.Memory.Namespace != "proj" {
			t.Errorf("exact scope leaked namespace %q", r.Memory.Namespace)
		}
	}

	// Subtree scope sees both the parent and the nested agent namespace.
	sub, err := svc.Recall(ctx, service.RecallInput{Namespace: "proj", Query: "Go", Limit: 10, Subtree: true})
	if err != nil {
		t.Fatalf("subtree recall: %v", err)
	}
	namespaces := make([]string, 0, len(sub))
	for _, r := range sub {
		namespaces = append(namespaces, r.Memory.Namespace)
	}
	if !slices.Contains(namespaces, "proj") || !slices.Contains(namespaces, "proj/reviewer") {
		t.Fatalf("subtree recall should span proj and proj/reviewer, got %v", namespaces)
	}
}
