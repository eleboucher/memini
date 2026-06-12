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

// longTermTiers are the durable tiers eligible for demotion.
var longTermTiers = []memory.Tier{memory.TierSemantic, memory.TierProcedural}

// DemoteStale demotes durable (semantic/procedural) memories that have never
// been recalled (AccessCount 0), were last updated before olderThan, and are
// neither highly important (>= 0.5) nor pinned, down to the episodic tier —
// giving them the episodic TTL. Unused "durable" debris (e.g. a low-quality bulk
// import) then ages out on its own, while anything recalled even once is
// reinforced and kept. The mirror image of promotion. Returns the count demoted.
func DemoteStale(ctx context.Context, st store.Store, olderThan, now time.Time) (int, error) {
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
			if m.AccessCount > 0 || m.Importance >= 0.5 {
				continue
			}
			if !m.UpdatedAt.Before(olderThan) {
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
			total++
		}
	}
	return total, nil
}
