package maintenance_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eleboucher/memini/internal/maintenance"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/store"
	"github.com/eleboucher/memini/internal/store/sqlitevec"
)

// seedPooled writes memories into one namespace, each tagged with the source
// namespace a botched --merge-into import would have preserved in metadata.
func seedPooled(t *testing.T, st store.Store, ns string, srcByID map[string]string) {
	t.Helper()
	now := time.Now().UTC()
	i := 0
	for id, src := range srcByID {
		m := &memory.Memory{
			ID: id, Namespace: ns, Tier: memory.TierEpisodic, Content: "memory " + id,
			CreatedAt: now, UpdatedAt: now, LastAccessedAt: now,
			Embedding: []float32{float32(i + 1), 0, 0, 0},
		}
		if src != "" {
			m.Metadata = map[string]any{"import_source_namespace": src}
		}
		if err := st.Upsert(context.Background(), m); err != nil {
			t.Fatalf("upsert %s: %v", id, err)
		}
		i++
	}
}

func TestSplitRecoversPooledNamespaces(t *testing.T) {
	ctx := context.Background()
	st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "m.db"), 4)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	seedPooled(t, st, "pool", map[string]string{
		"a1": "alice", "a2": "alice", "b1": "bob", "orphan": "",
	})

	// Dry-run reports the grouping without moving anything.
	dry, err := maintenance.Split(ctx, st, "pool", nil, true)
	if err != nil {
		t.Fatalf("split dry-run: %v", err)
	}
	if dry.Moved != 3 || dry.Skipped != 1 || dry.Targets["alice"] != 2 || dry.Targets["bob"] != 1 {
		t.Fatalf("dry-run report = %+v, want alice=2 bob=1 moved=3 skipped=1", dry)
	}
	if _, err := st.Get(ctx, "alice", "a1"); err == nil {
		t.Fatal("dry-run must not move anything")
	}

	// Apply, then assert isolation: each tenant's memories live in their own ns.
	rep, err := maintenance.Split(ctx, st, "pool", nil, false)
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	if rep.Moved != 3 || rep.Skipped != 1 {
		t.Fatalf("split report = %+v, want moved=3 skipped=1", rep)
	}
	for ns, want := range map[string]int{"alice": 2, "bob": 1, "pool": 1} {
		mems, err := st.List(ctx, ns, store.Filter{IncludeSuperseded: true, IncludeExpired: true}, 0)
		if err != nil {
			t.Fatalf("list %s: %v", ns, err)
		}
		if len(mems) != want {
			t.Errorf("namespace %q has %d memories, want %d", ns, len(mems), want)
		}
	}
	// The orphan (no grouping key) stayed in the pool.
	if _, err := st.Get(ctx, "pool", "orphan"); err != nil {
		t.Errorf("orphan should remain in pool: %v", err)
	}
}

func TestMoveRelocatesNamespace(t *testing.T) {
	ctx := context.Background()
	st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "m.db"), 4)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	seedPooled(t, st, "old", map[string]string{"x": "", "y": ""})

	rep, err := maintenance.Move(ctx, st, "old", "new", false)
	if err != nil {
		t.Fatalf("move: %v", err)
	}
	if rep.Moved != 2 {
		t.Fatalf("move report = %+v, want moved=2", rep)
	}
	mems, err := st.List(ctx, "new", store.Filter{IncludeSuperseded: true, IncludeExpired: true}, 0)
	if err != nil {
		t.Fatalf("list new: %v", err)
	}
	if len(mems) != 2 {
		t.Errorf("new namespace has %d memories, want 2", len(mems))
	}
}

func TestSplitSkipsInvalidTargets(t *testing.T) {
	ctx := context.Background()
	st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "m.db"), 4)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	// Targets come from stored metadata, so hostile or malformed values must
	// stay put instead of minting unaddressable namespaces. The slash-wrapped
	// value is valid after normalization and lands in "alice".
	seedPooled(t, st, "pool", map[string]string{
		"ok":      " alice/ ",
		"toolong": strings.Repeat("x", 300),
		"nulbyte": "bad\x00ns",
		"pattern": "work/*",
		"slashes": "///",
	})

	rep, err := maintenance.Split(ctx, st, "pool", nil, false)
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	if rep.Moved != 1 || rep.Skipped != 4 {
		t.Fatalf("split report = %+v, want moved=1 skipped=4", rep)
	}
	if _, err := st.Get(ctx, "alice", "ok"); err != nil {
		t.Errorf("normalized target should land in alice: %v", err)
	}
}
