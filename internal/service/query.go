package service

import (
	"context"
	"sort"
	"time"

	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/store"
)

// ListInput selects a slice of a namespace's memories for browsing. The zero
// value (besides Namespace) lists all live memories, newest store order.
type ListInput struct {
	Namespace         string
	Tiers             []memory.Tier
	IncludeExpired    bool
	IncludeSuperseded bool
	// Limit caps the result count; <= 0 returns all matches.
	Limit int
	// AllNamespaces lists across every namespace instead of in.Namespace, with
	// Limit applied as a single global cap (newest first). Backs the admin UI's
	// "All projects" view.
	AllNamespaces bool
}

// List returns memories in a namespace matching the filter, without embeddings.
// It backs the UI memory browser and the client-derived relationship graph.
func (s *Service) List(ctx context.Context, in ListInput) ([]*memory.Memory, error) {
	f := store.Filter{
		Tiers:             in.Tiers,
		IncludeExpired:    in.IncludeExpired,
		IncludeSuperseded: in.IncludeSuperseded,
		Now:               s.now(),
	}
	if !in.AllNamespaces {
		return s.store.List(ctx, in.Namespace, f, in.Limit)
	}

	names, err := s.store.ListNamespaces(ctx)
	if err != nil {
		return nil, err
	}
	var all []*memory.Memory
	for _, ns := range names {
		// Each namespace contributes its top Limit: any of them may hold the
		// globally newest memories, decided by the merge sort below.
		mems, err := s.store.List(ctx, ns, f, in.Limit)
		if err != nil {
			return nil, err
		}
		all = append(all, mems...)
	}
	// Newest first, so the global cap keeps the most recent memories.
	sort.SliceStable(all, func(i, j int) bool { return all[i].CreatedAt.After(all[j].CreatedAt) })
	if in.Limit > 0 && len(all) > in.Limit {
		all = all[:in.Limit]
	}
	return all, nil
}

// Stats summarizes a namespace for the UI dashboard. Counts are computed from a
// full listing, so callers should treat it as a curated-namespace overview, not
// a hot-path metric (Prometheus /metrics remains the source for operational
// counters).
type Stats struct {
	Namespace     string              `json:"namespace"`
	Total         int                 `json:"total"`      // live memories (excludes expired/superseded)
	ByTier        map[memory.Tier]int `json:"by_tier"`    // live count per tier
	Expired       int                 `json:"expired"`    // past-TTL, not yet swept
	Superseded    int                 `json:"superseded"` // contradiction-tombstoned
	TotalAccesses int                 `json:"total_accesses"`
	AvgImportance float64             `json:"avg_importance"`
	LastWriteAt   *time.Time          `json:"last_write_at,omitempty"`
}

// Stats computes a per-namespace overview by scanning all of its memories
// (including expired and superseded, so those can be counted separately).
func (s *Service) Stats(ctx context.Context, namespace string) (Stats, error) {
	all, err := s.store.List(ctx, namespace, store.Filter{
		IncludeExpired:    true,
		IncludeSuperseded: true,
	}, 0)
	if err != nil {
		return Stats{}, err
	}

	st := Stats{Namespace: namespace, ByTier: map[memory.Tier]int{}}
	now := s.now()
	var importanceSum float64
	for _, m := range all {
		switch {
		case m.SupersededBy != nil:
			st.Superseded++
		case m.Expired(now):
			st.Expired++
		default:
			st.Total++
			st.ByTier[m.Tier]++
			st.TotalAccesses += m.AccessCount
			importanceSum += m.Importance
		}
		if st.LastWriteAt == nil || m.CreatedAt.After(*st.LastWriteAt) {
			t := m.CreatedAt
			st.LastWriteAt = &t
		}
	}
	if st.Total > 0 {
		st.AvgImportance = importanceSum / float64(st.Total)
	}
	return st, nil
}

// StatsAll merges per-namespace overviews into a single store-wide one
// (namespace reported as ""), backing the admin UI's "All projects" dashboard.
func (s *Service) StatsAll(ctx context.Context) (Stats, error) {
	names, err := s.store.ListNamespaces(ctx)
	if err != nil {
		return Stats{}, err
	}
	merged := Stats{ByTier: map[memory.Tier]int{}}
	var importanceWeighted float64
	for _, ns := range names {
		st, err := s.Stats(ctx, ns)
		if err != nil {
			return Stats{}, err
		}
		merged.Total += st.Total
		merged.Expired += st.Expired
		merged.Superseded += st.Superseded
		merged.TotalAccesses += st.TotalAccesses
		// Weight by live total so the merged average isn't skewed by empty or
		// tombstone-only namespaces.
		importanceWeighted += st.AvgImportance * float64(st.Total)
		for tier, n := range st.ByTier {
			merged.ByTier[tier] += n
		}
		if st.LastWriteAt != nil && (merged.LastWriteAt == nil || st.LastWriteAt.After(*merged.LastWriteAt)) {
			merged.LastWriteAt = st.LastWriteAt
		}
	}
	if merged.Total > 0 {
		merged.AvgImportance = importanceWeighted / float64(merged.Total)
	}
	return merged, nil
}

// Namespaces returns the distinct namespaces holding memories, for the UI
// tenant switcher.
func (s *Service) Namespaces(ctx context.Context) ([]string, error) {
	return s.store.ListNamespaces(ctx)
}

// DeleteNamespace removes every memory in a namespace. Returns the number of
// memories deleted.
func (s *Service) DeleteNamespace(ctx context.Context, namespace string) (int64, error) {
	start := time.Now()
	defer func() { s.metrics.OpDuration("delete_namespace", time.Since(start)) }()
	n, err := s.store.DeleteNamespace(ctx, namespace)
	if err != nil {
		s.metrics.ForgetResult("error")
		return 0, err
	}
	s.metrics.ForgetResult("ok")
	return n, nil
}
