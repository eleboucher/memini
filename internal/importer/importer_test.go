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
