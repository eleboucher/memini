package maintenance

import (
	"context"
	"errors"
	"time"

	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/store"
)

type BackfillConfidenceReport struct {
	Inspected int `json:"inspected"`
	Seeded    int `json:"seeded"`
	Skipped   int `json:"skipped"`
}

func BackfillConfidence(ctx context.Context, st store.Store, now time.Time) (BackfillConfidenceReport, error) {
	return backfillConfidence(ctx, st, now, true)
}

func BackfillConfidencePreview(ctx context.Context, st store.Store, now time.Time) (BackfillConfidenceReport, error) {
	return backfillConfidence(ctx, st, now, false)
}

func backfillConfidence(ctx context.Context, st store.Store, now time.Time, apply bool) (BackfillConfidenceReport, error) {
	var rep BackfillConfidenceReport
	namespaces, err := st.ListNamespaces(ctx)
	if err != nil {
		return rep, err
	}
	seed := memory.ConfidenceSeedImported
	durableTiers := []memory.Tier{memory.TierSemantic, memory.TierProcedural}
	for _, ns := range namespaces {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		mems, err := st.List(ctx, ns, store.Filter{Tiers: durableTiers}, 0)
		if err != nil {
			return rep, err
		}
		for _, m := range mems {
			rep.Inspected++
			if m.Confidence != nil {
				rep.Skipped++
				continue
			}
			if !apply {
				rep.Seeded++
				continue
			}
			if err := st.SetConfidence(ctx, ns, m.ID, seed, now); err != nil {
				if errors.Is(err, store.ErrNotFound) {
					rep.Skipped++
					continue
				}
				return rep, err
			}
			rep.Seeded++
		}
	}
	return rep, nil
}
