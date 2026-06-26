package service_test

import (
	"context"
	"errors"
	"fmt"
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

func newService(t *testing.T, opts ...service.Option) *service.Service {
	t.Helper()
	st, err := sqlitevec.Open(context.Background(), filepath.Join(t.TempDir(), "svc.db"), dims)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	var n int
	base := make([]service.Option, 0, 3+len(opts))
	base = append(base,
		service.WithClock(func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }),
		service.WithIDGenerator(func() string { n++; return "id-" + string(rune('a'+n-1)) }),
		service.WithSyncReinforce(),
	)
	return service.New(st, embedtest.New(dims), append(base, opts...)...)
}

func TestRememberTemporalValidity(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()

	// Record a historical fact: the office was in Paris for Q1 2024.
	from := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2024, 4, 1, 0, 0, 0, 0, time.UTC)
	m, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "alice", Content: "the office is in Paris", Tier: memory.TierSemantic,
		ValidFrom: &from, ValidTo: &to,
	})
	if err != nil {
		t.Fatalf("remember: %v", err)
	}
	if m.ValidFrom == nil || !m.ValidFrom.Equal(from) {
		t.Fatalf("valid_from = %v, want %v", m.ValidFrom, from)
	}
	if m.ValidTo == nil || !m.ValidTo.Equal(to) {
		t.Fatalf("valid_to = %v, want %v", m.ValidTo, to)
	}

	// The window round-trips through the store.
	got, err := svc.Get(ctx, "alice", m.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ValidFrom == nil || !got.ValidFrom.Equal(from) || got.ValidTo == nil || !got.ValidTo.Equal(to) {
		t.Fatalf("round-trip window = [%v,%v], want [%v,%v]", got.ValidFrom, got.ValidTo, from, to)
	}

	// Time-travel recall inside the window surfaces the fact; before it, nothing.
	inside, err := svc.Recall(ctx, service.RecallInput{
		Namespace: "alice", Query: "where is the office", Limit: 5,
		AsOf: time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("recall inside: %v", err)
	}
	if !containsID(inside, m.ID) {
		t.Fatalf("as_of inside the validity window should surface the fact, got %v", idsOf(inside))
	}
	before, err := svc.Recall(ctx, service.RecallInput{
		Namespace: "alice", Query: "where is the office", Limit: 5,
		AsOf: time.Date(2023, 6, 1, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("recall before: %v", err)
	}
	if containsID(before, m.ID) {
		t.Fatalf("as_of before valid_from must not surface the fact, got %v", idsOf(before))
	}
}

func TestReinforcePreservesCustomTTL(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	now := time.Unix(1_700_000_000, 0).UTC() // matches newService's fixed clock
	ttl := time.Hour

	m, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "alice", Content: "short lived note", Tier: memory.TierWorking, TTL: &ttl,
	})
	if err != nil {
		t.Fatalf("remember: %v", err)
	}
	if m.ExpiresAt == nil || !m.ExpiresAt.Equal(now.Add(ttl)) {
		t.Fatalf("initial ExpiresAt = %v, want %v", m.ExpiresAt, now.Add(ttl))
	}

	// Recall reinforces the returned memory (sync in tests).
	if _, err := svc.Recall(ctx, service.RecallInput{
		Namespace: "alice", Query: "short lived note", Limit: 5,
	}); err != nil {
		t.Fatalf("recall: %v", err)
	}

	got, err := svc.Get(ctx, "alice", m.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ExpiresAt == nil {
		t.Fatal("reinforced memory lost its expiry")
	}
	// Must slide by the caller's 1h TTL, not the 24h working-tier default.
	if !got.ExpiresAt.Equal(now.Add(ttl)) {
		t.Fatalf("ExpiresAt after reinforce = %v, want %v (custom TTL preserved, not tier default now+%v)",
			got.ExpiresAt, now.Add(ttl), memory.TierWorking.DefaultTTL())
	}
}

func TestRecallClampsHighLimit(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	// Seed more than the clamp (100) so an unbounded limit could otherwise
	// return them all.
	for i := range 105 {
		if _, err := svc.Remember(ctx, service.RememberInput{
			Namespace: "alice", Content: fmt.Sprintf("fact number %d about widgets", i), Tier: memory.TierSemantic,
		}); err != nil {
			t.Fatalf("remember %d: %v", i, err)
		}
	}
	res, err := svc.Recall(ctx, service.RecallInput{Namespace: "alice", Query: "widgets", Limit: 100000})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(res) > 100 {
		t.Fatalf("recall returned %d results; an excessive limit must be clamped to 100", len(res))
	}
}

func containsID(res []store.Scored, id string) bool {
	for _, s := range res {
		if s.Memory.ID == id {
			return true
		}
	}
	return false
}

func idsOf(res []store.Scored) []string {
	out := make([]string, len(res))
	for i, s := range res {
		out[i] = s.Memory.ID
	}
	return out
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
	// The fuzzy vector gate (WithWriteDedup) is off by default; with the exact
	// fingerprint path also disabled, an identical repeat is stored verbatim.
	svc := newService(t, service.WithFingerprintDedup(false))
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

func TestFingerprintDedupByDefault(t *testing.T) {
	svc := newService(t) // fingerprint dedup is on by default
	ctx := context.Background()

	first, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "alice", Content: "the user likes coffee", Tier: memory.TierSemantic,
	})
	if err != nil {
		t.Fatalf("remember: %v", err)
	}
	// A restatement differing only in case/whitespace shares a fingerprint, so it
	// coalesces into the existing memory instead of storing a duplicate.
	dup, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "alice", Content: "  The user   likes coffee  ", Tier: memory.TierSemantic,
	})
	if err != nil {
		t.Fatalf("remember dup: %v", err)
	}
	if dup.ID != first.ID {
		t.Fatalf("restatement got new ID %q, want coalesced into %q", dup.ID, first.ID)
	}
	// Same content in a different tier is a distinct intent: not coalesced.
	if _, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "alice", Content: "the user likes coffee", Tier: memory.TierWorking,
	}); err != nil {
		t.Fatalf("remember other tier: %v", err)
	}
	all, err := svc.List(ctx, service.ListInput{Namespace: "alice"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("expected 2 memories (restatement coalesced, other tier kept), got %d", len(all))
	}
	// The coalesced restatement reinforced the canonical memory.
	got, err := svc.Get(ctx, "alice", first.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.AccessCount < 1 {
		t.Fatalf("canonical access_count = %d, want >= 1 (reinforced by the restatement)", got.AccessCount)
	}
}

func TestStatsLowConfidenceDurable(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	low := 0.2 // below memory.ConfidenceDemoteFloor (0.35)
	high := 0.9

	// Two durable facts below the floor (reclaimable debris), one above, plus a
	// short-term memory (confidence is not tracked there, so it never counts).
	for i, c := range []*float64{&low, &low, &high} {
		if _, err := svc.Remember(ctx, service.RememberInput{
			Namespace: "alice", Content: fmt.Sprintf("durable fact %d", i),
			Tier: memory.TierSemantic, Confidence: c,
		}); err != nil {
			t.Fatalf("remember: %v", err)
		}
	}
	if _, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "alice", Content: "scratch note", Tier: memory.TierWorking,
	}); err != nil {
		t.Fatalf("remember working: %v", err)
	}

	st, err := svc.Stats(ctx, "alice")
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if st.LowConfidenceDurable != 2 {
		t.Fatalf("low_confidence_durable = %d, want 2", st.LowConfidenceDurable)
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

	// Two memories each across three namespaces (distinct content so the
	// fingerprint dedup default doesn't coalesce the pair).
	for _, ns := range []string{"alice", "bob", "carol"} {
		for i := range 2 {
			if _, err := svc.Remember(ctx, service.RememberInput{
				Namespace: ns, Content: fmt.Sprintf("%s fact %d", ns, i), Tier: memory.TierSemantic,
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

// TestRememberSeedsImportance pins the tier-based importance seeding: a write
// that carries no importance no longer lands at 0; an explicit value wins; and
// an update keeps the existing importance instead of resetting it to the seed.
func TestRememberSeedsImportance(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()

	ep, err := svc.Remember(ctx, service.RememberInput{Namespace: "n", Content: "an episodic note", Tier: memory.TierEpisodic})
	if err != nil {
		t.Fatalf("remember episodic: %v", err)
	}
	if ep.Importance != 0.3 {
		t.Fatalf("episodic importance = %v, want 0.3 (tier seed)", ep.Importance)
	}

	se, err := svc.Remember(ctx, service.RememberInput{Namespace: "n", Content: "a durable fact", Tier: memory.TierSemantic})
	if err != nil {
		t.Fatalf("remember semantic: %v", err)
	}
	if se.Importance != 0.6 {
		t.Fatalf("semantic importance = %v, want 0.6 (tier seed)", se.Importance)
	}

	ex, err := svc.Remember(ctx, service.RememberInput{Namespace: "n", Content: "explicit", Tier: memory.TierEpisodic, Importance: 0.9})
	if err != nil {
		t.Fatalf("remember explicit: %v", err)
	}
	if ex.Importance != 0.9 {
		t.Fatalf("explicit importance = %v, want 0.9 (caller wins)", ex.Importance)
	}

	upd, err := svc.Remember(ctx, service.RememberInput{Namespace: "n", ID: ex.ID, Content: "explicit v2", Tier: memory.TierEpisodic})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if upd.Importance != 0.9 {
		t.Fatalf("update importance = %v, want 0.9 preserved (not reset to seed)", upd.Importance)
	}
}

// TestSupersedeTombstonesMemory pins Service.Supersede: stamps
// superseded_by + valid_to so default recall hides the row, while it stays
// in the store for the audit chain.
func TestSupersedeTombstonesMemory(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()

	// Seed the long-lived episodic digest the hook will write on SessionEnd.
	canonical, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "alice", Content: "session digest for alice", Tier: memory.TierEpisodic,
		ID: "session-end:test-uuid",
	})
	if err != nil {
		t.Fatalf("remember canonical: %v", err)
	}
	// And the working-tier marker the Stop hook emitted on the same turn.
	working, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "alice", Content: "session digest for alice", Tier: memory.TierWorking,
		ID: "stop:test-uuid",
	})
	if err != nil {
		t.Fatalf("remember working: %v", err)
	}

	// Supersede the byte-identical stop: row.
	if err := svc.Supersede(ctx, "alice", working.ID, canonical.ID); err != nil {
		t.Fatalf("supersede: %v", err)
	}

	// Default recall now hides the working-tier row.
	res, err := svc.Recall(ctx, service.RecallInput{Namespace: "alice", Query: "session digest", Limit: 10})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	for _, r := range res {
		if r.Memory.ID == working.ID {
			t.Fatalf("superseded %q leaked into default recall", working.ID)
		}
	}

	// IncludeSuperseded surfaces the tombstone; superseded_by is set.
	all, err := svc.List(ctx, service.ListInput{Namespace: "alice", IncludeSuperseded: true})
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	var seen bool
	for _, m := range all {
		if m.ID != working.ID {
			continue
		}
		seen = true
		if m.SupersededBy == nil || *m.SupersededBy != canonical.ID {
			t.Fatalf("superseded_by = %v, want %q", m.SupersededBy, canonical.ID)
		}
		if m.ValidTo == nil {
			t.Fatal("supersession did not stamp valid_to")
		}
	}
	if !seen {
		t.Fatalf("superseded %q missing from IncludeSuperseded list", working.ID)
	}

	// NotFound: superseding a row that does not exist is reported (the hook
	// tolerates a 404, but the service surfaces it so callers can branch).
	if err := svc.Supersede(ctx, "alice", "stop:does-not-exist", canonical.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("supersede missing = %v, want store.ErrNotFound", err)
	}

	// Invalid input: empty id or empty by are both rejected up front.
	if err := svc.Supersede(ctx, "alice", "", canonical.ID); !errors.Is(err, service.ErrInvalidInput) {
		t.Fatalf("supersede empty id = %v, want ErrInvalidInput", err)
	}
	if err := svc.Supersede(ctx, "alice", working.ID, ""); !errors.Is(err, service.ErrInvalidInput) {
		t.Fatalf("supersede empty by = %v, want ErrInvalidInput", err)
	}

	// Idempotency: re-superseding with a different pointer overwrites
	// superseded_by (matches the consolidate-flow pattern).
	other, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "alice", Content: "newer canonical digest", Tier: memory.TierEpisodic,
		ID: "session-end:newer",
	})
	if err != nil {
		t.Fatalf("remember newer: %v", err)
	}
	if err := svc.Supersede(ctx, "alice", working.ID, other.ID); err != nil {
		t.Fatalf("supersede again: %v", err)
	}
	got, err := svc.Get(ctx, "alice", working.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.SupersededBy == nil || *got.SupersededBy != other.ID {
		t.Fatalf("superseded_by = %v, want %q (idempotency: latest write wins)", got.SupersededBy, other.ID)
	}
}

