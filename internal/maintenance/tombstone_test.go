package maintenance_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/eleboucher/memini/internal/maintenance"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/store"
	"github.com/eleboucher/memini/internal/store/sqlitevec"
)

func TestPurgeTombstones(t *testing.T) {
	ctx := context.Background()
	st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "m.db"), 4)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	now := time.Now().UTC()
	old := now.Add(-100 * 24 * time.Hour)
	rep := "rep"

	// The grace period is measured from valid_to (when the row was tombstoned),
	// not its content updated_at — superseding never bumps updated_at. gcOld is
	// freshly updated but was tombstoned long ago (purge); gcRecent has stale
	// content but was tombstoned just now (keep); live is never superseded.
	mk := func(id string, updated time.Time) *memory.Memory {
		return &memory.Memory{
			ID: id, Namespace: "ns", Tier: memory.TierSemantic, Content: id,
			CreatedAt: updated, UpdatedAt: updated, LastAccessedAt: updated,
			Embedding: []float32{1, 0, 0, 0},
		}
	}
	gcOld := mk("gcOld", now)
	gcOld.SupersededBy, gcOld.ValidTo = &rep, &old
	gcRecent := mk("gcRecent", old)
	gcRecent.SupersededBy, gcRecent.ValidTo = &rep, &now
	for _, m := range []*memory.Memory{mk("rep", now), mk("live", old), gcOld, gcRecent} {
		if err := st.Upsert(ctx, m); err != nil {
			t.Fatalf("upsert %s: %v", m.ID, err)
		}
	}

	// GC tombstones older than 30 days: only gcOld qualifies.
	n, err := maintenance.PurgeTombstones(ctx, st, now.Add(-30*24*time.Hour))
	if err != nil {
		t.Fatalf("purge tombstones: %v", err)
	}
	if n != 1 {
		t.Fatalf("purged %d tombstones, want 1 (only the old one)", n)
	}
	if _, err := st.Get(ctx, "ns", "gcOld"); err != store.ErrNotFound {
		t.Errorf("gcOld should be hard-deleted, got %v", err)
	}
	if _, err := st.Get(ctx, "ns", "gcRecent"); err != nil {
		t.Errorf("gcRecent (recent tombstone) should survive: %v", err)
	}
	if _, err := st.Get(ctx, "ns", "live"); err != nil {
		t.Errorf("live memory must never be GC'd: %v", err)
	}
}
