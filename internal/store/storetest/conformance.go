package storetest

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
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
	t.Run("TagMetadataFilter", func(t *testing.T) { testTagMetadataFilter(t, st, dims) })
	t.Run("ExcludeMetadataFilter", func(t *testing.T) { testExcludeMetadataFilter(t, st, dims) })
	t.Run("SetSuperseded", func(t *testing.T) { testSetSuperseded(t, st, dims) })
	t.Run("Reinforce", func(t *testing.T) { testReinforce(t, st, dims) })
	t.Run("DeleteIfExpiredBefore", func(t *testing.T) { testDeleteIfExpiredBefore(t, st, dims) })
	t.Run("KeywordSearchHostileQueries", func(t *testing.T) { testKeywordHostileQueries(t, st, dims) })
	t.Run("FilterNow", func(t *testing.T) { testFilterNow(t, st, dims) })
	t.Run("ConcurrentAccess", func(t *testing.T) { testConcurrentAccess(t, st, dims) })
	t.Run("Reassign", func(t *testing.T) { testReassign(t, st, dims) })
	t.Run("Retier", func(t *testing.T) { testRetier(t, st, dims) })
	t.Run("DeleteNamespace", func(t *testing.T) { testDeleteNamespace(t, st, dims) })
	t.Run("ListNamespaces", func(t *testing.T) { testListNamespaces(t, st, dims) })
	t.Run("TemporalAsOf", func(t *testing.T) { testTemporalAsOf(t, st, dims) })
	t.Run("SetConfidence", func(t *testing.T) { testSetConfidence(t, st, dims) })
	t.Run("GetByFingerprint", func(t *testing.T) { testGetByFingerprint(t, st, dims) })
}

func testGetByFingerprint(t *testing.T, st store.Store, dims int) {
	ctx := context.Background()
	ns := t.Name()
	now := time.Now().UTC()
	m := mem(ns, "fact", "the user likes coffee", vec(dims, 1)) // semantic
	mustUpsert(t, st, m)

	// A normalized restatement (case/whitespace) shares the fingerprint.
	fp := memory.Fingerprint("  The user   likes COFFEE ")
	got, err := st.GetByFingerprint(ctx, ns, memory.TierSemantic, fp, now)
	if err != nil {
		t.Fatalf("get by fingerprint: %v", err)
	}
	if got.ID != m.ID {
		t.Fatalf("fingerprint matched %q, want %q", got.ID, m.ID)
	}

	// Wrong tier, unknown content, and empty fingerprint all miss.
	if _, err := st.GetByFingerprint(ctx, ns, memory.TierWorking, fp, now); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("tier mismatch: want ErrNotFound, got %v", err)
	}
	if _, err := st.GetByFingerprint(ctx, ns, memory.TierSemantic, memory.Fingerprint("unrelated"), now); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown content: want ErrNotFound, got %v", err)
	}
	if _, err := st.GetByFingerprint(ctx, ns, memory.TierSemantic, "", now); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("empty fingerprint: want ErrNotFound, got %v", err)
	}

	// A superseded match is excluded so a dead duplicate never absorbs a write.
	repl := mem(ns, "repl", "the user prefers tea", vec(dims, 0, 1))
	mustUpsert(t, st, repl)
	if err := st.SetSuperseded(ctx, ns, m.ID, repl.ID); err != nil {
		t.Fatalf("supersede: %v", err)
	}
	if _, err := st.GetByFingerprint(ctx, ns, memory.TierSemantic, fp, now); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("superseded match: want ErrNotFound, got %v", err)
	}
}

