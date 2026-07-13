package maintenance_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eleboucher/memini/internal/maintenance"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/store"
	"github.com/eleboucher/memini/internal/store/sqlitevec"
)

// seedPooled writes memories into one namespace, each tagged with the source
// namespace a botched --merge-into import would have preserved in metadata.
func seedPooled(t *testing.T, st store.Store, ns string, srcByID map[string]string) {
	t.Helper()
	now := time.Now().UTC()
	i := 0
	for id, src := range srcByID {
		m := &memory.Memory{
			ID: id, Namespace: ns, Tier: memory.TierEpisodic, Content: "memory " + id,
			CreatedAt: now, UpdatedAt: now, LastAccessedAt: now,
			Embedding: []float32{float32(i + 1), 0, 0, 0},
		}
		if src != "" {
			m.Metadata = map[string]any{"import_source_namespace": src}
		}
		if err := st.Upsert(context.Background(), m); err != nil {
			t.Fatalf("upsert %s: %v", id, err)
		}
		i++
	}
}

func TestSplitRecoversPooledNamespaces(t *testing.T) {
	ctx := context.Background()
	st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "m.db"), 4)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	seedPooled(t, st, "pool", map[string]string{
		"a1": "alice", "a2": "alice", "b1": "bob", "orphan": "",
	})

	// Dry-run reports the grouping without moving anything.
	dry, err := maintenance.Split(ctx, st, "pool", nil, true)
	if err != nil {
		t.Fatalf("split dry-run: %v", err)
	}
	if dry.Moved != 3 || dry.Skipped != 1 || dry.Targets["alice"] != 2 || dry.Targets["bob"] != 1 {
		t.Fatalf("dry-run report = %+v, want alice=2 bob=1 moved=3 skipped=1", dry)
	}
	if _, err := st.Get(ctx, "alice", "a1"); err == nil {
		t.Fatal("dry-run must not move anything")
	}

	// Apply, then assert isolation: each tenant's memories live in their own ns.
	rep, err := maintenance.Split(ctx, st, "pool", nil, false)
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	if rep.Moved != 3 || rep.Skipped != 1 {
		t.Fatalf("split report = %+v, want moved=3 skipped=1", rep)
	}
	for ns, want := range map[string]int{"alice": 2, "bob": 1, "pool": 1} {
		mems, err := st.List(ctx, ns, store.Filter{IncludeSuperseded: true, IncludeExpired: true}, 0)
		if err != nil {
			t.Fatalf("list %s: %v", ns, err)
		}
		if len(mems) != want {
			t.Errorf("namespace %q has %d memories, want %d", ns, len(mems), want)
		}
	}
	// The orphan (no grouping key) stayed in the pool.
	if _, err := st.Get(ctx, "pool", "orphan"); err != nil {
		t.Errorf("orphan should remain in pool: %v", err)
	}
}

func TestMoveRelocatesNamespace(t *testing.T) {
	ctx := context.Background()
	st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "m.db"), 4)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	seedPooled(t, st, "old", map[string]string{"x": "", "y": ""})

	rep, err := maintenance.Move(ctx, st, "old", "new", false)
	if err != nil {
		t.Fatalf("move: %v", err)
	}
	if rep.Moved != 2 {
		t.Fatalf("move report = %+v, want moved=2", rep)
	}
	mems, err := st.List(ctx, "new", store.Filter{IncludeSuperseded: true, IncludeExpired: true}, 0)
	if err != nil {
		t.Fatalf("list new: %v", err)
	}
	if len(mems) != 2 {
		t.Errorf("new namespace has %d memories, want 2", len(mems))
	}
}

