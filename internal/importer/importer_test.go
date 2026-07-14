package importer_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/eleboucher/memini/internal/embed/embedtest"
	"github.com/eleboucher/memini/internal/importer"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/store"
	"github.com/eleboucher/memini/internal/store/sqlitevec"
)

func TestImportPreservesIDAndTimestamps(t *testing.T) {
	ctx := context.Background()
	st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "import.db"), 32)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()

	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	recs := []importer.Record{
		{ID: "keep-1", Namespace: "proj", Tier: memory.TierSemantic,
			Content: "durable fact", Importance: 0.7, CreatedAt: created, UpdatedAt: created},
		{ID: "", Content: "", Tier: memory.TierSemantic}, // empty content -> skipped
		{ID: "no-ns", Content: "needs default ns", Tier: memory.TierSemantic},
	}

	rep, err := importer.NewLocal(st, embedtest.New(32)).Import(ctx, recs,
		importer.Options{DefaultNamespace: "fallback"})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if rep.Total != 3 || rep.Imported != 2 || rep.Skipped != 1 {
		t.Fatalf("report = %+v, want total=3 imported=2 skipped=1", rep)
	}

	got, err := st.Get(ctx, "proj", "keep-1")
	if err != nil {
		t.Fatalf("Get keep-1: %v", err)
	}
	if !got.CreatedAt.Equal(created) {
		t.Errorf("CreatedAt = %v, want preserved %v", got.CreatedAt, created)
	}
	if got.Importance != 0.7 || got.Tier != memory.TierSemantic {
		t.Errorf("fields not preserved: %+v", got)
	}

	// The no-namespace record should land in the fallback namespace.
	ms, err := st.List(ctx, "fallback", store.Filter{}, 0)
	if err != nil {
		t.Fatalf("List fallback: %v", err)
	}
	var found bool
	for _, m := range ms {
		if m.Content == "needs default ns" {
			found = true
		}
	}
	if !found {
		t.Error("default namespace not applied: record not in 'fallback'")
	}
}

func TestImportQualityGates(t *testing.T) {
	ctx := context.Background()
	st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "import.db"), 32)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()

	recs := []importer.Record{
		{ID: "keep", Namespace: "p", Content: "a substantial, useful memory", Importance: 0.5},
		{ID: "stub", Namespace: "p", Content: "  ok  ", Importance: 0.5},              // too short
		{ID: "weak", Namespace: "p", Content: "long enough content", Importance: 0.1}, // below importance floor
	}

	rep, err := importer.NewLocal(st, embedtest.New(32)).Import(ctx, recs,
		importer.Options{MinContentLen: 10, MinImportance: 0.3})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if rep.Imported != 1 || rep.Skipped != 2 {
		t.Fatalf("report = %+v, want imported=1 skipped=2", rep)
	}
	if _, err := st.Get(ctx, "p", "keep"); err != nil {
		t.Errorf("keep should be imported: %v", err)
	}
	if _, err := st.Get(ctx, "p", "stub"); err == nil {
		t.Error("stub should be skipped (too short)")
	}
	if _, err := st.Get(ctx, "p", "weak"); err == nil {
		t.Error("weak should be skipped (below importance floor)")
	}
}

func TestImportDropsRuntimeNoise(t *testing.T) {
	ctx := context.Background()
	st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "import.db"), 32)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()

	recs := []importer.Record{
		{ID: "cron", Namespace: "p", Importance: 0.5, Content: "User: [cron:abc watcher] check repos and post to Discord"},
		{ID: "meta", Namespace: "p", Importance: 0.5, Content: "Info (untrusted metadata):\n```json\n{\"chat_id\":1}\n```\nUser: real question"},
		// Metadata preamble stacked in front of a cron marker: strip-then-check drops it.
		{ID: "metacron", Namespace: "p", Importance: 0.5, Content: "Info (untrusted metadata):\n```json\n{}\n```\nUser: [cron:x] do it"},
		{ID: "clean", Namespace: "p", Importance: 0.5, Content: "a genuine memory worth keeping"},
	}

	rep, err := importer.NewLocal(st, embedtest.New(32)).Import(ctx, recs, importer.Options{})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if rep.Imported != 2 || rep.Skipped != 2 {
		t.Fatalf("report = %+v, want imported=2 skipped=2", rep)
	}
	if _, err := st.Get(ctx, "p", "cron"); err == nil {
		t.Error("cron framing record should be dropped")
	}
	if _, err := st.Get(ctx, "p", "metacron"); err == nil {
		t.Error("metadata-then-cron record should be dropped (strip then check)")
	}
	// The metadata preamble is peeled; the real message survives.
	m, err := st.Get(ctx, "p", "meta")
	if err != nil {
		t.Fatalf("meta should be imported (preamble stripped): %v", err)
	}
	if m.Content != "User: real question" {
		t.Errorf("meta content = %q, want %q", m.Content, "User: real question")
	}
}