// The upper gate of the write-time dedup split: a near-duplicate at or above
// AutoSupersedeMinScore stores the new memory and tombstones the old one in the
// background. Fingerprint dedup is off so the split path (not the exact-match
// coalesce) is what runs.
func TestAutoSupersedeReplacesNearDuplicate(t *testing.T) {
	svc := newService(t, service.WithFingerprintDedup(false), service.WithAutoSupersede(0.5))
	ctx := context.Background()

	old, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "alice", Content: "the user likes coffee", Tier: memory.TierSemantic,
	})
	if err != nil {
		t.Fatalf("remember old: %v", err)
	}

	var superseded bool
	nw, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "alice", Content: "the user likes coffee", Tier: memory.TierSemantic,
		AutoSuperseded: &superseded,
	})
	if err != nil {
		t.Fatalf("remember new: %v", err)
	}
	if !superseded {
		t.Fatal("expected AutoSuperseded=true for an identical near-duplicate")
	}
	if nw.ID == old.ID {
		t.Fatalf("auto-supersede should store a new memory, not coalesce into %q", old.ID)
	}

	svc.WaitBackground() // the supersede runs fire-and-forget

	all, err := svc.List(ctx, service.ListInput{Namespace: "alice", Tiers: []memory.Tier{memory.TierSemantic}})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 1 || all[0].ID != nw.ID {
		t.Fatalf("expected only the new memory %q live after supersede, got %+v", nw.ID, all)
	}
}

