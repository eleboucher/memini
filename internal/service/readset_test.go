package service

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/eleboucher/memini/internal/embed/embedtest"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/store"
	"github.com/eleboucher/memini/internal/store/sqlitevec"
)

const readsetTestDims = 64

// newReadsetSvc builds a Service over a real sqlite-vec store for read-set
// tests, so ListNamespaces/subtree expansion exercise real store behavior
// rather than a hand-rolled fake.
func newReadsetSvc(t *testing.T, opts ...Option) (*Service, *sqlitevec.Store) {
	t.Helper()
	st, err := sqlitevec.Open(context.Background(), filepath.Join(t.TempDir(), "readset.db"), readsetTestDims)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	base := []Option{
		WithClock(func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }),
	}
	svc := New(st, embedtest.New(readsetTestDims), append(base, opts...)...)
	return svc, st
}

// seedNamespace writes one bare memory into ns purely so it shows up in
// ListNamespaces (the store only lists namespaces that hold memories).
func seedNamespace(t *testing.T, st *sqlitevec.Store, ns string) {
	t.Helper()
	now := time.Unix(1_700_000_000, 0).UTC()
	m := &memory.Memory{
		ID: ns + "-seed", Namespace: ns, Tier: memory.TierSemantic, Content: "seed",
		CreatedAt: now, UpdatedAt: now, LastAccessedAt: now,
	}
	if err := st.Upsert(context.Background(), m); err != nil {
		t.Fatalf("seed namespace %s: %v", ns, err)
	}
}

// namespacesOf extracts the namespace strings from a resolved read-set, in
// order, for assertions.
func namespacesOf(entries []scopeEntry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.ns
	}
	return out
}

func assertNamespaces(t *testing.T, got []scopeEntry, want []string) {
	t.Helper()
	gotNS := namespacesOf(got)
	if len(gotNS) != len(want) {
		t.Fatalf("namespaces = %v, want %v", gotNS, want)
	}
	for i := range want {
		if gotNS[i] != want[i] {
			t.Fatalf("namespaces = %v, want %v", gotNS, want)
		}
	}
}

// countingListStore wraps a Store, counting ListNamespaces calls so tests can
// assert the "no scan unless subtree/pattern" property directly instead of
// indirectly.
type countingListStore struct {
	store.Store
	calls int
}

func (c *countingListStore) ListNamespaces(ctx context.Context) ([]string, error) {
	c.calls++
	return c.Store.ListNamespaces(ctx)
}

func TestResolveReadSetExplicitReplacesDefault(t *testing.T) {
	svc, _ := newReadsetSvc(t, WithGlobalNamespace("global"))
	got, err := svc.resolveReadSet(context.Background(), readScope{
		primary:  "A",
		explicit: []string{"A", "B"},
	})
	if err != nil {
		t.Fatalf("resolveReadSet: %v", err)
	}
	assertNamespaces(t, got, []string{"A", "B"})
	for _, e := range got {
		if e.ns == "global" {
			t.Fatal("explicit namespaces must not merge the global namespace")
		}
	}
}

func TestResolveReadSetSubtreePatternExpansion(t *testing.T) {
	svc, st := newReadsetSvc(t)
	seedNamespace(t, st, "team")
	seedNamespace(t, st, "team/a")
	seedNamespace(t, st, "team/b")
	seedNamespace(t, st, "teamx") // sibling by string prefix, not by "/" nesting
	seedNamespace(t, st, "other")

	got, err := svc.resolveReadSet(context.Background(), readScope{
		primary:  "team",
		explicit: []string{"team/*"},
	})
	if err != nil {
		t.Fatalf("resolveReadSet: %v", err)
	}
	assertNamespaces(t, got, []string{"team", "team/a", "team/b"})
}

func TestResolveReadSetExplicitDedupOrder(t *testing.T) {
	svc, st := newReadsetSvc(t)
	seedNamespace(t, st, "B")
	seedNamespace(t, st, "B/x")

	got, err := svc.resolveReadSet(context.Background(), readScope{
		primary:  "A",
		explicit: []string{"A", "B", "A", "B/*"},
	})
	if err != nil {
		t.Fatalf("resolveReadSet: %v", err)
	}
	// First-occurrence order: A, B (from the raw "B" entry, not re-added by the
	// later "B/*" pattern), then B's only descendant.
	assertNamespaces(t, got, []string{"A", "B", "B/x"})
}

