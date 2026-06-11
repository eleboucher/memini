package service_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/eleboucher/memini/internal/embed"
	"github.com/eleboucher/memini/internal/embed/embedtest"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/service"
	"github.com/eleboucher/memini/internal/store"
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
		service.WithSyncReinforce(),
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

// recordingStore captures the per-leg k requested by Recall.
type recordingStore struct {
	store.Store
	vectorK, keywordK int
}

func (r *recordingStore) VectorSearch(ctx context.Context, ns string, vec []float32, f store.Filter, k int) ([]store.Scored, error) {
	r.vectorK = k
	return r.Store.VectorSearch(ctx, ns, vec, f, k)
}

func (r *recordingStore) KeywordSearch(ctx context.Context, ns, query string, f store.Filter, k int) ([]store.Scored, error) {
	r.keywordK = k
	return r.Store.KeywordSearch(ctx, ns, query, f, k)
}

// recordingEmbedder captures every text sent to the embedder.
type recordingEmbedder struct {
	embed.Embedder
	texts []string
}

func (r *recordingEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	r.texts = append(r.texts, texts...)
	return r.Embedder.Embed(ctx, texts)
}

func TestRecallDeepensCandidatePool(t *testing.T) {
	ctx := context.Background()
	st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "svc.db"), dims)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	rec := &recordingStore{Store: st}
	svc := service.New(rec, embedtest.New(dims), service.WithSyncReinforce())

	if _, err := svc.Remember(ctx, service.RememberInput{Namespace: "alice", Content: "hello"}); err != nil {
		t.Fatalf("remember: %v", err)
	}

	// Small limits use the pool floor so fusion sees deep per-leg rankings.
	if _, err := svc.Recall(ctx, service.RecallInput{Namespace: "alice", Query: "hello", Limit: 2}); err != nil {
		t.Fatalf("recall: %v", err)
	}
	if rec.vectorK != 50 || rec.keywordK != 50 {
		t.Fatalf("per-leg pool = (%d, %d), want floor (50, 50)", rec.vectorK, rec.keywordK)
	}

	// Large limits scale the pool by the over-fetch factor.
	if _, err := svc.Recall(ctx, service.RecallInput{Namespace: "alice", Query: "hello", Limit: 20}); err != nil {
		t.Fatalf("recall: %v", err)
	}
	if rec.vectorK != 100 || rec.keywordK != 100 {
		t.Fatalf("per-leg pool = (%d, %d), want k*factor (100, 100)", rec.vectorK, rec.keywordK)
	}
}

func TestRecallQueryPrefix(t *testing.T) {
	ctx := context.Background()
	st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "svc.db"), dims)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	rec := &recordingEmbedder{Embedder: embedtest.New(dims)}
	svc := service.New(st, rec,
		service.WithSyncReinforce(),
		service.WithQueryPrefix("Instruct: retrieve\nQuery: "),
	)

	// Documents are embedded bare; only recall queries get the prefix.
	if _, err := svc.Remember(ctx, service.RememberInput{Namespace: "alice", Content: "hello world"}); err != nil {
		t.Fatalf("remember: %v", err)
	}
	if _, err := svc.Recall(ctx, service.RecallInput{Namespace: "alice", Query: "hello", Limit: 1}); err != nil {
		t.Fatalf("recall: %v", err)
	}

	want := []string{"hello world", "Instruct: retrieve\nQuery: hello"}
	if len(rec.texts) != len(want) {
		t.Fatalf("embedded texts = %q, want %q", rec.texts, want)
	}
	for i := range want {
		if rec.texts[i] != want[i] {
			t.Errorf("embedded[%d] = %q, want %q", i, rec.texts[i], want[i])
		}
	}
}

func TestWriteDedupCoalescesNearIdentical(t *testing.T) {
	ctx := context.Background()
	st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "svc.db"), dims)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	svc := service.New(st, embedtest.New(dims),
		service.WithSyncReinforce(), service.WithWriteDedup(0.95))

	first, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "alice", Content: "the user likes coffee", Tier: memory.TierSemantic,
	})
	if err != nil {
		t.Fatalf("remember: %v", err)
	}

	// An identical repeat coalesces into the existing memory (same ID, no new row).
	dup, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "alice", Content: "the user likes coffee", Tier: memory.TierSemantic,
	})
	if err != nil {
		t.Fatalf("remember dup: %v", err)
	}
	if dup.ID != first.ID {
		t.Fatalf("dup got new ID %q, want coalesced into %q", dup.ID, first.ID)
	}

	// A genuinely different fact is stored as its own memory.
	if _, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "alice", Content: "kubernetes schedules pods across nodes", Tier: memory.TierSemantic,
	}); err != nil {
		t.Fatalf("remember distinct: %v", err)
	}

	all, err := svc.List(ctx, service.ListInput{Namespace: "alice", Tiers: []memory.Tier{memory.TierSemantic}})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("expected 2 memories (dup coalesced), got %d", len(all))
	}

	// The coalesced repeat reinforced the canonical memory.
	got, err := svc.Get(ctx, "alice", first.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.AccessCount < 1 {
		t.Fatalf("canonical memory access_count = %d, want >= 1 (reinforced by the repeat)", got.AccessCount)
	}
}

func TestWriteDedupDisabledByDefault(t *testing.T) {
	svc := newService(t) // no WithWriteDedup
	ctx := context.Background()

	for range 2 {
		if _, err := svc.Remember(ctx, service.RememberInput{
			Namespace: "bob", Content: "the user likes coffee", Tier: memory.TierSemantic,
		}); err != nil {
			t.Fatalf("remember: %v", err)
		}
	}
	all, err := svc.List(ctx, service.ListInput{Namespace: "bob", Tiers: []memory.Tier{memory.TierSemantic}})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("dedup off: expected 2 memories, got %d", len(all))
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

func TestListAndStatsAllNamespaces(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()

	// Two memories each across three namespaces.
	for _, ns := range []string{"alice", "bob", "carol"} {
		for range 2 {
			if _, err := svc.Remember(ctx, service.RememberInput{
				Namespace: ns, Content: ns + " fact", Tier: memory.TierSemantic,
			}); err != nil {
				t.Fatalf("remember %s: %v", ns, err)
			}
		}
	}

	// AllNamespaces aggregates every namespace; scoped lists stay isolated.
	all, err := svc.List(ctx, service.ListInput{AllNamespaces: true})
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 6 {
		t.Fatalf("aggregate list = %d memories, want 6", len(all))
	}

	// Limit applies as a single global cap, not per namespace.
	capped, err := svc.List(ctx, service.ListInput{AllNamespaces: true, Limit: 4})
	if err != nil {
		t.Fatalf("list all capped: %v", err)
	}
	if len(capped) != 4 {
		t.Fatalf("global cap = %d memories, want 4", len(capped))
	}

	stats, err := svc.StatsAll(ctx)
	if err != nil {
		t.Fatalf("stats all: %v", err)
	}
	if stats.Namespace != "" {
		t.Fatalf("aggregate stats namespace = %q, want empty", stats.Namespace)
	}
	if stats.Total != 6 {
		t.Fatalf("aggregate stats total = %d, want 6", stats.Total)
	}
}
