package service_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/eleboucher/memini/internal/embed/embedtest"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/service"
	"github.com/eleboucher/memini/internal/store/sqlitevec"
)

const dims = 64

func newService(t *testing.T) *service.Service {
	t.Helper()
	st, err := sqlitevec.Open(context.Background(), filepath.Join(t.TempDir(), "svc.db"), dims)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	var n int
	return service.New(st, embedtest.New(dims),
		service.WithClock(func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }),
		service.WithIDGenerator(func() string { n++; return "id-" + string(rune('a'+n-1)) }),
	)
}

func TestRememberAssignsDefaults(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()

	m, err := svc.Remember(ctx, service.RememberInput{Namespace: "alice", Content: "hello world"})
	if err != nil {
		t.Fatalf("remember: %v", err)
	}
	if m.ID == "" {
		t.Fatal("expected generated ID")
	}
	if m.Tier != memory.TierWorking {
		t.Fatalf("default tier = %q, want working", m.Tier)
	}
	if m.ExpiresAt == nil {
		t.Fatal("working tier should get a default TTL")
	}

	// Semantic tier is durable (no expiry).
	sem, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "alice", Content: "durable fact", Tier: memory.TierSemantic,
	})
	if err != nil {
		t.Fatalf("remember: %v", err)
	}
	if sem.ExpiresAt != nil {
		t.Fatalf("semantic tier should not expire, got %v", sem.ExpiresAt)
	}
}

func TestRecallHybrid(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()

	docs := []string{
		"the cat sat on the warm mat",
		"dogs are loyal and friendly animals",
		"kubernetes schedules containers across nodes",
		"postgres is a relational database system",
	}
	for _, d := range docs {
		if _, err := svc.Remember(ctx, service.RememberInput{
			Namespace: "alice", Content: d, Tier: memory.TierSemantic,
		}); err != nil {
			t.Fatalf("remember %q: %v", d, err)
		}
	}

	res, err := svc.Recall(ctx, service.RecallInput{
		Namespace: "alice", Query: "relational database postgres", Limit: 2,
	})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(res) == 0 {
		t.Fatal("expected results")
	}
	if res[0].Memory.Content != "postgres is a relational database system" {
		t.Fatalf("top hit = %q, want the postgres doc", res[0].Memory.Content)
	}
}

func TestRecallNamespaceIsolation(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()

	if _, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "alice", Content: "alice secret about databases", Tier: memory.TierSemantic,
	}); err != nil {
		t.Fatal(err)
	}
	res, err := svc.Recall(ctx, service.RecallInput{Namespace: "bob", Query: "databases", Limit: 5})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(res) != 0 {
		t.Fatalf("bob should see nothing, got %d results", len(res))
	}
}
