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

func TestForgetByTag(t *testing.T) {
	ctx := context.Background()
	st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "m.db"), 4)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	now := time.Now().UTC()
	add := func(id string, tags []string) {
		m := &memory.Memory{
			ID: id, Namespace: "ns", Tier: memory.TierSemantic, Content: id,
			CreatedAt: now, UpdatedAt: now, LastAccessedAt: now, Tags: tags,
			Embedding: []float32{1, 0, 0, 0},
		}
		if err := st.Upsert(ctx, m); err != nil {
			t.Fatalf("upsert %s: %v", id, err)
		}
	}
	add("a", []string{"import:mem0:2026-06-12"})
	add("b", []string{"import:mem0:2026-06-12"})
	add("c", []string{"manual"})

	n, err := maintenance.ForgetByTag(ctx, st, "ns", "import:mem0:2026-06-12")
	if err != nil {
		t.Fatalf("forget: %v", err)
	}
	if n != 2 {
		t.Fatalf("deleted %d, want 2", n)
	}
	// The untagged memory survives — a non-matching tag must not delete everything.
	if _, err := st.Get(ctx, "ns", "c"); err != nil {
		t.Errorf("untagged memory must survive: %v", err)
	}
	if _, err := st.Get(ctx, "ns", "a"); err != store.ErrNotFound {
		t.Errorf("tagged memory should be deleted, got %v", err)
	}

	// A tag that matches nothing deletes nothing.
	n, err = maintenance.ForgetByTag(ctx, st, "ns", "nonexistent")
	if err != nil {
		t.Fatalf("forget nonexistent: %v", err)
	}
	if n != 0 {
		t.Fatalf("non-matching tag deleted %d, want 0", n)
	}
}
