// Validates docs/examples/memory-life-story.md.
//
// One fact, birth to supersession: the narrative test walks the exact stages
// the doc tells — auto-classification, exact-restatement reinforcement, the
// episodic value gate, a merge hint on a reworded near-duplicate, recall
// reinforcement, a correction that supersedes, as_of time travel, and the
// history chain. Every payload/response the doc quotes is asserted here.
package service_test

import (
	"context"
	"testing"
	"time"

	"github.com/eleboucher/memini/internal/embed/embedtest"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/service"
)

func TestExampleMemoryLifeStory(t *testing.T) {
	ctx := context.Background()
	const ns = "acme/payments"

	// The fact whose life we follow, and the players around it.
	const fact = "We decided to deploy the payments service with rolling updates " +
		"instead of blue-green, the reason is the database migrations must run exactly once."
	const reworded = "We chose rolling updates rather than blue-green for the " +
		"payments service because the database migrations must run exactly once."
	const correction = "We decided to go with blue-green deploys for the payments " +
		"service instead of rolling updates because migrations kept colliding mid-rollout."
	const query = "rolling updates for the payments service"

	// A controllable clock: each stage happens an hour after the previous one,
	// so as_of time travel has unambiguous instants to aim at.
	t0 := time.Unix(1_700_000_000, 0).UTC()
	clock := t0
	svc := service.New(openTestStore(t), embedtest.New(dims),
		service.WithSyncReinforce(),
		// The write-time value gate the doc's stage 3 demonstrates.
		service.WithEpisodicMinChars(80),
		// The fuzzy vector dedup gate in hint mode: stage 4's near-duplicate
		// lands in the hint band instead of being silently merged.
		service.WithWriteDedup(0.5, service.WriteDedupHint),
		service.WithClock(func() time.Time { return clock }),
	)

	// State shared across the ordered stages.
	var born *memory.Memory      // the original fact
	var corrected *memory.Memory // the correction that supersedes it
	var asOfBeforeCorrection time.Time

	t.Run("1 remember with tier omitted is auto-classified", func(t *testing.T) {
		var eff memory.Tier
		m, err := svc.Remember(ctx, service.RememberInput{
			Namespace: ns, Content: fact, EffectiveTier: &eff,
		})
		if err != nil || m == nil {
			t.Fatalf("remember: m=%v err=%v", m, err)
		}
		if m.Tier != memory.TierSemantic || eff != memory.TierSemantic {
			t.Fatalf("tier = %q (effective %q), want %q", m.Tier, eff, memory.TierSemantic)
		}
		if v, _ := m.Metadata["tier_classified"].(string); v != "marker" {
			t.Fatalf("metadata tier_classified = %v, want %q", m.Metadata["tier_classified"], "marker")
		}
		if m.ExpiresAt != nil {
			t.Fatalf("a classified durable fact must not expire, got %v", m.ExpiresAt)
		}
		if m.ID == "" {
			t.Fatal("stored memory must carry an id")
		}
		born = m
	})

	t.Run("2 exact restatement reinforces instead of duplicating", func(t *testing.T) {
		if born == nil {
			t.Fatal("stage 1 failed")
		}
		clock = clock.Add(time.Hour)
		var reinforced bool
		m, err := svc.Remember(ctx, service.RememberInput{
			Namespace: ns, Content: fact, Reinforced: &reinforced,
		})
		if err != nil || m == nil {
			t.Fatalf("remember again: m=%v err=%v", m, err)
		}
		if !reinforced {
			t.Fatal("an exact restatement must report reinforced=true")
		}
		if m.ID != born.ID {
			t.Fatalf("restatement id = %q, want the existing %q", m.ID, born.ID)
		}
		all, err := svc.List(ctx, service.ListInput{Namespace: ns, Tiers: []memory.Tier{memory.TierSemantic}})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(all) != 1 {
			t.Fatalf("restatement must not create a duplicate row, got %d semantic memories", len(all))
		}
	})

	t.Run("3 low-signal episodic write is dropped by the value gate", func(t *testing.T) {
		clock = clock.Add(time.Hour)
		var eff memory.Tier
		m, err := svc.Remember(ctx, service.RememberInput{
			Namespace: ns, Content: "ok keep going", Tier: memory.TierEpisodic, EffectiveTier: &eff,
		})
		if err != nil {
			t.Fatalf("a gate drop is accepted-not-stored, never an error: %v", err)
		}
		if m != nil {
			t.Fatalf("trivial episodic write should be dropped (nil), got %q", m.Content)
		}
		if eff != memory.TierEpisodic {
			t.Fatalf("dropped write must still report the resolved tier, got %q", eff)
		}
	})

	t.Run("4 reworded near-duplicate returns a merge hint", func(t *testing.T) {
		if born == nil {
			t.Fatal("stage 1 failed")
		}
		clock = clock.Add(time.Hour)
		var hint service.MergeHint
		dup, err := svc.Remember(ctx, service.RememberInput{
			Namespace: ns, Content: reworded, MergeHint: &hint,
		})
		if err != nil || dup == nil {
			t.Fatalf("remember reworded: m=%v err=%v", dup, err)
		}
		if hint.SimilarID != born.ID {
			t.Fatalf("hint.SimilarID = %q, want the original %q", hint.SimilarID, born.ID)
		}
		if hint.Score < 0.5 || hint.Score >= 1 {
			t.Fatalf("hint.Score = %v, want within the hint band [0.5, 1)", hint.Score)
		}
		if hint.Tier != memory.TierSemantic {
			t.Fatalf("hint.Tier = %q, want %q", hint.Tier, memory.TierSemantic)
		}
		if dup.ID == born.ID {
			t.Fatal("hint mode stores the write; it must not coalesce")
		}
		// Act on the hint the way an agent would: the fresh copy adds nothing
		// over the original, so fold it away and keep the original.
		if err := svc.Forget(ctx, ns, dup.ID); err != nil {
			t.Fatalf("forget duplicate: %v", err)
		}
	})

	t.Run("5 recall reinforces what it returns", func(t *testing.T) {
		if born == nil {
			t.Fatal("stage 1 failed")
		}
		clock = clock.Add(time.Hour)
		before, err := svc.Get(ctx, ns, born.ID)
		if err != nil {
			t.Fatalf("get before: %v", err)
		}
		res, err := svc.Recall(ctx, service.RecallInput{Namespace: ns, Query: query, Limit: 5})
		if err != nil {
			t.Fatalf("recall: %v", err)
		}
		if !containsID(res, born.ID) {
			t.Fatalf("recall should surface the fact, got %v", idsOf(res))
		}
		after, err := svc.Get(ctx, ns, born.ID)
		if err != nil {
			t.Fatalf("get after: %v", err)
		}
		if after.AccessCount <= before.AccessCount {
			t.Fatalf("recall must reinforce: access_count %d -> %d", before.AccessCount, after.AccessCount)
		}
		// The doc quotes the exact counter movement: stage 2's fingerprint
		// reinforcement left it at 1, this recall moves it to 2.
		if before.AccessCount != 1 || after.AccessCount != 2 {
			t.Fatalf("access_count moved %d -> %d, doc quotes 1 -> 2", before.AccessCount, after.AccessCount)
		}
	})

	t.Run("6 a correction supersedes the original", func(t *testing.T) {
		if born == nil {
			t.Fatal("stage 1 failed")
		}
		asOfBeforeCorrection = clock // the world as stage 5 knew it
		clock = clock.Add(time.Hour)
		m, err := svc.Remember(ctx, service.RememberInput{Namespace: ns, Content: correction})
		if err != nil || m == nil {
			t.Fatalf("remember correction: m=%v err=%v", m, err)
		}
		if m.Tier != memory.TierSemantic {
			t.Fatalf("correction tier = %q, want %q", m.Tier, memory.TierSemantic)
		}
		corrected = m
		if err := svc.Supersede(ctx, ns, born.ID, corrected.ID); err != nil {
			t.Fatalf("supersede: %v", err)
		}
		res, err := svc.Recall(ctx, service.RecallInput{Namespace: ns, Query: query, Limit: 5})
		if err != nil {
			t.Fatalf("recall after correction: %v", err)
		}
		if len(res) != 1 || res[0].Memory.ID != corrected.ID {
			t.Fatalf("live recall must return only the correction, got %v", idsOf(res))
		}
	})

	t.Run("7 as_of before the correction returns the original belief", func(t *testing.T) {
		if born == nil || corrected == nil {
			t.Fatal("earlier stage failed")
		}
		res, err := svc.Recall(ctx, service.RecallInput{
			Namespace: ns, Query: query, Limit: 5, AsOf: asOfBeforeCorrection,
		})
		if err != nil {
			t.Fatalf("recall as_of: %v", err)
		}
		if !containsID(res, born.ID) {
			t.Fatalf("as_of before the correction must surface the original, got %v", idsOf(res))
		}
		if containsID(res, corrected.ID) {
			t.Fatalf("as_of before the correction must not surface the correction, got %v", idsOf(res))
		}
	})

	t.Run("8 history shows the chain oldest-first", func(t *testing.T) {
		if born == nil || corrected == nil {
			t.Fatal("earlier stage failed")
		}
		hist, err := svc.History(ctx, ns, corrected.ID)
		if err != nil {
			t.Fatalf("history: %v", err)
		}
		if len(hist) != 2 {
			t.Fatalf("history length = %d, want 2 (original + correction)", len(hist))
		}
		if hist[0].ID != born.ID || hist[1].ID != corrected.ID {
			t.Fatalf("history order = [%s %s], want oldest-first [%s %s]",
				hist[0].ID, hist[1].ID, born.ID, corrected.ID)
		}
		if hist[0].SupersededBy == nil || *hist[0].SupersededBy != corrected.ID {
			t.Fatalf("original SupersededBy = %v, want %q", hist[0].SupersededBy, corrected.ID)
		}
		// The chain reads the same from either end.
		fromOld, err := svc.History(ctx, ns, born.ID)
		if err != nil {
			t.Fatalf("history from the original: %v", err)
		}
		if len(fromOld) != 2 || fromOld[0].ID != hist[0].ID || fromOld[1].ID != hist[1].ID {
			t.Fatalf("history from the original differs: %v", fromOld)
		}
	})
}