func TestResolveReadSetExplicitGlobalGetsFullTiers(t *testing.T) {
	svc, _ := newReadsetSvc(t, WithGlobalNamespace("global"))
	got, err := svc.resolveReadSet(context.Background(), readScope{
		primary:  "proj",
		explicit: []string{"global"},
		reqTiers: []memory.Tier{memory.TierEpisodic}, // would exclude global entirely on the default path
	})
	if err != nil {
		t.Fatalf("resolveReadSet: %v", err)
	}
	if len(got) != 1 || got[0].ns != "global" {
		t.Fatalf("got %v, want a single global entry", namespacesOf(got))
	}
	if got[0].tiers != nil {
		t.Fatalf("explicit global entry should carry nil (full) tiers, got %v", got[0].tiers)
	}
}

// TestResolveReadSetDefaultSubtreeOverlapsGlobalKeepsWiderTiers exercises the
// dedup-by-namespace rule from the default path: when subtree expansion
// already surfaces a namespace equal to the configured global namespace, the
// merge must not downgrade it to the global's durable-only override.
func TestResolveReadSetDefaultSubtreeOverlapsGlobalKeepsWiderTiers(t *testing.T) {
	svc, st := newReadsetSvc(t, WithGlobalNamespace("proj/global"))
	seedNamespace(t, st, "proj")
	seedNamespace(t, st, "proj/global")

	got, err := svc.resolveReadSet(context.Background(), readScope{
		primary: "proj",
		subtree: true,
	})
	if err != nil {
		t.Fatalf("resolveReadSet: %v", err)
	}
	for _, e := range got {
		if e.ns == "proj/global" && e.tiers != nil {
			t.Fatalf("subtree-supplied entry should keep nil (wider) tiers, got %v", e.tiers)
		}
	}
}

// TestResolveReadSetExplicitPrimaryMovedFirst proves "primary is always kept
// and always first" holds even on the explicit path when the caller didn't
// list it first — primaryFirst reorders it, it doesn't just rely on the
// default path's construction order.
func TestResolveReadSetExplicitPrimaryMovedFirst(t *testing.T) {
	svc, _ := newReadsetSvc(t)
	got, err := svc.resolveReadSet(context.Background(), readScope{
		primary:  "A",
		explicit: []string{"B", "A", "C"},
	})
	if err != nil {
		t.Fatalf("resolveReadSet: %v", err)
	}
	assertNamespaces(t, got, []string{"A", "B", "C"})
}

func TestResolveReadSetExplicitTooManyRawEntries(t *testing.T) {
	svc, _ := newReadsetSvc(t)
	raw := make([]string, 17)
	for i := range raw {
		raw[i] = fmt.Sprintf("ns%d", i)
	}
	_, err := svc.resolveReadSet(context.Background(), readScope{primary: "A", explicit: raw})
	if err == nil {
		t.Fatal("expected an error for more than 16 raw explicit entries")
	}
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("error = %v, want ErrInvalidInput", err)
	}
}

func TestResolveReadSetExplicitInvalidNamespace(t *testing.T) {
	svc, _ := newReadsetSvc(t)
	_, err := svc.resolveReadSet(context.Background(), readScope{primary: "A", explicit: []string{""}})
	if err == nil || !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("error = %v, want ErrInvalidInput for an empty namespace entry", err)
	}
}

func TestResolveReadSetClampKeepsPrimaryFirst(t *testing.T) {
	svc, st := newReadsetSvc(t)
	for i := 0; i < 70; i++ {
		seedNamespace(t, st, fmt.Sprintf("root/ns%02d", i))
	}
	got, err := svc.resolveReadSet(context.Background(), readScope{primary: "root", subtree: true})
	if err != nil {
		t.Fatalf("resolveReadSet: %v", err)
	}
	if len(got) != readSetMaxEntries {
		t.Fatalf("len(got) = %d, want %d (clamped)", len(got), readSetMaxEntries)
	}
	if got[0].ns != "root" {
		t.Fatalf("got[0].ns = %q, want primary %q kept first after clamp", got[0].ns, "root")
	}
}

