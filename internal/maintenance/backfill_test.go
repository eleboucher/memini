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

func TestBackfillConfidenceSeedsNilDurable(t *testing.T) {
	ctx := context.Background()
	st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "m.db"), 4)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	now := time.Now().UTC().Truncate(time.Millisecond)
	conf := func(c float64) *float64 { return &c }
	exp := now.Add(24 * time.Hour)
	add := func(id string, tier memory.Tier, confidence *float64) {
		m := &memory.Memory{
			ID: id, Namespace: "ns", Tier: tier, Content: id,
			CreatedAt: now, UpdatedAt: now, LastAccessedAt: now,
			Confidence: confidence, Embedding: []float32{1, 0, 0, 0},
		}
		if tier.Term() == memory.ShortTerm {
			m.ExpiresAt = &exp
		}
		if err := st.Upsert(ctx, m); err != nil {
			t.Fatalf("upsert %s: %v", id, err)
		}
	}

	add("legacy-sem", memory.TierSemantic, nil)
	add("legacy-proc", memory.TierProcedural, nil)
	add("fresh-sem", memory.TierSemantic, conf(0.5))
	add("short-eps", memory.TierEpisodic, nil)
	add("short-work", memory.TierWorking, nil)

	rep, err := maintenance.BackfillConfidence(ctx, st, now)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if rep.Inspected != 3 {
		t.Errorf("inspected = %d, want 3", rep.Inspected)
	}
	if rep.Seeded != 2 {
		t.Errorf("seeded = %d, want 2", rep.Seeded)
	}
	if rep.Skipped != 1 {
		t.Errorf("skipped = %d, want 1", rep.Skipped)
	}

	for _, id := range []string{"legacy-sem", "legacy-proc"} {
		got, err := st.Get(ctx, "ns", id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		if got.Confidence == nil || *got.Confidence != memory.ConfidenceSeedImported {
			t.Errorf("%s: confidence = %v, want %v", id, got.Confidence, memory.ConfidenceSeedImported)
		}
		if !got.UpdatedAt.Equal(now) {
			t.Errorf("%s: updated_at = %v, want %v", id, got.UpdatedAt, now)
		}
	}

	fresh, err := st.Get(ctx, "ns", "fresh-sem")
	if err != nil {
		t.Fatalf("get fresh-sem: %v", err)
	}
	if fresh.Confidence == nil || *fresh.Confidence != 0.5 {
		t.Errorf("tracked memory reseeded, got confidence=%v", fresh.Confidence)
	}

	for _, id := range []string{"short-eps", "short-work"} {
		got, err := st.Get(ctx, "ns", id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		if got.Confidence != nil {
			t.Errorf("%s: confidence should be nil, got %v", id, *got.Confidence)
		}
		if !got.UpdatedAt.Equal(now) {
			t.Errorf("%s: updated_at bumped to %v", id, got.UpdatedAt)
		}
	}
}

func TestBackfillConfidenceIdempotent(t *testing.T) {
	ctx := context.Background()
	st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "m.db"), 4)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	now := time.Now().UTC()
	m := &memory.Memory{
		ID: "f", Namespace: "ns", Tier: memory.TierSemantic, Content: "f",
		CreatedAt: now, UpdatedAt: now, LastAccessedAt: now,
		Embedding: []float32{1, 0, 0, 0},
	}
	if err := st.Upsert(ctx, m); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	first, err := maintenance.BackfillConfidence(ctx, st, now)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if first.Seeded != 1 {
		t.Fatalf("first seeded %d, want 1", first.Seeded)
	}

	second, err := maintenance.BackfillConfidence(ctx, st, now)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if second.Seeded != 0 {
		t.Errorf("second seeded %d, want 0", second.Seeded)
	}
	if second.Skipped != 1 {
		t.Errorf("second skipped %d, want 1", second.Skipped)
	}
}

func TestBackfillConfidenceEmptyStore(t *testing.T) {
	ctx := context.Background()
	st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "m.db"), 4)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	rep, err := maintenance.BackfillConfidence(ctx, st, time.Now().UTC())
	if err != nil {
		t.Fatalf("backfill empty: %v", err)
	}
	if rep.Inspected != 0 || rep.Seeded != 0 || rep.Skipped != 0 {
		t.Errorf("empty store should report zeros, got %+v", rep)
	}
}
