package maintenance

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/store"
)

// PinnedTag marks a memory as pinned: exempt from retro-tiering demotion and
// always surfaced in a session briefing.
const PinnedTag = "pinned"

// demoteConfidenceFloor is the corroboration below which an old, unused durable
// memory is demoted. Memories without tracked confidence (legacy rows, and any
// short-term memory) report 1.0, so they are always above the floor and never
// demoted on this account — only uncorroborated, never-recalled facts fall.
const demoteConfidenceFloor = memory.ConfidenceDemoteFloor

// longTermTiers are the durable tiers eligible for demotion.
var longTermTiers = []memory.Tier{memory.TierSemantic, memory.TierProcedural}

// DemoteStale demotes durable (semantic/procedural) memories that have never
// been recalled (AccessCount 0), were last updated before olderThan, and are
// neither highly important (>= 0.75) nor pinned, down to the episodic tier —
// giving them the episodic TTL. Unused "durable" debris (e.g. a low-quality bulk
// import, or a default-importance fact that proved useless) then ages out on its
// own, while anything recalled even once is reinforced and kept. The mirror
// image of promotion. Returns the count demoted. report, when non-nil, is
// called once per demoted memory with the tier it was demoted FROM — the
// sweeper wires it to the memini_demoted_total counter so demotion volume is
// observable rather than log-only.
func DemoteStale(ctx context.Context, st store.Store, olderThan, now time.Time, report func(fromTier string)) (int, error) {
	namespaces, err := st.ListNamespaces(ctx)
	if err != nil {
		return 0, err
	}
	exp := now.Add(memory.TierEpisodic.DefaultTTL())
	total := 0
	for _, ns := range namespaces {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		mems, err := st.List(ctx, ns, store.Filter{Tiers: longTermTiers, Now: now}, 0)
		if err != nil {
			return total, err
		}
		for _, m := range mems {
			// Anything ever recalled, marked important, explicitly trusted
			// (corroborated/legacy memories read as fully confident), or pinned is
			// kept — only old, unused, low-confidence durable debris demotes.
			// The 0.75 importance bar sits above the 0.6 tier seed, so
			// default-importance memories are not immune to demotion. It reads raw
			// Importance deliberately, not EffectiveImportance: an assessed 0.9 does
			// not immunize a memory from demotion, because switching this silently
			// would widen demote immunity across a whole backfilled corpus. Moving it
			// to the assessed signal is a deliberate follow-up, not a side effect.
			if m.AccessCount > 0 || m.Importance >= 0.75 {
				continue
			}
			if !m.UpdatedAt.Before(olderThan) {
				continue
			}
			if m.EffectiveConfidence(now) >= demoteConfidenceFloor {
				continue
			}
			if slices.Contains(m.Tags, PinnedTag) {
				continue
			}
			if err := st.Retier(ctx, ns, m.ID, memory.TierEpisodic, &exp); err != nil {
				if errors.Is(err, store.ErrNotFound) {
					continue
				}
				return total, err
			}
			if report != nil {
				report(string(m.Tier))
			}
			total++
		}
	}
	return total, nil
}
