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
	t.Run("VectorRanking", func(t *testing.T) { testVectorRanking(t, st, dims) })
	t.Run("KeywordSearch", func(t *testing.T) { testKeyword(t, st, dims) })
	t.Run("Filters", func(t *testing.T) { testFilters(t, st, dims) })
	t.Run("SetSuperseded", func(t *testing.T) { testSetSuperseded(t, st, dims) })
	t.Run("Reinforce", func(t *testing.T) { testReinforce(t, st, dims) })
}

func vec(dims int, head ...float32) []float32 {
	v := make([]float32, dims)
	copy(v, head)
	return v
}

func mem(ns, id, content string, v []float32) *memory.Memory {
	now := time.Now().UTC().Truncate(time.Millisecond)
	return &memory.Memory{
		ID: id, Namespace: ns, Tier: memory.TierSemantic, Content: content,
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

	got, err := st.Get(ctx, ns, "a")
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

	if _, err := st.Get(ctx, ns+"-other", "a"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cross-namespace get: want ErrNotFound, got %v", err)
	}
	if err := st.Delete(ctx, ns, "a"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := st.Get(ctx, ns, "a"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("get after delete: want ErrNotFound, got %v", err)
	}
	if err := st.Delete(ctx, ns, "a"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("double delete: want ErrNotFound, got %v", err)
	}
}

func testUpdateInPlace(t *testing.T, st store.Store, dims int) {
	ctx := context.Background()
	ns := t.Name()
	mustUpsert(t, st, mem(ns, "a", "original text", vec(dims, 1)))
	mustUpsert(t, st, mem(ns, "a", "updated text", vec(dims, 0, 1)))

	got, err := st.Get(ctx, ns, "a")
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
	if res[0].Memory.ID != "a" || res[1].Memory.ID != "c" {
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
	if !slices.Contains(got, "c") {
		t.Fatalf("expected 'c' in keyword results, got %v", got)
	}
	if slices.Contains(got, "b") {
		t.Fatalf("did not expect 'b' (dogs) in results for 'cats', got %v", got)
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

	target := "live"
	superseded := mem(ns, "old", "outdated fact", vec(dims, 1))
	superseded.SupersededBy = &target
	mustUpsert(t, st, superseded)

	res, err := st.VectorSearch(ctx, ns, vec(dims, 1), store.Filter{}, 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if got := idsOf(res); len(got) != 1 || got[0] != "live" {
		t.Fatalf("default filter should yield only 'live', got %v", got)
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
	if !containsMem(exp, "exp") {
		t.Fatalf("ListExpired should include 'exp', got %v", memIDs(exp))
	}
}

func testSetSuperseded(t *testing.T, st store.Store, dims int) {
	ctx := context.Background()
	ns := t.Name()
	mustUpsert(t, st, mem(ns, "old", "the sky is green", vec(dims, 1)))
	mustUpsert(t, st, mem(ns, "new", "the sky is blue", vec(dims, 1)))

	if err := st.SetSuperseded(ctx, ns, "old", "new"); err != nil {
		t.Fatalf("set superseded: %v", err)
	}
	got, err := st.Get(ctx, ns, "old")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.SupersededBy == nil || *got.SupersededBy != "new" {
		t.Fatalf("superseded_by not set: %v", got.SupersededBy)
	}

	res, err := st.VectorSearch(ctx, ns, vec(dims, 1), store.Filter{}, 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if slices.Contains(idsOf(res), "old") {
		t.Fatalf("superseded memory should be excluded by default, got %v", idsOf(res))
	}

	if err := st.SetSuperseded(ctx, ns, "missing", "new"); !errors.Is(err, store.ErrNotFound) {
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
	if err := st.Reinforce(ctx, ns, []string{"short", "long"}, accessed, &slid); err != nil {
		t.Fatalf("reinforce: %v", err)
	}

	gotShort, _ := st.Get(ctx, ns, "short")
	gotLong, _ := st.Get(ctx, ns, "long")
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
	if err := st.Reinforce(ctx, ns, []string{"missing"}, accessed, nil); err != nil {
		t.Fatalf("reinforce of missing id should be a no-op, got %v", err)
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