// TestMoveRenamesLinkEndpoints verifies gap G5: Move rewrites namespace_links
// rows on both sides of the moved namespace, since Reassign only relocates
// memories (links are keyed by namespace, not memory ID).
func TestMoveRenamesLinkEndpoints(t *testing.T) {
	ctx := context.Background()
	st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "m.db"), 4)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	seedPooled(t, st, "old", map[string]string{"x": ""})

	// sqlitevec.Store implements store.LinkStore directly (asserted by var _
	// store.LinkStore = (*Store)(nil) in the package), so its concrete methods
	// are callable without a type assertion here.
	now := time.Now().UTC()
	if err := st.PutLink(ctx, store.NamespaceLink{Src: "old", Dst: "other", CreatedAt: now}); err != nil {
		t.Fatalf("put link (old as src): %v", err)
	}
	if err := st.PutLink(ctx, store.NamespaceLink{Src: "other", Dst: "old", CreatedAt: now}); err != nil {
		t.Fatalf("put link (old as dst): %v", err)
	}

	if _, err := maintenance.Move(ctx, st, "old", "new", false); err != nil {
		t.Fatalf("move: %v", err)
	}

	newLinks, err := st.ListLinks(ctx, "new")
	if err != nil {
		t.Fatalf("list links (new): %v", err)
	}
	if len(newLinks) != 1 || newLinks[0].Dst != "other" {
		t.Fatalf("move did not rewrite the src side of the link: %+v", newLinks)
	}
	otherLinks, err := st.ListLinks(ctx, "other")
	if err != nil {
		t.Fatalf("list links (other): %v", err)
	}
	if len(otherLinks) != 1 || otherLinks[0].Dst != "new" {
		t.Fatalf("move did not rewrite the dst side of the link: %+v", otherLinks)
	}
	oldLinks, err := st.ListLinks(ctx, "old")
	if err != nil {
		t.Fatalf("list links (old, after move): %v", err)
	}
	if len(oldLinks) != 0 {
		t.Fatalf("old namespace still has links after move: %v", oldLinks)
	}
}

// failRenameStore wraps a real store but fails RenameLinkEndpoints, to test
// Move's reporting when the link rename errors after Reassign has committed.
// The embedded interfaces supply every other Store/LinkStore method.
type failRenameStore struct {
	store.Store
	store.LinkStore
}

func (f *failRenameStore) RenameLinkEndpoints(context.Context, string, string) error {
	return errors.New("simulated link-rename failure")
}

// TestMoveReportsMovedOnLinkRenameFailure pins Move's partial-failure
// reporting: Reassign has already committed when RenameLinkEndpoints runs, so
// a link-rename error must surface alongside the true moved count — not a
// misleading zero indistinguishable from "nothing moved".
func TestMoveReportsMovedOnLinkRenameFailure(t *testing.T) {
	ctx := context.Background()
	st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "m.db"), 4)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	seedPooled(t, st, "old", map[string]string{"x": "", "y": ""})

	frs := &failRenameStore{Store: st, LinkStore: st}
	rep, err := maintenance.Move(ctx, frs, "old", "new", false)
	if err == nil {
		t.Fatal("move with a failing link rename should return the error")
	}
	if rep.Moved != 2 || rep.Targets["new"] != 2 {
		t.Fatalf("report = %+v, want Moved=2 Targets[new]=2 despite the link-rename error", rep)
	}
	// The memories really did move (Reassign committed before the failure).
	mems, err := st.List(ctx, "new", store.Filter{IncludeSuperseded: true, IncludeExpired: true}, 0)
	if err != nil {
		t.Fatalf("list new: %v", err)
	}
	if len(mems) != 2 {
		t.Fatalf("new namespace has %d memories, want 2", len(mems))
	}
}

