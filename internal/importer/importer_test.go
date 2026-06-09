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