func TestImportPreservesSourceNamespaces(t *testing.T) {
	ctx := context.Background()
	st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "import.db"), 32)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()

	recs := []importer.Record{
		{Namespace: "alice", Content: "alice's durable fact about auth"},
		{Namespace: "bob", Content: "bob's durable fact about billing"},
		{Content: "no namespace, falls to default"},
	}
	rep, err := importer.NewLocal(st, embedtest.New(32)).Import(ctx, recs,
		importer.Options{DefaultNamespace: "fallback"})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	want := map[string]int{"alice": 1, "bob": 1, "fallback": 1}
	if len(rep.Namespaces) != len(want) {
		t.Fatalf("namespace histogram = %v, want %v", rep.Namespaces, want)
	}
	for ns, n := range want {
		if rep.Namespaces[ns] != n {
			t.Errorf("namespace %q = %d, want %d (histogram %v)", ns, rep.Namespaces[ns], n, rep.Namespaces)
		}
	}
	// Each mnemory tenant's memories are isolated in their own namespace.
	for ns := range want {
		ms, err := st.List(ctx, ns, store.Filter{}, 0)
		if err != nil {
			t.Fatalf("List %s: %v", ns, err)
		}
		if len(ms) != 1 {
			t.Errorf("namespace %q has %d memories, want 1", ns, len(ms))
		}
	}
}

func TestImportMergeIntoPreservesOriginalNamespace(t *testing.T) {
	ctx := context.Background()
	st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "import.db"), 32)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()

	recs := []importer.Record{
		{ID: "a", Namespace: "alice", Content: "alice's memory about auth"},
		{ID: "b", Namespace: "bob", Content: "bob's memory about billing"},
	}
	rep, err := importer.NewLocal(st, embedtest.New(32)).Import(ctx, recs,
		importer.Options{ForceNamespace: "pool", Source: importer.SourceMem0})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if rep.Namespaces["pool"] != 2 || len(rep.Namespaces) != 1 {
		t.Fatalf("histogram = %v, want all in 'pool'", rep.Namespaces)
	}
	got, err := st.Get(ctx, "pool", "a")
	if err != nil {
		t.Fatalf("Get a: %v", err)
	}
	if got.Metadata["import_source_namespace"] != "alice" {
		t.Errorf("import_source_namespace = %v, want alice (so a split can recover it)", got.Metadata["import_source_namespace"])
	}
	if got.Metadata["import_source"] != "mem0" {
		t.Errorf("import_source = %v, want mem0", got.Metadata["import_source"])
	}
}

func TestImportIdempotentReimport(t *testing.T) {
	ctx := context.Background()
	st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "import.db"), 32)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()

	// No source IDs: deterministic content-addressed IDs make re-import a no-op.
	recs := []importer.Record{
		{Namespace: "p", Content: "first imported memory"},
		{Namespace: "p", Content: "second imported memory"},
	}
	opts := importer.Options{Source: importer.SourceMnemory, SkipExisting: true}
	im := importer.NewLocal(st, embedtest.New(32))

	first, err := im.Import(ctx, recs, opts)
	if err != nil {
		t.Fatalf("first Import: %v", err)
	}
	if first.Imported != 2 || first.Duplicates != 0 {
		t.Fatalf("first run = %+v, want imported=2 duplicates=0", first)
	}
	second, err := im.Import(ctx, recs, opts)
	if err != nil {
		t.Fatalf("second Import: %v", err)
	}
	if second.Imported != 0 || second.Duplicates != 2 {
		t.Fatalf("second run = %+v, want imported=0 duplicates=2", second)
	}
}

