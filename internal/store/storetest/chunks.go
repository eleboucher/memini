package storetest

import (
	"context"
	"fmt"
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
	t.Run("UpsertChunkContract", func(t *testing.T) { testChunkUpsertContract(t, st, cs, dims) })
	t.Run("PutChunksGuardsOnUpdatedAt", func(t *testing.T) { testPutChunks(t, st, cs, dims) })
	t.Run("DeleteCascades", func(t *testing.T) { testChunkDeleteCascade(t, st, cs, dims) })
	t.Run("DeleteNamespaceCascades", func(t *testing.T) { testChunkDeleteNamespaceCascade(t, st, cs, dims) })
	t.Run("ExpiryDeleteCascades", func(t *testing.T) { testChunkExpiryCascade(t, st, cs, dims) })
	t.Run("ReassignMovesChunks", func(t *testing.T) { testChunkReassign(t, st, cs, dims) })
	t.Run("FilterApplies", func(t *testing.T) { testChunkFilter(t, st, cs, dims) })
	t.Run("ListUnchunked", func(t *testing.T) { testListUnchunked(t, st, cs, dims) })
	t.Run("CountUnchunked", func(t *testing.T) { testCountUnchunked(t, st, cs, dims) })
	t.Run("WrongDimsErrors", func(t *testing.T) { testChunkWrongDims(t, st, cs, dims) })
	t.Run("SearchReturnsTheMatchedChunkText", func(t *testing.T) { testChunkMatchedText(t, st, cs, dims) })
}

// chunked builds a memory whose document vector points one way and whose chunks
// point elsewhere, so a hit can be attributed to the leg that produced it.
func chunked(ns, short, content string, docVec []float32, chunkVecs ...[]float32) *memory.Memory {
	m := mem(ns, short, content, docVec)
	for i, v := range chunkVecs {
		m.Chunks = append(m.Chunks, memory.Chunk{Idx: i, Text: chunkText(short, i), Embedding: v})
	}
	return m
}

