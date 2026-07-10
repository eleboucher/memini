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
	base := make([]Option, 0, 1+len(opts))
	base = append(base, WithClock(func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }))
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

// TestResolveReadSetTenantSharedMergesDurableOnly: a tenanted primary
// ("work/memini") implicitly reads its tenant-shared sibling ("work/_shared")
// with durable tiers only, exactly like the global namespace — no config.
func TestResolveReadSetTenantSharedMergesDurableOnly(t *testing.T) {
	svc, _ := newReadsetSvc(t)
	got, err := svc.resolveReadSet(context.Background(), readScope{primary: "work/memini"})
	if err != nil {
		t.Fatalf("resolveReadSet: %v", err)
	}
	assertNamespaces(t, got, []string{"work/memini", "work/_shared"})
	for _, e := range got {
		if e.ns == "work/_shared" {
			if len(e.tiers) == 0 {
				t.Fatal("tenant-shared entry should carry a durable-only tier override, got nil (full)")
			}
			for _, tr := range e.tiers {
				if tr != memory.TierSemantic && tr != memory.TierProcedural {
					t.Fatalf("tenant-shared entry tier %v is not durable", tr)
				}
			}
		}
	}
}

// TestResolveReadSetTenantSharedReadsSiblings: the layout the design promises —
// scope=subtree from work/memini reads its own subtree plus the tenant-shared
// namespace, never a personal/... sibling.
func TestResolveReadSetTenantSharedReadsSiblings(t *testing.T) {
	svc, st := newReadsetSvc(t)
	seedNamespace(t, st, "work/memini")
	seedNamespace(t, st, "work/memini/reviewer")
	seedNamespace(t, st, "work/_shared")
	seedNamespace(t, st, "personal/blog")

	got, err := svc.resolveReadSet(context.Background(), readScope{primary: "work/memini", subtree: true})
	if err != nil {
		t.Fatalf("resolveReadSet: %v", err)
	}
	assertNamespaces(t, got, []string{"work/memini", "work/memini/reviewer", "work/_shared"})
}

// TestResolveReadSetFlatNamespaceNoTenantShared: an untenanted (no "/")
// namespace has no tenant segment, so no shared merge — exact pre-tenant
// behavior for flat namespaces.
func TestResolveReadSetFlatNamespaceNoTenantShared(t *testing.T) {
	svc, _ := newReadsetSvc(t)
	got, err := svc.resolveReadSet(context.Background(), readScope{primary: "proj"})
	if err != nil {
		t.Fatalf("resolveReadSet: %v", err)
	}
	assertNamespaces(t, got, []string{"proj"})
}

// TestResolveReadSetTenantSharedItselfNoSelfMerge: reading the tenant-shared
// namespace directly does not add itself again.
func TestResolveReadSetTenantSharedItselfNoSelfMerge(t *testing.T) {
	svc, _ := newReadsetSvc(t)
	got, err := svc.resolveReadSet(context.Background(), readScope{primary: "work/_shared"})
	if err != nil {
		t.Fatalf("resolveReadSet: %v", err)
	}
	assertNamespaces(t, got, []string{"work/_shared"})
}

// TestResolveReadSetTenantSharedSkippedForEpisodicOnlyFilter: like the global
// namespace, the durable-only tenant-shared merge is skipped when the
// request's tier filter admits no durable tier.
func TestResolveReadSetTenantSharedSkippedForEpisodicOnlyFilter(t *testing.T) {
	svc, _ := newReadsetSvc(t)
	got, err := svc.resolveReadSet(context.Background(), readScope{
		primary:  "work/memini",
		reqTiers: []memory.Tier{memory.TierEpisodic},
	})
	if err != nil {
		t.Fatalf("resolveReadSet: %v", err)
	}
	assertNamespaces(t, got, []string{"work/memini"})
}

// TestResolveReadSetGlobalEqualsTenantSharedDedup: when the global namespace
// happens to be the tenant-shared namespace, it appears once, not twice.
func TestResolveReadSetGlobalEqualsTenantSharedDedup(t *testing.T) {
	svc, _ := newReadsetSvc(t, WithGlobalNamespace("work/_shared"))
	got, err := svc.resolveReadSet(context.Background(), readScope{primary: "work/memini"})
	if err != nil {
		t.Fatalf("resolveReadSet: %v", err)
	}
	assertNamespaces(t, got, []string{"work/memini", "work/_shared"})
}

// TestResolveReadSetExplicitDoesNotMergeTenantShared: explicit per-call
// namespaces replace the default read set entirely — the tenant-shared merge
// must not sneak back in.
func TestResolveReadSetExplicitDoesNotMergeTenantShared(t *testing.T) {
	svc, _ := newReadsetSvc(t)
	got, err := svc.resolveReadSet(context.Background(), readScope{
		primary:  "work/memini",
		explicit: []string{"work/memini", "work/other"},
	})
	if err != nil {
		t.Fatalf("resolveReadSet: %v", err)
	}
	assertNamespaces(t, got, []string{"work/memini", "work/other"})
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

// TestResolveReadSetTenantSharedMakesZeroScans: the tenant-shared merge is a
// single literal namespace, so a plain default read (no subtree) never scans,
// same as the global namespace.
func TestResolveReadSetTenantSharedMakesZeroScans(t *testing.T) {
	st, err := sqlitevec.Open(context.Background(), filepath.Join(t.TempDir(), "readset-ts-count.db"), readsetTestDims)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	counting := &countingListStore{Store: st}
	svc := New(counting, embedtest.New(readsetTestDims),
		WithClock(func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }))

	if _, err := svc.resolveReadSet(context.Background(), readScope{primary: "work/memini"}); err != nil {
		t.Fatalf("resolveReadSet: %v", err)
	}
	if counting.calls != 0 {
		t.Fatalf("tenant-shared merge made %d ListNamespaces calls, want 0", counting.calls)
	}
}

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

func TestResolveReadSetExplicitNormalizesEntries(t *testing.T) {
	svc, st := newReadsetSvc(t)
	seedNamespace(t, st, "work/memini")
	seedNamespace(t, st, "shared")
	seedNamespace(t, st, "shared/a")

	// Untrimmed, slash-wrapped, and doubled-separator entries must address the
	// stored namespaces literally, and normalized duplicates must collapse.
	got, err := svc.resolveReadSet(context.Background(), readScope{
		primary:  "work/memini",
		explicit: []string{" work//memini/ ", "work/memini", "shared//* "},
	})
	if err != nil {
		t.Fatalf("resolveReadSet: %v", err)
	}
	assertNamespaces(t, got, []string{"work/memini", "shared", "shared/a"})
}

func TestResolveReadSetExplicitBarePatternRejected(t *testing.T) {
	svc, _ := newReadsetSvc(t)
	// A bare "/*" has an empty base namespace; it must fail loudly rather
	// than normalize into a literal "*" entry that silently matches nothing.
	_, err := svc.resolveReadSet(context.Background(), readScope{primary: "A", explicit: []string{"/*"}})
	if err == nil || !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("error = %v, want ErrInvalidInput for a bare \"/*\" entry", err)
	}
}
