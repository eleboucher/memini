package service

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eleboucher/memini/internal/embed/embedtest"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/store"
	"github.com/eleboucher/memini/internal/store/sqlitevec"
)

// putScopeMem upserts a memory directly into the store with a controlled
// tier, creation time, and tags, so scope-header counts and child-rollup
// ordering are deterministic (Remember would stamp the clock's single now on
// everything and run write-path gates the rollup doesn't care about).
func putScopeMem(t *testing.T, st interface {
	Upsert(ctx context.Context, m *memory.Memory) error
}, ns, id string, tier memory.Tier, created time.Time, tags ...string,
) {
	t.Helper()
	m := &memory.Memory{
		ID: id, Namespace: ns, Tier: tier, Content: "content of " + id,
		CreatedAt: created, UpdatedAt: created, LastAccessedAt: created,
		Tags: tags,
	}
	if err := st.Upsert(context.Background(), m); err != nil {
		t.Fatalf("upsert %s/%s: %v", ns, id, err)
	}
}

// TestBriefingScopeHeaderFullCascade pins the header format exactly:
// primary, then each contributing cascade leg nearest-first with its durable
// contribution count, home last among the arrow legs, and a "+K link" suffix
// counting links that contributed durable memories.
func TestBriefingScopeHeaderFullCascade(t *testing.T) {
	svc, st := newReadsetSvc(t)
	ctx := context.Background()
	base := time.Unix(1_700_000_000, 0).UTC()

	putScopeMem(t, st, "acme/phoenix/api", "api-1", memory.TierSemantic, base.Add(-1*time.Minute))
	for i := range 3 {
		putScopeMem(t, st, "acme/phoenix", fmt.Sprintf("phx-%d", i), memory.TierSemantic, base.Add(-time.Duration(i+1)*time.Minute))
	}
	for i := range 4 {
		putScopeMem(t, st, "acme", fmt.Sprintf("acme-%d", i), memory.TierSemantic, base.Add(-time.Duration(i+1)*time.Minute))
	}
	for i := range 2 {
		putScopeMem(t, st, "personal", fmt.Sprintf("home-%d", i), memory.TierSemantic, base.Add(-time.Duration(i+1)*time.Minute))
	}
	putScopeMem(t, st, "shared/golang", "go-1", memory.TierSemantic, base.Add(-1*time.Minute))
	putLink(t, st, "acme/phoenix/api", "shared/golang", nil)

	b, err := svc.Briefing(ctx, "acme/phoenix/api", BriefingOpts{Home: "personal"})
	if err != nil {
		t.Fatalf("briefing: %v", err)
	}
	want := "Scope: acme/phoenix/api ← acme/phoenix(3) ← acme(4) ← personal(2), +1 link"
	if b.ScopeHeader != want {
		t.Fatalf("scope header = %q, want %q", b.ScopeHeader, want)
	}
}

// TestBriefingScopeHeaderFlatNamespace: a flat namespace with no cascade legs
// renders just the primary — no arrows, no suffix.
func TestBriefingScopeHeaderFlatNamespace(t *testing.T) {
	svc, st := newReadsetSvc(t)
	ctx := context.Background()
	putScopeMem(t, st, "acme", "a-1", memory.TierSemantic, time.Unix(1_700_000_000, 0).UTC())

	b, err := svc.Briefing(ctx, "acme", BriefingOpts{})
	if err != nil {
		t.Fatalf("briefing: %v", err)
	}
	if b.ScopeHeader != "Scope: acme" {
		t.Fatalf("scope header = %q, want %q", b.ScopeHeader, "Scope: acme")
	}
}