// TestResolveReadSetReadNamespacesMergesDurableOnly: a configured
// read-namespace contributes durable tiers only, exactly like the global
// namespace — MEMINI_READ_NAMESPACES generalizes it to a list.
func TestResolveReadSetReadNamespacesMergesDurableOnly(t *testing.T) {
	svc, _ := newReadsetSvc(t, WithReadNamespaces([]string{"shared"}))
	got, err := svc.resolveReadSet(context.Background(), readScope{primary: "proj"})
	if err != nil {
		t.Fatalf("resolveReadSet: %v", err)
	}
	assertNamespaces(t, got, []string{"proj", "shared"})
	for _, e := range got {
		if e.ns == "shared" {
			if len(e.tiers) == 0 {
				t.Fatal("shared entry should carry a durable-only tier override, got nil (full)")
			}
			for _, tr := range e.tiers {
				if tr != memory.TierSemantic && tr != memory.TierProcedural {
					t.Fatalf("shared entry tier %v is not durable", tr)
				}
			}
		}
	}
}

// TestResolveReadSetReadNamespacesSubtreePattern: a "/*" read-namespace entry
// expands to itself plus every nested namespace, sharing the read-set's
// single lazy ListNamespaces call.
func TestResolveReadSetReadNamespacesSubtreePattern(t *testing.T) {
	svc, st := newReadsetSvc(t, WithReadNamespaces([]string{"rules/*"}))
	seedNamespace(t, st, "rules")
	seedNamespace(t, st, "rules/go")
	seedNamespace(t, st, "rules/py")
	seedNamespace(t, st, "other")

	got, err := svc.resolveReadSet(context.Background(), readScope{primary: "proj"})
	if err != nil {
		t.Fatalf("resolveReadSet: %v", err)
	}
	assertNamespaces(t, got, []string{"proj", "rules", "rules/go", "rules/py"})
}

// TestResolveReadSetReadNamespacesSkippedForEpisodicOnlyFilter: like the
// global namespace, a read-namespace entry is skipped entirely (no fan-out,
// no ListNamespaces call for a "/*" pattern) when the request's tier filter
// admits no durable tier.
func TestResolveReadSetReadNamespacesSkippedForEpisodicOnlyFilter(t *testing.T) {
	st, err := sqlitevec.Open(context.Background(), filepath.Join(t.TempDir(), "readset-episodic.db"), readsetTestDims)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	counting := &countingListStore{Store: st}
	svc := New(counting, embedtest.New(readsetTestDims),
		WithClock(func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }),
		WithReadNamespaces([]string{"rules/*"}))

	got, err := svc.resolveReadSet(context.Background(), readScope{
		primary:  "proj",
		reqTiers: []memory.Tier{memory.TierEpisodic},
	})
	if err != nil {
		t.Fatalf("resolveReadSet: %v", err)
	}
	assertNamespaces(t, got, []string{"proj"})
	if counting.calls != 0 {
		t.Fatalf("episodic-only filter made %d ListNamespaces calls, want 0 (read-namespaces skipped entirely)", counting.calls)
	}
}

// TestResolveReadSetGlobalAndReadNamespacesDedup: WithGlobalNamespace and
// WithReadNamespaces both naming "g" must not produce a duplicate entry —
// containsNamespace's dedup covers the merge legs, not just subtree/global.
func TestResolveReadSetGlobalAndReadNamespacesDedup(t *testing.T) {
	svc, _ := newReadsetSvc(t, WithGlobalNamespace("g"), WithReadNamespaces([]string{"g", "h"}))
	got, err := svc.resolveReadSet(context.Background(), readScope{primary: "proj"})
	if err != nil {
		t.Fatalf("resolveReadSet: %v", err)
	}
	assertNamespaces(t, got, []string{"proj", "g", "h"})
}

// TestResolveReadSetExplicitDoesNotMergeReadNamespaces: explicit per-call
// namespaces replace the default read set entirely, same as the global
// namespace — configured read-namespaces must not sneak back in.
func TestResolveReadSetExplicitDoesNotMergeReadNamespaces(t *testing.T) {
	svc, _ := newReadsetSvc(t, WithReadNamespaces([]string{"shared"}))
	got, err := svc.resolveReadSet(context.Background(), readScope{
		primary:  "A",
		explicit: []string{"A", "B"},
	})
	if err != nil {
		t.Fatalf("resolveReadSet: %v", err)
	}
	assertNamespaces(t, got, []string{"A", "B"})
}

