package maintenance_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/eleboucher/memini/internal/maintenance"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/store/sqlitevec"
)

func TestRepairSupersession(t *testing.T) {
	ctx := context.Background()
	st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "m.db"), 4)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	now := time.Now().UTC()
	ptr := func(s string) *string { return &s }
	mk := func(id string, sup *string) *memory.Memory {
		return &memory.Memory{
			ID: id, Namespace: "ns", Tier: memory.TierEpisodic, Content: id,
			CreatedAt: now, UpdatedAt: now, LastAccessedAt: now,
			Embedding: []float32{1, 0, 0, 0}, SupersededBy: sup,
		}
	}
	rows := []*memory.Memory{
		mk("live", nil),                      // live head
		mk("ok", ptr("live")),                // healthy: reaches a live head
		mk("danglingHead", ptr("gone")),      // target missing -> stranded
		mk("chainTail", ptr("danglingHead")), // chains to a stranded row -> stranded
		mk("cycA", ptr("cycB")),              // cycle -> stranded
		mk("cycB", ptr("cycA")),
	}
	for _, m := range rows {
		if err := st.Upsert(ctx, m); err != nil {
			t.Fatalf("upsert %s: %v", m.ID, err)
		}
	}

	dry, err := maintenance.RepairSupersession(ctx, st, []string{"ns"}, true, nil)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if dry.Restored != 4 {
		t.Fatalf("dry-run restored = %d, want 4 (danglingHead, chainTail, cycA, cycB)", dry.Restored)
	}
	if got, _ := st.Get(ctx, "ns", "danglingHead"); got.SupersededBy == nil {
		t.Fatal("dry run must not mutate")
	}

	rep, err := maintenance.RepairSupersession(ctx, st, []string{"ns"}, false, nil)
	if err != nil {
		t.Fatalf("repair: %v", err)
	}
	if rep.Restored != 4 {
		t.Fatalf("restored = %d, want 4", rep.Restored)
	}
	live := func(id string) bool { m, _ := st.Get(ctx, "ns", id); return m.SupersededBy == nil }
	for _, id := range []string{"danglingHead", "chainTail", "cycA", "cycB"} {
		if !live(id) {
			t.Errorf("%s should be restored to live", id)
		}
	}
	if live("ok") {
		t.Error("ok was superseded into a live head; it must stay tombstoned")
	}
}
