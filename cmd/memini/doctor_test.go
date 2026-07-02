package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/store"
	"github.com/eleboucher/memini/internal/store/sqlitevec"
)

// seedPool writes n memories into namespace ns; attributable of them carry an
// import_source_namespace metadata key (so a split can re-home them).
func seedPool(t *testing.T, st store.Store, ns string, n, attributable int) {
	t.Helper()
	now := time.Now().UTC()
	for i := range n {
		m := &memory.Memory{
			ID: ns + "/" + strconv.Itoa(i), Namespace: ns, Tier: memory.TierEpisodic,
			Content: "memory " + strconv.Itoa(i), CreatedAt: now, UpdatedAt: now, LastAccessedAt: now,
			Embedding: []float32{1, 0, 0, 0},
		}
		if i < attributable {
			m.Metadata = map[string]any{"import_source_namespace": "tenant-" + strconv.Itoa(i%3)}
		}
		if err := st.Upsert(context.Background(), m); err != nil {
			t.Fatalf("upsert: %v", err)
		}
	}
}

func openTestStore(t *testing.T) store.Store {
	t.Helper()
	st, err := sqlitevec.Open(context.Background(), filepath.Join(t.TempDir(), "doctor.db"), 4)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func statsFor(t *testing.T, st store.Store) []nsStat {
	t.Helper()
	s, err := namespaceStats(context.Background(), st)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	return s
}

// TestDoctorFixPreviewWritesNothing: a preview (apply=false) must not mutate the
// store even when the pool is splittable.
func TestDoctorFixPreviewWritesNothing(t *testing.T) {
	st := openTestStore(t)
	seedPool(t, st, nsDefault, 600, 600) // fully attributable, over threshold
	var out bytes.Buffer

	if err := doctorFix(context.Background(), &out, statsFor(t, st), fixDeps{store: st, now: time.Now().UTC(), apply: false}); err != nil {
		t.Fatalf("doctorFix preview: %v", err)
	}
	names, _ := st.ListNamespaces(context.Background())
	if len(names) != 1 || names[0] != nsDefault {
		t.Fatalf("preview must not move anything; namespaces = %v", names)
	}
	if !strings.Contains(out.String(), "preview") {
		t.Errorf("expected a preview banner, got: %s", out.String())
	}
}

// TestDoctorFixSplitsAttributablePool: with >=90% attributable and apply=true,
// the pool is split into per-tenant namespaces.
func TestDoctorFixSplitsAttributablePool(t *testing.T) {
	st := openTestStore(t)
	seedPool(t, st, nsDefault, 600, 600)
	var out bytes.Buffer

	if err := doctorFix(context.Background(), &out, statsFor(t, st), fixDeps{store: st, now: time.Now().UTC(), apply: true}); err != nil {
		t.Fatalf("doctorFix apply: %v", err)
	}
	names, _ := st.ListNamespaces(context.Background())
	// 3 tenant namespaces (tenant-0/1/2); the default pool should be drained.
	for _, n := range names {
		if n == nsDefault {
			mems, _ := st.List(context.Background(), nsDefault, store.Filter{IncludeSuperseded: true, IncludeExpired: true}, 0)
			if len(mems) != 0 {
				t.Errorf("default should be drained, still has %d", len(mems))
			}
		}
	}
	if !strings.Contains(out.String(), "split") {
		t.Errorf("expected a split line, got: %s", out.String())
	}
}

// TestDoctorFixSkipsUnattributablePool: below the 90% attribution bar, the split
// is skipped and the pool is left intact (manual inspection advised).
func TestDoctorFixSkipsUnattributablePool(t *testing.T) {
	st := openTestStore(t)
	seedPool(t, st, nsDefault, 600, 100) // only ~17% attributable
	var out bytes.Buffer

	if err := doctorFix(context.Background(), &out, statsFor(t, st), fixDeps{store: st, now: time.Now().UTC(), apply: true}); err != nil {
		t.Fatalf("doctorFix: %v", err)
	}
	mems, _ := st.List(context.Background(), nsDefault, store.Filter{IncludeSuperseded: true, IncludeExpired: true}, 0)
	if len(mems) != 600 {
		t.Errorf("unattributable pool must be left intact, got %d of 600", len(mems))
	}
	if !strings.Contains(out.String(), "skip split") {
		t.Errorf("expected a skip-split line, got: %s", out.String())
	}
}

// TestDoctorFixBackfillsLegacyConfidence: the fix chain seeds nil confidence on
// durable memories written before 0.0.11, so they enter the demote lifecycle.
func TestDoctorFixBackfillsLegacyConfidence(t *testing.T) {
	st := openTestStore(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	m := &memory.Memory{
		ID: "legacy", Namespace: "nsDefault", Tier: memory.TierSemantic, Content: "x",
		CreatedAt: now, UpdatedAt: now, LastAccessedAt: now,
		Embedding: []float32{1, 0, 0, 0},
	}
	if err := st.Upsert(context.Background(), m); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	var out bytes.Buffer
	if err := doctorFix(context.Background(), &out, statsFor(t, st), fixDeps{store: st, now: now, apply: true}); err != nil {
		t.Fatalf("doctorFix: %v", err)
	}
	got, err := st.Get(context.Background(), "nsDefault", "legacy")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Confidence == nil || *got.Confidence != memory.ConfidenceSeedImported {
		t.Errorf("legacy durable memory should be backfilled, got confidence=%v", got.Confidence)
	}
	if !strings.Contains(out.String(), "backfilled") {
		t.Errorf("expected a backfilled line, got: %s", out.String())
	}
}

func TestPrintWritePathSignals(t *testing.T) {
	var buf strings.Builder
	printWritePathSignals(&buf, []nsStat{
		{namespace: "a", classified: 2, promoted: 1, corroborated: 3},
		{namespace: "b", classified: 1},
	})
	out := buf.String()
	for _, want := range []string{"marker-classified durable writes:  3", "promotion-produced facts:          1", "corroborated durable memories:     3"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}

	buf.Reset()
	printWritePathSignals(&buf, []nsStat{{namespace: "a"}})
	if buf.Len() != 0 {
		t.Fatalf("all-zero signals should print nothing, got:\n%s", buf.String())
	}
}