// TestBriefingScopeHeaderOmitsZeroContributionLegs: a cascade leg that
// contributed no durable memories to THIS briefing is omitted from the header
// (only primary is always shown), and a link whose target contributed nothing
// durable adds no "+K link" suffix.
func TestBriefingScopeHeaderOmitsZeroContributionLegs(t *testing.T) {
	svc, st := newReadsetSvc(t)
	ctx := context.Background()
	base := time.Unix(1_700_000_000, 0).UTC()

	putScopeMem(t, st, "acme/phoenix", "phx-1", memory.TierSemantic, base.Add(-1*time.Minute))
	// The ancestor holds only episodic chatter — durable-only legs fetch none
	// of it, so the leg contributes 0 and is omitted.
	putScopeMem(t, st, "acme", "acme-ep", memory.TierEpisodic, base.Add(-1*time.Minute))
	// Same for the link target: episodic only, so no "+1 link".
	putScopeMem(t, st, "shared/golang", "go-ep", memory.TierEpisodic, base.Add(-1*time.Minute))
	putLink(t, st, "acme/phoenix", "shared/golang", nil)

	b, err := svc.Briefing(ctx, "acme/phoenix", BriefingOpts{})
	if err != nil {
		t.Fatalf("briefing: %v", err)
	}
	if b.ScopeHeader != "Scope: acme/phoenix" {
		t.Fatalf("scope header = %q, want %q", b.ScopeHeader, "Scope: acme/phoenix")
	}
}

// TestBriefingScopeHeaderPluralLinks: two contributing links render "+2 links"
// (pluralized), after any contributing arrow legs.
func TestBriefingScopeHeaderPluralLinks(t *testing.T) {
	svc, st := newReadsetSvc(t)
	ctx := context.Background()
	base := time.Unix(1_700_000_000, 0).UTC()

	putScopeMem(t, st, "acme", "a-1", memory.TierSemantic, base.Add(-1*time.Minute))
	putScopeMem(t, st, "shared/golang", "go-1", memory.TierSemantic, base.Add(-1*time.Minute))
	putScopeMem(t, st, "shared/tooling", "tool-1", memory.TierProcedural, base.Add(-1*time.Minute))
	putLink(t, st, "acme", "shared/golang", nil)
	putLink(t, st, "acme", "shared/tooling", nil)

	b, err := svc.Briefing(ctx, "acme", BriefingOpts{})
	if err != nil {
		t.Fatalf("briefing: %v", err)
	}
	if b.ScopeHeader != "Scope: acme, +2 links" {
		t.Fatalf("scope header = %q, want %q", b.ScopeHeader, "Scope: acme, +2 links")
	}
}

