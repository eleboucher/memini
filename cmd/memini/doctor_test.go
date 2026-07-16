package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/eleboucher/memini/internal/config"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/nsresolve"
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
			m.Metadata = map[string]any{"import_source_namespace": "team-" + strconv.Itoa(i%3)}
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
// the pool is split into per-source namespaces.
func TestDoctorFixSplitsAttributablePool(t *testing.T) {
	st := openTestStore(t)
	seedPool(t, st, nsDefault, 600, 600)
	var out bytes.Buffer

	if err := doctorFix(context.Background(), &out, statsFor(t, st), fixDeps{store: st, now: time.Now().UTC(), apply: true}); err != nil {
		t.Fatalf("doctorFix apply: %v", err)
	}
	names, _ := st.ListNamespaces(context.Background())
	// 3 source namespaces (team-0/1/2); the default pool should be drained.
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
		{namespace: "a", classified: 2, promoted: 1, corroborated: 3, pendingEmbed: 2},
		{namespace: "b", classified: 1, pendingEmbed: 1},
	})
	out := buf.String()
	for _, want := range []string{"marker-classified durable writes:  3", "promotion-produced facts:          1", "corroborated durable memories:     3", "pending embed (vectorless, awaiting backfill):  3"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}

	// The pending-embed backlog is the only nonzero signal: the section must
	// still print (isolates the `&& pendingEmbed == 0` guard term).
	buf.Reset()
	printWritePathSignals(&buf, []nsStat{{namespace: "x", pendingEmbed: 1}})
	if got := buf.String(); !strings.Contains(got, "pending embed (vectorless, awaiting backfill):  1") {
		t.Fatalf("pending-embed-only signals should print the section, got:\n%s", got)
	}

	buf.Reset()
	printWritePathSignals(&buf, []nsStat{{namespace: "a"}})
	if buf.Len() != 0 {
		t.Fatalf("all-zero signals should print nothing, got:\n%s", buf.String())
	}
}

