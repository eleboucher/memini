package maintenance_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/eleboucher/memini/internal/embed/embedtest"
	"github.com/eleboucher/memini/internal/maintenance"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/store"
	"github.com/eleboucher/memini/internal/store/sqlitevec"
)

const scopesDims = 64

func openScopesStore(t *testing.T) (*sqlitevec.Store, *embedtest.Fake) {
	t.Helper()
	st, err := sqlitevec.Open(context.Background(), filepath.Join(t.TempDir(), "m.db"), scopesDims)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st, embedtest.New(scopesDims)
}

// putScoped upserts a memory with distinct, non-clustering content (unless
// content is deliberately duplicated across calls to exercise dedup).
func putScoped(t *testing.T, st *sqlitevec.Store, emb *embedtest.Fake, ns, id, content string, importance float64) {
	t.Helper()
	ctx := context.Background()
	vec, err := emb.Embed(ctx, []string{content})
	if err != nil {
		t.Fatalf("embed %s: %v", id, err)
	}
	ts := time.Now().UTC()
	m := &memory.Memory{
		ID: id, Namespace: ns, Tier: memory.TierSemantic, Content: content, Importance: importance,
		CreatedAt: ts, UpdatedAt: ts, LastAccessedAt: ts, Embedding: vec[0],
	}
	if err := st.Upsert(ctx, m); err != nil {
		t.Fatalf("upsert %s: %v", id, err)
	}
}

func listAllNS(t *testing.T, st *sqlitevec.Store, ns string) []*memory.Memory {
	t.Helper()
	mems, err := st.List(context.Background(), ns, store.Filter{IncludeSuperseded: true, IncludeExpired: true}, 0)
	if err != nil {
		t.Fatalf("list %s: %v", ns, err)
	}
	return mems
}

// TestMigrateScopesMergesSharedIntoParent covers the basic <t>/_shared -> <t>
// merge, including a 3-deep namespace (a/b/_shared -> a/b), and asserts the
// report names both sides of every merge.
func TestMigrateScopesMergesSharedIntoParent(t *testing.T) {
	ctx := context.Background()
	st, emb := openScopesStore(t)

	putScoped(t, st, emb, "acme/_shared", "s1", "shared fact one", 0.5)
	putScoped(t, st, emb, "acme/_shared", "s2", "shared fact two", 0.5)
	putScoped(t, st, emb, "acme", "existing", "tenant fact", 0.5)

	putScoped(t, st, emb, "a/b/_shared", "deep1", "deep shared fact", 0.5)

	rep, err := maintenance.MigrateScopes(ctx, st, maintenance.ScopesOptions{Embedder: emb})
	if err != nil {
		t.Fatalf("migrate scopes: %v", err)
	}
	if len(rep.Merges) != 2 {
		t.Fatalf("merges=%d, want 2; report=%+v", len(rep.Merges), rep)
	}
	byFrom := map[string]maintenance.ScopeMerge{}
	for _, m := range rep.Merges {
		byFrom[m.From] = m
	}
	acme, ok := byFrom["acme/_shared"]
	if !ok || acme.To != "acme" || acme.Moved != 2 {
		t.Fatalf("acme merge = %+v, want To=acme Moved=2", acme)
	}
	deep, ok := byFrom["a/b/_shared"]
	if !ok || deep.To != "a/b" || deep.Moved != 1 {
		t.Fatalf("a/b merge = %+v, want To=a/b Moved=1", deep)
	}

	// The old namespaces are empty; the target namespaces hold everything.
	if got := listAllNS(t, st, "acme/_shared"); len(got) != 0 {
		t.Errorf("acme/_shared still has %d memories after merge", len(got))
	}
	if got := listAllNS(t, st, "acme"); len(got) != 3 {
		t.Errorf("acme has %d memories after merge, want 3", len(got))
	}
	if got := listAllNS(t, st, "a/b"); len(got) != 1 {
		t.Errorf("a/b has %d memories after merge, want 1", len(got))
	}
}

// TestMigrateScopesRepointsLinks pins that the merge goes through
// maintenance.Move, which already rewrites namespace_links endpoints (gap
// G5) — a link touching the old _shared namespace must point at the merged
// parent afterward.
func TestMigrateScopesRepointsLinks(t *testing.T) {
	ctx := context.Background()
	st, emb := openScopesStore(t)

	putScoped(t, st, emb, "acme/_shared", "s1", "shared fact", 0.5)

	now := time.Now().UTC()
	if err := st.PutLink(ctx, store.NamespaceLink{Src: "acme/_shared", Dst: "other", CreatedAt: now}); err != nil {
		t.Fatalf("put link: %v", err)
	}

	if _, err := maintenance.MigrateScopes(ctx, st, maintenance.ScopesOptions{Embedder: emb}); err != nil {
		t.Fatalf("migrate scopes: %v", err)
	}

	links, err := st.ListLinks(ctx, "acme")
	if err != nil {
		t.Fatalf("list links (acme): %v", err)
	}
	if len(links) != 1 || links[0].Dst != "other" {
		t.Fatalf("merge did not repoint the link onto acme: %+v", links)
	}
	oldLinks, err := st.ListLinks(ctx, "acme/_shared")
	if err != nil {
		t.Fatalf("list links (acme/_shared): %v", err)
	}
	if len(oldLinks) != 0 {
		t.Fatalf("acme/_shared still has links after merge: %+v", oldLinks)
	}
}

