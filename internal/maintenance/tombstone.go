package maintenance

import (
	"context"
	"errors"
	"time"

	"github.com/eleboucher/memini/internal/store"
)

// PurgeTombstones hard-deletes superseded (tombstoned) memories last updated
// before olderThan, reclaiming the storage and vector-index space they occupy.
// Tombstones are already excluded from default recall, so this never changes
// those results; it only frees space and bounds how far back time-travel (as_of)
// recall can reach. Returns the count deleted.
func PurgeTombstones(ctx context.Context, st store.Store, olderThan time.Time) (int, error) {
	namespaces, err := st.ListNamespaces(ctx)
	if err != nil {
		return 0, err
	}
	total := 0
	for _, ns := range namespaces {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		mems, err := st.List(ctx, ns, store.Filter{IncludeSuperseded: true, IncludeExpired: true}, 0)
		if err != nil {
			return total, err
		}
		for _, m := range mems {
			if m.SupersededBy == nil {
				continue // live memory
			}
			if !m.UpdatedAt.Before(olderThan) {
				continue // tombstoned too recently to GC
			}
			if err := st.Delete(ctx, ns, m.ID); err != nil {
				if !errors.Is(err, store.ErrNotFound) {
					return total, err
				}
				continue
			}
			total++
		}
	}
	return total, nil
}
