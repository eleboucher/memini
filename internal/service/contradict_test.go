package service_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/eleboucher/memini/internal/embed/embedtest"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/service"
	"github.com/eleboucher/memini/internal/store/sqlitevec"
)

// contradictFixture builds a service over a mutable clock so a test can advance
// past contradictCooldown between the old write and the contradicting one.
type contradictFixture struct {
	svc *service.Service
	st  *sqlitevec.Store
	now *time.Time
}

func newContradictFixture(t *testing.T, opts ...service.Option) contradictFixture {
	t.Helper()
	ctx := context.Background()
	st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "svc.db"), dims)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	now := time.Unix(1_700_000_000, 0).UTC()
	all := make([]service.Option, 0, 2+len(opts))
	all = append(all, service.WithSyncReinforce(), service.WithClock(func() time.Time { return now }))
	all = append(all, opts...)
	return contradictFixture{svc: service.New(st, embedtest.New(dims), all...), st: st, now: &now}
}

func (f contradictFixture) remember(t *testing.T, content string) *memory.Memory {
	return f.rememberTier(t, content, memory.TierSemantic)
}

func (f contradictFixture) rememberTier(t *testing.T, content string, tier memory.Tier) *memory.Memory {
	t.Helper()
	m, err := f.svc.Remember(context.Background(), service.RememberInput{
		Namespace: "n", Content: content, Tier: tier,
	})
	if err != nil {
		t.Fatalf("remember %q: %v", content, err)
	}
	f.svc.WaitBackground()
	return m
}

func (f contradictFixture) get(t *testing.T, id string) *memory.Memory {
	t.Helper()
	m, err := f.svc.Get(context.Background(), "n", id)
	if err != nil {
		t.Fatalf("get %s: %v", id, err)
	}
	return m
}

const (
	oldFact    = "Decision: the primary datastore is Postgres."
	updateFact = "Decision: the primary datastore is MySQL."
)

// TestContradictionInvalidatesStaleFact is the happy path: a fresh durable write
// that contradicts an entrenched durable fact stamps the old fact's valid_to
// (dropping it from live recall) and shrinks its confidence.
func TestContradictionInvalidatesStaleFact(t *testing.T) {
	f := newContradictFixture(t, service.WithContradictionDownrank(0.5))
	old := f.remember(t, oldFact)

	*f.now = f.now.Add(48 * time.Hour) // clear the 24h cooldown
	f.remember(t, updateFact)

	got := f.get(t, old.ID)
	if got.ValidTo == nil {
		t.Fatal("contradicting write should stamp the stale fact's valid_to")
	}
	if got.Confidence == nil || *got.Confidence >= memory.ConfidenceSeedFresh {
		t.Errorf("stale confidence = %v, want shrunk below seed %v", got.Confidence, memory.ConfidenceSeedFresh)
	}
	if got.Metadata["contradicted_by"] == nil {
		t.Error("stale fact should record contradicted_by for audit")
	}

	// The invalidated fact drops out of live recall; the fresh one remains.
	res, err := f.svc.Recall(context.Background(), service.RecallInput{Namespace: "n", Query: "primary datastore", Limit: 5})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	for _, r := range res {
		if r.Memory.ID == old.ID {
			t.Errorf("invalidated fact should not appear in live recall, got %v", idsOf(res))
		}
	}
}

// TestContradictionIgnoresRestatement: a reworded restatement of a durable fact
// is not a contradiction — the old fact must stay live.
func TestContradictionIgnoresRestatement(t *testing.T) {
	f := newContradictFixture(t, service.WithContradictionDownrank(0.5))
	old := f.remember(t, oldFact)

	*f.now = f.now.Add(48 * time.Hour)
	f.remember(t, "Decision: we standardized on Postgres as the primary datastore.")

	if got := f.get(t, old.ID); got.ValidTo != nil {
		t.Errorf("restatement must not invalidate the fact, valid_to = %v", got.ValidTo)
	}
}

// TestContradictionCooldown: a contradiction within the cooldown window of the
// old fact's creation is skipped.
func TestContradictionCooldown(t *testing.T) {
	f := newContradictFixture(t, service.WithContradictionDownrank(0.5))
	old := f.remember(t, oldFact)
	f.remember(t, updateFact) // same instant → inside cooldown

	if got := f.get(t, old.ID); got.ValidTo != nil {
		t.Errorf("contradiction inside cooldown must be skipped, valid_to = %v", got.ValidTo)
	}
}

