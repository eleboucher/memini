package storetest

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/store"
)

// Run executes the full conformance suite against st. dims is the store's
// embedding dimensionality (>= 4). Subtests use distinct namespaces so they can
// share a single backing store without interfering.
func Run(t *testing.T, st store.Store, dims int) {
	t.Helper()
	if dims < 4 {
		t.Fatalf("storetest needs dims >= 4, got %d", dims)
	}
	t.Run("UpsertGetDelete", func(t *testing.T) { testUpsertGetDelete(t, st, dims) })
	t.Run("UpdateInPlace", func(t *testing.T) { testUpdateInPlace(t, st, dims) })
	t.Run("CrossNamespaceUpsert", func(t *testing.T) { testCrossNamespaceUpsert(t, st, dims) })
	t.Run("VectorRanking", func(t *testing.T) { testVectorRanking(t, st, dims) })
	t.Run("KeywordSearch", func(t *testing.T) { testKeyword(t, st, dims) })
	t.Run("Filters", func(t *testing.T) { testFilters(t, st, dims) })
	t.Run("SetSuperseded", func(t *testing.T) { testSetSuperseded(t, st, dims) })
	t.Run("Reinforce", func(t *testing.T) { testReinforce(t, st, dims) })
	t.Run("DeleteIfExpiredBefore", func(t *testing.T) { testDeleteIfExpiredBefore(t, st, dims) })
}

func vec(dims int, head ...float32) []float32 {
	v := make([]float32, dims)
	copy(v, head)
	return v
}

// id creates a globally-unique memory ID by scoping a short label to the
// namespace. This avoids cross-subtest collisions when subtests run against
// the same backing store (IDs are globally unique within the store).
func id(ns, short string) string { return ns + "/" + short }

func mem(ns, short, content string, v []float32) *memory.Memory {
	now := time.Now().UTC().Truncate(time.Millisecond)
	return &memory.Memory{
		ID: id(ns, short), Namespace: ns, Tier: memory.TierSemantic, Content: content,
		Importance: 0.5, CreatedAt: now, UpdatedAt: now, LastAccessedAt: now, Embedding: v,
	}
}

func mustUpsert(t *testing.T, st store.Store, m *memory.Memory) {
	t.Helper()
	if err := st.Upsert(context.Background(), m); err != nil {
		t.Fatalf("upsert %s: %v", m.ID, err)
	}
}

func testUpsertGetDelete(t *testing.T, st store.Store, dims int) {
	ctx := context.Background()
	ns := t.Name()
	m := mem(ns, "a", "the cat sat on the mat", vec(dims, 1))
	m.Tags = []string{"animals", "cat"}
	m.Metadata = map[string]any{"source": "test"}
	mustUpsert(t, st, m)

	got, err := st.Get(ctx, ns, id(ns, "a"))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Content != m.Content || got.Tier != memory.TierSemantic {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}
	if len(got.Tags) != 2 || got.Tags[0] != "animals" {
		t.Fatalf("tags not preserved: %v", got.Tags)
	}
	if got.Metadata["source"] != "test" {
		t.Fatalf("metadata not preserved: %v", got.Metadata)
	}

	if _, err := st.Get(ctx, ns+"-other", id(ns, "a")); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cross-namespace get: want ErrNotFound, got %v", err)
	}
	if err := st.Delete(ctx, ns, id(ns, "a")); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := st.Get(ctx, ns, id(ns, "a")); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("get after delete: want ErrNotFound, got %v", err)
	}
	if err := st.Delete(ctx, ns, id(ns, "a")); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("double delete: want ErrNotFound, got %v", err)
	}
}

func testUpdateInPlace(t *testing.T, st store.Store, dims int) {
	ctx := context.Background()
	ns := t.Name()
	mustUpsert(t, st, mem(ns, "a", "original text", vec(dims, 1)))
	mustUpsert(t, st, mem(ns, "a", "updated text", vec(dims, 0, 1)))

	got, err := st.Get(ctx, ns, id(ns, "a"))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Content != "updated text" {
		t.Fatalf("update not applied: %q", got.Content)
	}
	res, err := st.VectorSearch(ctx, ns, vec(dims, 0, 1), store.Filter{}, 5)
	if err != nil {
		t.Fatalf("vector search: %v", err)
	}
	if len(res) != 1 {
		t.Fatalf("expected exactly one row after in-place update, got %d", len(res))
	}
}

