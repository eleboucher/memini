package main

import (
	"bytes"
	"context"
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

// TestResolveDoctorReadSetDefault: a flat (untenanted) namespace with no
// global namespace reads just itself — no tenant segment, no shared merge.
func TestResolveDoctorReadSetDefault(t *testing.T) {
	entries, notes := resolveDoctorReadSet("proj", "", false)
	if len(notes) != 0 {
		t.Fatalf("expected no notes, got %v", notes)
	}
	if len(entries) != 1 || entries[0] != (doctorReadEntry{ns: "proj", tiers: "all", source: "default"}) {
		t.Fatalf("unexpected entries: %+v", entries)
	}
}

// TestResolveDoctorReadSetComposesAllSources: the global namespace and the
// derived tenant-shared namespace both contribute, in the order the real
// resolver applies them (primary, global, tenant-shared).
func TestResolveDoctorReadSetComposesAllSources(t *testing.T) {
	entries, notes := resolveDoctorReadSet("work/memini", "glob", true)
	if len(notes) != 0 {
		t.Fatalf("expected no notes, got %v", notes)
	}

	byNS := make(map[string]doctorReadEntry, len(entries))
	for _, e := range entries {
		byNS[e.ns] = e
	}
	want := map[string]doctorReadEntry{
		"work/memini":  {ns: "work/memini", tiers: "all", source: "default"},
		"glob":         {ns: "glob", tiers: "durable", source: "global"},
		"work/_shared": {ns: "work/_shared", tiers: "durable", source: "tenant-shared"},
	}
	if len(entries) != len(want) {
		t.Fatalf("entries = %+v, want %d", entries, len(want))
	}
	for ns, wantEntry := range want {
		if got, ok := byNS[ns]; !ok || got != wantEntry {
			t.Errorf("entry %q = %+v, want %+v (present=%v)", ns, got, wantEntry, ok)
		}
	}
}

// TestResolveDoctorReadSetFlagsDuplicateEntry: the global namespace naming the
// tenant-shared namespace surfaces as a redundant-entry note instead of
// silently appearing twice.
func TestResolveDoctorReadSetFlagsDuplicateEntry(t *testing.T) {
	entries, notes := resolveDoctorReadSet("work/memini", "work/_shared", true)

	foundDup := false
	for _, n := range notes {
		if strings.Contains(n, "work/_shared") && strings.Contains(n, "redundant") {
			foundDup = true
		}
	}
	if !foundDup {
		t.Errorf("expected a redundant-entry note for %q, got notes: %v", "work/_shared", notes)
	}
	count := 0
	for _, e := range entries {
		if e.ns == "work/_shared" {
			count++
			if e.source != "global" {
				t.Errorf("work/_shared should keep the global entry's source, got %q", e.source)
			}
		}
	}
	if count != 1 {
		t.Errorf("work/_shared should appear exactly once, got %d", count)
	}
}

// TestResolveDoctorReadSetSelfEntryNote: the global namespace naming primary
// itself is a no-op and surfaces as a note.
func TestResolveDoctorReadSetSelfEntryNote(t *testing.T) {
	_, notes := resolveDoctorReadSet("proj", "proj", false)
	if len(notes) != 1 || !strings.Contains(notes[0], "already the request namespace") {
		t.Fatalf("expected a self-entry note, got %v", notes)
	}
}

// TestResolveDoctorReadSetSharedNamespaceItself: the tenant-shared namespace
// reading itself gets no self-merge (work/_shared does not add work/_shared).
func TestResolveDoctorReadSetSharedNamespaceItself(t *testing.T) {
	entries, notes := resolveDoctorReadSet("work/_shared", "", true)
	if len(notes) != 0 {
		t.Fatalf("expected no notes, got %v", notes)
	}
	if len(entries) != 1 || entries[0].ns != "work/_shared" {
		t.Fatalf("expected only the primary entry, got %+v", entries)
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

// TestPrintRetrievalScopeDefaultOnly: with no configuration, the section
// renders just the request namespace and warns about nothing.
func TestPrintRetrievalScopeDefaultOnly(t *testing.T) {
	var out bytes.Buffer
	warnings := printRetrievalScope(&out, &config.Config{}, nil, "proj")
	got := out.String()
	if !strings.Contains(got, "Retrieval scope") {
		t.Errorf("missing section header, got:\n%s", got)
	}
	if !strings.Contains(got, "proj") {
		t.Errorf("missing request namespace, got:\n%s", got)
	}
	if warnings != 0 {
		t.Errorf("expected no warnings from a bare default-only read set, got %d", warnings)
	}
}

// TestPrintRetrievalScopeWarnsOnEmptyTenantShared: with the tenant-shared
// merge opted in (MEMINI_TENANT_SHARED), a tenanted namespace whose shared
// sibling holds no memories gets a 0-memories warning.
func TestPrintRetrievalScopeWarnsOnEmptyTenantShared(t *testing.T) {
	st := openTestStore(t)
	seedPool(t, st, "work/memini", 1, 0) // only the primary holds memories
	stats := statsFor(t, st)

	var out bytes.Buffer
	warnings := printRetrievalScope(&out, &config.Config{TenantShared: true}, stats, "work/memini")
	got := out.String()
	if !strings.Contains(got, "work/_shared") {
		t.Errorf("expected the tenant-shared namespace listed, got:\n%s", got)
	}
	if !strings.Contains(got, "0 memories") {
		t.Errorf("expected a 0-memories warning for the empty shared namespace, got:\n%s", got)
	}
	if warnings == 0 {
		t.Errorf("expected at least 1 warning, got 0")
	}
}

// TestPrintRetrievalScopeTenantSharedOffByDefault: without the opt-in, the
// tenant-shared sibling is absent from the read set and shows as "(off)", with
// no 0-memories warning for it.
func TestPrintRetrievalScopeTenantSharedOffByDefault(t *testing.T) {
	st := openTestStore(t)
	seedPool(t, st, "work/memini", 1, 0)
	stats := statsFor(t, st)

	var out bytes.Buffer
	warnings := printRetrievalScope(&out, &config.Config{}, stats, "work/memini")
	got := out.String()
	if !strings.Contains(got, "tenant-shared namespace: (off)") {
		t.Errorf("expected tenant-shared shown as (off), got:\n%s", got)
	}
	if strings.Contains(got, "work/_shared") {
		t.Errorf("tenant-shared must not appear in the read set when off, got:\n%s", got)
	}
	if warnings != 0 {
		t.Errorf("expected no warnings when tenant-shared is off, got %d", warnings)
	}
}
