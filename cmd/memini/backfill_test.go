package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/eleboucher/memini/internal/maintenance"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/store/sqlitevec"
)

func TestBackfillPreviewDoesNotWrite(t *testing.T) {
	ctx := context.Background()
	st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "m.db"), 4)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	now := time.Now().UTC().Truncate(time.Millisecond)
	legacy := &memory.Memory{
		ID: "legacy", Namespace: "ns", Tier: memory.TierSemantic, Content: "x",
		CreatedAt: now, UpdatedAt: now, LastAccessedAt: now,
		Embedding: []float32{1, 0, 0, 0},
	}
	if err := st.Upsert(ctx, legacy); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	rep, err := maintenance.BackfillConfidencePreview(ctx, st, now)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if rep.Seeded != 1 {
		t.Fatalf("preview seeded = %d, want 1", rep.Seeded)
	}
	got, err := st.Get(ctx, "ns", "legacy")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Confidence != nil {
		t.Errorf("preview must not write; confidence = %v", got.Confidence)
	}
	if !got.UpdatedAt.Equal(now) {
		t.Errorf("preview bumped updated_at: got %v", got.UpdatedAt)
	}
}

func TestBackfillApplySeedsAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "m.db"), 4)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	now := time.Now().UTC().Truncate(time.Millisecond)
	m := &memory.Memory{
		ID: "f", Namespace: "ns", Tier: memory.TierSemantic, Content: "x",
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
		t.Fatalf("first seeded = %d, want 1", first.Seeded)
	}
	got, _ := st.Get(ctx, "ns", "f")
	if got.Confidence == nil || *got.Confidence != memory.ConfidenceSeedImported {
		t.Errorf("after apply, confidence = %v, want %v", got.Confidence, memory.ConfidenceSeedImported)
	}

	second, err := maintenance.BackfillConfidence(ctx, st, now)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if second.Seeded != 0 {
		t.Errorf("second run seeded = %d, want 0", second.Seeded)
	}
	if second.Skipped != 1 {
		t.Errorf("second run skipped = %d, want 1", second.Skipped)
	}
}