// TestBriefingChildRollupInteriorNode: at an interior node the briefing lists
// DIRECT children only (one segment deeper), each aggregating its entire
// subtree — total is the all-tier live count, pinned/recent are capped at 3,
// recent holds durable tiers only (newest first), and children are ordered by
// most-recent write.
func TestBriefingChildRollupInteriorNode(t *testing.T) {
	svc, st := newReadsetSvc(t)
	ctx := context.Background()
	base := time.Unix(1_700_000_000, 0).UTC()

	putScopeMem(t, st, "acme", "root-1", memory.TierSemantic, base.Add(-30*time.Minute))

	// acme/phoenix subtree: a pinned semantic + an episodic in the child
	// itself, plus a semantic in the grandchild — the grandchild must roll up
	// into the DIRECT child acme/phoenix, never appear as its own entry.
	putScopeMem(t, st, "acme/phoenix", "phx-pin", memory.TierSemantic, base.Add(-10*time.Minute), "pinned")
	putScopeMem(t, st, "acme/phoenix", "phx-ep", memory.TierEpisodic, base.Add(-5*time.Minute))
	putScopeMem(t, st, "acme/phoenix/api", "api-1", memory.TierSemantic, base.Add(-1*time.Minute))

	// acme/hydra: five semantics so the 3-per-section recent cap bites.
	for i := range 5 {
		putScopeMem(t, st, "acme/hydra", fmt.Sprintf("hyd-%d", i), memory.TierSemantic, base.Add(-time.Duration(i+2)*time.Minute))
	}

	b, err := svc.Briefing(ctx, "acme", BriefingOpts{})
	if err != nil {
		t.Fatalf("briefing: %v", err)
	}
	if len(b.Children) != 2 {
		t.Fatalf("children = %d entries (%+v), want 2 direct children", len(b.Children), b.Children)
	}
	// Ordered by most-recent write: phoenix's subtree last wrote at -1m
	// (grandchild), hydra at -2m.
	phx, hyd := b.Children[0], b.Children[1]
	if phx.NS != "acme/phoenix" || hyd.NS != "acme/hydra" {
		t.Fatalf("children order = [%s, %s], want [acme/phoenix, acme/hydra] (most-recent write first)", phx.NS, hyd.NS)
	}

	if phx.Total != 3 {
		t.Errorf("acme/phoenix total = %d, want 3 (all tiers, subtree-aggregated)", phx.Total)
	}
	if len(phx.Pinned) != 1 || phx.Pinned[0].ID != "phx-pin" {
		t.Errorf("acme/phoenix pinned = %+v, want [phx-pin]", idList(phx.Pinned))
	}
	if len(phx.Recent) != 2 || phx.Recent[0].ID != "api-1" || phx.Recent[1].ID != "phx-pin" {
		t.Errorf("acme/phoenix recent = %v, want [api-1 phx-pin] (durable only, newest first)", idList(phx.Recent))
	}

	if hyd.Total != 5 {
		t.Errorf("acme/hydra total = %d, want 5", hyd.Total)
	}
	if len(hyd.Recent) != 3 || hyd.Recent[0].ID != "hyd-0" || hyd.Recent[1].ID != "hyd-1" || hyd.Recent[2].ID != "hyd-2" {
		t.Errorf("acme/hydra recent = %v, want [hyd-0 hyd-1 hyd-2] (capped at 3, newest first)", idList(hyd.Recent))
	}
	if b.ChildrenTruncated != 0 {
		t.Errorf("children_truncated = %d, want 0", b.ChildrenTruncated)
	}
}

// TestBriefingChildRollupLeafEmpty: a leaf namespace has no children rollup,
// but the scope header is still present.
func TestBriefingChildRollupLeafEmpty(t *testing.T) {
	svc, st := newReadsetSvc(t)
	ctx := context.Background()
	putScopeMem(t, st, "acme/phoenix/api", "api-1", memory.TierSemantic, time.Unix(1_700_000_000, 0).UTC())

	b, err := svc.Briefing(ctx, "acme/phoenix/api", BriefingOpts{})
	if err != nil {
		t.Fatalf("briefing: %v", err)
	}
	if len(b.Children) != 0 {
		t.Fatalf("leaf briefing children = %+v, want none", b.Children)
	}
	if b.ChildrenTruncated != 0 {
		t.Fatalf("children_truncated = %d, want 0", b.ChildrenTruncated)
	}
	if !strings.HasPrefix(b.ScopeHeader, "Scope: acme/phoenix/api") {
		t.Fatalf("scope header = %q, want it present for a leaf too", b.ScopeHeader)
	}
}

// TestBriefingChildRollupCapsAtTen: more than 10 direct children keeps the 10
// with the most recent writes and reports the omitted count via
// ChildrenTruncated — a wide tenant root cannot balloon briefing size.
func TestBriefingChildRollupCapsAtTen(t *testing.T) {
	svc, st := newReadsetSvc(t)
	ctx := context.Background()
	base := time.Unix(1_700_000_000, 0).UTC()

	for i := range 12 {
		ns := fmt.Sprintf("acme/team%02d", i)
		putScopeMem(t, st, ns, fmt.Sprintf("t-%02d", i), memory.TierSemantic, base.Add(-time.Duration(i+1)*time.Minute))
	}
	putScopeMem(t, st, "acme", "root-1", memory.TierSemantic, base)

	b, err := svc.Briefing(ctx, "acme", BriefingOpts{})
	if err != nil {
		t.Fatalf("briefing: %v", err)
	}
	if len(b.Children) != 10 {
		t.Fatalf("children = %d entries, want 10 (capped)", len(b.Children))
	}
	if b.ChildrenTruncated != 2 {
		t.Fatalf("children_truncated = %d, want 2", b.ChildrenTruncated)
	}
	// team00 wrote most recently (-1m) … team09 (-10m); team10/team11 dropped.
	for i, c := range b.Children {
		want := fmt.Sprintf("acme/team%02d", i)
		if c.NS != want {
			t.Fatalf("children[%d] = %s, want %s (sorted by most-recent write)", i, c.NS, want)
		}
	}
}

