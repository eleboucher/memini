package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/eleboucher/memini/internal/config"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/store"
	"github.com/eleboucher/memini/internal/store/sqlitevec"
)

// seedPool writes n memories into namespace ns; attributable of them carry an
// import_source_namespace metadata key (so a split can re-home them).
func seedPool(t *testing.T, st store.Store, ns string, n, attributable int) {
	t.Helper()
	now := time.Now().UTC()
	for i := range n {
		m := &memory.Memory{
			ID: ns + "/" + strconv.Itoa(i), Namespace: ns, Tier: memory.TierEpisodic,
			Content: "memory " + strconv.Itoa(i), CreatedAt: now, UpdatedAt: now, LastAccessedAt: now,
			Embedding: []float32{1, 0, 0, 0},
		}
		if i < attributable {
			m.Metadata = map[string]any{"import_source_namespace": "tenant-" + strconv.Itoa(i%3)}
		}
		if err := st.Upsert(context.Background(), m); err != nil {
			t.Fatalf("upsert: %v", err)
		}
	}
}

func openTestStore(t *testing.T) store.Store {
	t.Helper()
	st, err := sqlitevec.Open(context.Background(), filepath.Join(t.TempDir(), "doctor.db"), 4)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func statsFor(t *testing.T, st store.Store) []nsStat {
	t.Helper()
	s, err := namespaceStats(context.Background(), st)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	return s
}

// TestDoctorFixPreviewWritesNothing: a preview (apply=false) must not mutate the
// store even when the pool is splittable.
func TestDoctorFixPreviewWritesNothing(t *testing.T) {
	st := openTestStore(t)
	seedPool(t, st, nsDefault, 600, 600) // fully attributable, over threshold
	var out bytes.Buffer

	if err := doctorFix(context.Background(), &out, statsFor(t, st), fixDeps{store: st, now: time.Now().UTC(), apply: false}); err != nil {
		t.Fatalf("doctorFix preview: %v", err)
	}
	names, _ := st.ListNamespaces(context.Background())
	if len(names) != 1 || names[0] != nsDefault {
		t.Fatalf("preview must not move anything; namespaces = %v", names)
	}
	if !strings.Contains(out.String(), "preview") {
		t.Errorf("expected a preview banner, got: %s", out.String())
	}
}

// TestDoctorFixSplitsAttributablePool: with >=90% attributable and apply=true,
// the pool is split into per-tenant namespaces.
func TestDoctorFixSplitsAttributablePool(t *testing.T) {
	st := openTestStore(t)
	seedPool(t, st, nsDefault, 600, 600)
	var out bytes.Buffer

	if err := doctorFix(context.Background(), &out, statsFor(t, st), fixDeps{store: st, now: time.Now().UTC(), apply: true}); err != nil {
		t.Fatalf("doctorFix apply: %v", err)
	}
	names, _ := st.ListNamespaces(context.Background())
	// 3 tenant namespaces (tenant-0/1/2); the default pool should be drained.
	for _, n := range names {
		if n == nsDefault {
			mems, _ := st.List(context.Background(), nsDefault, store.Filter{IncludeSuperseded: true, IncludeExpired: true}, 0)
			if len(mems) != 0 {
				t.Errorf("default should be drained, still has %d", len(mems))
			}
		}
	}
	if !strings.Contains(out.String(), "split") {
		t.Errorf("expected a split line, got: %s", out.String())
	}
}

// TestDoctorFixSkipsUnattributablePool: below the 90% attribution bar, the split
// is skipped and the pool is left intact (manual inspection advised).
func TestDoctorFixSkipsUnattributablePool(t *testing.T) {
	st := openTestStore(t)
	seedPool(t, st, nsDefault, 600, 100) // only ~17% attributable
	var out bytes.Buffer

	if err := doctorFix(context.Background(), &out, statsFor(t, st), fixDeps{store: st, now: time.Now().UTC(), apply: true}); err != nil {
		t.Fatalf("doctorFix: %v", err)
	}
	mems, _ := st.List(context.Background(), nsDefault, store.Filter{IncludeSuperseded: true, IncludeExpired: true}, 0)
	if len(mems) != 600 {
		t.Errorf("unattributable pool must be left intact, got %d of 600", len(mems))
	}
	if !strings.Contains(out.String(), "skip split") {
		t.Errorf("expected a skip-split line, got: %s", out.String())
	}
}

// TestDoctorFixBackfillsLegacyConfidence: the fix chain seeds nil confidence on
// durable memories written before 0.0.11, so they enter the demote lifecycle.
func TestDoctorFixBackfillsLegacyConfidence(t *testing.T) {
	st := openTestStore(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	m := &memory.Memory{
		ID: "legacy", Namespace: "nsDefault", Tier: memory.TierSemantic, Content: "x",
		CreatedAt: now, UpdatedAt: now, LastAccessedAt: now,
		Embedding: []float32{1, 0, 0, 0},
	}
	if err := st.Upsert(context.Background(), m); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	var out bytes.Buffer
	if err := doctorFix(context.Background(), &out, statsFor(t, st), fixDeps{store: st, now: now, apply: true}); err != nil {
		t.Fatalf("doctorFix: %v", err)
	}
	got, err := st.Get(context.Background(), "nsDefault", "legacy")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Confidence == nil || *got.Confidence != memory.ConfidenceSeedImported {
		t.Errorf("legacy durable memory should be backfilled, got confidence=%v", got.Confidence)
	}
	if !strings.Contains(out.String(), "backfilled") {
		t.Errorf("expected a backfilled line, got: %s", out.String())
	}
}

func TestPrintWritePathSignals(t *testing.T) {
	var buf strings.Builder
	printWritePathSignals(&buf, []nsStat{
		{namespace: "a", classified: 2, promoted: 1, corroborated: 3},
		{namespace: "b", classified: 1},
	})
	out := buf.String()
	for _, want := range []string{"marker-classified durable writes:  3", "promotion-produced facts:          1", "corroborated durable memories:     3"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}

	buf.Reset()
	printWritePathSignals(&buf, []nsStat{{namespace: "a"}})
	if buf.Len() != 0 {
		t.Fatalf("all-zero signals should print nothing, got:\n%s", buf.String())
	}
}

// TestWarnEnvSlashMigration: an env-sourced default namespace containing "/"
// whose pre-fix, basename-flattened namespace still holds memories triggers
// a warning naming the remediation command; a fresh namespace (no memories
// under the old basename) or a non-slash / non-env-sourced namespace stays
// silent.
func TestWarnEnvSlashMigration(t *testing.T) {
	t.Run("warns when the flattened basename holds memories", func(t *testing.T) {
		st := openTestStore(t)
		seedPool(t, st, "project", 3, 0) // pre-fix flattened basename
		stats := statsFor(t, st)
		cfg := &config.Config{DefaultNamespace: "team/project", NamespaceSrc: config.NamespaceFromEnv}
		var out bytes.Buffer
		if n := warnEnvSlashMigration(&out, cfg, stats); n != 1 {
			t.Fatalf("warnings = %d, want 1", n)
		}
		got := out.String()
		if !strings.Contains(got, `"team/project"`) || !strings.Contains(got, `"project"`) {
			t.Errorf("expected both namespaces named in the warning, got:\n%s", got)
		}
		if !strings.Contains(got, "memini namespace move --from project --to team/project") {
			t.Errorf("expected the remediation command, got:\n%s", got)
		}
	})

	t.Run("silent when the basename has no memories", func(t *testing.T) {
		st := openTestStore(t)
		seedPool(t, st, "team/project", 1, 0) // already fully migrated
		stats := statsFor(t, st)
		cfg := &config.Config{DefaultNamespace: "team/project", NamespaceSrc: config.NamespaceFromEnv}
		var out bytes.Buffer
		if n := warnEnvSlashMigration(&out, cfg, stats); n != 0 {
			t.Fatalf("warnings = %d, want 0, output:\n%s", n, out.String())
		}
	})

	t.Run("silent when the namespace has no slash", func(t *testing.T) {
		st := openTestStore(t)
		seedPool(t, st, "project", 1, 0)
		stats := statsFor(t, st)
		cfg := &config.Config{DefaultNamespace: "project", NamespaceSrc: config.NamespaceFromEnv}
		var out bytes.Buffer
		if n := warnEnvSlashMigration(&out, cfg, stats); n != 0 {
			t.Fatalf("warnings = %d, want 0, output:\n%s", n, out.String())
		}
	})

	t.Run("silent when the namespace source is not env", func(t *testing.T) {
		st := openTestStore(t)
		seedPool(t, st, "project", 1, 0)
		stats := statsFor(t, st)
		cfg := &config.Config{DefaultNamespace: "team/project", NamespaceSrc: config.NamespaceFromGit}
		var out bytes.Buffer
		if n := warnEnvSlashMigration(&out, cfg, stats); n != 0 {
			t.Fatalf("warnings = %d, want 0, output:\n%s", n, out.String())
		}
	})
}

func TestStatsTotal(t *testing.T) {
	stats := []nsStat{{namespace: "a", total: 3}, {namespace: "b", total: 0}}
	if total, ok := statsTotal(stats, "a"); total != 3 || !ok {
		t.Errorf("statsTotal(a) = %d, %v; want 3, true", total, ok)
	}
	if total, ok := statsTotal(stats, "b"); total != 0 || !ok {
		t.Errorf("statsTotal(b) = %d, %v; want 0, true", total, ok)
	}
	if total, ok := statsTotal(stats, "missing"); total != 0 || ok {
		t.Errorf("statsTotal(missing) = %d, %v; want 0, false", total, ok)
	}
}

// --- read-set: local mirror (ancestor/home/link cascade) ---

func TestAncestorsOfFlatNamespace(t *testing.T) {
	if got := ancestorsOf("acme"); got != nil {
		t.Fatalf("ancestorsOf(flat) = %v, want nil", got)
	}
}

func TestAncestorsOfNested(t *testing.T) {
	got := ancestorsOf("acme/phoenix/api")
	want := []string{"acme/phoenix", "acme"}
	if len(got) != len(want) {
		t.Fatalf("ancestorsOf = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("ancestorsOf[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestLocalReadSetPrimaryOnly(t *testing.T) {
	st := openTestStore(t)
	entries, err := localReadSet(context.Background(), st, "acme", "")
	if err != nil {
		t.Fatalf("localReadSet: %v", err)
	}
	want := doctorReadEntry{ns: "acme", origin: originPrimary, tiers: tiersAll}
	if len(entries) != 1 || entries[0] != want {
		t.Fatalf("entries = %+v, want [%+v]", entries, want)
	}
}

func TestLocalReadSetAncestorsNearestFirst(t *testing.T) {
	st := openTestStore(t)
	entries, err := localReadSet(context.Background(), st, "acme/phoenix/api", "")
	if err != nil {
		t.Fatalf("localReadSet: %v", err)
	}
	want := []doctorReadEntry{
		{ns: "acme/phoenix/api", origin: originPrimary, tiers: tiersAll},
		{ns: "acme/phoenix", origin: originAncestor, tiers: "semantic,procedural"},
		{ns: "acme", origin: originAncestor, tiers: "semantic,procedural"},
	}
	if len(entries) != len(want) {
		t.Fatalf("entries = %+v, want %+v", entries, want)
	}
	for i := range want {
		if entries[i] != want[i] {
			t.Errorf("entries[%d] = %+v, want %+v", i, entries[i], want[i])
		}
	}
}

func TestLocalReadSetHomeLeg(t *testing.T) {
	st := openTestStore(t)
	entries, err := localReadSet(context.Background(), st, "acme/phoenix", "personal/kit")
	if err != nil {
		t.Fatalf("localReadSet: %v", err)
	}
	want := []doctorReadEntry{
		{ns: "acme/phoenix", origin: originPrimary, tiers: tiersAll},
		{ns: "acme", origin: originAncestor, tiers: "semantic,procedural"},
		{ns: "personal/kit", origin: originHome, tiers: "semantic,procedural"},
	}
	if len(entries) != len(want) {
		t.Fatalf("entries = %+v, want %+v", entries, want)
	}
	for i := range want {
		if entries[i] != want[i] {
			t.Errorf("entries[%d] = %+v, want %+v", i, entries[i], want[i])
		}
	}
}

// TestLocalReadSetDedupesAncestorAlsoHome: when the home namespace coincides
// with an already-present ancestor, it must not appear twice — the leg that
// added it first (ancestor, cascade order) keeps the entry.
func TestLocalReadSetDedupesAncestorAlsoHome(t *testing.T) {
	st := openTestStore(t)
	entries, err := localReadSet(context.Background(), st, "acme/phoenix", "acme")
	if err != nil {
		t.Fatalf("localReadSet: %v", err)
	}
	count := 0
	for _, e := range entries {
		if e.ns == "acme" {
			count++
			if e.origin != originAncestor {
				t.Errorf("acme origin = %q, want %q (first leg wins)", e.origin, originAncestor)
			}
		}
	}
	if count != 1 {
		t.Errorf("acme should appear exactly once, got %d in %+v", count, entries)
	}
}

func TestLocalReadSetLinksLeg(t *testing.T) {
	st := openTestStore(t)
	ls, err := linkStoreOf(st)
	if err != nil {
		t.Fatalf("linkStoreOf: %v", err)
	}
	if err := ls.PutLink(context.Background(), store.NamespaceLink{Src: "acme", Dst: "shared/golang", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("PutLink: %v", err)
	}
	entries, err := localReadSet(context.Background(), st, "acme", "")
	if err != nil {
		t.Fatalf("localReadSet: %v", err)
	}
	want := []doctorReadEntry{
		{ns: "acme", origin: originPrimary, tiers: tiersAll},
		{ns: "shared/golang", origin: originLink, tiers: "semantic,procedural"},
	}
	if len(entries) != len(want) {
		t.Fatalf("entries = %+v, want %+v", entries, want)
	}
	for i := range want {
		if entries[i] != want[i] {
			t.Errorf("entries[%d] = %+v, want %+v", i, entries[i], want[i])
		}
	}
}

func TestLocalReadSetLinkTiersNarrowed(t *testing.T) {
	st := openTestStore(t)
	ls, err := linkStoreOf(st)
	if err != nil {
		t.Fatalf("linkStoreOf: %v", err)
	}
	link := store.NamespaceLink{Src: "acme", Dst: "shared/golang", Tiers: []memory.Tier{memory.TierSemantic}, CreatedAt: time.Now().UTC()}
	if err := ls.PutLink(context.Background(), link); err != nil {
		t.Fatalf("PutLink: %v", err)
	}
	entries, err := localReadSet(context.Background(), st, "acme", "")
	if err != nil {
		t.Fatalf("localReadSet: %v", err)
	}
	found := false
	for _, e := range entries {
		if e.ns == "shared/golang" {
			found = true
			if e.tiers != "semantic" {
				t.Errorf("shared/golang tiers = %q, want %q", e.tiers, "semantic")
			}
		}
	}
	if !found {
		t.Fatalf("expected shared/golang in entries, got %+v", entries)
	}
}

func TestLocalReadSetDegradesWithoutLinkStore(t *testing.T) {
	entries, err := localReadSet(context.Background(), noLinkStore{}, "acme", "")
	if err != nil {
		t.Fatalf("localReadSet: %v", err)
	}
	if len(entries) != 1 || entries[0].ns != "acme" {
		t.Fatalf("expected only the primary entry, got %+v", entries)
	}
}

// --- read-set: server preference + fallback ---

func TestFetchServerReadSetSendsHeaders(t *testing.T) {
	var gotNS, gotHome, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotNS = r.Header.Get("X-Memini-Namespace")
		gotHome = r.Header.Get("X-Memini-Home")
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"entries":[]}`))
	}))
	defer srv.Close()

	if _, err := fetchServerReadSet(context.Background(), srv.URL, "secret-token", "acme/phoenix", "personal/kit"); err != nil {
		t.Fatalf("fetchServerReadSet: %v", err)
	}
	if gotNS != "acme/phoenix" {
		t.Errorf("X-Memini-Namespace = %q, want acme/phoenix", gotNS)
	}
	if gotHome != "personal/kit" {
		t.Errorf("X-Memini-Home = %q, want personal/kit", gotHome)
	}
	if gotAuth != "Bearer secret-token" {
		t.Errorf("Authorization = %q, want Bearer secret-token", gotAuth)
	}
}

func TestFetchServerReadSetParsesEntries(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"entries":[
			{"namespace":"acme/phoenix","origin":"primary"},
			{"namespace":"acme","origin":"ancestor","tiers":["semantic","procedural"]},
			{"namespace":"shared/golang","origin":"link","tiers":["semantic"]}
		]}`))
	}))
	defer srv.Close()

	entries, err := fetchServerReadSet(context.Background(), srv.URL, "", "acme/phoenix", "")
	if err != nil {
		t.Fatalf("fetchServerReadSet: %v", err)
	}
	want := []doctorReadEntry{
		{ns: "acme/phoenix", origin: originPrimary, tiers: tiersAll},
		{ns: "acme", origin: originAncestor, tiers: "semantic,procedural"},
		{ns: "shared/golang", origin: originLink, tiers: "semantic"},
	}
	if len(entries) != len(want) {
		t.Fatalf("entries = %+v, want %+v", entries, want)
	}
	for i := range want {
		if entries[i] != want[i] {
			t.Errorf("entries[%d] = %+v, want %+v", i, entries[i], want[i])
		}
	}
}

func TestFetchServerReadSetNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	if _, err := fetchServerReadSet(context.Background(), srv.URL, "", "acme", ""); err == nil {
		t.Fatal("expected an error for a non-200 response")
	}
}

func TestResolveReadSetPrefersReachableServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"entries":[{"namespace":"acme","origin":"primary"},{"namespace":"from-server","origin":"link","tiers":["semantic"]}]}`))
	}))
	defer srv.Close()
	t.Setenv("MEMINI_BASE_URL", srv.URL)

	st := openTestStore(t) // empty local store: the server's answer must win, not a local mirror
	entries, source, err := resolveReadSet(context.Background(), st, "acme", "")
	if err != nil {
		t.Fatalf("resolveReadSet: %v", err)
	}
	if !strings.HasPrefix(source, "server") {
		t.Errorf("source = %q, want a server-prefixed label", source)
	}
	found := false
	for _, e := range entries {
		if e.ns == "from-server" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected the server's entries to be used, got %+v", entries)
	}
}

func TestResolveReadSetFallsBackWhenServerUnreachable(t *testing.T) {
	t.Setenv("MEMINI_BASE_URL", "http://127.0.0.1:1") // nothing listens here
	st := openTestStore(t)

	entries, source, err := resolveReadSet(context.Background(), st, "acme/phoenix", "")
	if err != nil {
		t.Fatalf("resolveReadSet: %v", err)
	}
	if source != "local store" {
		t.Errorf("source = %q, want local store", source)
	}
	want := []doctorReadEntry{
		{ns: "acme/phoenix", origin: originPrimary, tiers: tiersAll},
		{ns: "acme", origin: originAncestor, tiers: "semantic,procedural"},
	}
	if len(entries) != len(want) {
		t.Fatalf("entries = %+v, want %+v", entries, want)
	}
}

func TestResolveReadSetLocalWhenBaseURLUnset(t *testing.T) {
	t.Setenv("MEMINI_BASE_URL", "")
	t.Setenv("MEMINI_URL", "")
	st := openTestStore(t)

	entries, source, err := resolveReadSet(context.Background(), st, "acme", "")
	if err != nil {
		t.Fatalf("resolveReadSet: %v", err)
	}
	if source != "local store" {
		t.Errorf("source = %q, want local store", source)
	}
	if len(entries) != 1 || entries[0].ns != "acme" {
		t.Fatalf("entries = %+v, want just the primary", entries)
	}
}

// --- read-set table rendering ---

func TestPrintRetrievalScopeTable(t *testing.T) {
	var out bytes.Buffer
	entries := []doctorReadEntry{
		{ns: "acme/phoenix", origin: originPrimary, tiers: tiersAll},
		{ns: "acme", origin: originAncestor, tiers: "semantic,procedural"},
		{ns: "personal/kit", origin: originHome, tiers: "semantic,procedural"},
		{ns: "shared/golang", origin: originLink, tiers: "semantic"},
	}
	printRetrievalScope(&out, entries, "local store", "acme/phoenix")
	got := out.String()
	for _, want := range []string{"NAMESPACE", "ORIGIN", "TIERS", "acme/phoenix", "ancestor", "home", "link", "shared/golang", "personal/kit"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "MEMINI_GLOBAL_NAMESPACE") || strings.Contains(got, "MEMINI_TENANT_SHARED") {
		t.Errorf("dead knobs must not be printed:\n%s", got)
	}
}

// --- dangling link note ---

func TestNoteDanglingLinksFlagsEmptyDestination(t *testing.T) {
	st := openTestStore(t)
	ls, err := linkStoreOf(st)
	if err != nil {
		t.Fatalf("linkStoreOf: %v", err)
	}
	if err := ls.PutLink(context.Background(), store.NamespaceLink{Src: "acme", Dst: "shared/golang", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("PutLink: %v", err)
	}
	stats := statsFor(t, st) // nothing written anywhere: shared/golang holds 0 memories

	var out bytes.Buffer
	noteDanglingLinks(context.Background(), &out, st, stats)
	got := out.String()
	if !strings.Contains(got, "shared/golang") {
		t.Errorf("expected the dangling destination named, got:\n%s", got)
	}
	if !strings.Contains(got, "note:") {
		t.Errorf("expected a note-level line, got:\n%s", got)
	}
	if strings.Contains(got, "WARN:") {
		t.Errorf("a dangling link is legal (a note, not a warning), got:\n%s", got)
	}
}

func TestNoteDanglingLinksSilentWhenDestinationHasMemories(t *testing.T) {
	st := openTestStore(t)
	seedPool(t, st, "shared/golang", 1, 0)
	ls, err := linkStoreOf(st)
	if err != nil {
		t.Fatalf("linkStoreOf: %v", err)
	}
	if err := ls.PutLink(context.Background(), store.NamespaceLink{Src: "acme", Dst: "shared/golang", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("PutLink: %v", err)
	}
	stats := statsFor(t, st)

	var out bytes.Buffer
	noteDanglingLinks(context.Background(), &out, st, stats)
	if out.Len() != 0 {
		t.Errorf("expected no note when the destination holds memories, got:\n%s", out.String())
	}
}

func TestNoteDanglingLinksDegradesWithoutLinkStore(t *testing.T) {
	var out bytes.Buffer
	noteDanglingLinks(context.Background(), &out, noLinkStore{}, nil)
	if out.Len() != 0 {
		t.Errorf("expected no output against a store without LinkStore support, got:\n%s", out.String())
	}
}

// --- dangling api key namespace binding note ---

func TestNoteDanglingKeyBindingsFlagsEmptyHome(t *testing.T) {
	st := openTestStore(t)
	ks, err := keyStoreOf(st)
	if err != nil {
		t.Fatalf("keyStoreOf: %v", err)
	}
	if err := ks.PutAPIKey(context.Background(), store.APIKey{Name: "kit", Hash: "deadbeef1", HomeNS: "personal/kit"}); err != nil {
		t.Fatalf("PutAPIKey: %v", err)
	}
	stats := statsFor(t, st) // nothing written anywhere: personal/kit holds 0 memories

	var out bytes.Buffer
	noteDanglingKeyBindings(context.Background(), &out, st, stats)
	got := out.String()
	if !strings.Contains(got, "kit") || !strings.Contains(got, "personal/kit") {
		t.Errorf("expected the key name and dangling home namespace named, got:\n%s", got)
	}
	if !strings.Contains(got, "note:") {
		t.Errorf("expected a note-level line, got:\n%s", got)
	}
	if strings.Contains(got, "WARN:") {
		t.Errorf("a dangling key binding is legal (a note, not a warning), got:\n%s", got)
	}
}

func TestNoteDanglingKeyBindingsFlagsEmptyDefaultNS(t *testing.T) {
	st := openTestStore(t)
	ks, err := keyStoreOf(st)
	if err != nil {
		t.Fatalf("keyStoreOf: %v", err)
	}
	if err := ks.PutAPIKey(context.Background(), store.APIKey{Name: "ci", Hash: "deadbeef2", DefaultNS: "acme"}); err != nil {
		t.Fatalf("PutAPIKey: %v", err)
	}
	stats := statsFor(t, st) // nothing written anywhere: acme holds 0 memories

	var out bytes.Buffer
	noteDanglingKeyBindings(context.Background(), &out, st, stats)
	got := out.String()
	if !strings.Contains(got, "ci") || !strings.Contains(got, "acme") {
		t.Errorf("expected the key name and dangling default namespace named, got:\n%s", got)
	}
}

func TestNoteDanglingKeyBindingsSilentWhenNamespacesHaveMemories(t *testing.T) {
	st := openTestStore(t)
	seedPool(t, st, "personal/kit", 1, 0)
	seedPool(t, st, "acme", 1, 0)
	ks, err := keyStoreOf(st)
	if err != nil {
		t.Fatalf("keyStoreOf: %v", err)
	}
	if err := ks.PutAPIKey(context.Background(), store.APIKey{
		Name: "kit", Hash: "deadbeef3", HomeNS: "personal/kit", DefaultNS: "acme",
	}); err != nil {
		t.Fatalf("PutAPIKey: %v", err)
	}
	stats := statsFor(t, st)

	var out bytes.Buffer
	noteDanglingKeyBindings(context.Background(), &out, st, stats)
	if out.Len() != 0 {
		t.Errorf("expected no note when both bound namespaces hold memories, got:\n%s", out.String())
	}
}

func TestNoteDanglingKeyBindingsSilentWhenUnbound(t *testing.T) {
	st := openTestStore(t)
	ks, err := keyStoreOf(st)
	if err != nil {
		t.Fatalf("keyStoreOf: %v", err)
	}
	if err := ks.PutAPIKey(context.Background(), store.APIKey{Name: "unbound", Hash: "deadbeef4"}); err != nil {
		t.Fatalf("PutAPIKey: %v", err)
	}
	stats := statsFor(t, st)

	var out bytes.Buffer
	noteDanglingKeyBindings(context.Background(), &out, st, stats)
	if out.Len() != 0 {
		t.Errorf("expected no note for a key with no namespace bindings, got:\n%s", out.String())
	}
}

func TestNoteDanglingKeyBindingsDegradesWithoutAPIKeyStore(t *testing.T) {
	var out bytes.Buffer
	noteDanglingKeyBindings(context.Background(), &out, noKeyStore{}, nil)
	if out.Len() != 0 {
		t.Errorf("expected no output against a store without APIKeyStore support, got:\n%s", out.String())
	}
}

// --- new warnings: global namespace pin, MEMINI_HOME unset ---

func TestWarnGlobalNamespacePinDiffersFromGit(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	n := warnGlobalNamespacePin(&out, dir, "team-wide-pin")
	if n != 1 {
		t.Fatalf("warnings = %d, want 1, output:\n%s", n, out.String())
	}
	got := out.String()
	if !strings.Contains(got, "team-wide-pin") {
		t.Errorf("expected the pinned namespace named, got:\n%s", got)
	}
	if !strings.Contains(got, "memini namespace split") {
		t.Errorf("expected the remediation command named, got:\n%s", got)
	}
}

func TestWarnGlobalNamespacePinMatchesGit(t *testing.T) {
	dir := t.TempDir()
	gitNS, _ := config.ResolveDirNamespace(dir)
	var out bytes.Buffer
	n := warnGlobalNamespacePin(&out, dir, gitNS)
	if n != 0 {
		t.Fatalf("warnings = %d, want 0 when the pin matches git resolution, output:\n%s", n, out.String())
	}
}

func TestWarnGlobalNamespacePinUnset(t *testing.T) {
	var out bytes.Buffer
	n := warnGlobalNamespacePin(&out, t.TempDir(), "")
	if n != 0 {
		t.Fatalf("warnings = %d, want 0 when no env override is set, output:\n%s", n, out.String())
	}
}

func TestWarnHomeUnset(t *testing.T) {
	var out bytes.Buffer
	n := warnHomeUnset(&out, "")
	if n != 1 {
		t.Fatalf("warnings = %d, want 1, output:\n%s", n, out.String())
	}
	if !strings.Contains(out.String(), "MEMINI_HOME") {
		t.Errorf("expected MEMINI_HOME named, got:\n%s", out.String())
	}
}

func TestWarnHomeSet(t *testing.T) {
	var out bytes.Buffer
	n := warnHomeUnset(&out, "personal/kit")
	if n != 0 {
		t.Fatalf("warnings = %d, want 0 when MEMINI_HOME is set, output:\n%s", n, out.String())
	}
}
