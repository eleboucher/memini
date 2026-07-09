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

// TestResolveDoctorReadSetDefault: with no links, env, or global namespace,
// the read set is just primary itself.
func TestResolveDoctorReadSetDefault(t *testing.T) {
	entries, notes := resolveDoctorReadSet("proj", nil, "", nil, nil)
	if len(notes) != 0 {
		t.Fatalf("expected no notes, got %v", notes)
	}
	if len(entries) != 1 || entries[0] != (doctorReadEntry{ns: "proj", tiers: "all", source: "default"}) {
		t.Fatalf("unexpected entries: %+v", entries)
	}
}

// TestResolveDoctorReadSetComposesAllSources: links, global namespace, and
// read-namespaces (including a "/*" subtree pattern) all contribute, in the
// order the real resolver applies them (primary, links, global,
// read-namespaces) so a namespace claimed by an earlier source keeps its
// tier access rather than being narrowed by a later one.
func TestResolveDoctorReadSetComposesAllSources(t *testing.T) {
	links := []store.NamespaceLink{
		{Namespace: "proj", Target: "shared", Tiers: "all"},
		{Namespace: "proj", Target: "team/*", Tiers: "durable"},
		{Namespace: "other", Target: "ignored"}, // different namespace, must not appear
	}
	all := []string{"proj", "shared", "team", "team/a", "team/b", "unrelated"}
	entries, notes := resolveDoctorReadSet("proj", []string{"rn1", "rn2/*"}, "glob", links, all)
	if len(notes) != 0 {
		t.Fatalf("expected no notes, got %v", notes)
	}

	byNS := make(map[string]doctorReadEntry, len(entries))
	for _, e := range entries {
		byNS[e.ns] = e
	}
	want := map[string]doctorReadEntry{
		"proj":   {ns: "proj", tiers: "all", source: "default"},
		"shared": {ns: "shared", tiers: "all", source: "link"},
		"team":   {ns: "team", tiers: "durable", source: "link"},
		"team/a": {ns: "team/a", tiers: "durable", source: "subtree-pattern"},
		"team/b": {ns: "team/b", tiers: "durable", source: "subtree-pattern"},
		"glob":   {ns: "glob", tiers: "durable", source: "global"},
		"rn1":    {ns: "rn1", tiers: "durable", source: "env"},
	}
	for ns, wantEntry := range want {
		if got, ok := byNS[ns]; !ok || got != wantEntry {
			t.Errorf("entry %q = %+v, want %+v (present=%v)", ns, got, wantEntry, ok)
		}
	}
	if _, ok := byNS["unrelated"]; ok {
		t.Errorf("unrelated namespace must not appear in the read set")
	}
	if _, ok := byNS["ignored"]; ok {
		t.Errorf("another namespace's link must not appear")
	}
	// rn2/* has no matching namespaces in `all`, so it contributes no subtree members.
	if _, ok := byNS["rn2"]; !ok {
		t.Errorf("rn2 base entry missing")
	}
}

// TestResolveDoctorReadSetFlagsSelfAndDuplicateEntries: a link/env entry
// naming primary itself, and two sources naming the same namespace, both
// surface as notes instead of silently vanishing.
func TestResolveDoctorReadSetFlagsSelfAndDuplicateEntries(t *testing.T) {
	links := []store.NamespaceLink{
		{Namespace: "proj", Target: "proj/*", Tiers: "durable"}, // self-subtree link: legal, not a "self" note
		{Namespace: "proj", Target: "shared", Tiers: "durable"},
	}
	entries, notes := resolveDoctorReadSet("proj", []string{"shared"}, "shared", links, nil)

	foundDup := false
	for _, n := range notes {
		if strings.Contains(n, "shared") && strings.Contains(n, "redundant") {
			foundDup = true
		}
	}
	if !foundDup {
		t.Errorf("expected a redundant-entry note for %q, got notes: %v", "shared", notes)
	}
	// shared must appear exactly once, with the earliest (link) source.
	count := 0
	for _, e := range entries {
		if e.ns == "shared" {
			count++
			if e.source != "link" {
				t.Errorf("shared should keep the link's tier access, got source %q", e.source)
			}
		}
	}
	if count != 1 {
		t.Errorf("shared should appear exactly once, got %d", count)
	}
}

// TestResolveDoctorReadSetSelfEntryNote: an env entry naming primary itself
// is a no-op and surfaces as a note.
func TestResolveDoctorReadSetSelfEntryNote(t *testing.T) {
	_, notes := resolveDoctorReadSet("proj", []string{"proj"}, "", nil, nil)
	if len(notes) != 1 || !strings.Contains(notes[0], "already the request namespace") {
		t.Fatalf("expected a self-entry note, got %v", notes)
	}
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

func TestOrUnsetList(t *testing.T) {
	if got := orUnsetList(nil); got != "(unset)" {
		t.Errorf("orUnsetList(nil) = %q, want (unset)", got)
	}
	if got := orUnsetList([]string{"a", "b"}); got != "a, b" {
		t.Errorf("orUnsetList = %q, want %q", got, "a, b")
	}
}

// TestPrintRetrievalScopeReportsDanglingLinkAndClamp: an end-to-end smoke of
// printRetrievalScope against a real store, covering the dangling-target
// warning (a linked namespace with zero memories) and the section header /
// table it prints.
func TestPrintRetrievalScopeReportsDanglingLinkAndClamp(t *testing.T) {
	st := openTestStore(t)
	seedPool(t, st, "proj", 2, 0)
	// "empty-target" is linked but never written to -> dangling.
	svc := linkService(st)
	if err := svc.LinkNamespaces(context.Background(), "proj", "empty-target", "durable"); err != nil {
		t.Fatalf("link: %v", err)
	}
	stats := statsFor(t, st)

	cfg := &config.Config{}
	var out bytes.Buffer
	warnings := printRetrievalScope(context.Background(), &out, cfg, st, stats, "proj", "proj")
	got := out.String()
	if !strings.Contains(got, "Retrieval scope") {
		t.Errorf("missing section header:\n%s", got)
	}
	if !strings.Contains(got, "empty-target") {
		t.Errorf("missing linked namespace in output:\n%s", got)
	}
	if !strings.Contains(got, "0 memories") {
		t.Errorf("expected a dangling-target warning:\n%s", got)
	}
	if warnings < 1 {
		t.Errorf("expected at least 1 warning for the dangling link, got %d", warnings)
	}
}

// TestPrintRetrievalScopeDegradesWithoutLinkStore: a store that doesn't
// implement store.LinkStore is handled gracefully, not with a panic or error.
func TestPrintRetrievalScopeDegradesWithoutLinkStore(t *testing.T) {
	var out bytes.Buffer
	warnings := printRetrievalScope(context.Background(), &out, &config.Config{}, noLinkStore{}, nil, "proj", "proj")
	if !strings.Contains(out.String(), "not supported by this backend") {
		t.Errorf("expected a not-supported note, got:\n%s", out.String())
	}
	if warnings != 0 {
		t.Errorf("expected no warnings from a bare default-only read set, got %d", warnings)
	}
}

// noLinkStore is a minimal store.Store that deliberately does not implement
// store.LinkStore, to exercise printRetrievalScope's degrade path.
type noLinkStore struct{ store.Store }
