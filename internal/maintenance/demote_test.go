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

	conf := func(c float64) *float64 { return &c }
	add := func(id string, updated time.Time, imp float64, access int, tags []string, confidence *float64) {
		m := &memory.Memory{
			ID: id, Namespace: "ns", Tier: memory.TierSemantic, Content: id, Importance: imp,
			AccessCount: access, CreatedAt: updated, UpdatedAt: updated, LastAccessedAt: updated,
			Tags: tags, Confidence: confidence, Embedding: []float32{1, 0, 0, 0},
		}
		if err := st.Upsert(ctx, m); err != nil {
			t.Fatalf("upsert %s: %v", id, err)
		}
	}
	add("debris", old, 0.2, 0, nil, conf(0.25))                // demote: old, unused, low importance + confidence
	add("used", old, 0.2, 5, nil, conf(0.25))                  // keep: recalled
	add("important", old, 0.9, 0, nil, conf(0.25))             // keep: important
	add("pinned", old, 0.2, 0, []string{"pinned"}, conf(0.25)) // keep: pinned
	add("recent", now, 0.2, 0, nil, conf(0.25))                // keep: too new
	add("legacy", old, 0.2, 0, nil, nil)                       // keep: untracked confidence = trusted

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
	for _, id := range []string{"used", "important", "pinned", "recent", "legacy"} {
		m, err := st.Get(ctx, "ns", id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		if m.Tier != memory.TierSemantic {
			t.Errorf("%s should stay semantic, got %q", id, m.Tier)
		}
	}
}
