package maintenance

import (
	"context"
	"time"

	"github.com/eleboucher/memini/internal/store"
)

// PruneEvents trims the activity log to a retention window and a row cap.
// A retention of 0 prunes nothing by age; a cap of 0 applies no cap. Returns
// the number of rows deleted, and 0 against a driver with no activity log
// (the same degrade-gracefully type assertion the log's writers use).
func PruneEvents(ctx context.Context, st store.Store, now time.Time, retention time.Duration, maxRows int) (int64, error) {
	els, ok := st.(store.EventLogStore)
	if !ok {
		return 0, nil
	}
	var olderThan time.Time
	if retention > 0 {
		olderThan = now.Add(-retention)
	}
	if olderThan.IsZero() && maxRows <= 0 {
		return 0, nil
	}
	return els.PruneEvents(ctx, olderThan, maxRows)
}
