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
	t.Run("PredecessorIDs", func(t *testing.T) { testPredecessorIDs(t, st, dims) })
	t.Run("Restore", func(t *testing.T) { testRestore(t, st, dims) })
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
	t.Run("MarkContradicted", func(t *testing.T) { testMarkContradicted(t, st, dims) })
	t.Run("GetByFingerprint", func(t *testing.T) { testGetByFingerprint(t, st, dims) })
	t.Run("LevelFilter", func(t *testing.T) { testLevelFilter(t, st, dims) })
	t.Run("VectorlessRow", func(t *testing.T) { testVectorlessRow(t, st, dims) })
	t.Run("NamespaceLinks", func(t *testing.T) { testNamespaceLinks(t, st, dims) })
	t.Run("NamespaceActivity", func(t *testing.T) { testNamespaceActivity(t, st, dims) })
}

// testNamespaceActivity verifies the optional ActivityStore aggregate: one
// row per namespace holding live memories, with Total counting only live rows
// (expired and superseded rows excluded) and LastWrite the max created_at
// among those live rows — a tombstoned row must neither count nor advance the
// clock. A namespace holding only non-live rows yields no row at all. Stores
// that do not implement ActivityStore skip.
func testNamespaceActivity(t *testing.T, st store.Store, dims int) {
	as, ok := st.(store.ActivityStore)
	if !ok {
		t.Skip("store does not implement store.ActivityStore")
	}
	ctx := context.Background()
	ns := t.Name()
	nsA, nsB, nsDead := ns+"-a", ns+"-b", ns+"-dead"
	base := time.Now().UTC().Truncate(time.Millisecond)

	older := mem(nsA, "older", "older live fact", vec(dims, 1))
	older.CreatedAt = base.Add(-2 * time.Hour)
	mustUpsert(t, st, older)
	newest := mem(nsA, "newest", "newest live fact", vec(dims, 2))
	newest.CreatedAt = base.Add(-1 * time.Hour)
	mustUpsert(t, st, newest)
	// An expired row CREATED AFTER the newest live one: it must not count
	// toward Total and must not advance LastWrite past the live max.
	expired := mem(nsA, "expired", "expired fact", vec(dims, 3))
	expired.CreatedAt = base.Add(-30 * time.Minute)
	exp := base.Add(-10 * time.Minute)
	expired.ExpiresAt = &exp
	mustUpsert(t, st, expired)
	// Same for a superseded row created after the newest live one.
	sup := mem(nsA, "sup", "superseded fact", vec(dims, 4))
	sup.CreatedAt = base.Add(-20 * time.Minute)
	by := id(nsA, "newest")
	sup.SupersededBy = &by
	mustUpsert(t, st, sup)

	bOnly := mem(nsB, "only", "b live fact", vec(dims, 5))
	bOnly.CreatedAt = base.Add(-3 * time.Hour)
	mustUpsert(t, st, bOnly)

	// A namespace whose every row is expired must yield no activity row.
	dead := mem(nsDead, "gone", "expired-only namespace", vec(dims, 6))
	dead.CreatedAt = base.Add(-2 * time.Hour)
	deadExp := base.Add(-1 * time.Hour)
	dead.ExpiresAt = &deadExp
	mustUpsert(t, st, dead)

	acts, err := as.NamespaceActivity(ctx, base)
	if err != nil {
		t.Fatalf("namespace activity: %v", err)
	}
	// The store is shared across conformance subtests, so assert only on this
	// test's namespaces rather than on the full row set.
	byNS := map[string]store.NamespaceActivity{}
	for _, a := range acts {
		byNS[a.NS] = a
	}
	a, ok := byNS[nsA]
	if !ok {
		t.Fatalf("no activity row for %s (rows: %v)", nsA, acts)
	}
	if a.Total != 2 {
		t.Errorf("%s total = %d, want 2 (live rows only — expired/superseded excluded)", nsA, a.Total)
	}
	if !a.LastWrite.Equal(newest.CreatedAt) {
		t.Errorf("%s last write = %v, want %v (max created_at among LIVE rows; the newer tombstoned rows must not advance it)",
			nsA, a.LastWrite, newest.CreatedAt)
	}
	b, ok := byNS[nsB]
	if !ok {
		t.Fatalf("no activity row for %s (rows: %v)", nsB, acts)
	}
	if b.Total != 1 || !b.LastWrite.Equal(bOnly.CreatedAt) {
		t.Errorf("%s = {total %d, last %v}, want {total 1, last %v}", nsB, b.Total, b.LastWrite, bOnly.CreatedAt)
	}
	if _, ok := byNS[nsDead]; ok {
		t.Errorf("%s holds only expired rows and must yield no activity row, got %+v", nsDead, byNS[nsDead])
	}
}