// TestMigrateScopesDedupsAfterMerge covers gap G14: Move relocates by unique
// ID with no content dedup, so a duplicate fact seeded in both the tenant and
// its _shared sibling must collapse in the target after the merge.
func TestMigrateScopesDedupsAfterMerge(t *testing.T) {
	ctx := context.Background()
	st, emb := openScopesStore(t)

	// Same content in both namespaces; higher importance in the shared one so
	// it wins as the dedup representative.
	putScoped(t, st, emb, "acme", "low", "the sky is blue", 0.1)
	putScoped(t, st, emb, "acme/_shared", "high", "the sky is blue", 0.9)
	putScoped(t, st, emb, "acme", "unrelated", "ferns reproduce via spores", 0.5)

	rep, err := maintenance.MigrateScopes(ctx, st, maintenance.ScopesOptions{
		Embedder: emb,
		Dedup:    maintenance.DedupOptions{Similarity: 0.5},
	})
	if err != nil {
		t.Fatalf("migrate scopes: %v", err)
	}
	if len(rep.Merges) != 1 || rep.Merges[0].DedupTombstoned != 1 {
		t.Fatalf("merges=%+v, want one merge with DedupTombstoned=1", rep.Merges)
	}

	low, err := st.Get(ctx, "acme", "low")
	if err != nil {
		t.Fatalf("get low: %v", err)
	}
	if low.SupersededBy == nil || *low.SupersededBy != "high" {
		t.Errorf("low.SupersededBy = %v, want high", low.SupersededBy)
	}
	high, err := st.Get(ctx, "acme", "high")
	if err != nil {
		t.Fatalf("get high: %v", err)
	}
	if high.SupersededBy != nil {
		t.Errorf("high unexpectedly superseded: %+v", high)
	}
	unrelated, err := st.Get(ctx, "acme", "unrelated")
	if err != nil {
		t.Fatalf("get unrelated: %v", err)
	}
	if unrelated.SupersededBy != nil {
		t.Errorf("unrelated memory touched by dedup: %+v", unrelated)
	}
}

// TestMigrateScopesIdempotent asserts that re-running once no `_shared`
// namespaces are left is a clean no-op.
func TestMigrateScopesIdempotent(t *testing.T) {
	ctx := context.Background()
	st, emb := openScopesStore(t)

	putScoped(t, st, emb, "acme/_shared", "s1", "shared fact", 0.5)

	if _, err := maintenance.MigrateScopes(ctx, st, maintenance.ScopesOptions{Embedder: emb}); err != nil {
		t.Fatalf("first migrate: %v", err)
	}

	rep, err := maintenance.MigrateScopes(ctx, st, maintenance.ScopesOptions{Embedder: emb})
	if err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	if len(rep.Merges) != 0 {
		t.Fatalf("second run merges = %+v, want none (idempotent no-op)", rep.Merges)
	}
}

// TestMigrateScopesLeavesBareSharedUntouched: a namespace literally named
// "_shared" (no prefix) has no parent to merge into and is left alone, noted
// in the report rather than silently skipped.
func TestMigrateScopesLeavesBareSharedUntouched(t *testing.T) {
	ctx := context.Background()
	st, emb := openScopesStore(t)

	putScoped(t, st, emb, "_shared", "b1", "bare shared fact", 0.5)

	rep, err := maintenance.MigrateScopes(ctx, st, maintenance.ScopesOptions{Embedder: emb})
	if err != nil {
		t.Fatalf("migrate scopes: %v", err)
	}
	if len(rep.Merges) != 0 {
		t.Fatalf("merges = %+v, want none (bare _shared has no parent)", rep.Merges)
	}
	if len(rep.BareShared) != 1 || rep.BareShared[0] != "_shared" {
		t.Fatalf("BareShared = %+v, want [\"_shared\"]", rep.BareShared)
	}
	if got := listAllNS(t, st, "_shared"); len(got) != 1 {
		t.Errorf("bare _shared namespace was touched: %d memories remain, want 1", len(got))
	}
}

