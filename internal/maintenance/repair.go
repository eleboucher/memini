package maintenance

import (
	"context"
	"errors"
	"log/slog"

	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/store"
)

// RepairReport summarizes one supersession-repair pass.
type RepairReport struct {
	Restored   int  `json:"restored"`
	Namespaces int  `json:"namespaces"`
	DryRun     bool `json:"dry_run"`
}

// RepairSupersession restores memories stranded by a broken supersession chain:
// a tombstoned row whose superseded_by chain never reaches a live memory (its
// representative was itself superseded, or deleted) is cleared back to live, so
// every cluster keeps a recallable head. A genuine duplicate is re-collapsed by
// the next dedup pass. Empty namespaces means every namespace; a per-namespace
// error is logged and skipped so one bad namespace can't abort the rest.
func RepairSupersession(ctx context.Context, st store.Store, namespaces []string, dryRun bool, log *slog.Logger) (RepairReport, error) {
	rep := RepairReport{DryRun: dryRun}
	if log == nil {
		log = slog.Default()
	}
	if len(namespaces) == 0 {
		var err error
		if namespaces, err = st.ListNamespaces(ctx); err != nil {
			return rep, err
		}
	}
	for _, ns := range namespaces {
		n, err := repairNamespace(ctx, st, ns, dryRun)
		if err != nil {
			log.Warn("supersession repair failed", "namespace", ns, "error", err)
			continue
		}
		if n > 0 {
			rep.Namespaces++
			rep.Restored += n
		}
	}
	return rep, nil
}

func repairNamespace(ctx context.Context, st store.Store, ns string, dryRun bool) (int, error) {
	mems, err := st.List(ctx, ns, store.Filter{IncludeSuperseded: true, IncludeExpired: true}, 0)
	if err != nil {
		return 0, err
	}
	byID := make(map[string]*memory.Memory, len(mems))
	for _, m := range mems {
		byID[m.ID] = m
	}
	var restored int
	for _, m := range mems {
		if m.SupersededBy == nil || reachesLive(m, byID) {
			continue
		}
		restored++
		if dryRun {
			continue
		}
		if err := st.Restore(ctx, ns, m.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
			return restored, err
		}
	}
	return restored, nil
}

// reachesLive reports whether m's superseded_by chain ends at a live row. A
// missing target or a cycle returns false: the cluster has no recallable head.
func reachesLive(m *memory.Memory, byID map[string]*memory.Memory) bool {
	seen := map[string]bool{}
	for m.SupersededBy != nil {
		if seen[m.ID] {
			return false
		}
		seen[m.ID] = true
		next, ok := byID[*m.SupersededBy]
		if !ok {
			return false
		}
		m = next
	}
	return true
}