func testDeleteNamespace(t *testing.T, st store.Store, dims int) {
	ctx := context.Background()
	nsDel := t.Name() + "-del"
	nsKeep := t.Name() + "-keep"
	mustUpsert(t, st, mem(nsDel, "a", "first to delete", vec(dims, 1)))
	mustUpsert(t, st, mem(nsDel, "b", "second to delete", vec(dims, 0, 1)))
	mustUpsert(t, st, mem(nsKeep, "c", "survivor", vec(dims, 1)))

	n, err := st.DeleteNamespace(ctx, nsDel)
	if err != nil {
		t.Fatalf("delete namespace: %v", err)
	}
	if n != 2 {
		t.Fatalf("DeleteNamespace returned %d, want 2", n)
	}
	// The rows are gone, including the vector index entries.
	if _, err := st.Get(ctx, nsDel, id(nsDel, "a")); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("deleted memory still present: %v", err)
	}
	res, err := st.VectorSearch(ctx, nsDel, vec(dims, 1), store.Filter{}, 5)
	if err != nil {
		t.Fatalf("vector search after delete: %v", err)
	}
	if len(res) != 0 {
		t.Errorf("vector index not cleared for the deleted namespace: %d hits", len(res))
	}
	// A sibling namespace is untouched.
	if _, err := st.Get(ctx, nsKeep, id(nsKeep, "c")); err != nil {
		t.Errorf("sibling namespace was affected by the delete: %v", err)
	}
	// Deleting an empty/unknown namespace returns 0 with no error.
	n, err = st.DeleteNamespace(ctx, t.Name()+"-empty")
	if err != nil {
		t.Fatalf("delete empty namespace: %v", err)
	}
	if n != 0 {
		t.Errorf("DeleteNamespace on an empty namespace returned %d, want 0", n)
	}
}

func testListNamespaces(t *testing.T, st store.Store, dims int) {
	ctx := context.Background()
	nsA := t.Name() + "-a"
	nsB := t.Name() + "-b"
	mustUpsert(t, st, mem(nsA, "a", "in a", vec(dims, 1)))
	mustUpsert(t, st, mem(nsB, "b", "in b", vec(dims, 0, 1)))

	got, err := st.ListNamespaces(ctx)
	if err != nil {
		t.Fatalf("list namespaces: %v", err)
	}
	// The conformance store is shared across subtests, so assert containment,
	// not equality.
	if !slices.Contains(got, nsA) || !slices.Contains(got, nsB) {
		t.Fatalf("ListNamespaces missing seeded namespaces: got %v, want to contain %q and %q", got, nsA, nsB)
	}
	// A namespace with multiple memories must appear exactly once (distinct).
	mustUpsert(t, st, mem(nsA, "a2", "also in a", vec(dims, 0, 0, 1)))
	got, err = st.ListNamespaces(ctx)
	if err != nil {
		t.Fatalf("list namespaces (after second insert): %v", err)
	}
	count := 0
	for _, ns := range got {
		if ns == nsA {
			count++
		}
	}
	if count != 1 {
		t.Errorf("namespace %q appears %d times, want 1 (must be distinct)", nsA, count)
	}
}

func testReassign(t *testing.T, st store.Store, dims int) {
	ctx := context.Background()
	from, to := t.Name()+"-from", t.Name()+"-to"

	moved := mem(from, "moved", "a memory to relocate", vec(dims, 1))
	stay := mem(from, "stay", "a memory that stays put", vec(dims, 0, 1))
	mustUpsert(t, st, moved)
	mustUpsert(t, st, stay)

	// Move only "moved" (plus a bogus ID, which must be skipped, not error).
	n, err := st.Reassign(ctx, from, []string{moved.ID, "does-not-exist"}, to)
	if err != nil {
		t.Fatalf("reassign: %v", err)
	}
	if n != 1 {
		t.Fatalf("reassign moved %d, want 1", n)
	}

	// The moved memory is now readable in the target namespace and gone from
	// the source.
	if _, err := st.Get(ctx, to, moved.ID); err != nil {
		t.Errorf("moved memory not in target namespace: %v", err)
	}
	if _, err := st.Get(ctx, from, moved.ID); err != store.ErrNotFound {
		t.Errorf("moved memory still in source namespace: %v", err)
	}
	// The untouched memory stays.
	if _, err := st.Get(ctx, from, stay.ID); err != nil {
		t.Errorf("stay memory should remain in source: %v", err)
	}

	// The vector index must follow the move: a search in the target namespace
	// finds it; a search in the source does not.
	res, err := st.VectorSearch(ctx, to, vec(dims, 1), store.Filter{}, 5)
	if err != nil {
		t.Fatalf("vector search target: %v", err)
	}
	if !containsScored(res, moved.ID) {
		t.Errorf("moved memory not vector-searchable in target namespace")
	}
	res, err = st.VectorSearch(ctx, from, vec(dims, 1), store.Filter{}, 5)
	if err != nil {
		t.Fatalf("vector search source: %v", err)
	}
	if containsScored(res, moved.ID) {
		t.Errorf("moved memory still vector-searchable in source namespace")
	}
}

