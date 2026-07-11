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
// tests, so ListNamespaces/subtree expansion/ListLinks exercise real store
// behavior rather than a hand-rolled fake.
func newReadsetSvc(t *testing.T) (*Service, *sqlitevec.Store) {
	t.Helper()
	st, err := sqlitevec.Open(context.Background(), filepath.Join(t.TempDir(), "readset.db"), readsetTestDims)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	svc := New(st, embedtest.New(readsetTestDims),
		WithClock(func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }))
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

// putLink stores a namespace link, failing the test on error.
func putLink(t *testing.T, st *sqlitevec.Store, src, dst string, tiers []memory.Tier) {
	t.Helper()
	l := store.NamespaceLink{Src: src, Dst: dst, Tiers: tiers, CreatedAt: time.Unix(1_700_000_000, 0).UTC()}
	if err := st.PutLink(context.Background(), l); err != nil {
		t.Fatalf("put link %s -> %s: %v", src, dst, err)
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

// entryFor returns the entry for ns, or ok=false when absent.
func entryFor(entries []scopeEntry, ns string) (scopeEntry, bool) {
	for _, e := range entries {
		if e.ns == ns {
			return e, true
		}
	}
	return scopeEntry{}, false
}

func tiersEqual(a, b []memory.Tier) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// countingListStore wraps a Store, counting ListNamespaces calls so tests can
// assert the "no scan unless subtree/pattern" property directly instead of
// indirectly. Because it embeds the store.Store interface (not the concrete
// driver), it does NOT satisfy store.LinkStore even when the wrapped driver
// does — the method set promoted from an embedded interface is the
// interface's own, not the dynamic value's. That makes it double as a "store
// predates LinkStore" fake for the graceful-degradation tests below.
type countingListStore struct {
	store.Store
	calls int
}

func (c *countingListStore) ListNamespaces(ctx context.Context) ([]string, error) {
	c.calls++
	return c.Store.ListNamespaces(ctx)
}

// --- ancestorsOf -------------------------------------------------------

func TestAncestorsOf(t *testing.T) {
	tests := []struct {
		name string
		ns   string
		want []string
	}{
		{"flat", "acme", nil},
		{"depth1", "acme/phoenix", []string{"acme"}},
		{"depth3", "acme/phoenix/api", []string{"acme/phoenix", "acme"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ancestorsOf(tt.ns)
			if len(got) != len(tt.want) {
				t.Fatalf("ancestorsOf(%q) = %v, want %v", tt.ns, got, tt.want)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Fatalf("ancestorsOf(%q) = %v, want %v", tt.ns, got, tt.want)
				}
			}
		})
	}
}

// --- parseScope ----------------------------------------------------------

func TestParseScope(t *testing.T) {
	tests := []struct {
		scope       string
		wantBare    bool
		wantSubtree bool
		wantErr     bool
	}{
		{"", false, false, false},
		{"full", false, false, false},
		{"project", true, false, false},
		{"everywhere", false, true, false},
		{"bogus", false, false, true},
	}
	for _, tt := range tests {
		bare, subtree, err := parseScope(tt.scope)
		if tt.wantErr {
			if err == nil || !errors.Is(err, ErrInvalidInput) {
				t.Errorf("parseScope(%q) error = %v, want ErrInvalidInput", tt.scope, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseScope(%q) unexpected error: %v", tt.scope, err)
		}
		if bare != tt.wantBare || subtree != tt.wantSubtree {
			t.Errorf("parseScope(%q) = (bare=%v, subtree=%v), want (bare=%v, subtree=%v)",
				tt.scope, bare, subtree, tt.wantBare, tt.wantSubtree)
		}
	}
}

// --- default read-set: flat / ancestors -----------------------------------

func TestResolveReadSetFlatNamespacePrimaryOnly(t *testing.T) {
	svc, _ := newReadsetSvc(t)
	got, err := svc.resolveReadSet(context.Background(), readScope{primary: "proj"})
	if err != nil {
		t.Fatalf("resolveReadSet: %v", err)
	}
	assertNamespaces(t, got, []string{"proj"})
}

func TestResolveReadSetDepth3AncestorsNearestFirst(t *testing.T) {
	svc, _ := newReadsetSvc(t)
	got, err := svc.resolveReadSet(context.Background(), readScope{primary: "acme/phoenix/api"})
	if err != nil {
		t.Fatalf("resolveReadSet: %v", err)
	}
	// Nearest ancestor ("acme/phoenix") must precede the farther one
	// ("acme") — load-bearing for FuseScores' first-seen tie-break.
	assertNamespaces(t, got, []string{"acme/phoenix/api", "acme/phoenix", "acme"})
	for _, ns := range []string{"acme/phoenix", "acme"} {
		e, ok := entryFor(got, ns)
		if !ok {
			t.Fatalf("ancestor %q missing", ns)
		}
		if !tiersEqual(e.tiers, durableTiers(nil)) {
			t.Fatalf("ancestor %q tiers = %v, want durable-only", ns, e.tiers)
		}
	}
}

// --- home leg --------------------------------------------------------------

func TestResolveReadSetHomeAppendedAfterAncestors(t *testing.T) {
	svc, _ := newReadsetSvc(t)
	got, err := svc.resolveReadSet(context.Background(), readScope{
		primary: "acme/phoenix", home: "personal/kit",
	})
	if err != nil {
		t.Fatalf("resolveReadSet: %v", err)
	}
	assertNamespaces(t, got, []string{"acme/phoenix", "acme", "personal/kit"})
	e, ok := entryFor(got, "personal/kit")
	if !ok || !tiersEqual(e.tiers, durableTiers(nil)) {
		t.Fatalf("home entry = %+v, ok=%v, want durable-only tiers", e, ok)
	}
}

func TestResolveReadSetHomeAbsentNoHomeLeg(t *testing.T) {
	svc, _ := newReadsetSvc(t)
	got, err := svc.resolveReadSet(context.Background(), readScope{primary: "acme/phoenix"})
	if err != nil {
		t.Fatalf("resolveReadSet: %v", err)
	}
	assertNamespaces(t, got, []string{"acme/phoenix", "acme"})
}

// TestResolveReadSetHomeEqualsPrimaryNoDup: home pointing at the caller's own
// primary namespace must not create a second entry, and primary keeps full
// (nil) tier access rather than being narrowed to durable-only.
func TestResolveReadSetHomeEqualsPrimaryNoDup(t *testing.T) {
	svc, _ := newReadsetSvc(t)
	got, err := svc.resolveReadSet(context.Background(), readScope{primary: "proj", home: "proj"})
	if err != nil {
		t.Fatalf("resolveReadSet: %v", err)
	}
	assertNamespaces(t, got, []string{"proj"})
	if got[0].tiers != nil {
		t.Fatalf("primary==home entry tiers = %v, want nil (full)", got[0].tiers)
	}
}

// TestResolveReadSetHomeEqualsAncestorDedup: home pointing at an ancestor
// already in the cascade collapses to one entry, not two.
func TestResolveReadSetHomeEqualsAncestorDedup(t *testing.T) {
	svc, _ := newReadsetSvc(t)
	got, err := svc.resolveReadSet(context.Background(), readScope{
		primary: "acme/phoenix/api", home: "acme/phoenix",
	})
	if err != nil {
		t.Fatalf("resolveReadSet: %v", err)
	}
	assertNamespaces(t, got, []string{"acme/phoenix/api", "acme/phoenix", "acme"})
}

// TestResolveReadSetSubtreeOverlapsHomeKeepsWiderTiers exercises the
// existing addEntry widest-tiers merge (readset.go): when subtree expansion
// already surfaces a namespace equal to home, the home leg must not
// downgrade it from full (nil) tiers to the durable-only override.
func TestResolveReadSetSubtreeOverlapsHomeKeepsWiderTiers(t *testing.T) {
	svc, st := newReadsetSvc(t)
	seedNamespace(t, st, "acme")
	seedNamespace(t, st, "acme/phoenix")

	got, err := svc.resolveReadSet(context.Background(), readScope{
		primary: "acme", subtree: true, home: "acme/phoenix",
	})
	if err != nil {
		t.Fatalf("resolveReadSet: %v", err)
	}
	e, ok := entryFor(got, "acme/phoenix")
	if !ok {
		t.Fatal("acme/phoenix missing from read-set")
	}
	if e.tiers != nil {
		t.Fatalf("subtree-supplied entry should keep nil (wider) tiers, got %v", e.tiers)
	}
}

// --- link leg ----------------------------------------------------------

func TestResolveReadSetLinkDefaultTiersFullDurable(t *testing.T) {
	svc, st := newReadsetSvc(t)
	putLink(t, st, "acme/phoenix", "shared/golang", nil) // nil = durable default
	got, err := svc.resolveReadSet(context.Background(), readScope{primary: "acme/phoenix"})
	if err != nil {
		t.Fatalf("resolveReadSet: %v", err)
	}
	e, ok := entryFor(got, "shared/golang")
	if !ok {
		t.Fatal("linked namespace missing from read-set")
	}
	if !tiersEqual(e.tiers, durableTiers(nil)) {
		t.Fatalf("link tiers = %v, want full durable set", e.tiers)
	}
}

func TestResolveReadSetLinkTierOverrideIntersectedWithDurable(t *testing.T) {
	svc, st := newReadsetSvc(t)
	putLink(t, st, "acme/phoenix", "shared/golang", []memory.Tier{memory.TierSemantic})
	got, err := svc.resolveReadSet(context.Background(), readScope{primary: "acme/phoenix"})
	if err != nil {
		t.Fatalf("resolveReadSet: %v", err)
	}
	e, ok := entryFor(got, "shared/golang")
	if !ok {
		t.Fatal("linked namespace missing from read-set")
	}
	if !tiersEqual(e.tiers, []memory.Tier{memory.TierSemantic}) {
		t.Fatalf("link tiers = %v, want [semantic]", e.tiers)
	}
}

// TestResolveReadSetLinkNeverAdmitsEpisodicOrWorking: the global tier rule
// wins over a link's own configuration — a link whose Tiers only lists
// non-durable tiers contributes nothing at all, not an entry with an empty
// (all-tiers) filter.
func TestResolveReadSetLinkNeverAdmitsEpisodicOrWorking(t *testing.T) {
	svc, st := newReadsetSvc(t)
	putLink(t, st, "proj", "shared/golang", []memory.Tier{memory.TierEpisodic, memory.TierWorking})
	got, err := svc.resolveReadSet(context.Background(), readScope{primary: "proj"})
	if err != nil {
		t.Fatalf("resolveReadSet: %v", err)
	}
	if _, ok := entryFor(got, "shared/golang"); ok {
		t.Fatal("link admitting only episodic/working tiers must contribute no entry")
	}
	assertNamespaces(t, got, []string{"proj"})
}

// TestResolveReadSetNoLinksForOtherSrc: links are directional and scoped by
// src — a link FROM a different namespace must not leak into this read-set.
func TestResolveReadSetNoLinksForOtherSrc(t *testing.T) {
	svc, st := newReadsetSvc(t)
	putLink(t, st, "other", "shared/golang", nil)
	got, err := svc.resolveReadSet(context.Background(), readScope{primary: "proj"})
	if err != nil {
		t.Fatalf("resolveReadSet: %v", err)
	}
	assertNamespaces(t, got, []string{"proj"})
}

// TestResolveReadSetNoLinkStoreDegradesGracefully: a store that predates
// LinkStore (simulated by countingListStore, which does not promote
// PutLink/ListLinks — see its doc comment) must resolve normally with no
// links leg and no error, ancestors/home still applying.
func TestResolveReadSetNoLinkStoreDegradesGracefully(t *testing.T) {
	st, err := sqlitevec.Open(context.Background(), filepath.Join(t.TempDir(), "readset-nolink.db"), readsetTestDims)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	// Prove the underlying store DOES support links, so absence below is
	// attributable to the wrapper, not a missing implementation.
	putLink(t, st, "acme/phoenix", "shared/golang", nil)
	counting := &countingListStore{Store: st}
	svc := New(counting, embedtest.New(readsetTestDims),
		WithClock(func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }))

	got, err := svc.resolveReadSet(context.Background(), readScope{
		primary: "acme/phoenix", home: "personal/kit",
	})
	if err != nil {
		t.Fatalf("resolveReadSet: %v", err)
	}
	assertNamespaces(t, got, []string{"acme/phoenix", "acme", "personal/kit"})
}

// --- bare / tier gate ----------------------------------------------------

// TestResolveReadSetBareSkipsAllLegs: Scope "project" (bare=true) reduces
// the read-set to primary alone, even with ancestors, a home, and a link all
// otherwise available.
func TestResolveReadSetBareSkipsAllLegs(t *testing.T) {
	svc, st := newReadsetSvc(t)
	putLink(t, st, "acme/phoenix/api", "shared/golang", nil)
	got, err := svc.resolveReadSet(context.Background(), readScope{
		primary: "acme/phoenix/api", home: "personal/kit", bare: true,
	})
	if err != nil {
		t.Fatalf("resolveReadSet: %v", err)
	}
	assertNamespaces(t, got, []string{"acme/phoenix/api"})
}

// TestResolveReadSetNonDurableTierFilterSkipsCascade: the existing
// durableTiers gate (readset.go) — an episodic-only request filter admits no
// durable tier, so the ancestor/home/link cascade contributes nothing.
func TestResolveReadSetNonDurableTierFilterSkipsCascade(t *testing.T) {
	svc, st := newReadsetSvc(t)
	putLink(t, st, "acme/phoenix/api", "shared/golang", nil)
	got, err := svc.resolveReadSet(context.Background(), readScope{
		primary:  "acme/phoenix/api",
		home:     "personal/kit",
		reqTiers: []memory.Tier{memory.TierEpisodic},
	})
	if err != nil {
		t.Fatalf("resolveReadSet: %v", err)
	}
	assertNamespaces(t, got, []string{"acme/phoenix/api"})
}

// --- clamp interaction -----------------------------------------------------

// TestResolveReadSetClampKeepsHomeViaPromotion: a subtree expansion alone
// past the 64-entry cap must not push home off the end — promoteProtected
// front-orders it (right after primary) before the clamp fires.
func TestResolveReadSetClampKeepsHomeViaPromotion(t *testing.T) {
	svc, st := newReadsetSvc(t)
	for i := 0; i < 70; i++ {
		seedNamespace(t, st, fmt.Sprintf("root/ns%02d", i))
	}
	got, err := svc.resolveReadSet(context.Background(), readScope{
		primary: "root", subtree: true, home: "personal/kit",
	})
	if err != nil {
		t.Fatalf("resolveReadSet: %v", err)
	}
	if len(got) != readSetMaxEntries {
		t.Fatalf("len(got) = %d, want %d (clamped)", len(got), readSetMaxEntries)
	}
	if _, ok := entryFor(got, "personal/kit"); !ok {
		t.Fatal("home namespace was clamped away")
	}
	if got[0].ns != "root" {
		t.Fatalf("got[0].ns = %q, want primary first", got[0].ns)
	}
}

// TestResolveReadSetClampKeepsNearAncestorsNaturally: a large LINKS leg
// pushing the set past the cap must not drop the near ancestors — they
// survive because they are appended right after primary, well before the
// links, with no explicit protection needed (promoteProtected is only
// called with sc.home).
func TestResolveReadSetClampKeepsNearAncestorsNaturally(t *testing.T) {
	svc, st := newReadsetSvc(t)
	for i := 0; i < 70; i++ {
		putLink(t, st, "acme/phoenix/api", fmt.Sprintf("shared/link%02d", i), nil)
	}
	got, err := svc.resolveReadSet(context.Background(), readScope{primary: "acme/phoenix/api"})
	if err != nil {
		t.Fatalf("resolveReadSet: %v", err)
	}
	if len(got) != readSetMaxEntries {
		t.Fatalf("len(got) = %d, want %d (clamped)", len(got), readSetMaxEntries)
	}
	assertNamespaces(t, got[:3], []string{"acme/phoenix/api", "acme/phoenix", "acme"})
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

// --- explicit path (unchanged mechanics, re-verified against the cascade) --

// TestResolveReadSetExplicitReplacesCascade: explicit namespaces replace the
// entire default read set — ancestors that would otherwise apply to this
// primary must not sneak in.
func TestResolveReadSetExplicitReplacesCascade(t *testing.T) {
	svc, _ := newReadsetSvc(t)
	got, err := svc.resolveReadSet(context.Background(), readScope{
		primary:  "acme/phoenix/api",
		explicit: []string{"A", "B"},
	})
	if err != nil {
		t.Fatalf("resolveReadSet: %v", err)
	}
	assertNamespaces(t, got, []string{"A", "B"})
	for _, ns := range []string{"acme/phoenix", "acme"} {
		if _, ok := entryFor(got, ns); ok {
			t.Fatalf("explicit namespaces must not merge ancestor %q", ns)
		}
	}
}

// TestResolveReadSetExplicitDoesNotMergeHomeOrLinks: explicit per-call
// namespaces replace the default read set entirely — home and stored links
// must not sneak back in.
func TestResolveReadSetExplicitDoesNotMergeHomeOrLinks(t *testing.T) {
	svc, st := newReadsetSvc(t)
	putLink(t, st, "acme/phoenix", "shared/golang", nil)
	got, err := svc.resolveReadSet(context.Background(), readScope{
		primary:  "acme/phoenix",
		home:     "personal/kit",
		explicit: []string{"acme/phoenix", "other"},
	})
	if err != nil {
		t.Fatalf("resolveReadSet: %v", err)
	}
	assertNamespaces(t, got, []string{"acme/phoenix", "other"})
}

// TestResolveReadSetExplicitEntryGetsFullTiersNotLinkOverride: an explicit
// entry always carries nil (the request's own tier filter), even when the
// same namespace would otherwise be reached via a narrower-tiered link — and
// even under a tier filter (episodic-only) that would exclude it entirely on
// the default path.
func TestResolveReadSetExplicitEntryGetsFullTiersNotLinkOverride(t *testing.T) {
	svc, st := newReadsetSvc(t)
	putLink(t, st, "acme/phoenix", "shared/golang", []memory.Tier{memory.TierSemantic})
	got, err := svc.resolveReadSet(context.Background(), readScope{
		primary:  "acme/phoenix",
		explicit: []string{"shared/golang"},
		reqTiers: []memory.Tier{memory.TierEpisodic},
	})
	if err != nil {
		t.Fatalf("resolveReadSet: %v", err)
	}
	if len(got) != 1 || got[0].ns != "shared/golang" {
		t.Fatalf("got %v, want a single shared/golang entry", namespacesOf(got))
	}
	if got[0].tiers != nil {
		t.Fatalf("explicit entry should carry nil (full) tiers, got %v", got[0].tiers)
	}
}

// TestResolveReadSetExplicitBeatsBareAndSubtree is the precedence test for
// gap G8: explicit namespaces replace the cascade outright regardless of
// bare/subtree (the readScope-level encoding of Scope "project"/"everywhere")
// — explicit always wins.
func TestResolveReadSetExplicitBeatsBareAndSubtree(t *testing.T) {
	svc, _ := newReadsetSvc(t)
	got, err := svc.resolveReadSet(context.Background(), readScope{
		primary:  "acme/phoenix/api",
		explicit: []string{"other"},
		bare:     true,
		subtree:  true,
	})
	if err != nil {
		t.Fatalf("resolveReadSet: %v", err)
	}
	assertNamespaces(t, got, []string{"other"})
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

// --- ListNamespaces scan discipline ----------------------------------------

func TestResolveReadSetNoListNamespacesUnlessSubtreeOrPattern(t *testing.T) {
	st, err := sqlitevec.Open(context.Background(), filepath.Join(t.TempDir(), "readset-count.db"), readsetTestDims)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	counting := &countingListStore{Store: st}
	svc := New(counting, embedtest.New(readsetTestDims),
		WithClock(func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }))

	// Plain default recall (no subtree, no ancestors/home/links): zero scans.
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

	// A depth-3 primary with ancestors: ancestors never scan either.
	counting.calls = 0
	if _, err := svc.resolveReadSet(context.Background(), readScope{primary: "acme/phoenix/api"}); err != nil {
		t.Fatalf("resolveReadSet: %v", err)
	}
	if counting.calls != 0 {
		t.Fatalf("ancestor resolution made %d ListNamespaces calls, want 0", counting.calls)
	}
}

// --- origin provenance (T5) -------------------------------------------------

// TestResolveReadSetOriginsAllFiveKinds resolves a read-set that exercises
// every origin in one shot — primary, ancestor, home, link, and (on the
// explicit path) call — and asserts each leg's recorded origin matches where
// it was appended, not some re-derived guess.
func TestResolveReadSetOriginsAllFiveKinds(t *testing.T) {
	svc, st := newReadsetSvc(t)
	putLink(t, st, "acme/phoenix/api", "shared/golang", nil)

	got, err := svc.resolveReadSet(context.Background(), readScope{
		primary: "acme/phoenix/api", home: "personal/kit",
	})
	if err != nil {
		t.Fatalf("resolveReadSet: %v", err)
	}
	wantOrigin := map[string]string{
		"acme/phoenix/api": OriginPrimary,
		"acme/phoenix":     OriginAncestor,
		"acme":             OriginAncestor,
		"personal/kit":     OriginHome,
		"shared/golang":    OriginLink,
	}
	if len(got) != len(wantOrigin) {
		t.Fatalf("resolveReadSet entries = %v, want namespaces %v", namespacesOf(got), wantOrigin)
	}
	for ns, want := range wantOrigin {
		e, ok := entryFor(got, ns)
		if !ok {
			t.Fatalf("namespace %q missing from read-set", ns)
		}
		if e.origin != want {
			t.Fatalf("origin(%q) = %q, want %q", ns, e.origin, want)
		}
	}

	// Explicit (per-call) path: a non-primary entry gets "call"; the primary
	// namespace itself is always "primary" even here.
	got, err = svc.resolveReadSet(context.Background(), readScope{
		primary: "acme/phoenix/api", explicit: []string{"acme/phoenix/api", "other/ns"},
	})
	if err != nil {
		t.Fatalf("resolveReadSet (explicit): %v", err)
	}
	e, ok := entryFor(got, "acme/phoenix/api")
	if !ok || e.origin != OriginPrimary {
		t.Fatalf("explicit primary entry origin = %+v, ok=%v, want %q", e, ok, OriginPrimary)
	}
	e, ok = entryFor(got, "other/ns")
	if !ok || e.origin != OriginCall {
		t.Fatalf("explicit non-primary entry origin = %+v, ok=%v, want %q", e, ok, OriginCall)
	}
}

// TestResolveReadSetSubtreeMembersAreOriginPrimary: subtree expansion of the
// primary namespace is treated as part of the primary leg — every subtree
// member gets origin "primary", not a distinct origin of its own.
func TestResolveReadSetSubtreeMembersAreOriginPrimary(t *testing.T) {
	svc, st := newReadsetSvc(t)
	seedNamespace(t, st, "acme")
	seedNamespace(t, st, "acme/phoenix")
	seedNamespace(t, st, "acme/phoenix/api")

	got, err := svc.resolveReadSet(context.Background(), readScope{primary: "acme", subtree: true})
	if err != nil {
		t.Fatalf("resolveReadSet: %v", err)
	}
	for _, ns := range []string{"acme", "acme/phoenix", "acme/phoenix/api"} {
		e, ok := entryFor(got, ns)
		if !ok || e.origin != OriginPrimary {
			t.Fatalf("subtree member %q origin = %+v, ok=%v, want %q", ns, e, ok, OriginPrimary)
		}
	}
}

// TestResolveReadSetOriginFirstAppendWins: when addEntry widens an existing
// entry's tiers (home landing on a namespace already present as an ancestor),
// the origin recorded at first append is kept — origin is never re-derived
// or overwritten by a later leg touching the same namespace.
func TestResolveReadSetOriginFirstAppendWins(t *testing.T) {
	svc, _ := newReadsetSvc(t)
	got, err := svc.resolveReadSet(context.Background(), readScope{
		primary: "acme/phoenix/api", home: "acme/phoenix", // home == an ancestor
	})
	if err != nil {
		t.Fatalf("resolveReadSet: %v", err)
	}
	e, ok := entryFor(got, "acme/phoenix")
	if !ok || e.origin != OriginAncestor {
		t.Fatalf("acme/phoenix origin = %+v, ok=%v, want %q (ancestor appended first)", e, ok, OriginAncestor)
	}
}

// TestToReadSetEntriesMapsOriginAndTiers pins the public mapping from the
// internal scopeEntry slice to the public ReadSetEntry shape T6's read-set
// endpoint (and Recall/Briefing's out-params) consume.
func TestToReadSetEntriesMapsOriginAndTiers(t *testing.T) {
	entries := []scopeEntry{
		{ns: "acme", origin: OriginPrimary},
		{ns: "shared/golang", origin: OriginLink, tiers: []memory.Tier{memory.TierSemantic}},
	}
	got := toReadSetEntries(entries)
	want := []ReadSetEntry{
		{NS: "acme", Origin: OriginPrimary},
		{NS: "shared/golang", Origin: OriginLink, Tiers: []memory.Tier{memory.TierSemantic}},
	}
	if len(got) != len(want) {
		t.Fatalf("toReadSetEntries = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i].NS != want[i].NS || got[i].Origin != want[i].Origin || !tiersEqual(got[i].Tiers, want[i].Tiers) {
			t.Fatalf("toReadSetEntries[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestResolveReadSetInfoDefaultScope pins the public
// ResolveReadSetInfo(ctx, ns, home) wrapper T6's read-set endpoint calls:
// default-scope resolution (no explicit/subtree override) mapped to
// []ReadSetEntry.
func TestResolveReadSetInfoDefaultScope(t *testing.T) {
	svc, _ := newReadsetSvc(t)
	got, err := svc.ResolveReadSetInfo(context.Background(), "acme/phoenix", "personal/kit")
	if err != nil {
		t.Fatalf("ResolveReadSetInfo: %v", err)
	}
	want := map[string]string{
		"acme/phoenix": OriginPrimary,
		"acme":         OriginAncestor,
		"personal/kit": OriginHome,
	}
	if len(got) != len(want) {
		t.Fatalf("ResolveReadSetInfo = %+v, want namespaces %v", got, want)
	}
	for _, e := range got {
		if want[e.NS] != e.Origin {
			t.Fatalf("ResolveReadSetInfo[%s].Origin = %q, want %q", e.NS, e.Origin, want[e.NS])
		}
	}
}
