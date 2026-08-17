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
	add("default", old, 0.6, 0, nil, conf(0.25))               // demote: tier-seed importance is not a keep signal
	add("used", old, 0.2, 5, nil, conf(0.25))                  // keep: recalled
	add("important", old, 0.9, 0, nil, conf(0.25))             // keep: important
	add("threshold", old, 0.75, 0, nil, conf(0.25))            // keep: at the importance bar
	add("pinned", old, 0.2, 0, []string{"pinned"}, conf(0.25)) // keep: pinned
	add("recent", now, 0.2, 0, nil, conf(0.25))                // keep: too new
	add("legacy", old, 0.2, 0, nil, nil)                       // keep: untracked confidence = trusted

	n, err := maintenance.DemoteStale(ctx, st, now.Add(-60*24*time.Hour), now, nil)
	if err != nil {
		t.Fatalf("demote: %v", err)
	}
	if n != 2 {
		t.Fatalf("demoted %d, want 2 (debris + default)", n)
	}
	for _, id := range []string{"debris", "default"} {
		got, err := st.Get(ctx, "ns", id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		if got.Tier != memory.TierEpisodic || got.ExpiresAt == nil {
			t.Errorf("%s should be episodic with a TTL, got tier=%q expires=%v", id, got.Tier, got.ExpiresAt)
		}
	}
	for _, id := range []string{"used", "important", "threshold", "pinned", "recent", "legacy"} {
		m, err := st.Get(ctx, "ns", id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		if m.Tier != memory.TierSemantic {
			t.Errorf("%s should stay semantic, got %q", id, m.Tier)
		}
	}
}

// TestDemoteStaleReportsPerDemotion pins the metrics hook: DemoteStale calls
// report once per demoted memory with the tier it was demoted FROM, so the
// hourly demotion volume is observable (memini_demoted_total) instead of
// silent. A nil report must be tolerated.
func TestDemoteStaleReportsPerDemotion(t *testing.T) {
	ctx := context.Background()
	st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "m.db"), 4)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	now := time.Now().UTC()
	old := now.Add(-100 * 24 * time.Hour)
	conf := func(c float64) *float64 { return &c }
	for _, m := range []*memory.Memory{
		{ID: "sem", Namespace: "ns", Tier: memory.TierSemantic, Content: "sem", Importance: 0.2,
			CreatedAt: old, UpdatedAt: old, LastAccessedAt: old, Confidence: conf(0.25), Embedding: []float32{1, 0, 0, 0}},
		{ID: "proc", Namespace: "ns", Tier: memory.TierProcedural, Content: "proc", Importance: 0.2,
			CreatedAt: old, UpdatedAt: old, LastAccessedAt: old, Confidence: conf(0.25), Embedding: []float32{0, 1, 0, 0}},
	} {
		if err := st.Upsert(ctx, m); err != nil {
			t.Fatalf("upsert %s: %v", m.ID, err)
		}
	}

	got := map[string]int{}
	n, err := maintenance.DemoteStale(ctx, st, now.Add(-60*24*time.Hour), now,
		func(fromTier string) { got[fromTier]++ })
	if err != nil {
		t.Fatalf("demote: %v", err)
	}
	if n != 2 {
		t.Fatalf("demoted %d, want 2", n)
	}
	if got["semantic"] != 1 || got["procedural"] != 1 {
		t.Fatalf("report calls = %v, want one per from-tier", got)
	}

	// nil report is fine (nothing left to demote, but the path must not panic).
	if _, err := maintenance.DemoteStale(ctx, st, now.Add(-60*24*time.Hour), now, nil); err != nil {
		t.Fatalf("nil report: %v", err)
	}
}
