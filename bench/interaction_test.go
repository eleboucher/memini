//go:build bench

package bench_test

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/eleboucher/memini/internal/contradict"
	"github.com/eleboucher/memini/internal/embed"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/service"
	"github.com/eleboucher/memini/internal/store"
	"github.com/eleboucher/memini/internal/store/sqlitevec"
)

// This file measures the INTERACTION of the two default-on write-path
// features — corroboration routing (short-term restatement → grow the durable
// neighbor's confidence, bumping its UpdatedAt) and contradiction routing
// (fresh durable write → invalidate the durable neighbor it contradicts,
// gated on now−UpdatedAt ≥ contradictCooldown). Each was built and measured
// alone; both act on the same durable fact across writes, through the shared
// UpdatedAt stamp. Three suspected failure modes, each driven through the
// real svc.Remember path on the production no-LLM stack:
//
//   - shield: a restatement corroborates the stale fact just before the
//     genuine update arrives, resetting the contradict cooldown — the update
//     is stored but the stale fact is never invalidated;
//   - resurrection: a restatement (or exact re-assertion) of an
//     already-invalidated fact regrows its confidence, or is silently
//     absorbed into the dead row;
//   - race: near-simultaneous restatement + update, SetConfidence vs
//     MarkContradicted on the same row — does the losing order leave a
//     valid_to'd row with regrown confidence?
//
// The pre-fix run confirmed all three (2026-07-05, qwen3/MiniLM/nomic): the
// shield blocked every corroborated topic and was permanent (identical
// retries fingerprint-swallowed, rephrased retries shadowed by the stored
// first update — 0/18 escapes); verbatim re-assertion was absorbed into the
// dead row 23/23; the race hit 1/24 on nomic. The GATE checks below pin the
// fixed behavior (CreatedAt-keyed cooldown, k=3 detector-scanning candidate
// loop, valid_to-filtered GetByFingerprint/SetConfidence) as tripwires.

// interactionSvc builds the write-path service under test: injected clock and
// the production corroboration/contradiction gates (0.70 / 0.625, mirroring
// cmd/memini/root.go). Everything else stays at service defaults — notably
// fingerprint dedup ON, because it is part of the seam.
func interactionSvc(st store.Store, e embed.Embedder, clk func() time.Time) *service.Service {
	return service.New(st, e,
		service.WithClock(clk),
		service.WithCorroboration(0.70),
		service.WithContradictionDownrank(0.625),
	)
}

// rephrasedUpdate is the "user states the update again, in other words" probe:
// same value, different fingerprint. Legs using it gate per topic on the pure
// detector so a phrasing the detector cannot read is reported, not miscounted.
func rephrasedUpdate(tp staleTopic) string {
	return "Heads up — " + tp.update
}

func mustGet(t *testing.T, ctx context.Context, st store.Store, ns, id string) *memory.Memory {
	t.Helper()
	m, err := st.Get(ctx, ns, id)
	if err != nil {
		t.Fatalf("get %s/%s: %v", ns, id, err)
	}
	return m
}

func confOf(m *memory.Memory) float64 {
	if m.Confidence == nil {
		return -1
	}
	return *m.Confidence
}