// idList extracts memory IDs for readable assertion failures.
func idList(ms []*memory.Memory) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.ID
	}
	return out
}

// TestBriefingChildRollupSkippedForBareAndExplicit: a scope=project (bare)
// briefing and an explicit-Namespaces briefing render no child rollup, so
// they must not pay for one — Children stays empty while the default scope
// still gets the rollup.
func TestBriefingChildRollupSkippedForBareAndExplicit(t *testing.T) {
	svc, st := newReadsetSvc(t)
	ctx := context.Background()
	base := time.Unix(1_700_000_000, 0).UTC()
	putScopeMem(t, st, "acme", "root-1", memory.TierSemantic, base.Add(-2*time.Minute))
	putScopeMem(t, st, "acme/phoenix", "phx-1", memory.TierSemantic, base.Add(-1*time.Minute))

	bare, err := svc.Briefing(ctx, "acme", BriefingOpts{Scope: "project"})
	if err != nil {
		t.Fatalf("bare briefing: %v", err)
	}
	if len(bare.Children) != 0 || bare.ChildrenTruncated != 0 {
		t.Fatalf("scope=project briefing children = %+v (truncated %d), want none — no rollup work for a bare read",
			bare.Children, bare.ChildrenTruncated)
	}

	explicit, err := svc.Briefing(ctx, "acme", BriefingOpts{Namespaces: []string{"acme", "acme/phoenix"}})
	if err != nil {
		t.Fatalf("explicit briefing: %v", err)
	}
	if len(explicit.Children) != 0 || explicit.ChildrenTruncated != 0 {
		t.Fatalf("explicit-namespaces briefing children = %+v (truncated %d), want none",
			explicit.Children, explicit.ChildrenTruncated)
	}

	// Control: the default scope still rolls up.
	def, err := svc.Briefing(ctx, "acme", BriefingOpts{})
	if err != nil {
		t.Fatalf("default briefing: %v", err)
	}
	if len(def.Children) != 1 {
		t.Fatalf("default briefing children = %+v, want the acme/phoenix rollup", def.Children)
	}
}

// listCall records one store.List invocation for the bounded-cost assertions.
type listCall struct {
	ns    string
	limit int
}

// rollupRecordingStore wraps a Store, recording every List call (namespace +
// limit) and forwarding the optional ActivityStore capability of the wrapped
// driver, so tests can pin that the child rollup issues only bounded queries
// and only for the children it keeps.
type rollupRecordingStore struct {
	store.Store
	mu    sync.Mutex
	lists []listCall
}

func (r *rollupRecordingStore) List(ctx context.Context, ns string, f store.Filter, limit int) ([]*memory.Memory, error) {
	r.mu.Lock()
	r.lists = append(r.lists, listCall{ns: ns, limit: limit})
	r.mu.Unlock()
	return r.Store.List(ctx, ns, f, limit)
}

func (r *rollupRecordingStore) NamespaceActivity(ctx context.Context, now time.Time) ([]store.NamespaceActivity, error) {
	as, ok := r.Store.(store.ActivityStore)
	if !ok {
		return nil, errors.New("wrapped store does not implement store.ActivityStore")
	}
	return as.NamespaceActivity(ctx, now)
}

