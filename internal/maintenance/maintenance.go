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

// Sweeper periodically purges expired memories and enforces the short-term cap.
type Sweeper struct {
	store        store.Store
	log          *slog.Logger
	interval     time.Duration
	shortTermCap int
}

// NewSweeper builds a sweeper that runs every interval, bounding short-term
// memory to shortTermCap per namespace (0 disables the cap).
func NewSweeper(st store.Store, log *slog.Logger, interval time.Duration, shortTermCap int) *Sweeper {
	return &Sweeper{
		store: st, log: log, interval: interval, shortTermCap: shortTermCap,
	}
}

// Run sweeps on a ticker until ctx is cancelled. It runs one sweep immediately.
func (s *Sweeper) Run(ctx context.Context) {
	t := time.NewTicker(s.interval)
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
	if n, err := EnforceShortTermCap(ctx, s.store, s.shortTermCap, now); err != nil {
		s.log.Warn("short-term cap enforcement failed", "error", err)
	} else if n > 0 {
		s.log.Info("evicted short-term memories over cap", "count", n)
	}
}