// TestMoveRenamesAPIKeyNamespaces verifies the K2 companion to
// TestMoveRenamesLinkEndpoints: Move rewrites api_keys rows whose home_ns or
// default_ns matches the moved namespace, since Reassign only relocates
// memories and RenameAPIKeyNamespaces is keyed by namespace, not memory ID.
func TestMoveRenamesAPIKeyNamespaces(t *testing.T) {
	ctx := context.Background()
	st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "m.db"), 4)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	seedPooled(t, st, "old", map[string]string{"x": ""})

	var ks store.APIKeyStore = st
	now := time.Now().UTC()
	if err := ks.PutAPIKey(ctx, store.APIKey{Name: "home-bot", Hash: "h1", HomeNS: "old", CreatedAt: now}); err != nil {
		t.Fatalf("put home key: %v", err)
	}
	if err := ks.PutAPIKey(ctx, store.APIKey{Name: "default-bot", Hash: "h2", DefaultNS: "old", CreatedAt: now}); err != nil {
		t.Fatalf("put default key: %v", err)
	}
	if err := ks.PutAPIKey(ctx, store.APIKey{Name: "unrelated-bot", Hash: "h3", HomeNS: "other", CreatedAt: now}); err != nil {
		t.Fatalf("put unrelated key: %v", err)
	}

	if _, err := maintenance.Move(ctx, st, "old", "new", false); err != nil {
		t.Fatalf("move: %v", err)
	}

	all, err := ks.ListAPIKeys(ctx)
	if err != nil {
		t.Fatalf("list api keys: %v", err)
	}
	byName := map[string]store.APIKey{}
	for _, k := range all {
		byName[k.Name] = k
	}
	if got := byName["home-bot"].HomeNS; got != "new" {
		t.Errorf("home-bot.HomeNS = %q, want new", got)
	}
	if got := byName["default-bot"].DefaultNS; got != "new" {
		t.Errorf("default-bot.DefaultNS = %q, want new", got)
	}
	if got := byName["unrelated-bot"].HomeNS; got != "other" {
		t.Errorf("unrelated-bot.HomeNS = %q, want untouched other", got)
	}
}

// failAPIKeyRenameStore wraps a real store but fails RenameAPIKeyNamespaces,
// mirroring failRenameStore's link-rename failure test above.
type failAPIKeyRenameStore struct {
	store.Store
	store.APIKeyStore
}

func (f *failAPIKeyRenameStore) RenameAPIKeyNamespaces(context.Context, string, string) error {
	return errors.New("simulated api key rename failure")
}

// TestMoveReportsMovedOnAPIKeyRenameFailure mirrors
// TestMoveReportsMovedOnLinkRenameFailure: a RenameAPIKeyNamespaces error
// must surface alongside the true moved count, not a misleading zero.
func TestMoveReportsMovedOnAPIKeyRenameFailure(t *testing.T) {
	ctx := context.Background()
	st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "m.db"), 4)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	seedPooled(t, st, "old", map[string]string{"x": "", "y": ""})

	var ks store.APIKeyStore = st
	frs := &failAPIKeyRenameStore{Store: st, APIKeyStore: ks}
	rep, err := maintenance.Move(ctx, frs, "old", "new", false)
	if err == nil {
		t.Fatal("move with a failing api key rename should return the error")
	}
	if rep.Moved != 2 || rep.Targets["new"] != 2 {
		t.Fatalf("report = %+v, want Moved=2 Targets[new]=2 despite the api key rename error", rep)
	}
	mems, err := st.List(ctx, "new", store.Filter{IncludeSuperseded: true, IncludeExpired: true}, 0)
	if err != nil {
		t.Fatalf("list new: %v", err)
	}
	if len(mems) != 2 {
		t.Fatalf("new namespace has %d memories, want 2", len(mems))
	}
}

