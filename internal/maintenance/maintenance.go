// Package maintenance keeps the store healthy: a background sweeper purges
// expired memories and bounds short-term capacity, and fsck additionally audits
// live memories for duplicate (poisoning) clusters.
package maintenance

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"time"

	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/store"
)

// purgeBatch bounds how many expired memories are fetched per round.
const purgeBatch = 500

// shortTermTiers are the tiers subject to the short-term capacity cap.
var shortTermTiers = []memory.Tier{memory.TierWorking, memory.TierEpisodic}

// PurgeExpired deletes memories whose TTL has passed as of now, in batches, and
// returns the number removed.
func PurgeExpired(ctx context.Context, st store.Store, now time.Time) (int, error) {
	total := 0
	for {
		expired, err := st.ListExpired(ctx, now, purgeBatch)
		if err != nil {
			return total, err
		}
		for _, m := range expired {
			if err := st.DeleteIfExpiredBefore(ctx, m.Namespace, m.ID, now); err != nil {
				if !errors.Is(err, store.ErrNotFound) {
					return total, err
				}
				// ErrNotFound: memory was already deleted or its TTL was slid
				// past `now` by Reinforce; either way, skip the count.
			} else {
				total++
			}
		}
		if len(expired) < purgeBatch {
			return total, nil
		}
	}
}

// EnforceShortTermCap evicts the lowest-retention short-term memories in each
// namespace that holds more than cap of them. cap <= 0 disables it. Returns the
// number evicted.
func EnforceShortTermCap(ctx context.Context, st store.Store, cap int, now time.Time) (int, error) {
	if cap <= 0 {
		return 0, nil
	}
	namespaces, err := st.ListNamespaces(ctx)
	if err != nil {
		return 0, err
	}
	total := 0
	for _, ns := range namespaces {
		mems, err := st.List(ctx, ns, store.Filter{Tiers: shortTermTiers}, 0)
		if err != nil {
			return total, err
		}
		if len(mems) <= cap {
			continue
		}
		sort.Slice(mems, func(i, j int) bool {
			return mems[i].RetentionScore(now) < mems[j].RetentionScore(now)
		})
		for _, m := range mems[:len(mems)-cap] {
			if err := st.Delete(ctx, ns, m.ID); err != nil {
				if !errors.Is(err, store.ErrNotFound) {
					return total, err
				}
			} else {
				total++
			}
		}
	}
	return total, nil
}

// Report summarizes a consistency sweep.
type Report struct {
	ExpiredPurged    int        `json:"expired_purged"`
	ShortTermEvicted int        `json:"short_term_evicted"`
	Namespaces       int        `json:"namespaces"`
	DuplicateGroups  [][]string `json:"duplicate_groups,omitempty"`
}

// Fsck purges expired memories, enforces the short-term cap, and audits live
// memories for duplicate clusters (same normalized content) as a poisoning
// backstop. Duplicates are reported, not auto-deleted.
func Fsck(ctx context.Context, st store.Store, cap int, now time.Time) (Report, error) {
	var rep Report
	purged, err := PurgeExpired(ctx, st, now)
	if err != nil {
		return rep, err
	}
	rep.ExpiredPurged = purged

	evicted, err := EnforceShortTermCap(ctx, st, cap, now)
	if err != nil {
		return rep, err
	}
	rep.ShortTermEvicted = evicted

	namespaces, err := st.ListNamespaces(ctx)
	if err != nil {
		return rep, err
	}
	rep.Namespaces = len(namespaces)
	for _, ns := range namespaces {
		mems, err := st.List(ctx, ns, store.Filter{}, 0)
		if err != nil {
			return rep, err
		}
		groups := map[string][]string{}
		for _, m := range mems {
			key := memory.NormalizeContent(m.Content)
			groups[key] = append(groups[key], m.ID)
		}
		for _, ids := range groups {
			if len(ids) > 1 {
				rep.DuplicateGroups = append(rep.DuplicateGroups, ids)
			}
		}
	}
	return rep, nil
}