func TestImportContentDedup(t *testing.T) {
	ctx := context.Background()
	st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "import.db"), 32)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()

	// Same content under two different ids, plus a distinct record. DedupContent
	// skips the exact repeat before embedding — something id-based SkipExisting
	// can't catch (different ids).
	recs := []importer.Record{
		{Namespace: "p", ID: "a", Content: "the cache is a write-through LRU"},
		{Namespace: "p", ID: "b", Content: "the cache is a write-through LRU"},
		{Namespace: "p", ID: "c", Content: "a wholly different memory"},
	}
	im := importer.NewLocal(st, embedtest.New(32))

	first, err := im.Import(ctx, recs, importer.Options{Source: importer.SourceMnemory, DedupContent: true})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if first.Imported != 2 || first.Duplicates != 1 {
		t.Fatalf("first run = %+v, want imported=2 duplicates=1 (b dups a within the batch)", first)
	}

	// Re-import: a and c are already stored and b still dups a → all duplicates.
	second, err := im.Import(ctx, recs, importer.Options{Source: importer.SourceMnemory, DedupContent: true})
	if err != nil {
		t.Fatalf("reimport: %v", err)
	}
	if second.Imported != 0 || second.Duplicates != 3 {
		t.Fatalf("second run = %+v, want imported=0 duplicates=3", second)
	}
}

func TestImportDefaultImportanceFloor(t *testing.T) {
	ctx := context.Background()
	st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "import.db"), 32)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()

	recs := []importer.Record{
		{ID: "unscored", Namespace: "p", Content: "an unscored bulk import record"},
		{ID: "scored", Namespace: "p", Content: "a source-scored record", Importance: 0.8},
	}
	if _, err := importer.NewLocal(st, embedtest.New(32)).Import(ctx, recs,
		importer.Options{DefaultImportance: 0.25}); err != nil {
		t.Fatalf("Import: %v", err)
	}
	unscored, err := st.Get(ctx, "p", "unscored")
	if err != nil {
		t.Fatalf("Get unscored: %v", err)
	}
	if unscored.Importance != 0.25 {
		t.Errorf("unscored importance = %v, want 0.25 floor", unscored.Importance)
	}
	scored, err := st.Get(ctx, "p", "scored")
	if err != nil {
		t.Fatalf("Get scored: %v", err)
	}
	if scored.Importance != 0.8 {
		t.Errorf("scored importance = %v, want 0.8 preserved", scored.Importance)
	}
}

func TestImportDryRunWritesNothing(t *testing.T) {
	ctx := context.Background()
	st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "import.db"), 32)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()

	recs := []importer.Record{
		{ID: "a", Namespace: "alice", Content: "alice's memory"},
		{ID: "b", Namespace: "bob", Content: "bob's memory"},
	}
	rep, err := importer.NewLocal(st, embedtest.New(32)).Import(ctx, recs,
		importer.Options{DryRun: true})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if rep.Imported != 0 {
		t.Errorf("dry-run imported = %d, want 0", rep.Imported)
	}
	if rep.Namespaces["alice"] != 1 || rep.Namespaces["bob"] != 1 {
		t.Errorf("dry-run histogram = %v, want it to still report destinations", rep.Namespaces)
	}
	if _, err := st.Get(ctx, "alice", "a"); err == nil {
		t.Error("dry-run should not have written 'a'")
	}
}

func TestImportUntypedDefaultsToEpisodic(t *testing.T) {
	ctx := context.Background()
	st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "import.db"), 32)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()

	// No tier set -> should default to episodic (decaying), not durable semantic.
	recs := []importer.Record{{ID: "u", Namespace: "p", Content: "an untyped imported memory"}}
	if _, err := importer.NewLocal(st, embedtest.New(32)).Import(ctx, recs, importer.Options{}); err != nil {
		t.Fatalf("Import: %v", err)
	}

	got, err := st.Get(ctx, "p", "u")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Tier != memory.TierEpisodic {
		t.Fatalf("untyped import tier = %q, want episodic", got.Tier)
	}
	if got.ExpiresAt == nil {
		t.Fatal("episodic import should carry a TTL, got none")
	}
}
