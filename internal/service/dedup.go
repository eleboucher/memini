package service

import (
	"context"
	"log/slog"
	"time"

	"github.com/eleboucher/memini/internal/maintenance"
	"github.com/eleboucher/memini/internal/memory"
)

// DedupInput configures a dedup pass invoked through the service. The zero
// value is valid and means "use the production defaults": 0.85 similarity,
// cluster size >= 2, all tiers, 20 neighbours per anchor, dry-run = false.
type DedupInput struct {
	// Similarity gates cluster membership. 0 falls back to the package
	// default (0.85). Negative disables the pass and Dedup returns an empty
	// report without erroring.
	Similarity float64
	// MinClusterSize is the smallest cluster acted on. 0 falls back to 2.
	MinClusterSize int
	// Tiers restricts the pass to these tiers; nil/empty means all tiers.
	Tiers []memory.Tier
	// Namespaces restricts the pass to these namespaces; nil/empty means every
	// namespace. API callers scope this to the request's namespace; only an
	// explicit all-namespaces request leaves it empty.
	Namespaces []string
	// NeighboursPerAnchor bounds the per-anchor vector-search fan-out.
	// 0 falls back to 20.
	NeighboursPerAnchor int
	// DryRun reports what would be done without tombstoning anything.
	DryRun bool
}

// Dedup runs a vector-cluster dedup pass: each cluster's representative (the
// member with the highest RetentionScore) is kept; the rest are tombstoned
// (SupersededBy → representative) so they're hidden from default search
// results. The action is reversible. With in.Namespaces empty the pass spans
// every namespace; callers usually scope it to one.
//
// It's mainly a post-import cleanup tool, since exports tend to be full of
// restatements. The default similarity (0.85) is a paraphrase-level threshold;
// raise it for stricter, lower it for looser merging.
func (s *Service) Dedup(ctx context.Context, in DedupInput) (maintenance.DedupReport, error) {
	start := time.Now()
	defer func() { s.metrics.OpDuration("dedup", time.Since(start)) }()
	rep, err := maintenance.Dedup(ctx, s.store, s.embedder, maintenance.DedupOptions{
		Similarity:          in.Similarity,
		MinClusterSize:      in.MinClusterSize,
		Tiers:               in.Tiers,
		Namespaces:          in.Namespaces,
		NeighboursPerAnchor: in.NeighboursPerAnchor,
		DryRun:              in.DryRun,
		Now:                 s.now(),
		Log:                 slog.Default(),
	})
	if err != nil {
		return rep, err
	}
	if rep.Tombstoned > 0 {
		s.metrics.DedupTombstoned(rep.Tombstoned)
	}
	return rep, nil
}