// TestCorroborationShieldsContradiction: prime hypothesis. Per topic, twin
// namespaces differing only in one short-term restatement 2h before the
// genuine durable update lands. Control measures the detector+gate ceiling;
// the delta between arms is the shield. Two escape legs then ask whether the
// shield ever lifts: an identical update retry after the cooldown lapses
// (fingerprint dedup swallows it before contradiction routing runs?) and a
// rephrased retry (does the stored-but-ineffective first update now shadow
// the stale fact as the top durable neighbor?).
func TestCorroborationShieldsContradiction(t *testing.T) {
	ctx := context.Background()
	embedders := sweepEmbedders(t, ctx)
	if len(embedders) == 0 {
		t.Skip("no sweep embedder reachable")
	}
	t0 := time.Unix(1_700_000_000, 0).UTC()
	day := 24 * time.Hour

	for _, emb := range embedders {
		t.Run(emb.name, func(t *testing.T) {
			st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "shield.db"), emb.dims)
			if err != nil {
				t.Fatalf("open store: %v", err)
			}
			t.Cleanup(func() { _ = st.Close() })

			cur := t0.Add(-30 * day)
			clk := func() time.Time { return cur }
			svc := interactionSvc(st, emb.e, clk)
			arms := []string{"shield", "control"}

			// t0−30d: the stale durable facts, in both arms.
			oldID := map[string]map[string]string{"shield": {}, "control": {}}
			for _, arm := range arms {
				for _, tp := range staleTopics {
					m, rerr := svc.Remember(ctx, service.RememberInput{Namespace: arm, Content: tp.oldFact, Tier: memory.TierSemantic})
					if rerr != nil {
						t.Fatalf("remember old fact %s/%s: %v", arm, tp.key, rerr)
					}
					oldID[arm][tp.key] = m.ID
				}
			}
			svc.WaitBackground()

			// t0−2h: one restatement, shield arm only.
			cur = t0.Add(-2 * time.Hour)
			for _, tp := range staleTopics {
				if _, rerr := svc.Remember(ctx, service.RememberInput{
					Namespace: "shield", Content: staleRestatements(tp)[0], Tier: memory.TierEpisodic,
				}); rerr != nil {
					t.Fatalf("remember restatement %s: %v", tp.key, rerr)
				}
			}
			svc.WaitBackground()
			corroborated := map[string]bool{}
			for _, tp := range staleTopics {
				m := mustGet(t, ctx, st, "shield", oldID["shield"][tp.key])
				corroborated[tp.key] = m.UpdatedAt.Equal(cur) // SetConfidence stamps UpdatedAt
			}

			// t0: the genuine update, both arms.
			cur = t0
			updateID := map[string]string{}
			for _, arm := range arms {
				for _, tp := range staleTopics {
					m, rerr := svc.Remember(ctx, service.RememberInput{Namespace: arm, Content: tp.update, Tier: memory.TierSemantic})
					if rerr != nil {
						t.Fatalf("remember update %s/%s: %v", arm, tp.key, rerr)
					}
					if arm == "shield" {
						updateID[tp.key] = m.ID
					}
				}
			}
			svc.WaitBackground()

			invalidated := func(arm, key string) bool {
				return mustGet(t, ctx, st, arm, oldID[arm][key]).ValidTo != nil
			}
			var ctrlInv, corr, shieldInvCorr, shieldInvUncorr, blocked int
			for _, tp := range staleTopics {
				ctrl := invalidated("control", tp.key)
				if ctrl {
					ctrlInv++
				}
				if corroborated[tp.key] {
					corr++
					switch {
					case invalidated("shield", tp.key):
						shieldInvCorr++
					case ctrl:
						// Invalidates without the restatement, stays live with it:
						// the corroboration shield. Must stay dead post-fix.
						blocked++
						t.Errorf("GATE: %s — corroboration shielded the stale fact from its update", tp.key)
					}
				} else if invalidated("shield", tp.key) {
					shieldInvUncorr++
				}
			}
			n := len(staleTopics)
			t.Logf("control: update invalidated stale fact %d/%d (detector+gate ceiling)", ctrlInv, n)
			t.Logf("shield:  corroborated %d/%d; of those, invalidated %d, shield-blocked %d; uncorroborated invalidated %d",
				corr, n, shieldInvCorr, blocked, shieldInvUncorr)

			// Escape leg 1 — t0+3d (cooldown long lapsed): the identical update
			// again. Fingerprint dedup should swallow it into the stored update
			// before contradiction routing ever runs.
			cur = t0.Add(3 * day)
			var swallowed, escaped1 int
			for _, tp := range staleTopics {
				if !corroborated[tp.key] || invalidated("shield", tp.key) {
					continue
				}
				m, rerr := svc.Remember(ctx, service.RememberInput{Namespace: "shield", Content: tp.update, Tier: memory.TierSemantic})
				if rerr != nil {
					t.Fatalf("remember update retry %s: %v", tp.key, rerr)
				}
				if m.ID == updateID[tp.key] {
					swallowed++
				}
			}
			svc.WaitBackground()
			for _, tp := range staleTopics {
				if corroborated[tp.key] && invalidated("shield", tp.key) {
					escaped1++
				}
			}
			t.Logf("retry identical update at t0+3d: %d swallowed by fingerprint dedup, %d stale facts invalidated", swallowed, escaped1)

			// Escape leg 2 — t0+6d: the update rephrased (new fingerprint). The
			// question is whether the stored first update now shadows the stale
			// fact as the top durable neighbor (k=2, detector reads it as a
			// restatement → no_signal).
			cur = t0.Add(6 * day)
			var eligible, escaped2 int
			for _, tp := range staleTopics {
				if !corroborated[tp.key] || invalidated("shield", tp.key) {
					continue
				}
				re := rephrasedUpdate(tp)
				if memory.Fingerprint(re) == memory.Fingerprint(tp.update) ||
					contradict.Classify(re, tp.oldFact, contradict.Default).Class != contradict.Update {
					t.Logf("  rephrase for %s not detector-eligible, skipping", tp.key)
					continue
				}
				eligible++
				if _, rerr := svc.Remember(ctx, service.RememberInput{Namespace: "shield", Content: re, Tier: memory.TierSemantic}); rerr != nil {
					t.Fatalf("remember rephrased update %s: %v", tp.key, rerr)
				}
				svc.WaitBackground()
				if invalidated("shield", tp.key) {
					escaped2++
				}
			}
			t.Logf("rephrased update at t0+6d: %d/%d eligible retries invalidated the stale fact", escaped2, eligible)
		})
	}
}

