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

func TestScrub(t *testing.T) {
	ctx := context.Background()
	st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "m.db"), 4)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	base := time.Now().UTC().Add(-time.Hour)
	add := func(id, ns, content string, created time.Time) {
		m := &memory.Memory{
			ID: id, Namespace: ns, Tier: memory.TierEpisodic, Content: content,
			CreatedAt: created, UpdatedAt: created, LastAccessedAt: created,
			Embedding: []float32{1, 0, 0, 0},
		}
		if err := st.Upsert(ctx, m); err != nil {
			t.Fatalf("upsert %s: %v", id, err)
		}
	}

	add("life1", "ns", "Session ended in ns (reason: other)", base)
	add("life2", "ns", "Stop checkpoint in ns", base)
	add("dup-old", "ns", "the cache is a write-through LRU", base)
	add("dup-new", "ns", "the cache is a write-through LRU", base.Add(time.Minute)) // exact dup, newer
	add("dup-ws", "ns", "The  cache is\na write-through   LRU", base.Add(2*time.Minute))
	add("keep", "ns", "postgres is the primary store", base)
	add("other-ns", "other", "the cache is a write-through LRU", base) // same content, different ns: kept

	// Preview must not mutate.
	prev, err := maintenance.Scrub(ctx, st, false)
	if err != nil {
		t.Fatalf("scrub preview: %v", err)
	}
	if prev.LifecycleNoise != 2 || prev.ExactDuplicates != 2 {
		t.Fatalf("preview = %+v, want lifecycle=2 exact=2", prev)
	}
	if all, _ := st.List(ctx, "ns", store.Filter{}, 0); len(all) != 6 {
		t.Fatalf("preview mutated the store: %d rows remain in ns", len(all))
	}

	// Apply.
	rep, err := maintenance.Scrub(ctx, st, true)
	if err != nil {
		t.Fatalf("scrub apply: %v", err)
	}
	if rep.LifecycleNoise != 2 || rep.ExactDuplicates != 2 {
		t.Fatalf("apply = %+v, want lifecycle=2 exact=2", rep)
	}

	// The oldest of the duplicate group survives; lifecycle markers are gone.
	if _, err := st.Get(ctx, "ns", "dup-old"); err != nil {
		t.Errorf("oldest duplicate should survive: %v", err)
	}
	for _, gone := range []string{"life1", "life2", "dup-new", "dup-ws"} {
		if _, err := st.Get(ctx, "ns", gone); err == nil {
			t.Errorf("%s should have been scrubbed", gone)
		}
	}
	if _, err := st.Get(ctx, "ns", "keep"); err != nil {
		t.Errorf("unique memory should survive: %v", err)
	}
	if _, err := st.Get(ctx, "other", "other-ns"); err != nil {
		t.Errorf("duplicate in a different namespace should survive: %v", err)
	}
}