// testVectorlessRow verifies stores accept memories with no embedding
// (len(m.Embedding) == 0): the row is stored and keyword-searchable but never
// surfaces from VectorSearch. This is the write path memini falls back to when
// the embedding provider is unavailable (Task 11). Re-upserting with a real
// vector must make it vector-searchable; re-upserting a vectored row without a
// vector must remove the now-stale vector-index entry, not just leave it
// unreachable through the new write.
func testVectorlessRow(t *testing.T, st store.Store, dims int) {
	ctx := context.Background()
	ns := t.Name()
	const content = "the office wifi password is on the whiteboard"

	// (a) upsert with nil embedding succeeds.
	mustUpsert(t, st, mem(ns, "row", content, nil))
	got, err := st.Get(ctx, ns, id(ns, "row"))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Content != content {
		t.Fatalf("content mismatch: %q", got.Content)
	}

	// (b) KeywordSearch finds it.
	kres, err := st.KeywordSearch(ctx, ns, "whiteboard", store.Filter{}, 10)
	if err != nil {
		t.Fatalf("keyword search: %v", err)
	}
	if !slices.Contains(idsOf(kres), id(ns, "row")) {
		t.Fatalf("vectorless memory should be keyword-searchable, got %v", idsOf(kres))
	}

	// (c) VectorSearch does not.
	vres, err := st.VectorSearch(ctx, ns, vec(dims, 1), store.Filter{}, 10)
	if err != nil {
		t.Fatalf("vector search: %v", err)
	}
	if slices.Contains(idsOf(vres), id(ns, "row")) {
		t.Fatalf("vectorless memory should not be vector-searchable, got %v", idsOf(vres))
	}

	// (d) re-upsert the same id WITH a vector: VectorSearch now finds it.
	mustUpsert(t, st, mem(ns, "row", content, vec(dims, 1)))
	vres, err = st.VectorSearch(ctx, ns, vec(dims, 1), store.Filter{}, 10)
	if err != nil {
		t.Fatalf("vector search after gaining an embedding: %v", err)
	}
	if !slices.Contains(idsOf(vres), id(ns, "row")) {
		t.Fatalf("memory should be vector-searchable after gaining an embedding, got %v", idsOf(vres))
	}

	// (e) re-upsert again WITHOUT a vector: the stale vec-index entry must be
	// removed (VectorSearch stops finding it) while KeywordSearch still does.
	mustUpsert(t, st, mem(ns, "row", content, nil))
	vres, err = st.VectorSearch(ctx, ns, vec(dims, 1), store.Filter{}, 10)
	if err != nil {
		t.Fatalf("vector search after losing the embedding: %v", err)
	}
	if slices.Contains(idsOf(vres), id(ns, "row")) {
		t.Fatalf("stale vector-index entry not removed on re-upsert without an embedding, got %v", idsOf(vres))
	}
	kres, err = st.KeywordSearch(ctx, ns, "whiteboard", store.Filter{}, 10)
	if err != nil {
		t.Fatalf("keyword search after losing the embedding: %v", err)
	}
	if !slices.Contains(idsOf(kres), id(ns, "row")) {
		t.Fatalf("keyword search should still find the memory after the vector is removed, got %v", idsOf(kres))
	}

	// (f) a wrong non-zero embedding length must still error.
	if err := st.Upsert(ctx, mem(ns, "bad", "wrong dims", vec(dims-1, 1))); err == nil {
		t.Fatalf("expected an error for a wrong non-zero embedding length")
	}

	// A vectorless memory must still be movable by Reassign: a driver whose
	// lookup inner-joins the vector index would otherwise silently skip it
	// (no vec-index row to join against), reporting 0 moved instead of moving it.
	toNS := ns + "-moved"
	n, err := st.Reassign(ctx, ns, []string{id(ns, "row")}, toNS)
	if err != nil {
		t.Fatalf("reassign vectorless memory: %v", err)
	}
	if n != 1 {
		t.Fatalf("reassign vectorless memory moved %d, want 1", n)
	}
	if _, err := st.Get(ctx, toNS, id(ns, "row")); err != nil {
		t.Fatalf("vectorless memory not found after reassign: %v", err)
	}
}

