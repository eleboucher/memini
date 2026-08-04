package maintenance_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/eleboucher/memini/internal/llm"
	"github.com/eleboucher/memini/internal/maintenance"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/store/sqlitevec"
)

const assessTestNS = "ns"

// assessResult is one canned reply from fakeAssessor, in call order.
type assessResult struct {
	scores []*float64
	err    error
}

// fakeAssessor replays canned per-call results and records what it was asked,
// so a test can assert both the batching and the selection that fed it.
type fakeAssessor struct {
	results []assessResult
	calls   [][]string
}

var _ llm.ImportanceAssessor = (*fakeAssessor)(nil)

func (f *fakeAssessor) AssessImportance(_ context.Context, contents []string) ([]*float64, error) {
	f.calls = append(f.calls, slices.Clone(contents))
	if len(f.results) == 0 {
		// Default: rate everything the same, so tests that only care about
		// selection or budget need no canned script.
		out := make([]*float64, len(contents))
		for i := range out {
			out[i] = new(0.42)
		}
		return out, nil
	}
	r := f.results[0]
	f.results = f.results[1:]
	return r.scores, r.err
}

func quietOpts(o maintenance.AssessOptions) maintenance.AssessOptions {
	o.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	return o
}

// openAssessStore opens an empty store for one test.
func openAssessStore(t *testing.T) *sqlitevec.Store {
	t.Helper()
	st, err := sqlitevec.Open(t.Context(), filepath.Join(t.TempDir(), "m.db"), 4)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// assessSeed describes a row to write; zero fields take a sensible default
// (durable tier at its seed importance, old enough to be eligible).
type assessSeed struct {
	id         string
	namespace  string
	tier       memory.Tier
	importance float64
	assessed   *float64
	createdAt  time.Time
}

func putAssessRow(t *testing.T, st *sqlitevec.Store, s assessSeed) {
	t.Helper()
	m := &memory.Memory{
		ID:                 s.id,
		Namespace:          s.namespace,
		Tier:               s.tier,
		Content:            s.id + " content",
		Importance:         s.importance,
		AssessedImportance: s.assessed,
		CreatedAt:          s.createdAt,
		UpdatedAt:          s.createdAt,
		LastAccessedAt:     s.createdAt,
		Embedding:          []float32{1, 0, 0, 0},
	}
	if err := st.Upsert(t.Context(), m); err != nil {
		t.Fatalf("upsert %s: %v", s.id, err)
	}
}

func getAssessed(t *testing.T, st *sqlitevec.Store, ns, id string) *float64 {
	t.Helper()
	m, err := st.Get(t.Context(), ns, id)
	if err != nil {
		t.Fatalf("get %s: %v", id, err)
	}
	return m.AssessedImportance
}

func wantAssessed(t *testing.T, st *sqlitevec.Store, id string, want float64) {
	t.Helper()
	got := getAssessed(t, st, assessTestNS, id)
	if got == nil {
		t.Fatalf("%s assessed_importance = nil, want %v", id, want)
	}
	if *got != want {
		t.Errorf("%s assessed_importance = %v, want %v", id, *got, want)
	}
}

func wantUnassessed(t *testing.T, st *sqlitevec.Store, id string) {
	t.Helper()
	if got := getAssessed(t, st, assessTestNS, id); got != nil {
		t.Errorf("%s assessed_importance = %v, want nil (left for a later pass)", id, *got)
	}
}

// TestAssessBackfillSelection pins every arm of the candidate filter: the sweep
// fills in durable rows that never got an assessment and leaves everything else
// — already assessed, too young, explicitly-set importance, non-durable tier —
// untouched.
func TestAssessBackfillSelection(t *testing.T) {
	st := openAssessStore(t)
	now := time.Now().UTC()
	old := now.Add(-48 * time.Hour)
	seed := memory.SeedImportance(memory.TierSemantic)

	putAssessRow(t, st, assessSeed{id: "eligible", namespace: assessTestNS,
		tier: memory.TierSemantic, importance: seed, createdAt: old})
	putAssessRow(t, st, assessSeed{id: "eligible-proc", namespace: assessTestNS,
		tier: memory.TierProcedural, importance: memory.SeedImportance(memory.TierProcedural), createdAt: old})
	putAssessRow(t, st, assessSeed{id: "already", namespace: assessTestNS,
		tier: memory.TierSemantic, importance: seed, assessed: new(0.7), createdAt: old})
	putAssessRow(t, st, assessSeed{id: "young", namespace: assessTestNS,
		tier: memory.TierSemantic, importance: seed, createdAt: now.Add(-time.Minute)})
	putAssessRow(t, st, assessSeed{id: "explicit", namespace: assessTestNS,
		tier: memory.TierSemantic, importance: 0.85, createdAt: old})
	putAssessRow(t, st, assessSeed{id: "episodic", namespace: assessTestNS,
		tier: memory.TierEpisodic, importance: memory.SeedImportance(memory.TierEpisodic), createdAt: old})
	putAssessRow(t, st, assessSeed{id: "working", namespace: assessTestNS,
		tier: memory.TierWorking, importance: memory.SeedImportance(memory.TierWorking), createdAt: old})

	fake := &fakeAssessor{}
	n, err := maintenance.AssessImportanceBackfill(t.Context(), st, fake, quietOpts(maintenance.AssessOptions{}), now)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if n != 2 {
		t.Fatalf("assessed %d rows, want 2 (eligible + eligible-proc)", n)
	}
	if len(fake.calls) != 1 || len(fake.calls[0]) != 2 {
		t.Fatalf("assessor calls = %v, want one call of 2 contents", fake.calls)
	}
	wantAssessed(t, st, "eligible", 0.42)
	wantAssessed(t, st, "eligible-proc", 0.42)
	for _, id := range []string{"young", "explicit", "episodic", "working"} {
		wantUnassessed(t, st, id)
	}
	// The pre-existing assessment must survive untouched, not be re-rated.
	wantAssessed(t, st, "already", 0.7)
}

// TestAssessBackfillBudget pins that MaxPerRun bounds one pass across every
// namespace (not per namespace), that the excess is chunked into Batch-sized
// calls, and that the rows picked are the oldest ones.
func TestAssessBackfillBudget(t *testing.T) {
	st := openAssessStore(t)
	now := time.Now().UTC()
	seed := memory.SeedImportance(memory.TierSemantic)

	// Three namespaces, four rows each. Ages interleave across namespaces so a
	// per-namespace cap (or a per-namespace ordering) would pick a different set.
	namespaces := []string{"ns-a", "ns-b", "ns-c"}
	age := 100
	var oldest []string
	for i := range 4 {
		for _, ns := range namespaces {
			id := ns + "-" + string(rune('0'+i))
			putAssessRow(t, st, assessSeed{id: id, namespace: ns, tier: memory.TierSemantic,
				importance: seed, createdAt: now.Add(-time.Duration(age) * time.Hour)})
			if age > 95 { // ages 100..96: the five the budget should reach
				oldest = append(oldest, id)
			}
			age--
		}
	}

	fake := &fakeAssessor{}
	n, err := maintenance.AssessImportanceBackfill(t.Context(), st, fake,
		quietOpts(maintenance.AssessOptions{Batch: 2, MaxPerRun: 5}), now)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if n != 5 {
		t.Fatalf("assessed %d rows, want 5 (MaxPerRun)", n)
	}
	if got := []int{len(fake.calls[0]), len(fake.calls[1]), len(fake.calls[2])}; len(fake.calls) != 3 ||
		got[0] != 2 || got[1] != 2 || got[2] != 1 {
		t.Fatalf("batch sizes = %v (%d calls), want 2,2,1", got, len(fake.calls))
	}
	// The five oldest rows are exactly the ones stamped.
	assessedCount := 0
	for _, ns := range namespaces {
		for i := range 4 {
			id := ns + "-" + string(rune('0'+i))
			got := getAssessed(t, st, ns, id)
			wantSet := slices.Contains(oldest, id)
			if wantSet && got == nil {
				t.Errorf("%s should have been assessed (oldest %d)", id, len(oldest))
			}
			if !wantSet && got != nil {
				t.Errorf("%s assessed %v, want nil (outside the budget)", id, *got)
			}
			if got != nil {
				assessedCount++
			}
		}
	}
	if assessedCount != 5 {
		t.Errorf("%d rows carry an assessment, want 5", assessedCount)
	}
}

// TestAssessBackfillFirstBatchFailureAborts pins the "provider is down" path:
// the pass gives up after the first failure rather than hammering the LLM with
// every remaining batch, writes nothing, and reports no error — deferring until
// the next tick is the expected outcome, not a fault.
func TestAssessBackfillFirstBatchFailureAborts(t *testing.T) {
	st := openAssessStore(t)
	now := time.Now().UTC()
	seed := memory.SeedImportance(memory.TierSemantic)
	for i := range 4 {
		putAssessRow(t, st, assessSeed{id: "m" + string(rune('0'+i)), namespace: assessTestNS,
			tier: memory.TierSemantic, importance: seed, createdAt: now.Add(-time.Duration(100-i) * time.Hour)})
	}

	fake := &fakeAssessor{results: []assessResult{{err: errors.New("connection refused")}}}
	n, err := maintenance.AssessImportanceBackfill(t.Context(), st, fake,
		quietOpts(maintenance.AssessOptions{Batch: 2}), now)
	if err != nil {
		t.Fatalf("backfill: %v (a down LLM defers, it does not fail the pass)", err)
	}
	if n != 0 {
		t.Fatalf("assessed %d rows, want 0", n)
	}
	if len(fake.calls) != 1 {
		t.Errorf("assessor called %d times, want 1 (pass aborts after the first failure)", len(fake.calls))
	}
	for i := range 4 {
		wantUnassessed(t, st, "m"+string(rune('0'+i)))
	}
}

// TestAssessBackfillLaterBatchFailureKeepsEarlier pins that a mid-pass failure
// is local: the batches that already landed keep their scores and the failed
// one is simply left for the next pass.
func TestAssessBackfillLaterBatchFailureKeepsEarlier(t *testing.T) {
	st := openAssessStore(t)
	now := time.Now().UTC()
	seed := memory.SeedImportance(memory.TierSemantic)
	for i := range 4 {
		putAssessRow(t, st, assessSeed{id: "m" + string(rune('0'+i)), namespace: assessTestNS,
			tier: memory.TierSemantic, importance: seed, createdAt: now.Add(-time.Duration(100-i) * time.Hour)})
	}

	fake := &fakeAssessor{results: []assessResult{
		{scores: []*float64{new(0.5), new(0.6)}},
		{err: errors.New("rate limited")},
	}}
	n, err := maintenance.AssessImportanceBackfill(t.Context(), st, fake,
		quietOpts(maintenance.AssessOptions{Batch: 2}), now)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if n != 2 {
		t.Fatalf("assessed %d rows, want 2 (the batch that succeeded)", n)
	}
	wantAssessed(t, st, "m0", 0.5)
	wantAssessed(t, st, "m1", 0.6)
	wantUnassessed(t, st, "m2")
	wantUnassessed(t, st, "m3")
}

// TestAssessBackfillNilScoreStaysNull pins that a declined item is not a failed
// one: its row keeps a NULL assessment (so a later pass retries it) while the
// rest of the batch is written normally.
func TestAssessBackfillNilScoreStaysNull(t *testing.T) {
	st := openAssessStore(t)
	now := time.Now().UTC()
	seed := memory.SeedImportance(memory.TierSemantic)
	putAssessRow(t, st, assessSeed{id: "declined", namespace: assessTestNS,
		tier: memory.TierSemantic, importance: seed, createdAt: now.Add(-100 * time.Hour)})
	putAssessRow(t, st, assessSeed{id: "rated", namespace: assessTestNS,
		tier: memory.TierSemantic, importance: seed, createdAt: now.Add(-99 * time.Hour)})

	fake := &fakeAssessor{results: []assessResult{{scores: []*float64{nil, new(0.55)}}}}
	n, err := maintenance.AssessImportanceBackfill(t.Context(), st, fake,
		quietOpts(maintenance.AssessOptions{}), now)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if n != 1 {
		t.Fatalf("assessed %d rows, want 1 (the declined one is not counted)", n)
	}
	wantUnassessed(t, st, "declined")
	wantAssessed(t, st, "rated", 0.55)
}

// TestAssessBackfillClamps pins that a model rating outside [0.1, 0.9] is
// bounded before it reaches the store, matching the write path's clamp.
func TestAssessBackfillClamps(t *testing.T) {
	st := openAssessStore(t)
	now := time.Now().UTC()
	seed := memory.SeedImportance(memory.TierSemantic)
	putAssessRow(t, st, assessSeed{id: "high", namespace: assessTestNS,
		tier: memory.TierSemantic, importance: seed, createdAt: now.Add(-100 * time.Hour)})
	putAssessRow(t, st, assessSeed{id: "low", namespace: assessTestNS,
		tier: memory.TierSemantic, importance: seed, createdAt: now.Add(-99 * time.Hour)})

	fake := &fakeAssessor{results: []assessResult{{scores: []*float64{new(0.99), new(0.01)}}}}
	if _, err := maintenance.AssessImportanceBackfill(t.Context(), st, fake,
		quietOpts(maintenance.AssessOptions{}), now); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	wantAssessed(t, st, "high", 0.9)
	wantAssessed(t, st, "low", 0.1)
}

// TestAssessBackfillWrongLengthIsABatchError pins the positional contract at
// the job level too: scores that do not line up one-to-one with the batch are
// unusable, so the batch is discarded rather than mis-attributed.
func TestAssessBackfillWrongLengthIsABatchError(t *testing.T) {
	st := openAssessStore(t)
	now := time.Now().UTC()
	seed := memory.SeedImportance(memory.TierSemantic)
	for i := range 4 {
		putAssessRow(t, st, assessSeed{id: "m" + string(rune('0'+i)), namespace: assessTestNS,
			tier: memory.TierSemantic, importance: seed, createdAt: now.Add(-time.Duration(100-i) * time.Hour)})
	}

	fake := &fakeAssessor{results: []assessResult{
		{scores: []*float64{new(0.5), new(0.6)}},
		{scores: []*float64{new(0.7)}}, // one score for a batch of two
	}}
	n, err := maintenance.AssessImportanceBackfill(t.Context(), st, fake,
		quietOpts(maintenance.AssessOptions{Batch: 2}), now)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if n != 2 {
		t.Fatalf("assessed %d rows, want 2 (only the well-formed batch)", n)
	}
	wantAssessed(t, st, "m0", 0.5)
	wantAssessed(t, st, "m1", 0.6)
	wantUnassessed(t, st, "m2")
	wantUnassessed(t, st, "m3")
}

// TestAssessJobDisabled pins the two off-switches: no interval and no assessor
// both make Run return immediately rather than block on a ticker.
func TestAssessJobDisabled(t *testing.T) {
	st := openAssessStore(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, tc := range []struct {
		name     string
		assessor llm.ImportanceAssessor
		interval time.Duration
	}{
		{"zero interval", &fakeAssessor{}, 0},
		{"no assessor", nil, time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			done := make(chan struct{})
			go func() {
				defer close(done)
				maintenance.NewAssessJob(st, tc.assessor, log, tc.interval, maintenance.AssessOptions{}).
					Run(t.Context())
			}()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("Run did not return; a disabled job must be a no-op")
			}
		})
	}
}
