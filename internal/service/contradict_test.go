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
	t.Helper()
	m, err := f.svc.Remember(context.Background(), service.RememberInput{
		Namespace: "n", Content: content, Tier: memory.TierSemantic,
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
// old fact's last update is skipped.
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