// testNamespaceLinks exercises the optional LinkStore capability: put/list
// round-trip (including tiers and note), upsert-overwrites on a (src,dst)
// conflict, DeleteLink's existed-bool return, ListLinks/ListAllLinks scoping,
// the DeleteNamespace cascade (gap G5), and RenameLinkEndpoints rewriting both
// sides of a link. Stores that do not implement LinkStore skip. Split into
// sub-tests (rather than one long function) to keep each check focused.
func testNamespaceLinks(t *testing.T, st store.Store, dims int) {
	_ = dims // links carry no embedding; kept for signature parity with the other subtests
	ls, ok := st.(store.LinkStore)
	if !ok {
		t.Skip("store does not implement store.LinkStore")
	}
	ns := t.Name()
	t.Run("PutListRoundTripAndUpsert", func(t *testing.T) { testLinkRoundTripAndUpsert(t, ls, ns) })
	t.Run("ScopingAndDelete", func(t *testing.T) { testLinkScopingAndDelete(t, ls, ns) })
	t.Run("DeleteNamespaceCascade", func(t *testing.T) { testLinkDeleteNamespaceCascade(t, st, ls, ns) })
	t.Run("RenameEndpoints", func(t *testing.T) { testLinkRenameEndpoints(t, ls, ns) })
	t.Run("RenameCollisionKeepsExisting", func(t *testing.T) { testLinkRenameCollisionKeepsExisting(t, ls, ns) })
	t.Run("RenameReciprocalPair", func(t *testing.T) { testLinkRenameReciprocalPair(t, ls, ns) })
}