// TestNamespaceStatsPendingEmbed: a memory carrying pending_embed="true"
// metadata is counted toward the namespace's pendingEmbed backlog; a memory
// without the flag is not.
func TestNamespaceStatsPendingEmbed(t *testing.T) {
	st := openTestStore(t)
	now := time.Now().UTC()
	flagged := &memory.Memory{
		ID: "acme/vectorless", Namespace: "acme", Tier: memory.TierEpisodic,
		Content: "degraded write awaiting backfill", CreatedAt: now, UpdatedAt: now, LastAccessedAt: now,
		Embedding: []float32{1, 0, 0, 0},
		Metadata:  map[string]any{"pending_embed": "true"},
	}
	if err := st.Upsert(context.Background(), flagged); err != nil {
		t.Fatalf("upsert flagged: %v", err)
	}
	plain := &memory.Memory{
		ID: "acme/embedded", Namespace: "acme", Tier: memory.TierEpisodic,
		Content: "ordinary write", CreatedAt: now, UpdatedAt: now, LastAccessedAt: now,
		Embedding: []float32{0, 1, 0, 0},
	}
	if err := st.Upsert(context.Background(), plain); err != nil {
		t.Fatalf("upsert plain: %v", err)
	}

	stats := statsFor(t, st)
	total, ok := 0, false
	for _, s := range stats {
		if s.namespace == "acme" {
			total, ok = s.pendingEmbed, true
		}
	}
	if !ok {
		t.Fatalf("namespace acme missing from stats: %+v", stats)
	}
	if total != 1 {
		t.Fatalf("pendingEmbed = %d, want 1 (only the flagged memory counts)", total)
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
	var gotNS, gotHome, gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotNS = r.Header.Get("X-Memini-Namespace")
		gotHome = r.Header.Get("X-Memini-Home")
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"entries":[]}`))
	}))
	defer srv.Close()

	if _, err := fetchServerReadSet(context.Background(), srv.URL, "secret-token", "acme/phoenix", "personal/kit"); err != nil {
		t.Fatalf("fetchServerReadSet: %v", err)
	}
	if gotPath != "/v1/namespaces/readset" {
		t.Errorf("path = %q, want /v1/namespaces/readset (no dash)", gotPath)
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

// --- lingering dead files: overrides.json / project-map.json / namespace cache ---

func writeDeadFile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestWarnLingeringDeadFiles(t *testing.T) {
	tests := []struct {
		name                                       string
		writeOverrides, writeProjectMap, writeNSNS bool
		wantWarnings                               int
	}{
		{name: "none present", wantWarnings: 0},
		{name: "overrides only", writeOverrides: true, wantWarnings: 1},
		{name: "legacy caches only", writeProjectMap: true, writeNSNS: true, wantWarnings: 2},
		{name: "all present", writeOverrides: true, writeProjectMap: true, writeNSNS: true, wantWarnings: 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			configDir := t.TempDir()
			cacheDir := t.TempDir()
			t.Setenv("XDG_CONFIG_HOME", configDir)
			t.Setenv("XDG_CACHE_HOME", cacheDir)
			if tt.writeOverrides {
				writeDeadFile(t, filepath.Join(configDir, "memini", "overrides.json"))
			}
			if tt.writeProjectMap {
				writeDeadFile(t, filepath.Join(cacheDir, "memini", "project-map.json"))
			}
			if tt.writeNSNS {
				writeDeadFile(t, filepath.Join(cacheDir, "memini", "namespace"))
			}

			var out bytes.Buffer
			n := warnLingeringDeadFiles(&out)
			if n != tt.wantWarnings {
				t.Fatalf("warnings = %d, want %d, output:\n%s", n, tt.wantWarnings, out.String())
			}
			got := out.String()
			if tt.writeOverrides && (!strings.Contains(got, "overrides.json") || !strings.Contains(got, "migrate to server pins")) {
				t.Errorf("expected the overrides.json advisory, got:\n%s", got)
			}
			if tt.writeProjectMap && (!strings.Contains(got, "project-map.json") || !strings.Contains(got, "safe to delete")) {
				t.Errorf("expected the project-map.json advisory, got:\n%s", got)
			}
			if tt.writeNSNS && !strings.Contains(got, "namespace") {
				t.Errorf("expected the namespace cache advisory, got:\n%s", got)
			}
		})
	}
}

func TestWarnRemovedEnvVars(t *testing.T) {
	// Start from a clean slate so a developer's real shell can't leak a set var
	// into the "none set" case.
	for _, k := range removedEnvVars {
		t.Setenv(k, "")
	}

	t.Run("none set produces no warning", func(t *testing.T) {
		var out bytes.Buffer
		if n := warnRemovedEnvVars(&out); n != 0 {
			t.Fatalf("warnings = %d, want 0, output:\n%s", n, out.String())
		}
	})

	t.Run("set vars are named in one combined warning", func(t *testing.T) {
		t.Setenv("MEMINI_URL", "http://old.example")
		t.Setenv("MEMINI_NAMESPACE_SCOPE", "owner_repo")
		var out bytes.Buffer
		n := warnRemovedEnvVars(&out)
		if n != 1 {
			t.Fatalf("warnings = %d, want 1, output:\n%s", n, out.String())
		}
		got := out.String()
		for _, want := range []string{"MEMINI_URL", "MEMINI_NAMESPACE_SCOPE", "env-vars.md"} {
			if !strings.Contains(got, want) {
				t.Errorf("missing %q in:\n%s", want, got)
			}
		}
		// Unset vars must not be named.
		if strings.Contains(got, "MEMINI_TOKEN") || strings.Contains(got, "MEMINI_MCP_URL") {
			t.Errorf("unset vars must not be named:\n%s", got)
		}
	})
}

func TestWarnLingeringConfigJSON(t *testing.T) {
	write := func(t *testing.T, dir, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(dir, "memini"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "memini", "config.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("no config.json produces no warning", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		var out bytes.Buffer
		if n := warnLingeringConfigJSON(&out); n != 0 {
			t.Fatalf("warnings = %d, want 0, output:\n%s", n, out.String())
		}
	})

	t.Run("config.json without tenantRoots/template produces no warning", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("XDG_CONFIG_HOME", dir)
		write(t, dir, `{"somethingElse": true}`)
		var out bytes.Buffer
		if n := warnLingeringConfigJSON(&out); n != 0 {
			t.Fatalf("warnings = %d, want 0, output:\n%s", n, out.String())
		}
	})

	t.Run("tenantRoots/template is flagged with migration instructions", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("XDG_CONFIG_HOME", dir)
		write(t, dir, `{"tenantRoots":[{"path":"~/dev","tenant":"work"}],"template":"{tenant}/{project}"}`)
		var out bytes.Buffer
		n := warnLingeringConfigJSON(&out)
		if n != 1 {
			t.Fatalf("warnings = %d, want 1, output:\n%s", n, out.String())
		}
		got := out.String()
		for _, want := range []string{"config.json", "namespace_prefix"} {
			if !strings.Contains(got, want) {
				t.Errorf("missing %q in:\n%s", want, got)
			}
		}
	})
}

// --- handshake probe ---

func TestProbeHandshakeSendsFactsAndIdentifiesAsDoctor(t *testing.T) {
	var gotPath, gotMethod, gotAuth, gotContentType string
	var gotBody handshakeProbeRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		gotAuth = r.Header.Get("Authorization")
		gotContentType = r.Header.Get("Content-Type")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decoding request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"namespace":"acme","namespace_source":"remote","identity":{"authenticated":true,"key_name":"ci"}}`))
	}))
	defer srv.Close()

	facts := nsresolve.Facts{
		RemoteURL: "git@github.com:acme/app.git", ToplevelPath: "/home/kit/app",
		ToplevelBasename: "app", CwdBasename: "app", Agent: "reviewer", EnvNamespace: "pinned",
	}
	resp, err := probeHandshake(context.Background(), srv.URL, "secret", facts)
	if err != nil {
		t.Fatalf("probeHandshake: %v", err)
	}

	if gotMethod != http.MethodPost || gotPath != "/v1/handshake" {
		t.Errorf("request = %s %s, want POST /v1/handshake", gotMethod, gotPath)
	}
	if gotAuth != "Bearer secret" {
		t.Errorf("Authorization = %q, want Bearer secret", gotAuth)
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotContentType)
	}
	if gotBody.Client.Name != "memini-doctor" {
		t.Errorf("client.name = %q, want memini-doctor", gotBody.Client.Name)
	}
	wantProject := handshakeProbeProject{
		RemoteURL: facts.RemoteURL, ToplevelPath: facts.ToplevelPath, ToplevelBasename: facts.ToplevelBasename,
		CwdBasename: facts.CwdBasename, Agent: facts.Agent, EnvNamespace: facts.EnvNamespace,
	}
	if gotBody.Project != wantProject {
		t.Errorf("project facts = %+v, want %+v (sent verbatim)", gotBody.Project, wantProject)
	}

	if resp.Namespace != "acme" || resp.NamespaceSource != "remote" {
		t.Errorf("resp = %+v, want namespace=acme source=remote", resp)
	}
	if !resp.Identity.Authenticated || resp.Identity.KeyName != "ci" {
		t.Errorf("identity = %+v, want authenticated key_name=ci", resp.Identity)
	}
}