// SweeperConfig configures the periodic maintenance sweep.
type SweeperConfig struct {
	// Interval is how often the sweep runs.
	Interval time.Duration
	// ShortTermCap bounds working+episodic memories per namespace; the lowest-
	// retention ones over the cap are evicted. 0 disables it.
	ShortTermCap int
	// TombstoneTTL hard-deletes superseded memories last updated before now-TTL,
	// reclaiming space. 0 disables it (tombstones are kept indefinitely but stay
	// excluded from recall).
	TombstoneTTL time.Duration
	// DemoteAfter demotes never-recalled, low-importance durable memories older
	// than this to the episodic tier so unused debris ages out. 0 disables it.
	DemoteAfter time.Duration
	// ActivityRetention drops activity-log events older than now-retention.
	// 0 disables age-based pruning.
	ActivityRetention time.Duration
	// ActivityMaxRows caps the activity log, dropping the oldest rows beyond it.
	// 0 disables the cap. With both bounds 0 the log grows without limit.
	ActivityMaxRows int
	// OnDemoted, when non-nil, is called once per memory the demote stage
	// retiers, with the tier it was demoted from (feeds memini_demoted_total).
	OnDemoted func(fromTier string)
}

// Sweeper periodically purges expired memories, enforces the short-term cap, and
// (optionally) garbage-collects old tombstones.
type Sweeper struct {
	store store.Store
	log   *slog.Logger
	cfg   SweeperConfig
}

// NewSweeper builds a sweeper that runs every cfg.Interval.
func NewSweeper(st store.Store, log *slog.Logger, cfg SweeperConfig) *Sweeper {
	return &Sweeper{store: st, log: log, cfg: cfg}
}

// Run sweeps on a ticker until ctx is cancelled. It runs one sweep immediately.
// It is a no-op when Interval <= 0 (time.NewTicker panics on a non-positive
// duration); config validation rejects that, but guard here too so a
// misconfigured interval cannot crash the sweeper goroutine.
func (s *Sweeper) Run(ctx context.Context) {
	if s.cfg.Interval <= 0 {
		s.log.Warn("sweep interval is not positive; sweeper disabled", "interval", s.cfg.Interval)
		return
	}
	t := time.NewTicker(s.cfg.Interval)
	defer t.Stop()
	for {
		s.sweep(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Sweeper) sweep(ctx context.Context) {
	now := time.Now().UTC()
	if n, err := PurgeExpired(ctx, s.store, now); err != nil {
		s.log.Warn("decay sweep failed", "error", err)
	} else if n > 0 {
		s.log.Info("decay sweep purged expired memories", "count", n)
	}
	if n, err := EnforceShortTermCap(ctx, s.store, s.cfg.ShortTermCap, now); err != nil {
		s.log.Warn("short-term cap enforcement failed", "error", err)
	} else if n > 0 {
		s.log.Info("evicted short-term memories over cap", "count", n)
	}
	if s.cfg.TombstoneTTL > 0 {
		if n, err := PurgeTombstones(ctx, s.store, now.Add(-s.cfg.TombstoneTTL)); err != nil {
			s.log.Warn("tombstone GC failed", "error", err)
		} else if n > 0 {
			s.log.Info("garbage-collected old tombstones", "count", n)
		}
	}
	if s.cfg.DemoteAfter > 0 {
		if n, err := DemoteStale(ctx, s.store, now.Add(-s.cfg.DemoteAfter), now, s.cfg.OnDemoted); err != nil {
			s.log.Warn("retro-tiering demotion failed", "error", err)
		} else if n > 0 {
			s.log.Info("demoted stale durable memories to episodic", "count", n)
		}
	}
	if n, err := PruneEvents(ctx, s.store, now, s.cfg.ActivityRetention, s.cfg.ActivityMaxRows); err != nil {
		s.log.Warn("activity log pruning failed", "error", err)
	} else if n > 0 {
		s.log.Info("pruned activity log", "count", n)
	}
}
