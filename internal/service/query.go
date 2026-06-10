package service

import (
	"context"
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
}

// List returns memories in a namespace matching the filter, without embeddings.
// It backs the UI memory browser and the client-derived relationship graph.
func (s *Service) List(ctx context.Context, in ListInput) ([]*memory.Memory, error) {
	return s.store.List(ctx, in.Namespace, store.Filter{
		Tiers:             in.Tiers,
		IncludeExpired:    in.IncludeExpired,
		IncludeSuperseded: in.IncludeSuperseded,
	}, in.Limit)
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

// Namespaces returns the distinct namespaces holding memories, for the UI
// tenant switcher.
func (s *Service) Namespaces(ctx context.Context) ([]string, error) {
	return s.store.ListNamespaces(ctx)
}