func TestProbeHandshakeNoAuthHeaderWhenAPIKeyEmpty(t *testing.T) {
	var gotAuth string
	sawAuth := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, sawAuth = r.Header.Get("Authorization"), r.Header.Get("Authorization") != ""
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"namespace":"acme","namespace_source":"cwd","identity":{"authenticated":false}}`))
	}))
	defer srv.Close()

	if _, err := probeHandshake(context.Background(), srv.URL, "", nsresolve.Facts{CwdBasename: "acme"}); err != nil {
		t.Fatalf("probeHandshake: %v", err)
	}
	if sawAuth {
		t.Errorf("Authorization = %q, want no header when apiKey is empty", gotAuth)
	}
}

func TestProbeHandshakeNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	if _, err := probeHandshake(context.Background(), srv.URL, "", nsresolve.Facts{CwdBasename: "x"}); err == nil {
		t.Fatal("expected an error for a non-200 response")
	}
}

func TestRunHandshakeProbeUnreachableWarns(t *testing.T) {
	var out bytes.Buffer
	n := runHandshakeProbe(context.Background(), &out, "http://127.0.0.1:1", "", t.TempDir(), "acme", config.NamespaceFromCWD)
	if n != 1 {
		t.Fatalf("warnings = %d, want 1, output:\n%s", n, out.String())
	}
	if !strings.Contains(out.String(), "WARN") {
		t.Errorf("expected a WARN line for an unreachable probe target, got:\n%s", out.String())
	}
}

func TestRunHandshakeProbeHappyPathAddsNoWarning(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"namespace":"acme","namespace_source":"cwd","identity":{"authenticated":true}}`))
	}))
	defer srv.Close()

	var out bytes.Buffer
	n := runHandshakeProbe(context.Background(), &out, srv.URL, "", t.TempDir(), "acme", config.NamespaceFromCWD)
	if n != 0 {
		t.Fatalf("warnings = %d, want 0, output:\n%s", n, out.String())
	}
	if !strings.Contains(out.String(), "Handshake probe") {
		t.Errorf("expected the probe header line, got:\n%s", out.String())
	}
}