// TestMoveRenamesProjectMapNamespaces is the config-handshake companion to
// TestMoveRenamesAPIKeyNamespaces: Move rewrites project_map pins whose
// namespace matches the moved namespace (exactly — a namespace that merely
// starts with fromNS is untouched), since a pin is keyed by project identity,
// not memory ID, so Reassign never relocates it. Without this, a handshake for
// the moved project would keep resolving to the now-empty old namespace.
func TestMoveRenamesProjectMapNamespaces(t *testing.T) {
	ctx := context.Background()
	st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "m.db"), 4)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	seedPooled(t, st, "old", map[string]string{"x": ""})

	var pms store.ProjectMapStore = st
	if err := pms.PutProjectMapEntries(ctx, []store.ProjectMapEntry{
		{Key: "remote:github.com/acme/app", Namespace: "old"},
		{Key: "path:/srv/app", Namespace: "old"},
		{Key: "remote:github.com/acme/other", Namespace: "other"},
		{Key: "remote:github.com/acme/prefix", Namespace: "oldish"}, // starts with "old" but isn't it
	}); err != nil {
		t.Fatalf("put project map entries: %v", err)
	}

	if _, err := maintenance.Move(ctx, st, "old", "new", false); err != nil {
		t.Fatalf("move: %v", err)
	}

	all, err := pms.ListProjectMapEntries(ctx)
	if err != nil {
		t.Fatalf("list project map entries: %v", err)
	}
	byKey := map[string]store.ProjectMapEntry{}
	for _, e := range all {
		byKey[e.Key] = e
	}
	if got := byKey["remote:github.com/acme/app"].Namespace; got != "new" {
		t.Errorf("remote pin namespace = %q, want new", got)
	}
	if got := byKey["path:/srv/app"].Namespace; got != "new" {
		t.Errorf("path pin namespace = %q, want new", got)
	}
	if got := byKey["remote:github.com/acme/other"].Namespace; got != "other" {
		t.Errorf("unrelated pin namespace = %q, want untouched other", got)
	}
	if got := byKey["remote:github.com/acme/prefix"].Namespace; got != "oldish" {
		t.Errorf("prefix-only pin namespace = %q, want untouched oldish (exact match only)", got)
	}
}

// failProjectMapRenameStore wraps a real store but fails
// RenameProjectMapNamespaces, mirroring failAPIKeyRenameStore above.
type failProjectMapRenameStore struct {
	store.Store
	store.ProjectMapStore
}

func (f *failProjectMapRenameStore) RenameProjectMapNamespaces(context.Context, string, string) error {
	return errors.New("simulated project map rename failure")
}

// TestMoveReportsMovedOnProjectMapRenameFailure mirrors the link/api-key
// partial-failure tests: a RenameProjectMapNamespaces error must surface
// alongside the true moved count, since Reassign has already committed.
func TestMoveReportsMovedOnProjectMapRenameFailure(t *testing.T) {
	ctx := context.Background()
	st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "m.db"), 4)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	seedPooled(t, st, "old", map[string]string{"x": "", "y": ""})

	var pms store.ProjectMapStore = st
	frs := &failProjectMapRenameStore{Store: st, ProjectMapStore: pms}
	rep, err := maintenance.Move(ctx, frs, "old", "new", false)
	if err == nil {
		t.Fatal("move with a failing project map rename should return the error")
	}
	if rep.Moved != 2 || rep.Targets["new"] != 2 {
		t.Fatalf("report = %+v, want Moved=2 Targets[new]=2 despite the project map rename error", rep)
	}
	mems, err := st.List(ctx, "new", store.Filter{IncludeSuperseded: true, IncludeExpired: true}, 0)
	if err != nil {
		t.Fatalf("list new: %v", err)
	}
	if len(mems) != 2 {
		t.Fatalf("new namespace has %d memories, want 2", len(mems))
	}
}

func TestSplitSkipsInvalidTargets(t *testing.T) {
	ctx := context.Background()
	st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "m.db"), 4)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	// Targets come from stored metadata, so hostile or malformed values must
	// stay put instead of minting unaddressable namespaces. The slash-wrapped
	// value is valid after normalization and lands in "alice".
	seedPooled(t, st, "pool", map[string]string{
		"ok":      " alice/ ",
		"toolong": strings.Repeat("x", 300),
		"nulbyte": "bad\x00ns",
		"pattern": "work/*",
		"slashes": "///",
	})

	rep, err := maintenance.Split(ctx, st, "pool", nil, false)
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	if rep.Moved != 1 || rep.Skipped != 4 {
		t.Fatalf("split report = %+v, want moved=1 skipped=4", rep)
	}
	if _, err := st.Get(ctx, "alice", "ok"); err != nil {
		t.Errorf("normalized target should land in alice: %v", err)
	}
}