// testLinkRoundTripAndUpsert covers PutLink/ListLinks round-tripping every
// field (including tiers and note), and that a second PutLink for the same
// (src,dst) overwrites in place rather than duplicating the row.
func testLinkRoundTripAndUpsert(t *testing.T, ls store.LinkStore, ns string) {
	ctx := context.Background()
	src := ns + "-rt-src"
	dst := ns + "-rt-dst"
	now := time.Now().UTC().Truncate(time.Millisecond)

	link := store.NamespaceLink{
		Src: src, Dst: dst,
		Tiers:     []memory.Tier{memory.TierSemantic, memory.TierProcedural},
		Note:      "shared golang helpers",
		CreatedAt: now,
	}
	if err := ls.PutLink(ctx, link); err != nil {
		t.Fatalf("put link: %v", err)
	}
	got, err := ls.ListLinks(ctx, src)
	if err != nil {
		t.Fatalf("list links: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("list links = %d, want 1", len(got))
	}
	if got[0].Src != src || got[0].Dst != dst || got[0].Note != link.Note {
		t.Fatalf("round-trip mismatch: %+v", got[0])
	}
	if !slices.Equal(got[0].Tiers, link.Tiers) {
		t.Fatalf("tiers = %v, want %v", got[0].Tiers, link.Tiers)
	}
	if !got[0].CreatedAt.Equal(now) {
		t.Fatalf("created_at = %v, want %v", got[0].CreatedAt, now)
	}

	// Upsert overwrites in place: same (src,dst), different tiers/note. Must
	// not duplicate the row.
	overwrite := store.NamespaceLink{
		Src: src, Dst: dst,
		Tiers:     []memory.Tier{memory.TierSemantic},
		Note:      "updated note",
		CreatedAt: now.Add(time.Minute),
	}
	if err := ls.PutLink(ctx, overwrite); err != nil {
		t.Fatalf("put link (overwrite): %v", err)
	}
	got, err = ls.ListLinks(ctx, src)
	if err != nil {
		t.Fatalf("list links after overwrite: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("list links after overwrite = %d, want 1 (upsert must not duplicate)", len(got))
	}
	if got[0].Note != "updated note" || !slices.Equal(got[0].Tiers, overwrite.Tiers) {
		t.Fatalf("overwrite not applied: %+v", got[0])
	}
}

// testLinkScopingAndDelete covers ListLinks scoping to a single Src,
// ListAllLinks returning everything, an unknown Src yielding an empty (not
// error) list, and DeleteLink's existed-bool return.
func testLinkScopingAndDelete(t *testing.T, ls store.LinkStore, ns string) {
	ctx := context.Background()
	src := ns + "-sc-src"
	dst := ns + "-sc-dst"
	other := ns + "-sc-other"
	now := time.Now().UTC().Truncate(time.Millisecond)

	if err := ls.PutLink(ctx, store.NamespaceLink{Src: src, Dst: dst, CreatedAt: now}); err != nil {
		t.Fatalf("put link1: %v", err)
	}
	if err := ls.PutLink(ctx, store.NamespaceLink{Src: src, Dst: other, CreatedAt: now}); err != nil {
		t.Fatalf("put link2: %v", err)
	}
	if err := ls.PutLink(ctx, store.NamespaceLink{Src: other, Dst: dst, CreatedAt: now}); err != nil {
		t.Fatalf("put link3: %v", err)
	}

	got, err := ls.ListLinks(ctx, src)
	if err != nil {
		t.Fatalf("list links (src): %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("list links (src) = %d, want 2", len(got))
	}

	all, err := ls.ListAllLinks(ctx)
	if err != nil {
		t.Fatalf("list all links: %v", err)
	}
	if len(all) < 3 {
		t.Fatalf("list all links = %d, want >= 3", len(all))
	}

	// Empty/unknown src -> empty list, no error.
	empty, err := ls.ListLinks(ctx, ns+"-unknown")
	if err != nil {
		t.Fatalf("list links (unknown src): %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("list links (unknown src) = %d, want 0", len(empty))
	}

	// DeleteLink returns existed=true, then existed=false on the second call.
	existed, err := ls.DeleteLink(ctx, src, other)
	if err != nil {
		t.Fatalf("delete link: %v", err)
	}
	if !existed {
		t.Fatalf("delete link: existed = false, want true")
	}
	existed, err = ls.DeleteLink(ctx, src, other)
	if err != nil {
		t.Fatalf("delete link (again): %v", err)
	}
	if existed {
		t.Fatalf("delete link (again): existed = true, want false")
	}
}

// testLinkDeleteNamespaceCascade covers gap G5: DeleteNamespace must also
// drop namespace_links rows referencing the namespace on either side.
func testLinkDeleteNamespaceCascade(t *testing.T, st store.Store, ls store.LinkStore, ns string) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	cascSrc := ns + "-casc-src"
	cascDst := ns + "-casc-dst"
	if err := ls.PutLink(ctx, store.NamespaceLink{Src: cascSrc, Dst: cascDst, CreatedAt: now}); err != nil {
		t.Fatalf("put cascade link (as src): %v", err)
	}
	if err := ls.PutLink(ctx, store.NamespaceLink{Src: cascDst, Dst: cascSrc, CreatedAt: now}); err != nil {
		t.Fatalf("put cascade link (as dst): %v", err)
	}
	if _, err := st.DeleteNamespace(ctx, cascSrc); err != nil {
		t.Fatalf("delete namespace: %v", err)
	}
	remaining, err := ls.ListAllLinks(ctx)
	if err != nil {
		t.Fatalf("list all links after delete namespace: %v", err)
	}
	for _, l := range remaining {
		if l.Src == cascSrc || l.Dst == cascSrc {
			t.Fatalf("link referencing deleted namespace %q survived: %+v", cascSrc, l)
		}
	}
}

// testLinkRenameEndpoints covers gap G5: RenameLinkEndpoints rewrites a
// namespace wherever it appears, as either Src or Dst, and is a no-op when
// from == to.
func testLinkRenameEndpoints(t *testing.T, ls store.LinkStore, ns string) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	from := ns + "-rename-from"
	to := ns + "-rename-to"
	third := ns + "-rename-third"
	if err := ls.PutLink(ctx, store.NamespaceLink{Src: from, Dst: third, Note: "from as src", CreatedAt: now}); err != nil {
		t.Fatalf("put rename link (as src): %v", err)
	}
	if err := ls.PutLink(ctx, store.NamespaceLink{Src: third, Dst: from, Note: "from as dst", CreatedAt: now}); err != nil {
		t.Fatalf("put rename link (as dst): %v", err)
	}
	if err := ls.RenameLinkEndpoints(ctx, from, to); err != nil {
		t.Fatalf("rename link endpoints: %v", err)
	}
	toLinks, err := ls.ListLinks(ctx, to)
	if err != nil {
		t.Fatalf("list links (to): %v", err)
	}
	if len(toLinks) != 1 || toLinks[0].Dst != third || toLinks[0].Note != "from as src" {
		t.Fatalf("rename did not rewrite src side: %+v", toLinks)
	}
	thirdLinks, err := ls.ListLinks(ctx, third)
	if err != nil {
		t.Fatalf("list links (third): %v", err)
	}
	if len(thirdLinks) != 1 || thirdLinks[0].Dst != to || thirdLinks[0].Note != "from as dst" {
		t.Fatalf("rename did not rewrite dst side: %+v", thirdLinks)
	}
	fromLinks, err := ls.ListLinks(ctx, from)
	if err != nil {
		t.Fatalf("list links (from, after rename): %v", err)
	}
	if len(fromLinks) != 0 {
		t.Fatalf("old src namespace %q still has links after rename: %v", from, fromLinks)
	}

	// Rename is a no-op when from == to.
	if err := ls.RenameLinkEndpoints(ctx, to, to); err != nil {
		t.Fatalf("rename link endpoints (noop): %v", err)
	}
}

// testLinkRenameCollisionKeepsExisting pins the collision semantics of
// RenameLinkEndpoints: when a rewritten link lands on a key where the target
// namespace already has its own link, the pre-existing link survives
// untouched and the renamed one is dropped — a rename must never silently
// widen or narrow tier access the target had explicitly configured.
func testLinkRenameCollisionKeepsExisting(t *testing.T, ls store.LinkStore, ns string) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	a := ns + "-coll-a"
	b := ns + "-coll-b"
	c := ns + "-coll-c"

	// B's own, pre-existing grant to C: narrow tiers, distinct note.
	existing := store.NamespaceLink{
		Src: b, Dst: c,
		Tiers:     []memory.Tier{memory.TierSemantic},
		Note:      "b's own grant",
		CreatedAt: now,
	}
	if err := ls.PutLink(ctx, existing); err != nil {
		t.Fatalf("put pre-existing link (b,c): %v", err)
	}
	// A's link to C: wider tiers. Renaming A->B makes it collide with (b,c).
	inherited := store.NamespaceLink{
		Src: a, Dst: c,
		Tiers:     []memory.Tier{memory.TierSemantic, memory.TierProcedural},
		Note:      "inherited from a",
		CreatedAt: now.Add(time.Minute),
	}
	if err := ls.PutLink(ctx, inherited); err != nil {
		t.Fatalf("put colliding link (a,c): %v", err)
	}

	if err := ls.RenameLinkEndpoints(ctx, a, b); err != nil {
		t.Fatalf("rename link endpoints: %v", err)
	}

	got, err := ls.ListLinks(ctx, b)
	if err != nil {
		t.Fatalf("list links (b): %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("links from %q after collision = %d, want exactly 1", b, len(got))
	}
	if got[0].Note != existing.Note || !slices.Equal(got[0].Tiers, existing.Tiers) {
		t.Fatalf("pre-existing link was clobbered by the rename: %+v, want note=%q tiers=%v",
			got[0], existing.Note, existing.Tiers)
	}
	if !got[0].CreatedAt.Equal(existing.CreatedAt) {
		t.Fatalf("pre-existing link created_at was rewritten: %v, want %v", got[0].CreatedAt, existing.CreatedAt)
	}
	// The renamed source has nothing left.
	aLinks, err := ls.ListLinks(ctx, a)
	if err != nil {
		t.Fatalf("list links (a): %v", err)
	}
	if len(aLinks) != 0 {
		t.Fatalf("renamed namespace %q still has links: %v", a, aLinks)
	}
}

// testLinkRenameReciprocalPair pins the multi-way collision: link(from,to)
// and link(to,from) both collapse onto the self-link (to,to) when from is
// renamed to to. Exactly one row must survive, and — with the rename's
// key-ordered scan — deterministically the first in (src,dst) key order,
// which is link(from,to).
func testLinkRenameReciprocalPair(t *testing.T, ls store.LinkStore, ns string) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	from := ns + "-recip-from"
	to := ns + "-recip-to"

	if err := ls.PutLink(ctx, store.NamespaceLink{Src: from, Dst: to, Note: "from to to", CreatedAt: now}); err != nil {
		t.Fatalf("put link (from,to): %v", err)
	}
	if err := ls.PutLink(ctx, store.NamespaceLink{Src: to, Dst: from, Note: "to to from", CreatedAt: now}); err != nil {
		t.Fatalf("put link (to,from): %v", err)
	}

	if err := ls.RenameLinkEndpoints(ctx, from, to); err != nil {
		t.Fatalf("rename link endpoints (reciprocal pair): %v", err)
	}

	// Nothing may reference the old namespace anymore, and the pair must have
	// collapsed to exactly one self-link — not zero (both dropped) and not an
	// error (unique-key violation).
	toLinks, err := ls.ListLinks(ctx, to)
	if err != nil {
		t.Fatalf("list links (to): %v", err)
	}
	if len(toLinks) != 1 || toLinks[0].Dst != to {
		t.Fatalf("reciprocal pair should collapse to one self-link (to,to), got %+v", toLinks)
	}
	// Deterministic winner: (from,to) sorts before (to,from), so its rewrite
	// is inserted first and the second is dropped on conflict.
	if toLinks[0].Note != "from to to" {
		t.Fatalf("self-link note = %q, want %q (first row in key order wins)", toLinks[0].Note, "from to to")
	}
	fromLinks, err := ls.ListLinks(ctx, from)
	if err != nil {
		t.Fatalf("list links (from): %v", err)
	}
	if len(fromLinks) != 0 {
		t.Fatalf("old namespace %q still has links after rename: %v", from, fromLinks)
	}
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

	// A validity-closed (contradicted) match is excluded too: re-asserting a
	// contradicted fact must store a live row, not corroborate the dead one.
	closed := mem(ns, "closed", "the office is in Berlin", vec(dims, 0, 0, 1))
	seed := 0.4
	closed.Confidence = &seed
	mustUpsert(t, st, closed)
	if err := st.MarkContradicted(ctx, ns, closed.ID, repl.ID, 0.2, now.Add(-time.Minute)); err != nil {
		t.Fatalf("mark contradicted: %v", err)
	}
	closedFP := memory.Fingerprint(closed.Content)
	if _, err := st.GetByFingerprint(ctx, ns, memory.TierSemantic, closedFP, now); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("validity-closed match: want ErrNotFound, got %v", err)
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

	// A validity-closed (contradicted) row is not touched: corroboration must
	// never regrow an invalidated fact, even when the invalidation raced in
	// between the caller's read and its write.
	if err := st.MarkContradicted(ctx, ns, m.ID, "other", 0.2, now); err != nil {
		t.Fatalf("mark contradicted: %v", err)
	}
	if err := st.SetConfidence(ctx, ns, m.ID, 0.9, now.Add(time.Second)); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("set confidence on validity-closed: want ErrNotFound, got %v", err)
	}
	got, err = st.Get(ctx, ns, m.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Confidence == nil || *got.Confidence != 0.2 {
		t.Errorf("confidence after refused regrow = %v, want 0.2", got.Confidence)
	}
}

func testMarkContradicted(t *testing.T, st store.Store, dims int) {
	ctx := context.Background()
	ns := t.Name()
	m := mem(ns, "old", "the sky is green", vec(dims, 1))
	m.Tier = memory.TierSemantic
	seed := 0.7
	m.Confidence = &seed
	m.AccessCount = 3
	mustUpsert(t, st, m)
	mustUpsert(t, st, mem(ns, "new", "the sky is blue", vec(dims, 1)))

	now := time.Now().UTC().Truncate(time.Millisecond)
	if err := st.MarkContradicted(ctx, ns, id(ns, "old"), id(ns, "new"), 0.13, now); err != nil {
		t.Fatalf("mark contradicted: %v", err)
	}
	got, err := st.Get(ctx, ns, id(ns, "old"))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Confidence == nil || *got.Confidence != 0.13 {
		t.Errorf("confidence = %v, want 0.13", got.Confidence)
	}
	if got.ValidTo == nil || !got.ValidTo.Equal(now) {
		t.Errorf("valid_to = %v, want stamped to %v", got.ValidTo, now)
	}
	if got.Metadata["contradicted_by"] != id(ns, "new") {
		t.Errorf("contradicted_by = %v, want %q", got.Metadata["contradicted_by"], id(ns, "new"))
	}
	// The pre-update confidence (0.7) is snapshotted for audit and reversal.
	if prev, ok := got.Metadata["contradicted_prev_confidence"].(float64); !ok || prev != 0.7 {
		t.Errorf("contradicted_prev_confidence = %v, want 0.7", got.Metadata["contradicted_prev_confidence"])
	}

	// Default recall excludes the invalidated fact (valid_to in the past)...
	res, err := st.VectorSearch(ctx, ns, vec(dims, 1), store.Filter{Now: now.Add(time.Second)}, 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if slices.Contains(idsOf(res), id(ns, "old")) {
		t.Fatalf("contradicted memory should be excluded from live recall, got %v", idsOf(res))
	}
	// ...but AsOf time-travel before the stamp still surfaces it (history kept).
	asof, err := st.VectorSearch(ctx, ns, vec(dims, 1), store.Filter{AsOf: now.Add(-time.Hour)}, 10)
	if err != nil {
		t.Fatalf("asof search: %v", err)
	}
	if !slices.Contains(idsOf(asof), id(ns, "old")) {
		t.Fatalf("AsOf before valid_to should still surface the fact, got %v", idsOf(asof))
	}

	if err := st.MarkContradicted(ctx, ns, id(ns, "missing"), id(ns, "new"), 0.1, now); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("mark contradicted on missing: want ErrNotFound, got %v", err)
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

// scoredIDs returns memory IDs from a Scored slice.
func scoredIDs(res []store.Scored) []string {
	ids := make([]string, len(res))
	for i, r := range res {
		ids[i] = r.Memory.ID
	}
	return ids
}

// testLevelFilter verifies that Filter.Levels restricts results to memories
// whose derivation level matches one of the listed values; empty means no
// constraint.
func testLevelFilter(t *testing.T, st store.Store, dims int) {
	ctx := context.Background()
	ns := t.Name()

	// Insert three memories with different levels.
	explicit := mem(ns, "exp", "user stated this directly", vec(dims, 1))
	explicit.Level = memory.LevelExplicit
	mustUpsert(t, st, explicit)

	deduced := mem(ns, "ded", "LLM distilled this fact", vec(dims, 2))
	deduced.Level = memory.LevelDeduced
	mustUpsert(t, st, deduced)

	unnamed := mem(ns, "unl", "no level set (legacy)", vec(dims, 3))
	// Level is empty string (zero value).
	mustUpsert(t, st, unnamed)

	// Empty level filter matches all three (no constraint).
	all := mustList(t, st, ns, store.Filter{Levels: []memory.Level{}})
	if len(all) != 3 {
		t.Fatalf("empty levels filter should yield 3, got %d", len(all))
	}

	// Filter to explicit only.
	expOnly := mustSearch(t, st, ns, vec(dims, 1), store.Filter{Levels: []memory.Level{memory.LevelExplicit}}, 10)
	if len(expOnly) != 1 || scoredIDs(expOnly)[0] != id(ns, "exp") {
		t.Fatalf("level=explicit should yield exp only, got %v", scoredIDs(expOnly))
	}

	// Filter to deduced only.
	dedOnly := mustSearch(t, st, ns, vec(dims, 2), store.Filter{Levels: []memory.Level{memory.LevelDeduced}}, 10)
	if len(dedOnly) != 1 || scoredIDs(dedOnly)[0] != id(ns, "ded") {
		t.Fatalf("level=deduced should yield ded only, got %v", scoredIDs(dedOnly))
	}

	// Multi-level filter: explicit + deduced (still excludes unnamed).
	multi := mustSearch(t, st, ns, vec(dims, 1), store.Filter{Levels: []memory.Level{memory.LevelExplicit, memory.LevelDeduced}}, 10)
	if len(multi) != 2 {
		t.Fatalf("level=explicit+deduced should yield 2, got %d", len(multi))
	}

	// VectorSearch with level filter.
	vRes, err := st.VectorSearch(ctx, ns, vec(dims, 1), store.Filter{Levels: []memory.Level{memory.LevelExplicit}}, 10)
	if err != nil {
		t.Fatalf("VectorSearch level filter: %v", err)
	}
	if len(vRes) != 1 || scoredIDs(vRes)[0] != id(ns, "exp") {
		t.Fatalf("VectorSearch level=explicit should yield exp only, got %v", scoredIDs(vRes))
	}

	// KeywordSearch with level filter.
	kRes, err := st.KeywordSearch(ctx, ns, "distilled", store.Filter{Levels: []memory.Level{memory.LevelDeduced}}, 10)
	if err != nil {
		t.Fatalf("KeywordSearch level filter: %v", err)
	}
	if len(kRes) != 1 || scoredIDs(kRes)[0] != id(ns, "ded") {
		t.Fatalf("KeywordSearch level=deduced should yield ded only, got %v", scoredIDs(kRes))
	}
}

// mustSearch is a helper for testLevelFilter that calls VectorSearch and
// fatals on error.
func mustSearch(t *testing.T, st store.Store, ns string, vec []float32, f store.Filter, k int) []store.Scored {
	t.Helper()
	res, err := st.VectorSearch(context.Background(), ns, vec, f, k)
	if err != nil {
		t.Fatalf("VectorSearch: %v", err)
	}
	return res
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

func testPredecessorIDs(t *testing.T, st store.Store, dims int) {
	ctx := context.Background()
	ns := t.Name()
	mustUpsert(t, st, mem(ns, "v1", "draft one", vec(dims, 1)))
	mustUpsert(t, st, mem(ns, "v2", "draft two", vec(dims, 1)))
	mustUpsert(t, st, mem(ns, "v3", "draft three", vec(dims, 1)))
	// A merge: both v1 and v2 are superseded by v3.
	if err := st.SetSuperseded(ctx, ns, id(ns, "v1"), id(ns, "v3")); err != nil {
		t.Fatalf("supersede v1: %v", err)
	}
	if err := st.SetSuperseded(ctx, ns, id(ns, "v2"), id(ns, "v3")); err != nil {
		t.Fatalf("supersede v2: %v", err)
	}

	preds, err := st.PredecessorIDs(ctx, ns, id(ns, "v3"))
	if err != nil {
		t.Fatalf("predecessor ids: %v", err)
	}
	slices.Sort(preds)
	want := []string{id(ns, "v1"), id(ns, "v2")}
	slices.Sort(want)
	if !slices.Equal(preds, want) {
		t.Fatalf("predecessors of v3 = %v, want %v", preds, want)
	}

	// A leaf nothing supersedes has no predecessors.
	if got, err := st.PredecessorIDs(ctx, ns, id(ns, "v1")); err != nil || len(got) != 0 {
		t.Fatalf("predecessors of v1 = %v, %v; want empty", got, err)
	}
}

func testRestore(t *testing.T, st store.Store, dims int) {
	ctx := context.Background()
	ns := t.Name()
	mustUpsert(t, st, mem(ns, "old", "the sky is green", vec(dims, 1)))
	mustUpsert(t, st, mem(ns, "new", "the sky is blue", vec(dims, 1)))
	if err := st.SetSuperseded(ctx, ns, id(ns, "old"), id(ns, "new")); err != nil {
		t.Fatalf("set superseded: %v", err)
	}

	if err := st.Restore(ctx, ns, id(ns, "old")); err != nil {
		t.Fatalf("restore: %v", err)
	}
	got, err := st.Get(ctx, ns, id(ns, "old"))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.SupersededBy != nil {
		t.Fatalf("superseded_by not cleared: %v", got.SupersededBy)
	}
	if got.ValidTo != nil {
		t.Fatalf("valid_to not cleared: %v", got.ValidTo)
	}

	res, err := st.VectorSearch(ctx, ns, vec(dims, 1), store.Filter{}, 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if !slices.Contains(idsOf(res), id(ns, "old")) {
		t.Fatalf("restored memory should be searchable again, got %v", idsOf(res))
	}

	if err := st.Restore(ctx, ns, id(ns, "missing")); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("restore on missing: want ErrNotFound, got %v", err)
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