func testCrossNamespaceUpsert(t *testing.T, st store.Store, dims int) {
	ctx := context.Background()
	nsA := t.Name() + "-a"
	nsB := t.Name() + "-b"

	// Use an ID scoped to nsA; nsB should not be allowed to claim it.
	sharedID := id(nsA, "x")
	m := mem(nsA, "x", "original", vec(dims, 1))
	mustUpsert(t, st, m)

	// Upserting the same ID under a different namespace must be rejected.
	attacker := &memory.Memory{
		ID: sharedID, Namespace: nsB, Tier: memory.TierSemantic, Content: "attacker",
		Importance: 0.5, CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt,
		LastAccessedAt: m.LastAccessedAt, Embedding: vec(dims, 0, 1),
	}
	if err := st.Upsert(ctx, attacker); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("cross-namespace upsert: want ErrConflict, got %v", err)
	}

	// The original memory must be untouched.
	got, err := st.Get(ctx, nsA, sharedID)
	if err != nil {
		t.Fatalf("get after failed cross-ns upsert: %v", err)
	}
	if got.Content != "original" {
		t.Fatalf("original memory was modified: %q", got.Content)
	}

	// The attacker namespace must not have a copy.
	if _, err := st.Get(ctx, nsB, sharedID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("attacker namespace should not have the memory, got %v", err)
	}
}

func testVectorRanking(t *testing.T, st store.Store, dims int) {
	ctx := context.Background()
	ns := t.Name()
	mustUpsert(t, st, mem(ns, "a", "the cat sat on the mat", vec(dims, 1)))
	mustUpsert(t, st, mem(ns, "b", "dogs are loyal animals", vec(dims, 0, 1)))
	mustUpsert(t, st, mem(ns, "c", "felines love naps", vec(dims, 0.9, 0.1)))
	mustUpsert(t, st, mem(ns+"-other", "z", "secret", vec(dims, 1)))

	res, err := st.VectorSearch(ctx, ns, vec(dims, 1), store.Filter{}, 2)
	if err != nil {
		t.Fatalf("vector search: %v", err)
	}
	if len(res) != 2 {
		t.Fatalf("want 2 results, got %d", len(res))
	}
	if res[0].Memory.ID != id(ns, "a") || res[1].Memory.ID != id(ns, "c") {
		t.Fatalf("ranking wrong: %v", idsOf(res))
	}
	for _, r := range res {
		if r.Memory.Namespace != ns {
			t.Fatalf("namespace leak: %s", r.Memory.Namespace)
		}
	}
}

func testKeyword(t *testing.T, st store.Store, dims int) {
	ctx := context.Background()
	ns := t.Name()
	mustUpsert(t, st, mem(ns, "a", "the cat sat on the mat", vec(dims, 1)))
	mustUpsert(t, st, mem(ns, "b", "dogs are loyal animals", vec(dims, 0, 1)))
	mustUpsert(t, st, mem(ns, "c", "felines and cats love naps", vec(dims, 0.9, 0.1)))

	res, err := st.KeywordSearch(ctx, ns, "cats", store.Filter{}, 10)
	if err != nil {
		t.Fatalf("keyword search: %v", err)
	}
	// Backends differ on stemming, but "c" must match and "dogs" (b) must not.
	got := idsOf(res)
	if !slices.Contains(got, id(ns, "c")) {
		t.Fatalf("expected %q in keyword results, got %v", id(ns, "c"), got)
	}
	if slices.Contains(got, id(ns, "b")) {
		t.Fatalf("did not expect %q (dogs) in results for 'cats', got %v", id(ns, "b"), got)
	}
}