// TestContradictionKillSwitch: with the downrank disabled (minScore 0), nothing
// is invalidated.
func TestContradictionKillSwitch(t *testing.T) {
	f := newContradictFixture(t) // no WithContradictionDownrank → disabled
	old := f.remember(t, oldFact)

	*f.now = f.now.Add(48 * time.Hour)
	f.remember(t, updateFact)

	if got := f.get(t, old.ID); got.ValidTo != nil {
		t.Errorf("kill-switch off: no invalidation expected, valid_to = %v", got.ValidTo)
	}
}

// TestContradictionUnblockedByCorroboration: a short-term restatement
// corroborates the stale fact (bumping its UpdatedAt) right before the genuine
// update lands. The cooldown keys on CreatedAt, so the update must still
// invalidate — keying on UpdatedAt let a daily-restated stale fact shield
// itself from every update (bench/interaction_test.go).
func TestContradictionUnblockedByCorroboration(t *testing.T) {
	f := newContradictFixture(t,
		service.WithContradictionDownrank(0.5), service.WithCorroboration(0.5))
	old := f.remember(t, oldFact)

	*f.now = f.now.Add(48 * time.Hour)
	f.rememberTier(t, "Reminder: "+oldFact, memory.TierEpisodic)
	if got := f.get(t, old.ID); !got.UpdatedAt.Equal(*f.now) {
		t.Fatalf("setup: restatement should corroborate the fact, UpdatedAt = %v", got.UpdatedAt)
	}

	*f.now = f.now.Add(2 * time.Hour) // well inside a last-touched window
	f.remember(t, updateFact)
	if got := f.get(t, old.ID); got.ValidTo == nil {
		t.Fatal("update must invalidate the stale fact even right after corroboration")
	}
}

// TestContradictionScansPastShadowingUpdate: an earlier write of the same
// update value (blocked by the cooldown, but stored) sits closer to a
// rephrased retry than the stale fact does. The candidate scan must continue
// past that restatement-classified neighbor and invalidate the stale fact.
func TestContradictionScansPastShadowingUpdate(t *testing.T) {
	f := newContradictFixture(t, service.WithContradictionDownrank(0.5))
	old := f.remember(t, oldFact)
	f.remember(t, updateFact) // same instant → inside cooldown, blocked but stored
	if got := f.get(t, old.ID); got.ValidTo != nil {
		t.Fatalf("setup: first update inside cooldown should not invalidate")
	}

	*f.now = f.now.Add(48 * time.Hour)
	f.remember(t, "Decision: the primary datastore is now MySQL.")
	if got := f.get(t, old.ID); got.ValidTo == nil {
		t.Fatal("retry must scan past the earlier same-value update and invalidate the stale fact")
	}
}

// TestReassertedContradictedFactStoresLiveRow: re-asserting a contradicted
// fact verbatim must store a fresh live memory — not be absorbed into the
// invalidated row by fingerprint dedup, which would regrow confidence on a
// valid_to'd fact and silently drop the write from live recall.
func TestReassertedContradictedFactStoresLiveRow(t *testing.T) {
	f := newContradictFixture(t, service.WithContradictionDownrank(0.5))
	old := f.remember(t, oldFact)

	*f.now = f.now.Add(48 * time.Hour)
	f.remember(t, updateFact)
	dead := f.get(t, old.ID)
	if dead.ValidTo == nil {
		t.Fatal("setup: update should invalidate the old fact")
	}
	deadConf := *dead.Confidence

	*f.now = f.now.Add(48 * time.Hour)
	re := f.remember(t, oldFact)
	if re.ID == old.ID {
		t.Fatal("re-assertion must not be absorbed into the invalidated row")
	}
	if re.ValidTo != nil {
		t.Errorf("re-assertion should be live, valid_to = %v", re.ValidTo)
	}
	if got := f.get(t, old.ID); *got.Confidence != deadConf {
		t.Errorf("invalidated row's confidence changed: %v → %v", deadConf, *got.Confidence)
	}
}