func TestResolveReadSetNoListNamespacesUnlessSubtreeOrPattern(t *testing.T) {
	st, err := sqlitevec.Open(context.Background(), filepath.Join(t.TempDir(), "readset-count.db"), readsetTestDims)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	counting := &countingListStore{Store: st}
	svc := New(counting, embedtest.New(readsetTestDims),
		WithClock(func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }))

	// Plain default recall (no subtree, no global configured): zero scans.
	if _, err := svc.resolveReadSet(context.Background(), readScope{primary: "A"}); err != nil {
		t.Fatalf("resolveReadSet: %v", err)
	}
	if counting.calls != 0 {
		t.Fatalf("plain default resolution made %d ListNamespaces calls, want 0", counting.calls)
	}

	// Explicit namespaces with no "/*" pattern: still zero scans.
	if _, err := svc.resolveReadSet(context.Background(), readScope{primary: "A", explicit: []string{"A", "B"}}); err != nil {
		t.Fatalf("resolveReadSet: %v", err)
	}
	if counting.calls != 0 {
		t.Fatalf("pattern-free explicit resolution made %d ListNamespaces calls, want 0", counting.calls)
	}

	// Subtree: exactly one scan.
	if _, err := svc.resolveReadSet(context.Background(), readScope{primary: "A", subtree: true}); err != nil {
		t.Fatalf("resolveReadSet: %v", err)
	}
	if counting.calls != 1 {
		t.Fatalf("subtree resolution made %d ListNamespaces calls, want 1", counting.calls)
	}

	// Multiple "/*" patterns in one explicit list: still exactly one shared scan.
	counting.calls = 0
	if _, err := svc.resolveReadSet(context.Background(), readScope{primary: "A", explicit: []string{"A/*", "B/*", "C"}}); err != nil {
		t.Fatalf("resolveReadSet: %v", err)
	}
	if counting.calls != 1 {
		t.Fatalf("multi-pattern explicit resolution made %d ListNamespaces calls, want 1 (shared)", counting.calls)
	}
}

// TestResolveReadSetReadNamespacesListNamespacesCalls: a literal
// read-namespace entry never scans (same as the global namespace); a "/*"
// read-namespace entry scans exactly once, sharing the call with subtree
// expansion when both are in play.
func TestResolveReadSetReadNamespacesListNamespacesCalls(t *testing.T) {
	newCountingSvc := func(t *testing.T, readNamespaces []string) (*Service, *countingListStore) {
		t.Helper()
		st, err := sqlitevec.Open(context.Background(), filepath.Join(t.TempDir(), "readset-rn-count.db"), readsetTestDims)
		if err != nil {
			t.Fatalf("open store: %v", err)
		}
		t.Cleanup(func() { _ = st.Close() })
		counting := &countingListStore{Store: st}
		svc := New(counting, embedtest.New(readsetTestDims),
			WithClock(func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }),
			WithReadNamespaces(readNamespaces))
		return svc, counting
	}

	t.Run("literal entry makes zero scans", func(t *testing.T) {
		svc, counting := newCountingSvc(t, []string{"shared"})
		if _, err := svc.resolveReadSet(context.Background(), readScope{primary: "A"}); err != nil {
			t.Fatalf("resolveReadSet: %v", err)
		}
		if counting.calls != 0 {
			t.Fatalf("literal read-namespace resolution made %d ListNamespaces calls, want 0", counting.calls)
		}
	})

	t.Run("subtree pattern shares the subtree scan", func(t *testing.T) {
		svc, counting := newCountingSvc(t, []string{"rules/*"})
		if _, err := svc.resolveReadSet(context.Background(), readScope{primary: "A", subtree: true}); err != nil {
			t.Fatalf("resolveReadSet: %v", err)
		}
		if counting.calls != 1 {
			t.Fatalf("subtree + read-namespace pattern made %d ListNamespaces calls, want 1 (shared)", counting.calls)
		}
	})
}