// TestContradictedFactResurrection: reverse hypothesis. After a clean
// invalidation, does a short-term restatement of the OLD value regrow the dead
// fact's confidence (vector path), and does an exact durable re-assertion get
// absorbed into the dead row by fingerprint dedup (GetByFingerprint does not
// filter valid_to) — corroborating an invalidated fact and losing the write
// from live recall?
func TestContradictedFactResurrection(t *testing.T) {
	ctx := context.Background()
	embedders := sweepEmbedders(t, ctx)
	if len(embedders) == 0 {
		t.Skip("no sweep embedder reachable")
	}
	t0 := time.Unix(1_700_000_000, 0).UTC()
	day := 24 * time.Hour

	for _, emb := range embedders {
		t.Run(emb.name, func(t *testing.T) {
			st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "resurrect.db"), emb.dims)
			if err != nil {
				t.Fatalf("open store: %v", err)
			}
			t.Cleanup(func() { _ = st.Close() })

			cur := t0.Add(-30 * day)
			clk := func() time.Time { return cur }
			svc := interactionSvc(st, emb.e, clk)
			const ns = "resurrect"

			oldID := map[string]string{}
			for _, tp := range staleTopics {
				m, rerr := svc.Remember(ctx, service.RememberInput{Namespace: ns, Content: tp.oldFact, Tier: memory.TierSemantic})
				if rerr != nil {
					t.Fatalf("remember old fact %s: %v", tp.key, rerr)
				}
				oldID[tp.key] = m.ID
			}
			svc.WaitBackground()

			// t0: the update, no restatement in between → clean invalidation.
			cur = t0
			updateID := map[string]string{}
			for _, tp := range staleTopics {
				m, rerr := svc.Remember(ctx, service.RememberInput{Namespace: ns, Content: tp.update, Tier: memory.TierSemantic})
				if rerr != nil {
					t.Fatalf("remember update %s: %v", tp.key, rerr)
				}
				updateID[tp.key] = m.ID
			}
			svc.WaitBackground()

			dead := map[string]float64{} // topic → confidence right after invalidation
			for _, tp := range staleTopics {
				m := mustGet(t, ctx, st, ns, oldID[tp.key])
				if m.ValidTo != nil {
					dead[tp.key] = confOf(m)
				}
			}
			t.Logf("invalidated %d/%d topics (only these run the resurrection legs)", len(dead), len(staleTopics))

			// t0+2d: a short-term restatement of the OLD value. The dead fact is
			// valid_to-filtered out of corroborate's durable lookup — so who gets
			// corroborated? Nobody, or the NEW fact (nearest live durable)?
			cur = t0.Add(2 * day)
			for _, tp := range staleTopics {
				if _, ok := dead[tp.key]; !ok {
					continue
				}
				if _, rerr := svc.Remember(ctx, service.RememberInput{
					Namespace: ns, Content: staleRestatements(tp)[1], Tier: memory.TierEpisodic,
				}); rerr != nil {
					t.Fatalf("remember stale echo %s: %v", tp.key, rerr)
				}
			}
			svc.WaitBackground()
			var regrown, newCorroborated int
			for _, tp := range staleTopics {
				before, ok := dead[tp.key]
				if !ok {
					continue
				}
				m := mustGet(t, ctx, st, ns, oldID[tp.key])
				if confOf(m) > before+1e-9 {
					regrown++
				}
				if mustGet(t, ctx, st, ns, updateID[tp.key]).UpdatedAt.Equal(cur) {
					newCorroborated++
				}
			}
			t.Logf("stale echo at t0+2d: dead fact confidence regrown on %d/%d; NEW fact corroborated by the OLD value's echo on %d/%d",
				regrown, len(dead), newCorroborated, len(dead))
			if regrown > 0 {
				t.Errorf("GATE: %d invalidated facts regrew confidence from a short-term echo", regrown)
			}

			// t0+4d: the OLD fact re-asserted verbatim as a durable write.
			// GetByFingerprint filters superseded_by and expiry but NOT valid_to,
			// so the dead row can absorb the write: reinforced, corroborated, and
			// returned as the stored memory — while staying out of live recall.
			cur = t0.Add(4 * day)
			var absorbed, deadRegrown, reassertLive, flippedBack int
			for _, tp := range staleTopics {
				before, ok := dead[tp.key]
				if !ok {
					continue
				}
				m, rerr := svc.Remember(ctx, service.RememberInput{Namespace: ns, Content: tp.oldFact, Tier: memory.TierSemantic})
				if rerr != nil {
					t.Fatalf("re-assert old fact %s: %v", tp.key, rerr)
				}
				svc.WaitBackground()
				after := mustGet(t, ctx, st, ns, oldID[tp.key])
				if m != nil && m.ID == oldID[tp.key] {
					absorbed++
					t.Errorf("GATE: %s — verbatim re-assertion absorbed into the invalidated row", tp.key)
				}
				if confOf(after) > before+1e-9 {
					deadRegrown++
					t.Errorf("GATE: %s — invalidated fact's confidence regrew on re-assertion", tp.key)
				}
				if m != nil && m.ID != oldID[tp.key] && m.ValidTo == nil {
					reassertLive++
				}
				// Latest-assertion-wins: the re-asserted old value may in turn
				// invalidate the update. Informative, not gated.
				if mustGet(t, ctx, st, ns, updateID[tp.key]).ValidTo != nil {
					flippedBack++
				}
			}
			t.Logf("verbatim re-assertion at t0+4d: %d/%d absorbed into the invalidated row, dead confidence regrown on %d, stored live on %d, update flipped back on %d",
				absorbed, len(dead), deadRegrown, reassertLive, flippedBack)
		})
	}
}