// --- handshake probe: reporting local-vs-server agreement ---

func TestReportHandshakeProbeMatchIsNotAWarning(t *testing.T) {
	var out bytes.Buffer
	resp := handshakeProbeResponse{Namespace: "acme", NamespaceSource: "remote", Identity: handshakeProbeIdentity{Authenticated: true}}
	reportHandshakeProbe(&out, resp, "acme", config.NamespaceSource("remote"))
	got := out.String()
	if !strings.Contains(got, "matches the local derivation") {
		t.Errorf("expected a match line, got:\n%s", got)
	}
	if strings.Contains(got, "WARN") {
		t.Errorf("a match must never warn, got:\n%s", got)
	}
}

// TestReportHandshakeProbePinDisagreesExplainedNotAlarmed: a pin is the
// textbook legitimate divergence — the server's pin outranks local
// derivation by design, so doctor must explain it, not raise a WARN.
func TestReportHandshakeProbePinDisagreesExplainedNotAlarmed(t *testing.T) {
	var out bytes.Buffer
	resp := handshakeProbeResponse{
		Namespace: "acme/pinned", NamespaceSource: nsresolve.SourcePin,
		Identity: handshakeProbeIdentity{Authenticated: true, KeyName: "ci"},
	}
	reportHandshakeProbe(&out, resp, "acme", config.NamespaceSource("remote"))
	got := out.String()
	if !strings.Contains(got, "differs from the local derivation") {
		t.Errorf("expected a differs line, got:\n%s", got)
	}
	if !strings.Contains(got, "pin") {
		t.Errorf("expected the pin explanation, got:\n%s", got)
	}
	if strings.Contains(got, "WARN") {
		t.Errorf("a legitimate pin divergence must be explained, not alarmed (no WARN), got:\n%s", got)
	}
}

func TestReportHandshakeProbeKeyDefaultDisagreesExplained(t *testing.T) {
	var out bytes.Buffer
	resp := handshakeProbeResponse{
		Namespace: "keys-own-default", NamespaceSource: nsresolve.SourceKeyDefault,
		Identity: handshakeProbeIdentity{Authenticated: true},
	}
	reportHandshakeProbe(&out, resp, "acme", config.NamespaceSource("remote"))
	got := out.String()
	if !strings.Contains(got, "default namespace") {
		t.Errorf("expected the key-default explanation, got:\n%s", got)
	}
	if strings.Contains(got, "WARN") {
		t.Errorf("a legitimate key-default divergence must be explained, not alarmed (no WARN), got:\n%s", got)
	}
}

func TestReportHandshakeProbeUnknownDivergenceIsNotAlarmedEither(t *testing.T) {
	var out bytes.Buffer
	resp := handshakeProbeResponse{Namespace: "weird", NamespaceSource: "cwd", Identity: handshakeProbeIdentity{Authenticated: true}}
	reportHandshakeProbe(&out, resp, "acme", config.NamespaceSource("remote"))
	got := out.String()
	if !strings.Contains(got, "worth investigating") {
		t.Errorf("expected the fallback explanation for an unexplained divergence, got:\n%s", got)
	}
	if strings.Contains(got, "WARN") {
		t.Errorf("even an unexplained divergence is a note, not a WARN, got:\n%s", got)
	}
}

func TestHandshakeMismatchReasonKnownSources(t *testing.T) {
	for _, src := range []string{nsresolve.SourcePin, nsresolve.SourceKeyDefault} {
		if _, ok := handshakeMismatchReason(src); !ok {
			t.Errorf("handshakeMismatchReason(%q) ok = false, want true", src)
		}
	}
}

func TestHandshakeMismatchReasonUnknownSource(t *testing.T) {
	for _, src := range []string{nsresolve.SourceEnv, nsresolve.SourceRemote, nsresolve.SourceToplevel, nsresolve.SourceCwd, nsresolve.SourceServerDefault, nsresolve.SourceDeclared} {
		if _, ok := handshakeMismatchReason(src); ok {
			t.Errorf("handshakeMismatchReason(%q) ok = true, want false (only pin/key_default are legitimate divergences)", src)
		}
	}
}
