package maintenance_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/eleboucher/memini/internal/maintenance"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/store"
	"github.com/eleboucher/memini/internal/store/sqlitevec"
)

func TestPurgeExpired(t *testing.T) {
	ctx := context.Background()
	st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "m.db"), 4)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	now := time.Now().UTC()
	past := now.Add(-time.Hour)

	expired := &memory.Memory{
		ID: "old", Namespace: "ns", Tier: memory.TierWorking, Content: "stale",
		CreatedAt: past, UpdatedAt: past, LastAccessedAt: past, ExpiresAt: &past,
		Embedding: []float32{1, 0, 0, 0},
	}
	live := &memory.Memory{
		ID: "live", Namespace: "ns", Tier: memory.TierSemantic, Content: "fresh",
		CreatedAt: now, UpdatedAt: now, LastAccessedAt: now,
		Embedding: []float32{0, 1, 0, 0},
	}
	for _, m := range []*memory.Memory{expired, live} {
		if err := st.Upsert(ctx, m); err != nil {
			t.Fatalf("upsert %s: %v", m.ID, err)
		}
	}

	n, err := maintenance.PurgeExpired(ctx, st, now)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n != 1 {
		t.Fatalf("purged %d, want 1", n)
	}
	if _, err := st.Get(ctx, "ns", "old"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expired memory should be gone, got %v", err)
	}
	if _, err := st.Get(ctx, "ns", "live"); err != nil {
		t.Fatalf("live memory should remain: %v", err)
	}
}

func openStore(t *testing.T) *sqlitevec.Store {
	t.Helper()
	st, err := sqlitevec.Open(context.Background(), filepath.Join(t.TempDir(), "m.db"), 4)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func put(t *testing.T, st *sqlitevec.Store, id string, tier memory.Tier, content string, importance float64) {
	t.Helper()
	now := time.Now().UTC()
	m := &memory.Memory{
		ID: id, Namespace: "ns", Tier: tier, Content: content, Importance: importance,
		CreatedAt: now, UpdatedAt: now, LastAccessedAt: now, Embedding: []float32{1, 0, 0, 0},
	}
	if err := st.Upsert(context.Background(), m); err != nil {
		t.Fatalf("upsert %s: %v", id, err)
	}
}

func TestEnforceShortTermCap(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)

	// Four short-term (working) memories with increasing importance, plus a
	// durable one that must never be evicted.
	put(t, st, "w1", memory.TierWorking, "a", 0.1)
	put(t, st, "w2", memory.TierWorking, "b", 0.2)
	put(t, st, "w3", memory.TierWorking, "c", 0.3)
	put(t, st, "w4", memory.TierWorking, "d", 0.4)
	put(t, st, "s1", memory.TierSemantic, "durable", 0.0)

	n, err := maintenance.EnforceShortTermCap(ctx, st, 2, time.Now())
	if err != nil {
		t.Fatalf("enforce: %v", err)
	}
	if n != 2 {
		t.Fatalf("evicted %d, want 2", n)
	}
	// Lowest-importance short-term go first; highest-importance + durable remain.
	for id, wantPresent := range map[string]bool{"w1": false, "w2": false, "w3": true, "w4": true, "s1": true} {
		_, err := st.Get(ctx, "ns", id)
		present := err == nil
		if present != wantPresent {
			t.Errorf("%s present=%v, want %v", id, present, wantPresent)
		}
	}
}

func TestFsckDetectsDuplicates(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	put(t, st, "d1", memory.TierSemantic, "the sky is blue", 0)
	put(t, st, "d2", memory.TierSemantic, "The   sky is   blue", 0) // same after normalization
	put(t, st, "u1", memory.TierSemantic, "grass is green", 0)

	rep, err := maintenance.Fsck(ctx, st, 0, time.Now())
	if err != nil {
		t.Fatalf("fsck: %v", err)
	}
	if len(rep.DuplicateGroups) != 1 || len(rep.DuplicateGroups[0]) != 2 {
		t.Fatalf("expected one duplicate group of 2, got %v", rep.DuplicateGroups)
	}
}
