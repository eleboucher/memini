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

func TestDemoteStale(t *testing.T) {
	ctx := context.Background()
	st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "m.db"), 4)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	now := time.Now().UTC()
	old := now.Add(-100 * 24 * time.Hour)

	add := func(id string, updated time.Time, imp float64, access int, tags []string) {
		m := &memory.Memory{
			ID: id, Namespace: "ns", Tier: memory.TierSemantic, Content: id, Importance: imp,
			AccessCount: access, CreatedAt: updated, UpdatedAt: updated, LastAccessedAt: updated,
			Tags: tags, Embedding: []float32{1, 0, 0, 0},
		}
		if err := st.Upsert(ctx, m); err != nil {
			t.Fatalf("upsert %s: %v", id, err)
		}
	}
	add("debris", old, 0.2, 0, nil)                // demote: old, unused, low importance
	add("used", old, 0.2, 5, nil)                  // keep: recalled
	add("important", old, 0.9, 0, nil)             // keep: important
	add("pinned", old, 0.2, 0, []string{"pinned"}) // keep: pinned
	add("recent", now, 0.2, 0, nil)                // keep: too new

	n, err := maintenance.DemoteStale(ctx, st, now.Add(-60*24*time.Hour), now)
	if err != nil {
		t.Fatalf("demote: %v", err)
	}
	if n != 1 {
		t.Fatalf("demoted %d, want 1 (only debris)", n)
	}
	got, err := st.Get(ctx, "ns", "debris")
	if err != nil {
		t.Fatalf("get debris: %v", err)
	}
	if got.Tier != memory.TierEpisodic || got.ExpiresAt == nil {
		t.Errorf("debris should be episodic with a TTL, got tier=%q expires=%v", got.Tier, got.ExpiresAt)
	}
	for _, id := range []string{"used", "important", "pinned", "recent"} {
		m, err := st.Get(ctx, "ns", id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		if m.Tier != memory.TierSemantic {
			t.Errorf("%s should stay semantic, got %q", id, m.Tier)
		}
	}
}
