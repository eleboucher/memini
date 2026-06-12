package maintenance

import (
	"context"
	"errors"
	"slices"

	"github.com/eleboucher/memini/internal/store"
)

// ForgetByTag deletes every memory in a namespace carrying tag, including
// superseded and expired ones, and returns the count deleted. With the import
// provenance tag (import:<source>:<date>) this undoes a single bulk import.
func ForgetByTag(ctx context.Context, st store.Store, namespace, tag string) (int64, error) {
	mems, err := st.List(ctx, namespace, store.Filter{IncludeSuperseded: true, IncludeExpired: true}, 0)
	if err != nil {
		return 0, err
	}
	var deleted int64
	for _, m := range mems {
		if !slices.Contains(m.Tags, tag) {
			continue
		}
		if err := st.Delete(ctx, namespace, m.ID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				continue // raced with another delete
			}
			return deleted, err
		}
		deleted++
	}
	return deleted, nil
}