func testRetier(t *testing.T, st store.Store, dims int) {
	ctx := context.Background()
	ns := t.Name()
	m := mem(ns, "m", "a durable fact", vec(dims, 1))
	m.Tier = memory.TierSemantic
	mustUpsert(t, st, m)

	exp := time.Now().UTC().Add(90 * 24 * time.Hour).Truncate(time.Millisecond)
	if err := st.Retier(ctx, ns, m.ID, memory.TierEpisodic, &exp); err != nil {
		t.Fatalf("retier: %v", err)
	}
	got, err := st.Get(ctx, ns, m.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Tier != memory.TierEpisodic {
		t.Errorf("tier = %q, want episodic", got.Tier)
	}
	if got.ExpiresAt == nil || !got.ExpiresAt.Equal(exp) {
		t.Errorf("expiry = %v, want %v", got.ExpiresAt, exp)
	}
	if err := st.Retier(ctx, ns, "missing", memory.TierEpisodic, &exp); err != store.ErrNotFound {
		t.Errorf("retier missing: want ErrNotFound, got %v", err)
	}
}

// testTemporalAsOf verifies time-travel recall: a superseded fact is excluded
// from default recall but reappears for an as_of within its validity window.
func testTemporalAsOf(t *testing.T, st store.Store, dims int) {
	ctx := context.Background()
	ns := t.Name()
	old := mem(ns, "old", "the capital is Bonn", vec(dims, 1))
	cur := mem(ns, "cur", "the capital is Berlin", vec(dims, 1))
	mustUpsert(t, st, old)
	mustUpsert(t, st, cur)

	// Supersede "old" with "cur": old gets superseded_by + valid_to=now.
	if err := st.SetSuperseded(ctx, ns, old.ID, cur.ID); err != nil {
		t.Fatalf("supersede: %v", err)
	}

	q := vec(dims, 1)
	// Default recall hides the superseded fact.
	now, err := st.VectorSearch(ctx, ns, q, store.Filter{}, 5)
	if err != nil {
		t.Fatalf("recall now: %v", err)
	}
	if containsScored(now, old.ID) {
		t.Errorf("superseded 'old' must not appear in default recall")
	}
	// Time-travel to before the supersession surfaces the then-valid fact.
	past := time.Now().Add(-time.Hour).UTC()
	asof, err := st.VectorSearch(ctx, ns, q, store.Filter{AsOf: past}, 5)
	if err != nil {
		t.Fatalf("recall as_of: %v", err)
	}
	if !containsScored(asof, old.ID) {
		t.Errorf("as_of recall before supersession should surface 'old'")
	}
}

func testSetConfidence(t *testing.T, st store.Store, dims int) {
	ctx := context.Background()
	ns := t.Name()
	m := mem(ns, "m", "a durable fact", vec(dims, 1))
	m.Tier = memory.TierSemantic
	seed := 0.4
	m.Confidence = &seed
	mustUpsert(t, st, m)

	now := time.Now().UTC().Truncate(time.Millisecond)
	if err := st.SetConfidence(ctx, ns, m.ID, 0.46, now); err != nil {
		t.Fatalf("set confidence: %v", err)
	}
	got, err := st.Get(ctx, ns, m.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Confidence == nil || *got.Confidence != 0.46 {
		t.Errorf("confidence = %v, want 0.46", got.Confidence)
	}
	if !got.UpdatedAt.Equal(now) {
		t.Errorf("updated_at = %v, want bumped to %v (decay baseline reset)", got.UpdatedAt, now)
	}
	if err := st.SetConfidence(ctx, ns, "missing", 0.5, now); err != store.ErrNotFound {
		t.Errorf("set confidence on missing: want ErrNotFound, got %v", err)
	}
}

func containsScored(res []store.Scored, id string) bool {
	for _, r := range res {
		if r.Memory.ID == id {
			return true
		}
	}
	return false
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

	original := mem(ns, "a", "original text", vec(dims, 1))
	createdAt := original.CreatedAt
	mustUpsert(t, st, original)

	// An update-by-ID carries a fresh CreatedAt/UpdatedAt (the service rebuilds
	// the Memory with now). The store must keep the original created_at but
	// advance updated_at.
	update := mem(ns, "a", "updated text", vec(dims, 0, 1))
	update.CreatedAt = createdAt.Add(time.Hour) // a (wrong) newer creation time
	update.UpdatedAt = createdAt.Add(time.Hour)
	mustUpsert(t, st, update)

	got, err := st.Get(ctx, ns, id(ns, "a"))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Content != "updated text" {
		t.Fatalf("update not applied: %q", got.Content)
	}
	if !got.CreatedAt.Equal(createdAt) {
		t.Errorf("created_at mutated on update: got %v, want %v (immutable)", got.CreatedAt, createdAt)
	}
	if !got.UpdatedAt.After(createdAt) {
		t.Errorf("updated_at not advanced on update: got %v, want > %v", got.UpdatedAt, createdAt)
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

// testTagMetadataFilter verifies that Filter.Tags (AND semantics) and
// Filter.Metadata (top-level key=value) narrow List, VectorSearch and
// KeywordSearch across backends.
func testTagMetadataFilter(t *testing.T, st store.Store, dims int) {
	ctx := context.Background()
	ns := t.Name()
	// Tokens reused as both memory ids and tags; consts keep goconst quiet.
	const (
		tagAuth = "auth"
		bug     = "bug"
		perf    = "perf"
		keyCat  = "category"
		catBug  = "bug_fixes"
	)

	bm := mem(ns, bug, "fixed the auth race condition", vec(dims, 1))
	bm.Tags = []string{bug, tagAuth}
	bm.Metadata = map[string]any{keyCat: catBug}
	mustUpsert(t, st, bm)

	pm := mem(ns, perf, "auth handler latency tuning", vec(dims, 1))
	pm.Tags = []string{perf, tagAuth}
	pm.Metadata = map[string]any{keyCat: "performance_findings"}
	mustUpsert(t, st, pm)

	plain := mem(ns, "plain", "unrelated note about auth", vec(dims, 1))
	mustUpsert(t, st, plain)

	// Single tag matches every memory carrying it.
	byTag := mustList(t, st, ns, store.Filter{Tags: []string{tagAuth}})
	if got := memIDs(byTag); len(got) != 2 || !containsMem(byTag, id(ns, bug)) {
		t.Fatalf("tag=auth should yield bug+perf, got %v", got)
	}

	// Multiple tags are ANDed.
	got := memIDs(mustList(t, st, ns, store.Filter{Tags: []string{tagAuth, bug}}))
	if len(got) != 1 || got[0] != id(ns, bug) {
		t.Fatalf("tags=auth+bug should yield only bug, got %v", got)
	}

	// Metadata key=value narrows to the matching category.
	got = memIDs(mustList(t, st, ns, store.Filter{Metadata: map[string]string{keyCat: "bug_fixes"}}))
	if len(got) != 1 || got[0] != id(ns, bug) {
		t.Fatalf("category=bug_fixes should yield only bug, got %v", got)
	}

	// ExcludeMetadata drops the matching category, keeping the rest.
	excluded := mustList(t, st, ns, store.Filter{ExcludeMetadata: map[string]string{keyCat: "bug_fixes"}})
	if len(excluded) != 2 || containsMem(excluded, id(ns, bug)) {
		t.Fatalf("exclude category=bug_fixes should drop only bug, got %v", memIDs(excluded))
	}

	// Tag + metadata filters compose on search legs too.
	f := store.Filter{Tags: []string{perf}, Metadata: map[string]string{keyCat: "performance_findings"}}
	vres, err := st.VectorSearch(ctx, ns, vec(dims, 1), f, 10)
	if err != nil {
		t.Fatalf("vector search: %v", err)
	}
	if ids := idsOf(vres); len(ids) != 1 || ids[0] != id(ns, perf) {
		t.Fatalf("filtered vector search should yield only perf, got %v", ids)
	}
	kres, err := st.KeywordSearch(ctx, ns, tagAuth, store.Filter{Tags: []string{bug}}, 10)
	if err != nil {
		t.Fatalf("keyword search: %v", err)
	}
	if ids := idsOf(kres); len(ids) != 1 || ids[0] != id(ns, bug) {
		t.Fatalf("filtered keyword search should yield only bug, got %v", ids)
	}
}

// testExcludeMetadataFilter verifies Filter.ExcludeMetadata drops memories
// carrying any listed key=value pair (the inverse of Metadata) across List,
// VectorSearch and KeywordSearch — the mechanism the OpenClaw plugin uses to
// keep a session from recalling its own just-captured turns.
func testExcludeMetadataFilter(t *testing.T, st store.Store, dims int) {
	ctx := context.Background()
	ns := t.Name()
	const keySession = "session"

	mine := mem(ns, "mine", "deploy notes for the auth service", vec(dims, 1))
	mine.Metadata = map[string]any{keySession: "s1"}
	mustUpsert(t, st, mine)

	other := mem(ns, "other", "deploy notes for the auth service", vec(dims, 1))
	other.Metadata = map[string]any{keySession: "s2"}
	mustUpsert(t, st, other)

	untagged := mem(ns, "untagged", "deploy notes for the auth service", vec(dims, 1))
	mustUpsert(t, st, untagged)

	// Excluding session s1 drops only the s1 capture; s2 and untagged remain.
	exclude := store.Filter{ExcludeMetadata: map[string]string{keySession: "s1"}}
	got := memIDs(mustList(t, st, ns, exclude))
	if len(got) != 2 || slices.Contains(got, id(ns, "mine")) {
		t.Fatalf("exclude session=s1 should yield other+untagged, got %v", got)
	}

	vres, err := st.VectorSearch(ctx, ns, vec(dims, 1), exclude, 10)
	if err != nil {
		t.Fatalf("vector search: %v", err)
	}
	if ids := idsOf(vres); slices.Contains(ids, id(ns, "mine")) || len(ids) != 2 {
		t.Fatalf("filtered vector search should drop the s1 capture, got %v", ids)
	}

	kres, err := st.KeywordSearch(ctx, ns, "deploy", exclude, 10)
	if err != nil {
		t.Fatalf("keyword search: %v", err)
	}
	if ids := idsOf(kres); slices.Contains(ids, id(ns, "mine")) || len(ids) != 2 {
		t.Fatalf("filtered keyword search should drop the s1 capture, got %v", ids)
	}
}

func mustList(t *testing.T, st store.Store, ns string, f store.Filter) []*memory.Memory {
	t.Helper()
	ms, err := st.List(context.Background(), ns, f, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	return ms
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

// testKeywordHostileQueries pins that keyword search treats user queries as
// data: FTS/tsquery operators, quotes, and non-ASCII input must never produce
// a syntax error from the underlying engine. Hit counts are backend-specific
// (the sqlite tokenizer drops non-ASCII; postgres stems it), so only the
// no-error contract is asserted here.
func testKeywordHostileQueries(t *testing.T, st store.Store, dims int) {
	ctx := context.Background()
	const ns = "hostile-queries"
	mustUpsert(t, st, mem(ns, "a", "plain ascii content about cats", vec(dims, 1)))

	queries := []string{
		`NEAR(foo bar)`,
		`"quoted phrase"`,
		`col:value AND x* OR (y)`,
		`cat's -toy`,
		"東京タワー",
		`café naïve`,
		`); DROP TABLE memories; --`,
	}
	for _, q := range queries {
		if _, err := st.KeywordSearch(ctx, ns, q, store.Filter{}, 5); err != nil {
			t.Errorf("KeywordSearch(%q) errored: %v", q, err)
		}
	}

	// A sane query must still match — guards against a sanitizer that starts
	// neutralizing everything into zero results.
	res, err := st.KeywordSearch(ctx, ns, "cats", store.Filter{}, 5)
	if err != nil {
		t.Fatalf("KeywordSearch(cats): %v", err)
	}
	if !slices.Contains(idsOf(res), id(ns, "a")) {
		t.Fatalf("plain query no longer matches after hostile queries: %v", idsOf(res))
	}
}

// testFilterNow pins that expiry filtering honors Filter.Now (the caller's
// injected clock) instead of the wall clock, and that the zero value falls
// back to time.Now(). The expiry clause is duplicated per backend, so this
// runs through the conformance suite to keep them in lockstep.
func testFilterNow(t *testing.T, st store.Store, dims int) {
	ctx := context.Background()
	const ns = "filter-now"
	base := time.Now().UTC().Truncate(time.Millisecond)
	expiry := base.Add(time.Hour)

	m := mem(ns, "ttl", "fact with a one hour ttl", vec(dims, 1))
	m.ExpiresAt = &expiry
	mustUpsert(t, st, m)

	list := func(f store.Filter) []*memory.Memory {
		t.Helper()
		mems, err := st.List(ctx, ns, f, 0)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		return mems
	}

	if got := list(store.Filter{Now: base}); len(got) != 1 {
		t.Fatalf("before expiry (Now=base): want 1 memory, got %d", len(got))
	}
	if got := list(store.Filter{Now: base.Add(2 * time.Hour)}); len(got) != 0 {
		t.Fatalf("after expiry (Now=base+2h): want 0 memories, got %d", len(got))
	}
	if got := list(store.Filter{Now: base.Add(2 * time.Hour), IncludeExpired: true}); len(got) != 1 {
		t.Fatalf("IncludeExpired after expiry: want 1 memory, got %d", len(got))
	}
	// Zero Now falls back to the wall clock, which is well before the expiry.
	if got := list(store.Filter{}); len(got) != 1 {
		t.Fatalf("zero Now (wall clock, before expiry): want 1 memory, got %d", len(got))
	}

	// Search legs honor it too.
	res, err := st.VectorSearch(ctx, ns, vec(dims, 1), store.Filter{Now: base.Add(2 * time.Hour)}, 5)
	if err != nil {
		t.Fatalf("vector search: %v", err)
	}
	if len(res) != 0 {
		t.Fatalf("vector search after expiry: want 0, got %d", len(res))
	}
}

// testConcurrentAccess hammers one namespace from several goroutines with
// mixed reads, writes, reinforcement, and deletes. It asserts no operation
// fails (beyond ErrNotFound on a racing delete) and exists chiefly so the
// -race runs in CI exercise real store concurrency: sqlite's single-writer
// handling (busy_timeout) and the pgx pool both get contention here.
func testConcurrentAccess(t *testing.T, st store.Store, dims int) {
	ctx := context.Background()
	const ns = "concurrent"
	const workers, iters = 8, 25

	var wg sync.WaitGroup
	// Each iteration can emit up to 5 errors; size the channel for the worst
	// case so a pathologically failing store fails loudly instead of blocking
	// the senders and timing the test out.
	errs := make(chan error, workers*iters*5)
	for w := range workers {
		wg.Go(func() {
			for i := range iters {
				short := fmt.Sprintf("w%d-i%d", w, i)
				m := mem(ns, short, fmt.Sprintf("memory %s about shared topic", short), vec(dims, float32(w), float32(i)))
				if err := st.Upsert(ctx, m); err != nil {
					errs <- fmt.Errorf("upsert %s: %w", short, err)
					continue
				}
				if _, err := st.VectorSearch(ctx, ns, vec(dims, float32(w)), store.Filter{}, 5); err != nil {
					errs <- fmt.Errorf("vector search: %w", err)
				}
				if _, err := st.KeywordSearch(ctx, ns, "shared topic", store.Filter{}, 5); err != nil {
					errs <- fmt.Errorf("keyword search: %w", err)
				}
				if err := st.Reinforce(ctx, ns, []string{m.ID}, time.Now().UTC(), nil); err != nil {
					errs <- fmt.Errorf("reinforce: %w", err)
				}
				if _, err := st.Get(ctx, ns, m.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
					errs <- fmt.Errorf("get: %w", err)
				}
				// Delete every third memory so reads race tombstoned rows too.
				if i%3 == 0 {
					if err := st.Delete(ctx, ns, m.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
						errs <- fmt.Errorf("delete: %w", err)
					}
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	// The store must still be coherent: a full list succeeds and contains
	// exactly the non-deleted writes.
	mems, err := st.List(ctx, ns, store.Filter{}, 0)
	if err != nil {
		t.Fatalf("list after hammer: %v", err)
	}
	deletedPerWorker := (iters + 2) / 3 // i%3==0 for i in [0,iters)
	want := workers * (iters - deletedPerWorker)
	if len(mems) != want {
		t.Fatalf("list = %d memories, want %d", len(mems), want)
	}
}