// The lower gate: a near-duplicate in the [MergeHintMinScore, AutoSupersede)
// band stores the new memory AND returns a hint pointing at the existing one,
// so the caller can decide to merge.
func TestMergeHintReturnedWithoutSuppression(t *testing.T) {
	svc := newService(t, service.WithFingerprintDedup(false),
		service.WithMergeHint(0.5), service.WithAutoSupersede(0.99))
	ctx := context.Background()

	first, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "alice", Content: "the user likes coffee", Tier: memory.TierSemantic,
	})
	if err != nil {
		t.Fatalf("remember first: %v", err)
	}

	var hint service.MergeHint
	var superseded bool
	second, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "alice", Content: "the user really likes strong coffee", Tier: memory.TierSemantic,
		MergeHint: &hint, AutoSuperseded: &superseded,
	})
	if err != nil {
		t.Fatalf("remember second: %v", err)
	}
	if superseded {
		t.Fatal("a merge-hint-band write must not supersede")
	}
	if hint.SimilarID != first.ID {
		t.Fatalf("hint.SimilarID = %q, want %q", hint.SimilarID, first.ID)
	}
	if hint.Score < 0.5 || hint.Score >= 0.99 {
		t.Fatalf("hint.Score = %v, want within the merge-hint band [0.5, 0.99)", hint.Score)
	}

	svc.WaitBackground()
	all, err := svc.List(ctx, service.ListInput{Namespace: "alice", Tiers: []memory.Tier{memory.TierSemantic}})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("merge-hint band keeps both memories, got %d: %+v", len(all), all)
	}
	_ = second
}