// TestCorroborateContradictRace: third hypothesis. A restatement and a genuine
// update land near-simultaneously; corroborate's SetConfidence and contradict's
// MarkContradicted race on the same row. The stale fact is 25h old so both
// cooldowns pass without confidence decay: corroborate would store
// GrowConfidence(0.4)=0.46, invalidation stores ≤0.36 — so a valid_to'd row
// with confidence ≥0.40 means SetConfidence landed after MarkContradicted
// (invalidated fact with regrown confidence). Run under -race for the data-race
// angle; this test measures the lost-update angle.
func TestCorroborateContradictRace(t *testing.T) {
	ctx := context.Background()
	embedders := sweepEmbedders(t, ctx)
	if len(embedders) == 0 {
		t.Skip("no sweep embedder reachable")
	}
	t0 := time.Unix(1_700_000_000, 0).UTC()

	for _, emb := range embedders {
		t.Run(emb.name, func(t *testing.T) {
			st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "race.db"), emb.dims)
			if err != nil {
				t.Fatalf("open store: %v", err)
			}
			t.Cleanup(func() { _ = st.Close() })

			cur := t0
			clk := func() time.Time { return cur }
			svc := interactionSvc(st, emb.e, clk)

			// Pre-pass: keep only topics whose update invalidates the 25h-old
			// fact sequentially, so a race-arm "still live" can only mean the
			// corroboration shield, not a detector/gate miss.
			var topics []staleTopic
			for _, tp := range staleTopics {
				ns := "pre-" + tp.key
				cur = t0.Add(-25 * time.Hour)
				a, rerr := svc.Remember(ctx, service.RememberInput{Namespace: ns, Content: tp.oldFact, Tier: memory.TierSemantic})
				if rerr != nil {
					t.Fatalf("pre-pass old fact %s: %v", tp.key, rerr)
				}
				svc.WaitBackground()
				cur = t0
				if _, rerr := svc.Remember(ctx, service.RememberInput{Namespace: ns, Content: tp.update, Tier: memory.TierSemantic}); rerr != nil {
					t.Fatalf("pre-pass update %s: %v", tp.key, rerr)
				}
				svc.WaitBackground()
				if mustGet(t, ctx, st, ns, a.ID).ValidTo != nil {
					topics = append(topics, tp)
				}
			}
			if len(topics) == 0 {
				t.Skip("no topic passes the sequential contradiction pre-pass")
			}
			t.Logf("pre-pass: %d/%d topics invalidate sequentially", len(topics), len(staleTopics))

			const trials = 24
			counts := map[string]int{}
			for trial := range trials {
				tp := topics[trial%len(topics)]
				ns := fmt.Sprintf("race-%d", trial)
				cur = t0.Add(-25 * time.Hour)
				a, rerr := svc.Remember(ctx, service.RememberInput{Namespace: ns, Content: tp.oldFact, Tier: memory.TierSemantic})
				if rerr != nil {
					t.Fatalf("race old fact %s: %v", tp.key, rerr)
				}
				svc.WaitBackground()

				cur = t0
				var wg sync.WaitGroup
				errs := make(chan error, 2)
				wg.Add(2)
				go func() {
					defer wg.Done()
					_, e := svc.Remember(ctx, service.RememberInput{
						Namespace: ns, Content: staleRestatements(tp)[0], Tier: memory.TierEpisodic,
					})
					errs <- e
				}()
				go func() {
					defer wg.Done()
					_, e := svc.Remember(ctx, service.RememberInput{Namespace: ns, Content: tp.update, Tier: memory.TierSemantic})
					errs <- e
				}()
				wg.Wait()
				close(errs)
				for e := range errs {
					if e != nil {
						t.Fatalf("race trial %d: %v", trial, e)
					}
				}
				svc.WaitBackground()

				m := mustGet(t, ctx, st, ns, a.ID)
				switch {
				case m.ValidTo == nil && m.UpdatedAt.Equal(t0):
					counts["live: corroborated first (shield)"]++
				case m.ValidTo == nil:
					counts["live: contradiction missed"]++
				case confOf(m) >= 0.40:
					counts["INVALIDATED+REGROWN (inconsistent)"]++
				default:
					counts["invalidated clean"]++
				}
			}
			for outcome, c := range counts {
				t.Logf("%-38s %d/%d", outcome, c, trials)
			}
			if c := counts["INVALIDATED+REGROWN (inconsistent)"]; c > 0 {
				t.Errorf("GATE: %d/%d trials left an invalidated fact with regrown confidence", c, trials)
			}
			if c := counts["live: corroborated first (shield)"]; c > 0 {
				t.Errorf("GATE: %d/%d trials were shielded by a racing corroboration", c, trials)
			}
		})
	}
}