// TestBriefingChildRollupBoundedQueries: the 10-child cap must bound COST,
// not just output — at a wide interior node the rollup issues no unbounded
// (limit 0) List for any descendant namespace, and no List at all for the
// children dropped by the cap. Only the read-set's own section fetches (the
// primary here) may list unbounded.
func TestBriefingChildRollupBoundedQueries(t *testing.T) {
	st, err := sqlitevec.Open(context.Background(), filepath.Join(t.TempDir(), "bounded.db"), readsetTestDims)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	base := time.Unix(1_700_000_000, 0).UTC()

	putScopeMem(t, st, "acme", "root-1", memory.TierSemantic, base)
	// 12 direct children, team00 newest … team11 oldest, so team10/team11 are
	// dropped by the cap. team00 also has a grandchild namespace.
	for i := range 12 {
		ns := fmt.Sprintf("acme/team%02d", i)
		putScopeMem(t, st, ns, fmt.Sprintf("t-%02d", i), memory.TierSemantic, base.Add(-time.Duration(i+1)*time.Minute))
	}
	putScopeMem(t, st, "acme/team00/api", "gc-1", memory.TierSemantic, base.Add(-30*time.Second))

	rec := &rollupRecordingStore{Store: st}
	svc := New(rec, embedtest.New(readsetTestDims),
		WithClock(func() time.Time { return base }))

	b, err := svc.Briefing(context.Background(), "acme", BriefingOpts{})
	if err != nil {
		t.Fatalf("briefing: %v", err)
	}
	if len(b.Children) != 10 || b.ChildrenTruncated != 2 {
		t.Fatalf("children = %d (truncated %d), want 10 (truncated 2)", len(b.Children), b.ChildrenTruncated)
	}

	kept := map[string]bool{}
	for _, c := range b.Children {
		kept[c.NS] = true
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for _, call := range rec.lists {
		if !strings.HasPrefix(call.ns, "acme/") {
			continue // the read-set's own section fetch (primary) may be unbounded
		}
		if call.limit <= 0 {
			t.Errorf("unbounded List(%q, limit=%d) for a descendant namespace — rollup queries must be capped", call.ns, call.limit)
		}
		// The namespace must belong to a KEPT child's subtree: a child dropped
		// by the cap must cost nothing beyond the aggregate row.
		child := call.ns
		if i := strings.IndexByte(child[len("acme/"):], '/'); i >= 0 {
			child = child[:len("acme/")+i]
		}
		if !kept[child] {
			t.Errorf("List(%q) queried a namespace under dropped child %q — capped-out children must not be fetched", call.ns, child)
		}
	}
}

// TestBriefingChildRollupDegradesWithoutActivityStore: against a store that
// predates ActivityStore (countingListStore embeds the Store interface, so
// the capability is not promoted), the briefing degrades to no rollup —
// never to the old unbounded per-namespace scan path.
func TestBriefingChildRollupDegradesWithoutActivityStore(t *testing.T) {
	st, err := sqlitevec.Open(context.Background(), filepath.Join(t.TempDir(), "degrade.db"), readsetTestDims)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	base := time.Unix(1_700_000_000, 0).UTC()
	putScopeMem(t, st, "acme", "root-1", memory.TierSemantic, base.Add(-2*time.Minute))
	putScopeMem(t, st, "acme/phoenix", "phx-1", memory.TierSemantic, base.Add(-1*time.Minute))

	svc := New(&countingListStore{Store: st}, embedtest.New(readsetTestDims),
		WithClock(func() time.Time { return base }))
	b, err := svc.Briefing(context.Background(), "acme", BriefingOpts{})
	if err != nil {
		t.Fatalf("briefing: %v", err)
	}
	if len(b.Children) != 0 || b.ChildrenTruncated != 0 {
		t.Fatalf("children = %+v (truncated %d), want none — no unbounded fallback without ActivityStore",
			b.Children, b.ChildrenTruncated)
	}
	if !strings.HasPrefix(b.ScopeHeader, "Scope: acme") {
		t.Fatalf("scope header = %q, want it still present when the rollup degrades", b.ScopeHeader)
	}
}