func testFilters(t *testing.T, st store.Store, dims int) {
	ctx := context.Background()
	ns := t.Name()

	past := time.Now().Add(-time.Hour)
	expired := mem(ns, "exp", "stale fact", vec(dims, 1))
	expired.ExpiresAt = &past
	mustUpsert(t, st, expired)

	live := mem(ns, "live", "fresh fact", vec(dims, 1))
	mustUpsert(t, st, live)

	target := id(ns, "live")
	superseded := mem(ns, "old", "outdated fact", vec(dims, 1))
	superseded.SupersededBy = &target
	mustUpsert(t, st, superseded)

	res, err := st.VectorSearch(ctx, ns, vec(dims, 1), store.Filter{}, 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if got := idsOf(res); len(got) != 1 || got[0] != id(ns, "live") {
		t.Fatalf("default filter should yield only %q, got %v", id(ns, "live"), got)
	}

	res, err = st.VectorSearch(ctx, ns, vec(dims, 1),
		store.Filter{IncludeExpired: true, IncludeSuperseded: true}, 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(res) != 3 {
		t.Fatalf("inclusive filter should yield 3, got %v", idsOf(res))
	}

	exp, err := st.ListExpired(ctx, time.Now(), 100)
	if err != nil {
		t.Fatalf("list expired: %v", err)
	}
	if !containsMem(exp, id(ns, "exp")) {
		t.Fatalf("ListExpired should include %q, got %v", id(ns, "exp"), memIDs(exp))
	}
}

func testSetSuperseded(t *testing.T, st store.Store, dims int) {
	ctx := context.Background()
	ns := t.Name()
	mustUpsert(t, st, mem(ns, "old", "the sky is green", vec(dims, 1)))
	mustUpsert(t, st, mem(ns, "new", "the sky is blue", vec(dims, 1)))

	if err := st.SetSuperseded(ctx, ns, id(ns, "old"), id(ns, "new")); err != nil {
		t.Fatalf("set superseded: %v", err)
	}
	got, err := st.Get(ctx, ns, id(ns, "old"))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.SupersededBy == nil || *got.SupersededBy != id(ns, "new") {
		t.Fatalf("superseded_by not set: %v", got.SupersededBy)
	}

	res, err := st.VectorSearch(ctx, ns, vec(dims, 1), store.Filter{}, 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if slices.Contains(idsOf(res), id(ns, "old")) {
		t.Fatalf("superseded memory should be excluded by default, got %v", idsOf(res))
	}

	if err := st.SetSuperseded(ctx, ns, id(ns, "missing"), id(ns, "new")); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("set superseded on missing: want ErrNotFound, got %v", err)
	}
}

func testReinforce(t *testing.T, st store.Store, dims int) {
	ctx := context.Background()
	ns := t.Name()
	now := time.Now().UTC().Truncate(time.Millisecond)
	exp := now.Add(time.Hour)

	short := mem(ns, "short", "transient", vec(dims, 1))
	short.Tier = memory.TierWorking
	short.ExpiresAt = &exp
	mustUpsert(t, st, short)

	long := mem(ns, "long", "durable", vec(dims, 1)) // semantic, no expiry
	mustUpsert(t, st, long)

	accessed := now.Add(30 * time.Minute)
	slid := accessed.Add(time.Hour)
	if err := st.Reinforce(ctx, ns, []string{id(ns, "short"), id(ns, "long")}, accessed, &slid); err != nil {
		t.Fatalf("reinforce: %v", err)
	}

	gotShort, _ := st.Get(ctx, ns, id(ns, "short"))
	gotLong, _ := st.Get(ctx, ns, id(ns, "long"))
	if gotShort.AccessCount != 1 || gotLong.AccessCount != 1 {
		t.Fatalf("access_count not bumped: short=%d long=%d", gotShort.AccessCount, gotLong.AccessCount)
	}
	if !gotShort.LastAccessedAt.Equal(accessed) {
		t.Fatalf("last_accessed_at = %v, want %v", gotShort.LastAccessedAt, accessed)
	}
	if gotShort.ExpiresAt == nil || !gotShort.ExpiresAt.Equal(slid) {
		t.Fatalf("short-term TTL not slid: %v, want %v", gotShort.ExpiresAt, slid)
	}
	if gotLong.ExpiresAt != nil {
		t.Fatalf("durable memory must not gain an expiry, got %v", gotLong.ExpiresAt)
	}
	if err := st.Reinforce(ctx, ns, []string{id(ns, "missing")}, accessed, nil); err != nil {
		t.Fatalf("reinforce of missing id should be a no-op, got %v", err)
	}
}

func testDeleteIfExpiredBefore(t *testing.T, st store.Store, dims int) {
	ctx := context.Background()
	ns := t.Name()
	now := time.Now().UTC()

	past := now.Add(-time.Hour)
	future := now.Add(time.Hour)

	expired := mem(ns, "exp", "stale", vec(dims, 1))
	expired.ExpiresAt = &past
	mustUpsert(t, st, expired)

	live := mem(ns, "live", "fresh", vec(dims, 0, 1))
	mustUpsert(t, st, live)

	// Cutoff older than the expiry: memory is not yet considered expired at that time.
	if err := st.DeleteIfExpiredBefore(ctx, ns, id(ns, "exp"), past.Add(-time.Minute)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expired-before older cutoff: want ErrNotFound, got %v", err)
	}

	// A memory without an expiry must not be deleted.
	if err := st.DeleteIfExpiredBefore(ctx, ns, id(ns, "live"), future); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("durable memory: want ErrNotFound, got %v", err)
	}

	// Actually-expired memory is deleted.
	if err := st.DeleteIfExpiredBefore(ctx, ns, id(ns, "exp"), now); err != nil {
		t.Fatalf("delete expired: %v", err)
	}
	if _, err := st.Get(ctx, ns, id(ns, "exp")); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("memory should be gone, got %v", err)
	}

	// Idempotent: deleting again returns ErrNotFound.
	if err := st.DeleteIfExpiredBefore(ctx, ns, id(ns, "exp"), now); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("double delete: want ErrNotFound, got %v", err)
	}
}

func idsOf(res []store.Scored) []string {
	out := make([]string, len(res))
	for i, r := range res {
		out[i] = r.Memory.ID
	}
	return out
}

func memIDs(ms []*memory.Memory) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.ID
	}
	return out
}

func containsMem(ms []*memory.Memory, id string) bool {
	for _, m := range ms {
		if m.ID == id {
			return true
		}
	}
	return false
}