func TestHistoryWalksTheSupersessionChain(t *testing.T) {
	svc := newService(t, service.WithFingerprintDedup(false))
	ctx := context.Background()
	ns := "alice"

	v1, err := svc.Remember(ctx, service.RememberInput{Namespace: ns, Content: "office is in Paris", Tier: memory.TierSemantic})
	if err != nil {
		t.Fatalf("v1: %v", err)
	}
	v2, err := svc.Remember(ctx, service.RememberInput{Namespace: ns, Content: "office is in Berlin", Tier: memory.TierSemantic})
	if err != nil {
		t.Fatalf("v2: %v", err)
	}
	if err := svc.Supersede(ctx, ns, v1.ID, v2.ID); err != nil {
		t.Fatalf("supersede v1: %v", err)
	}
	v3, err := svc.Remember(ctx, service.RememberInput{Namespace: ns, Content: "office is in Lisbon", Tier: memory.TierSemantic})
	if err != nil {
		t.Fatalf("v3: %v", err)
	}
	if err := svc.Supersede(ctx, ns, v2.ID, v3.ID); err != nil {
		t.Fatalf("supersede v2: %v", err)
	}

	// From the live tip, history must surface the whole lineage including the
	// tombstoned ancestors.
	hist, err := svc.History(ctx, ns, v3.ID)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	got := make(map[string]bool, len(hist))
	for _, m := range hist {
		got[m.ID] = true
	}
	for _, want := range []string{v1.ID, v2.ID, v3.ID} {
		if !got[want] {
			t.Fatalf("history %v missing %q", got, want)
		}
	}
	if len(hist) != 3 {
		t.Fatalf("history length = %d, want 3", len(hist))
	}

	// A missing id is ErrNotFound, not an empty slice.
	if _, err := svc.History(ctx, ns, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("history of missing id: want ErrNotFound, got %v", err)
	}
}