// TestMigrateScopesDryRun asserts dry-run changes nothing while still
// reporting the merges (and their sizes) it would perform.
func TestMigrateScopesDryRun(t *testing.T) {
	ctx := context.Background()
	st, emb := openScopesStore(t)

	putScoped(t, st, emb, "acme/_shared", "s1", "shared fact one", 0.5)
	putScoped(t, st, emb, "acme/_shared", "s2", "shared fact two", 0.5)
	putScoped(t, st, emb, "acme", "existing", "tenant fact", 0.5)

	rep, err := maintenance.MigrateScopes(ctx, st, maintenance.ScopesOptions{
		DryRun:   true,
		Embedder: emb,
	})
	if err != nil {
		t.Fatalf("migrate scopes dry-run: %v", err)
	}
	if !rep.DryRun {
		t.Error("report.DryRun = false, want true")
	}
	if len(rep.Merges) != 1 || rep.Merges[0].From != "acme/_shared" || rep.Merges[0].To != "acme" || rep.Merges[0].Moved != 2 {
		t.Fatalf("dry-run merges = %+v, want one merge acme/_shared->acme Moved=2", rep.Merges)
	}
	if rep.Merges[0].DedupTombstoned != 0 {
		t.Errorf("dry-run must not tombstone anything, got %d", rep.Merges[0].DedupTombstoned)
	}

	// Nothing actually moved.
	if got := listAllNS(t, st, "acme/_shared"); len(got) != 2 {
		t.Errorf("acme/_shared has %d memories after dry-run, want unchanged 2", len(got))
	}
	if got := listAllNS(t, st, "acme"); len(got) != 1 {
		t.Errorf("acme has %d memories after dry-run, want unchanged 1", len(got))
	}
}

// TestMigrateScopesRequiresEmbedderToApply pins a fail-fast guard: applying
// (DryRun: false) with a merge to do and no Embedder configured must error
// before moving anything, rather than panicking deep inside the dedup pass
// on a nil Embedder once Move has already committed.
func TestMigrateScopesRequiresEmbedderToApply(t *testing.T) {
	ctx := context.Background()
	st, _ := openScopesStore(t)

	putScoped(t, st, embedtest.New(scopesDims), "acme/_shared", "s1", "shared fact", 0.5)

	_, err := maintenance.MigrateScopes(ctx, st, maintenance.ScopesOptions{})
	if err == nil {
		t.Fatal("want an error applying with no Embedder configured, got nil")
	}
	// Nothing moved: the guard fires before any Move.
	if got := listAllNS(t, st, "acme/_shared"); len(got) != 1 {
		t.Errorf("acme/_shared has %d memories after a failed apply, want unchanged 1", len(got))
	}

	// Similarity < 0 is the explicit dedup opt-out, so an apply with no
	// Embedder is legal in that case.
	rep, err := maintenance.MigrateScopes(ctx, st, maintenance.ScopesOptions{
		Dedup: maintenance.DedupOptions{Similarity: -1},
	})
	if err != nil {
		t.Fatalf("migrate scopes with dedup opted out: %v", err)
	}
	if len(rep.Merges) != 1 || rep.Merges[0].Moved != 1 {
		t.Fatalf("merges = %+v, want one merge Moved=1", rep.Merges)
	}
}

// TestMigrateScopesGlobalNamespaceEnvNotRewritten pins that a set
// MEMINI_GLOBAL_NAMESPACE is surfaced in the report (for the CLI to print
// adoption instructions from) but never used to drive a silent rewrite: with
// no `_shared` namespaces present, the run is still a no-op.
func TestMigrateScopesGlobalNamespaceEnvNotRewritten(t *testing.T) {
	ctx := context.Background()
	st, emb := openScopesStore(t)
	t.Setenv("MEMINI_GLOBAL_NAMESPACE", "shared/golang")

	putScoped(t, st, emb, "acme", "existing", "tenant fact", 0.5)

	rep, err := maintenance.MigrateScopes(ctx, st, maintenance.ScopesOptions{Embedder: emb})
	if err != nil {
		t.Fatalf("migrate scopes: %v", err)
	}
	if rep.GlobalNamespaceEnv != "shared/golang" {
		t.Errorf("GlobalNamespaceEnv = %q, want shared/golang", rep.GlobalNamespaceEnv)
	}
	if len(rep.Merges) != 0 {
		t.Errorf("merges = %+v, want none: the env var must not drive a rewrite", rep.Merges)
	}
	// The namespace itself must be untouched: still just "existing" in acme,
	// nothing minted or moved under shared/golang.
	if got := listAllNS(t, st, "acme"); len(got) != 1 {
		t.Errorf("acme has %d memories, want unchanged 1", len(got))
	}
	if got := listAllNS(t, st, "shared/golang"); len(got) != 0 {
		t.Errorf("shared/golang unexpectedly has %d memories", len(got))
	}
}
