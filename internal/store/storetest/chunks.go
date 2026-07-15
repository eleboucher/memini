package storetest

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/store"
)

// testChunks is the conformance suite for store.ChunkStore. Both backends must
// behave identically here, which is the point: sqlite reaches its chunks
// through a rowid mapping table and Postgres through a foreign key, and those
// two very different shapes have to be indistinguishable from the outside.
func testChunks(t *testing.T, st store.Store, dims int) {
	cs, ok := st.(store.ChunkStore)
	if !ok {
		t.Skip("store does not implement store.ChunkStore")
	}
	t.Run("SearchPoolsToBestChunk", func(t *testing.T) { testChunkPooling(t, st, cs, dims) })
	t.Run("DocVectorStillFindsAChunkedMemory", func(t *testing.T) { testChunkNoRegression(t, st, cs, dims) })
	t.Run("UpsertWithoutChunksClearsThem", func(t *testing.T) { testChunkWipeOnUpsert(t, st, cs, dims) })
	t.Run("DeleteCascades", func(t *testing.T) { testChunkDeleteCascade(t, st, cs, dims) })
	t.Run("DeleteNamespaceCascades", func(t *testing.T) { testChunkDeleteNamespaceCascade(t, st, cs, dims) })
	t.Run("ExpiryDeleteCascades", func(t *testing.T) { testChunkExpiryCascade(t, st, cs, dims) })
	t.Run("ReassignMovesChunks", func(t *testing.T) { testChunkReassign(t, st, cs, dims) })
	t.Run("FilterApplies", func(t *testing.T) { testChunkFilter(t, st, cs, dims) })
	t.Run("ListUnchunked", func(t *testing.T) { testListUnchunked(t, st, cs, dims) })
	t.Run("WrongDimsErrors", func(t *testing.T) { testChunkWrongDims(t, st, cs, dims) })
}

// chunked builds a memory whose document vector points one way and whose chunks
// point elsewhere, so a hit can be attributed to the leg that produced it.
func chunked(ns, short, content string, docVec []float32, chunkVecs ...[]float32) *memory.Memory {
	m := mem(ns, short, content, docVec)
	for i, v := range chunkVecs {
		m.Chunks = append(m.Chunks, memory.Chunk{Idx: i, Embedding: v})
	}
	return m
}

// testChunkPooling is the core promise: a memory is returned once, scored by
// its BEST chunk, not once per chunk and not by its average.
func testChunkPooling(t *testing.T, st store.Store, cs store.ChunkStore, dims int) {
	ctx := context.Background()
	ns := t.Name()

	// Chunk 1 sits exactly on the query; chunk 0 is far from it.
	mustUpsert(t, st, chunked(ns, "multi", "long content", vec(dims, 0, 1),
		vec(dims, 0, 0, 1), // far
		vec(dims, 1),       // exact match for the query below
	))
	mustUpsert(t, st, chunked(ns, "single", "other content", vec(dims, 0, 1),
		vec(dims, 0.5, 0.5),
	))

	got, err := cs.ChunkVectorSearch(ctx, ns, vec(dims, 1), store.Filter{}, 10)
	if err != nil {
		t.Fatalf("chunk search: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %v, want 2 results — a memory must appear once however many chunks it has", ids(got))
	}
	if got[0].Memory.ID != id(ns, "multi") {
		t.Fatalf("best result = %q, want the memory whose best chunk matches exactly", got[0].Memory.ID)
	}
	// Pooled on the BEST chunk: averaging in the far chunk would have sunk it
	// below "single".
	if got[0].Score <= got[1].Score {
		t.Errorf("scores = %v, %v: the exact-matching chunk must win", got[0].Score, got[1].Score)
	}
	// Same space as VectorSearch's, since recall gates on absolute values.
	if got[0].Score <= 0 || got[0].Score > 1 {
		t.Errorf("score %v outside VectorSearch's (0,1] range", got[0].Score)
	}
}

// testChunkNoRegression is the assertion that makes chunking safe to ship: the
// document leg is untouched, so a chunked memory is still found exactly as it
// was before. Chunks may only ADD hits.
func testChunkNoRegression(t *testing.T, st store.Store, _ store.ChunkStore, dims int) {
	ctx := context.Background()
	ns := t.Name()
	mustUpsert(t, st, chunked(ns, "m", "content", vec(dims, 1), vec(dims, 0, 0, 1)))

	got, err := st.VectorSearch(ctx, ns, vec(dims, 1), store.Filter{}, 10)
	if err != nil {
		t.Fatalf("vector search: %v", err)
	}
	if len(got) != 1 || got[0].Memory.ID != id(ns, "m") {
		t.Fatalf("VectorSearch = %v, want the chunked memory found by its document vector", ids(got))
	}
	// And exactly once — VectorSearch must not have grown a chunk join.
	if len(got) > 1 {
		t.Errorf("VectorSearch returned %d rows for one memory", len(got))
	}
}