// chunkText is a per-chunk marker, so a search result can be traced to the exact
// chunk that produced it rather than merely to its memory.
func chunkText(short string, idx int) string {
	return fmt.Sprintf("passage %d of %s", idx, short)
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

// testChunkUpsertContract pins Memory.Chunks' three-way contract on Upsert:
// nil preserves the rows while the content is unchanged (a metadata stamp or
// promotion must not wipe an index it never touched), nil clears them when the
// content changed (stale chunks would make recall return a memory whose text
// no longer holds the passage that matched), and an explicit empty slice
// clears them regardless (reembed's model swap needs exactly that).
func testChunkUpsertContract(t *testing.T, st store.Store, cs store.ChunkStore, dims int) {
	ctx := context.Background()
	ns := t.Name()
	mustUpsert(t, st, chunked(ns, "m", "content", vec(dims, 0, 1), vec(dims, 1)))
	if got, err := cs.ChunkVectorSearch(ctx, ns, vec(dims, 1), store.Filter{}, 10); err != nil || len(got) != 1 {
		t.Fatalf("precondition: chunk search = %v (err %v), want 1 hit", ids(got), err)
	}

	// Same content, nil chunks: preserved.
	mustUpsert(t, st, mem(ns, "m", "content", vec(dims, 0, 1)))
	if got, err := cs.ChunkVectorSearch(ctx, ns, vec(dims, 1), store.Filter{}, 10); err != nil || len(got) != 1 {
		t.Fatalf("nil chunks with unchanged content cleared the rows: %v (err %v)", ids(got), err)
	}

	// Same content, explicit empty slice: cleared anyway.
	m := mem(ns, "m", "content", vec(dims, 0, 1))
	m.Chunks = []memory.Chunk{}
	mustUpsert(t, st, m)
	assertNoChunks(t, cs, ns, "after an explicit empty-slice upsert")

	// Changed content, nil chunks: cleared.
	mustUpsert(t, st, chunked(ns, "m", "content", vec(dims, 0, 1), vec(dims, 1)))
	mustUpsert(t, st, mem(ns, "m", "content rewritten", vec(dims, 0, 1)))
	assertNoChunks(t, cs, ns, "after a content rewrite that did not recompute chunks")
}

// testPutChunks pins the backfill's write primitive: chunk rows only, guarded
// by updated_at inside the store's own transaction. The document-vector
// assertion is the heart of it — the bug this method replaced was a backfill
// whose Get-then-Upsert round-trip could never carry the vector, and so
// destroyed it for every memory it chunked.
func testPutChunks(t *testing.T, st store.Store, cs store.ChunkStore, dims int) {
	ctx := context.Background()
	ns := t.Name()
	mustUpsert(t, st, mem(ns, "m", "content", vec(dims, 0, 1)))
	// The guard value must be the store's own round-tripped timestamp, exactly
	// as the backfill sees it (ListUnchunked scans it from the row).
	fresh, err := st.Get(ctx, ns, id(ns, "m"))
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	ok, err := cs.PutChunks(ctx, ns, id(ns, "m"), fresh.UpdatedAt,
		[]memory.Chunk{{Idx: 0, Text: "passage", Embedding: vec(dims, 1)}})
	if err != nil || !ok {
		t.Fatalf("PutChunks = %v (err %v), want a write under a matching updated_at", ok, err)
	}
	if got, err := cs.ChunkVectorSearch(ctx, ns, vec(dims, 1), store.Filter{}, 10); err != nil || len(got) != 1 {
		t.Fatalf("chunk search after PutChunks = %v (err %v), want the chunk", ids(got), err)
	}
	if got, err := st.VectorSearch(ctx, ns, vec(dims, 0, 1), store.Filter{}, 10); err != nil || len(got) != 1 {
		t.Fatalf("VectorSearch after PutChunks = %v (err %v): the document vector must survive untouched",
			ids(got), err)
	}

	// A stale guard writes nothing and reports false rather than erroring: the
	// row changed under the caller, and the rows it has (the current ones) stay.
	ok, err = cs.PutChunks(ctx, ns, id(ns, "m"), fresh.UpdatedAt.Add(-time.Second),
		[]memory.Chunk{{Idx: 0, Text: "stale", Embedding: vec(dims, 0, 0, 1)}})
	if err != nil {
		t.Fatalf("PutChunks (stale guard): %v", err)
	}
	if ok {
		t.Fatal("PutChunks accepted a stale updated_at")
	}
	if got, _ := cs.ChunkVectorSearch(ctx, ns, vec(dims, 1), store.Filter{}, 10); len(got) != 1 || got[0].MatchedChunk != "passage" {
		t.Fatalf("a refused PutChunks changed the rows: %v", ids(got))
	}

	// A missing row is false, not an error.
	if ok, err := cs.PutChunks(ctx, ns, id(ns, "absent"), fresh.UpdatedAt, nil); err != nil || ok {
		t.Fatalf("PutChunks(absent) = %v (err %v), want false, nil", ok, err)
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
	assertNoChunks(t, cs, ns, "after Delete")
}

func testChunkDeleteNamespaceCascade(t *testing.T, st store.Store, cs store.ChunkStore, dims int) {
	ctx := context.Background()
	ns := t.Name()
	mustUpsert(t, st, chunked(ns, "m", "content", vec(dims, 0, 1), vec(dims, 1)))
	if _, err := st.DeleteNamespace(ctx, ns); err != nil {
		t.Fatalf("delete namespace: %v", err)
	}
	assertNoChunks(t, cs, ns, "after DeleteNamespace")
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
	assertNoChunks(t, cs, ns, "after DeleteIfExpiredBefore")
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

	// long-unchunked2 (below) is created and deliberately never chunked, so a
	// second run against a store that kept the first run's data would find it
	// already queued and miscount. State the precondition, as Reassign does.
	if _, err := st.DeleteNamespace(ctx, ns); err != nil {
		t.Fatalf("clear %s: %v", ns, err)
	}

	mustUpsert(t, st, mem(ns, "long-unchunked", long, vec(dims, 1)))
	mustUpsert(t, st, mem(ns, "short", "tiny", vec(dims, 1)))
	mustUpsert(t, st, chunked(ns, "long-chunked", long, vec(dims, 1), vec(dims, 1)))

	got, err := cs.ListUnchunked(ctx, ns, 10, "", 100)
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
	if got, err := cs.ListUnchunked(ctx, ns, 100, "", 100); err != nil {
		t.Fatalf("list unchunked (high floor): %v", err)
	} else if len(got) != 0 {
		t.Fatalf("ListUnchunked with minRunes=100 = %v, want none", memIDs(got))
	}
	// limit <= 0 is not "unbounded" here — the backfill always bounds its batch.
	if got, err := cs.ListUnchunked(ctx, ns, 10, "", 0); err != nil || len(got) != 0 {
		t.Fatalf("ListUnchunked(limit=0) = %v (err %v), want none", memIDs(got), err)
	}

	// The cursor pages by id, so the backfill can move past a row it cannot
	// process rather than re-listing it into every batch.
	mustUpsert(t, st, mem(ns, "long-unchunked2", long, vec(dims, 1)))
	first, err := cs.ListUnchunked(ctx, ns, 10, "", 1)
	if err != nil || len(first) != 1 {
		t.Fatalf("ListUnchunked(limit=1) = %v (err %v), want one row", memIDs(first), err)
	}
	rest, err := cs.ListUnchunked(ctx, ns, 10, first[0].ID, 100)
	if err != nil {
		t.Fatalf("list unchunked (cursor): %v", err)
	}
	if len(rest) != 1 || rest[0].ID == first[0].ID {
		t.Fatalf("ListUnchunked(after=%q) = %v, want only the other row", first[0].ID, memIDs(rest))
	}
}

// testCountUnchunked pins the backlog measurement the pending gauge publishes:
// the whole queue, not the batch ListUnchunked happens to show.
func testCountUnchunked(t *testing.T, st store.Store, cs store.ChunkStore, dims int) {
	ctx := context.Background()
	ns := t.Name()
	long := strings.Repeat("x", 50)
	mustUpsert(t, st, mem(ns, "a", long, vec(dims, 1)))
	mustUpsert(t, st, mem(ns, "b", long, vec(dims, 1)))
	mustUpsert(t, st, chunked(ns, "c", long, vec(dims, 1), vec(dims, 1)))

	if n, err := cs.CountUnchunked(ctx, ns, 10); err != nil || n != 2 {
		t.Fatalf("CountUnchunked = %d (err %v), want 2", n, err)
	}
	if n, err := cs.CountUnchunked(ctx, ns, 100); err != nil || n != 0 {
		t.Fatalf("CountUnchunked(high floor) = %d (err %v), want 0", n, err)
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

// assertNoChunks fails when any chunk row survives in the namespace. It counts
// rows rather than searching for them: ChunkVectorSearch joins back to
// memories, so a probe through it is structurally blind to the exact failure
// the cascade tests exist to catch — an orphaned row whose memory is gone can
// never satisfy the join, and the old search-based assertion passed vacuously
// over a backend that leaked on every expiry sweep.
func assertNoChunks(t *testing.T, cs store.ChunkStore, ns, when string) {
	t.Helper()
	n, err := cs.CountChunks(context.Background(), ns)
	if err != nil {
		t.Fatalf("count chunks %s: %v", when, err)
	}
	if n != 0 {
		t.Fatalf("%d chunk rows survive %s", n, when)
	}
}

func ids(s []store.Scored) []string {
	out := make([]string, len(s))
	for i, x := range s {
		out[i] = x.Memory.ID
	}
	return out
}

// testChunkMatchedText pins that a hit carries the text of the chunk that
// actually won, not just its memory. Rerank judges a candidate on a truncated
// view (300 bytes for the LLM backend, 2048 runes for the cross-encoder), so
// without this it judges the memory's prefix, fails to see the passage that
// retrieved it, and drops the memory chunked recall just found.
func testChunkMatchedText(t *testing.T, st store.Store, cs store.ChunkStore, dims int) {
	ctx := context.Background()
	ns := t.Name()
	// Chunk 1 sits on the query; chunk 0 is far away.
	mustUpsert(t, st, chunked(ns, "m", "content", vec(dims, 0, 1),
		vec(dims, 0, 0, 1),
		vec(dims, 1),
	))
	got, err := cs.ChunkVectorSearch(ctx, ns, vec(dims, 1), store.Filter{}, 10)
	if err != nil {
		t.Fatalf("chunk search: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %v, want 1 result", ids(got))
	}
	// Specifically chunk 1's text: the pooled row must carry the WINNER's text,
	// not an arbitrary chunk of the same memory.
	if want := chunkText("m", 1); got[0].MatchedChunk != want {
		t.Errorf("MatchedChunk = %q, want %q (the chunk that produced the best score)",
			got[0].MatchedChunk, want)
	}

	// The document leg never sets it: an empty value is how a caller tells
	// "matched on a passage" from "matched on the memory".
	docs, err := st.VectorSearch(ctx, ns, vec(dims, 0, 1), store.Filter{}, 10)
	if err != nil {
		t.Fatalf("vector search: %v", err)
	}
	for _, d := range docs {
		if d.MatchedChunk != "" {
			t.Errorf("VectorSearch set MatchedChunk = %q, want empty", d.MatchedChunk)
		}
	}
}