// TestResolveReadSetLinkDurableMergesDurableOnly: a "durable"-tiers link
// contributes a durable-only leg, exactly like the global namespace.
// TestResolveReadSetLinkAllMergesFullTiers: an "all"-tiers link carries nil
// (full, request's own filter) tiers — unlike global/read-namespaces, which
// are always durable-only.
// TestResolveReadSetLinkAllSurvivesEpisodicOnlyFilter: an "all"-tiers link is
// never skipped by the durable-admission gate (unlike a durable-only link),
// since its nil tiers just pass through the request's own filter.
// TestResolveReadSetLinkDurableSkippedForEpisodicOnlyFilter: a durable-only
// link is skipped entirely (no ListNamespaces call for a "/*" target) when
// the request's tier filter admits no durable tier — same rule as global.
// TestResolveReadSetLinkOneHop: A links to B, B links to C. Resolving A's
// default read set must never surface C — links are 1-hop, not transitive.
// TestResolveReadSetLinkIgnoredOnExplicit: links are part of the default path
// only — an explicit per-call namespace list must not pick up A's links.
// TestResolveReadSetLinkOverlapWidensTiers: two overlapping links for the
// same primary (a "/*" pattern link, durable, whose expansion covers an
// exact-match link, all, on one of its own members) must leave that member
// with full (all) tier access, not the pattern's narrower durable-only
// override. store.LinkStore's ListNamespaceLinks returns links ordered by
// target ("*" sorts before "c", so "b/*" is always processed before "b/c"
// regardless of which was created first), so this exercises the actual
// processing order addEntry must handle correctly in both directions.
// TestResolveReadSetClampKeepsGlobal: a subtree expansion past the 64-entry
// clamp must not push the global namespace's merged entry off the end; the
// clamp keeps the front of the slice, so the global entry must be
// front-ordered (right after primary) before clamping, per promoteGlobal.
func TestResolveReadSetClampKeepsGlobal(t *testing.T) {
	svc, st := newReadsetSvc(t, WithGlobalNamespace("global"))
	seedNamespace(t, st, "global")
	for i := 0; i < 70; i++ {
		seedNamespace(t, st, fmt.Sprintf("root/ns%02d", i))
	}
	got, err := svc.resolveReadSet(context.Background(), readScope{primary: "root", subtree: true})
	if err != nil {
		t.Fatalf("resolveReadSet: %v", err)
	}
	if len(got) != readSetMaxEntries {
		t.Fatalf("len(got) = %d, want %d (clamped)", len(got), readSetMaxEntries)
	}
	var found bool
	for _, e := range got {
		if e.ns == "global" {
			found = true
		}
	}
	if !found {
		t.Fatal("global namespace was clamped away")
	}
}

// TestResolveReadSetUnderCapKeepsGlobalLast: with a small subtree (well
// under the 64-entry clamp) plus a configured global namespace, the global
// entry must stay in its traditional LAST position. Entry order is
// observable beyond the clamp: FuseScores breaks exact score ties by
// first-seen order across namespaces, so front-ordering global on an
// under-cap set would let its tied hits outrank subtree children where they
// previously ranked last, a default-path behavior change this branch must
// not make. promoteGlobal therefore only reorders when the clamp will
// actually fire.
func TestResolveReadSetUnderCapKeepsGlobalLast(t *testing.T) {
	svc, st := newReadsetSvc(t, WithGlobalNamespace("global"))
	seedNamespace(t, st, "global")
	for i := 0; i < 10; i++ {
		seedNamespace(t, st, fmt.Sprintf("root/ns%02d", i))
	}
	got, err := svc.resolveReadSet(context.Background(), readScope{primary: "root", subtree: true})
	if err != nil {
		t.Fatalf("resolveReadSet: %v", err)
	}
	// primary + 10 children + global, well under the clamp.
	if len(got) != 12 {
		t.Fatalf("len(got) = %d, want 12 (primary + 10 children + global)", len(got))
	}
	if got[0].ns != "root" {
		t.Fatalf("got[0].ns = %q, want primary %q first", got[0].ns, "root")
	}
	if last := got[len(got)-1].ns; last != "global" {
		t.Fatalf("got[last].ns = %q, want %q last (pre-branch merge order preserved under the cap)", last, "global")
	}
}

// TestResolveReadSetLinkSubtreeTargetExpands: a link to "ns/*" expands to the
// bare namespace plus every namespace nested under it, sharing the read-set's
// single lazy ListNamespaces call.