// testChunkWipeOnUpsert pins Memory.Chunks' contract. Missing chunks are
// repaired by the backfill; stale chunks would make recall return a memory
// whose content no longer holds the passage that matched.
func testChunkWipeOnUpsert(t *testing.T, st store.Store, cs store.ChunkStore, dims int) {
	ctx := context.Background()
	ns := t.Name()
	mustUpsert(t, st, chunked(ns, "m", "content", vec(dims, 0, 1), vec(dims, 1)))

	if got, err := cs.ChunkVectorSearch(ctx, ns, vec(dims, 1), store.Filter{}, 10); err != nil || len(got) != 1 {
		t.Fatalf("precondition: chunk search = %v (err %v), want 1 hit", ids(got), err)
	}
	// Re-upsert the same id with no chunks: the old ones must go.
	mustUpsert(t, st, mem(ns, "m", "content rewritten", vec(dims, 0, 1)))
	got, err := cs.ChunkVectorSearch(ctx, ns, vec(dims, 1), store.Filter{}, 10)
	if err != nil {
		t.Fatalf("chunk search: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("chunk search = %v, want none: an upsert without chunks must clear them", ids(got))
	}
}

func testChunkDeleteCascade(t *testing.T, st store.Store, cs store.ChunkStore, dims int) {
	ctx := context.Background()
	ns := t.Name()
	mustUpsert(t, st, chunked(ns, "m", "content", vec(dims, 0, 1), vec(dims, 1)))
	if err := st.Delete(ctx, ns, id(ns, "m")); err != nil {
		t.Fatalf("delete: %v", err)
	}
	// An orphaned chunk row would be a live vector pointing at a memory that no
	// longer exists — a join miss at best, another memory's row at worst once
	// the id is reused.
	assertNoChunks(t, cs, ns, dims, "after Delete")
}

func testChunkDeleteNamespaceCascade(t *testing.T, st store.Store, cs store.ChunkStore, dims int) {
	ctx := context.Background()
	ns := t.Name()
	mustUpsert(t, st, chunked(ns, "m", "content", vec(dims, 0, 1), vec(dims, 1)))
	if _, err := st.DeleteNamespace(ctx, ns); err != nil {
		t.Fatalf("delete namespace: %v", err)
	}
	assertNoChunks(t, cs, ns, dims, "after DeleteNamespace")
}

func testChunkExpiryCascade(t *testing.T, st store.Store, cs store.ChunkStore, dims int) {
	ctx := context.Background()
	ns := t.Name()
	past := time.Now().Add(-time.Hour)
	m := chunked(ns, "m", "content", vec(dims, 0, 1), vec(dims, 1))
	m.ExpiresAt = &past
	mustUpsert(t, st, m)
	if err := st.DeleteIfExpiredBefore(ctx, ns, id(ns, "m"), time.Now()); err != nil {
		t.Fatalf("delete if expired: %v", err)
	}
	assertNoChunks(t, cs, ns, dims, "after DeleteIfExpiredBefore")
}

// testChunkReassign covers the one path a Postgres FK does not: the memory
// still exists, it just lives somewhere else now.
func testChunkReassign(t *testing.T, st store.Store, cs store.ChunkStore, dims int) {
	ctx := context.Background()
	from, to := t.Name()+"-from", t.Name()+"-to"
	// Every other subtest is idempotent because its namespace is its own and it
	// only ever upserts the same ids back over themselves. This one moves an id
	// BETWEEN namespaces, so a second run against a store that kept the first
	// run's data (the Postgres integration database is reused) would find the
	// id already living in `to` and fail the upsert with ErrConflict. Clear both
	// ends first so the test states its own preconditions.
	for _, ns := range []string{from, to} {
		if _, err := st.DeleteNamespace(ctx, ns); err != nil {
			t.Fatalf("clear %s: %v", ns, err)
		}
	}
	mustUpsert(t, st, chunked(from, "m", "content", vec(dims, 0, 1), vec(dims, 1)))

	if _, err := st.Reassign(ctx, from, []string{id(from, "m")}, to); err != nil {
		t.Fatalf("reassign: %v", err)
	}
	// Chunks must follow the memory...
	got, err := cs.ChunkVectorSearch(ctx, to, vec(dims, 1), store.Filter{}, 10)
	if err != nil {
		t.Fatalf("chunk search (to): %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("chunk search in the new namespace = %v, want the moved memory", ids(got))
	}
	// ...and must not be reachable in the old one, which would leak a memory
	// across a namespace boundary.
	if got, err := cs.ChunkVectorSearch(ctx, from, vec(dims, 1), store.Filter{}, 10); err != nil {
		t.Fatalf("chunk search (from): %v", err)
	} else if len(got) != 0 {
		t.Fatalf("chunk search in the old namespace = %v, want none: chunks leaked", ids(got))
	}
}

// testChunkFilter pins that Filter applies to the MEMORY. Without it, chunk
// recall would return expired or superseded memories the document leg hides.
func testChunkFilter(t *testing.T, st store.Store, cs store.ChunkStore, dims int) {
	ctx := context.Background()
	ns := t.Name()
	mustUpsert(t, st, chunked(ns, "sem", "a", vec(dims, 0, 1), vec(dims, 1)))
	sup := chunked(ns, "old", "b", vec(dims, 0, 1), vec(dims, 1))
	sup.Tier = memory.TierEpisodic
	mustUpsert(t, st, sup)

	got, err := cs.ChunkVectorSearch(ctx, ns, vec(dims, 1),
		store.Filter{Tiers: []memory.Tier{memory.TierSemantic}}, 10)
	if err != nil {
		t.Fatalf("chunk search: %v", err)
	}
	if len(got) != 1 || got[0].Memory.ID != id(ns, "sem") {
		t.Fatalf("filtered chunk search = %v, want only the semantic memory", ids(got))
	}

	// Superseded rows are excluded by default, exactly as in VectorSearch.
	if err := st.SetSuperseded(ctx, ns, id(ns, "sem"), id(ns, "old")); err != nil {
		t.Fatalf("set superseded: %v", err)
	}
	got, err = cs.ChunkVectorSearch(ctx, ns, vec(dims, 1),
		store.Filter{Tiers: []memory.Tier{memory.TierSemantic}}, 10)
	if err != nil {
		t.Fatalf("chunk search after supersede: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("chunk search returned a superseded memory: %v", ids(got))
	}
}

// testListUnchunked covers the backfill's work queue: long memories with no
// chunks, and nothing else.
func testListUnchunked(t *testing.T, st store.Store, cs store.ChunkStore, dims int) {
	ctx := context.Background()
	ns := t.Name()
	long := strings.Repeat("x", 50)

	mustUpsert(t, st, mem(ns, "long-unchunked", long, vec(dims, 1)))
	mustUpsert(t, st, mem(ns, "short", "tiny", vec(dims, 1)))
	mustUpsert(t, st, chunked(ns, "long-chunked", long, vec(dims, 1), vec(dims, 1)))

	got, err := cs.ListUnchunked(ctx, ns, 10, 100)
	if err != nil {
		t.Fatalf("list unchunked: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("ListUnchunked = %v, want only the long memory that has no chunks", memIDs(got))
	}
	if got[0].ID != id(ns, "long-unchunked") {
		t.Fatalf("ListUnchunked returned %q, want %q", got[0].ID, id(ns, "long-unchunked"))
	}
	// minRunes counts characters: a 50-rune memory is not "over 100".
	if got, err := cs.ListUnchunked(ctx, ns, 100, 100); err != nil {
		t.Fatalf("list unchunked (high floor): %v", err)
	} else if len(got) != 0 {
		t.Fatalf("ListUnchunked with minRunes=100 = %v, want none", memIDs(got))
	}
	// limit <= 0 is not "unbounded" here — the backfill always bounds its batch.
	if got, err := cs.ListUnchunked(ctx, ns, 10, 0); err != nil || len(got) != 0 {
		t.Fatalf("ListUnchunked(limit=0) = %v (err %v), want none", memIDs(got), err)
	}
}

func testChunkWrongDims(t *testing.T, st store.Store, cs store.ChunkStore, dims int) {
	ctx := context.Background()
	ns := t.Name()
	m := mem(ns, "bad", "content", vec(dims, 1))
	m.Chunks = []memory.Chunk{{Idx: 0, Embedding: vec(dims-1, 1)}}
	if err := st.Upsert(ctx, m); err == nil {
		t.Error("upsert accepted a chunk of the wrong width; it would corrupt the index silently")
	}
	if _, err := cs.ChunkVectorSearch(ctx, ns, vec(dims-1, 1), store.Filter{}, 10); err == nil {
		t.Error("chunk search accepted a query vector of the wrong width")
	}
}

// assertNoChunks fails when any chunk row survives in the namespace.
func assertNoChunks(t *testing.T, cs store.ChunkStore, ns string, dims int, when string) {
	t.Helper()
	got, err := cs.ChunkVectorSearch(context.Background(), ns, vec(dims, 1), store.Filter{IncludeExpired: true, IncludeSuperseded: true}, 10)
	if err != nil {
		t.Fatalf("chunk search %s: %v", when, err)
	}
	if len(got) != 0 {
		t.Fatalf("chunks survive %s: %v", when, ids(got))
	}
}

func ids(s []store.Scored) []string {
	out := make([]string, len(s))
	for i, x := range s {
		out[i] = x.Memory.ID
	}
	return out
}
