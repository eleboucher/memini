package maintenance

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"time"

	"github.com/eleboucher/memini/internal/embed"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/store"
)

// Dedup knobs. Defaults are picked so a periodic pass is safe (low false-
// positive risk) but not toothless on a corpus full of restatements.
const (
	defaultDedupSimilarity       = 0.85
	defaultDedupMinClusterSize   = 2
	defaultDedupNeighboursAnchor = 20
)

// DedupOptions configures one dedup pass.
type DedupOptions struct {
	// Similarity is the minimum cosine-like score (the store's vector
	// distance-to-score mapping) for two memories to join a cluster.
	// 0 falls back to defaultDedupSimilarity. Negative disables dedup and
	// the call returns an empty report.
	Similarity float64
	// MinClusterSize is the smallest cluster acted on. A pair of near-
	// duplicates below this is left alone. 0 falls back to
	// defaultDedupMinClusterSize.
	MinClusterSize int
	// Tiers restricts the pass to these tiers; nil/empty means all tiers.
	Tiers []memory.Tier
	// Namespaces restricts the pass to these namespaces; nil/empty means every
	// namespace. Clusters never span namespaces, so scoping the pass to one
	// (the post-import case) is both cheaper and avoids touching other tenants.
	Namespaces []string
	// NeighboursPerAnchor bounds the per-anchor vector-search fan-out. Larger
	// values tighten clusters at higher vector-search cost. 0 falls back to
	// defaultDedupNeighboursAnchor.
	NeighboursPerAnchor int
	// DryRun reports what would be done without tombstoning anything.
	DryRun bool
	// Now is the instant retention scoring and expiry filtering are evaluated
	// at. Zero means time.Now().UTC().
	Now time.Time
	// Log receives progress messages; nil falls back to slog.Default().
	Log *slog.Logger
}

// ClusterAction describes one near-duplicate cluster found by a pass and the
// representative selection the pass would commit.
type ClusterAction struct {
	RepresentativeID string   `json:"representative_id"`
	TombstonedIDs    []string `json:"tombstoned_ids"`
	Size             int      `json:"size"`
}

// DedupReport summarizes one dedup pass.
type DedupReport struct {
	Namespaces    int             `json:"namespaces"`
	MemoriesSeen  int             `json:"memories_seen"`
	ClustersFound int             `json:"clusters_found"`
	Tombstoned    int             `json:"tombstoned"`
	DryRun        bool            `json:"dry_run"`
	Actions       []ClusterAction `json:"actions,omitempty"`
}

// Dedup clusters live memories per namespace by embedding similarity and
// tombstones the lower-scored members of each cluster, pointing them at the
// cluster's representative. The representative is the member with the highest
// RetentionScore (importance × access × recency), tie-broken by updated-at and
// then created-at so re-imports don't shadow the original.
//
// Tombstoning is reversible: SetSuperseded excludes the duplicates from
// default search results but keeps them in storage. To free space, follow up
// with a store-level GC (not implemented here). The action is symmetric with
// consolidation's supersede, so the read path needs no changes.
//
// Dedup is O(n · vector_search(n)) per namespace; with the embedder cache warm
// (the typical post-import case) the batched embed is near-free. For very
// large corpora, NeighboursPerAnchor bounds the union-find fan-out; the
// cluster of a memory can only ever be as wide as that fan-out allows.
//
// st is required; emb is required unless Similarity <= 0 (in which case the
// pass is a no-op). opts.Similarity <= 0 short-circuits to an empty report.
func Dedup(ctx context.Context, st store.Store, emb embed.Embedder, opts DedupOptions) (DedupReport, error) {
	var rep DedupReport
	if opts.Similarity < 0 {
		return rep, nil
	}
	if opts.Similarity == 0 {
		opts.Similarity = defaultDedupSimilarity
	}
	if opts.MinClusterSize < 2 {
		opts.MinClusterSize = defaultDedupMinClusterSize
	}
	if opts.NeighboursPerAnchor <= 0 {
		opts.NeighboursPerAnchor = defaultDedupNeighboursAnchor
	}
	if opts.Now.IsZero() {
		opts.Now = time.Now().UTC()
	}
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}

	namespaces := opts.Namespaces
	if len(namespaces) == 0 {
		var err error
		if namespaces, err = st.ListNamespaces(ctx); err != nil {
			return rep, err
		}
	}
	rep.DryRun = opts.DryRun

	for _, ns := range namespaces {
		f := store.Filter{Tiers: opts.Tiers, Now: opts.Now}
		mems, err := st.List(ctx, ns, f, 0)
		if err != nil {
			return rep, err
		}
		// Drop expired entries the filter might have leaked (defensive: List
		// already excludes them by default, but we don't want to cluster them).
		live := mems[:0]
		for _, m := range mems {
			if !m.Expired(opts.Now) {
				live = append(live, m)
			}
		}
		mems = live
		if len(mems) < opts.MinClusterSize {
			continue
		}

		texts := make([]string, len(mems))
		for i, m := range mems {
			texts[i] = m.Content
		}
		vecs, err := emb.Embed(ctx, texts)
		if err != nil {
			return rep, err
		}

		// Union-find over the similarity graph: two memories with cosine
		// score >= opts.Similarity share a root. Processing order doesn't
		// matter — the closure is computed transitively, then components are
		// resolved once.
		u := newUnionFind(idsOf(mems))
		for i, anchor := range mems {
			cands, err := st.VectorSearch(ctx, ns, vecs[i], f, opts.NeighboursPerAnchor)
			if err != nil {
				return rep, err
			}
			for _, c := range cands {
				if c.Memory.ID == anchor.ID {
					continue
				}
				if c.Score < opts.Similarity {
					break // best-first; further results are lower-scored
				}
				if _, ok := u.parent[c.Memory.ID]; !ok {
					// Result not in our current namespace snapshot (e.g.
					// another tier dropped by the filter). Skip rather than
					// corrupt the DSU with an unknown id.
					continue
				}
				u.union(anchor.ID, c.Memory.ID)
			}
		}

		components := map[string][]*memory.Memory{}
		for _, m := range mems {
			root := u.find(m.ID)
			components[root] = append(components[root], m)
		}

		for _, comp := range components {
			if len(comp) < opts.MinClusterSize {
				continue
			}
			// Highest-retention memory is the cluster representative.
			sort.SliceStable(comp, func(i, j int) bool {
				return betterRepresentative(comp[i], comp[j], opts.Now)
			})
			keep := comp[0]
			rest := comp[1:]
			action := ClusterAction{
				RepresentativeID: keep.ID,
				TombstonedIDs:    idsOf(rest),
				Size:             len(comp),
			}
			rep.Actions = append(rep.Actions, action)
			rep.ClustersFound++

			if opts.DryRun {
				rep.Tombstoned += len(rest)
				continue
			}
			for _, m := range rest {
				if err := st.SetSuperseded(ctx, ns, m.ID, keep.ID); err != nil {
					if errors.Is(err, store.ErrNotFound) {
						continue // raced with a concurrent write/delete
					}
					return rep, err
				}
				rep.Tombstoned++
			}
		}
		rep.Namespaces++
		rep.MemoriesSeen += len(mems)
	}
	if rep.Tombstoned > 0 {
		log.InfoContext(ctx, "dedup pass complete",
			"namespaces", rep.Namespaces,
			"memories_seen", rep.MemoriesSeen,
			"clusters", rep.ClustersFound,
			"tombstoned", rep.Tombstoned,
			"dry_run", rep.DryRun)
	}
	return rep, nil
}

// betterRepresentative reports whether a is a better cluster representative
// than b: higher RetentionScore first, then more-recent UpdatedAt, then
// more-recent CreatedAt. The stable sort keeps the relative order of ties so
// the result is deterministic.
func betterRepresentative(a, b *memory.Memory, now time.Time) bool {
	ra, rb := a.RetentionScore(now), b.RetentionScore(now)
	if ra != rb {
		return ra > rb
	}
	if !a.UpdatedAt.Equal(b.UpdatedAt) {
		return a.UpdatedAt.After(b.UpdatedAt)
	}
	return a.CreatedAt.After(b.CreatedAt)
}

func idsOf(mems []*memory.Memory) []string {
	out := make([]string, len(mems))
	for i, m := range mems {
		out[i] = m.ID
	}
	return out
}

// unionFind is a path-compressing disjoint-set over memory IDs.
type unionFind struct {
	parent map[string]string
}

func newUnionFind(ids []string) *unionFind {
	p := make(map[string]string, len(ids))
	for _, id := range ids {
		p[id] = id
	}
	return &unionFind{parent: p}
}

func (u *unionFind) find(x string) string {
	if u.parent[x] != x {
		u.parent[x] = u.find(u.parent[x])
	}
	return u.parent[x]
}

func (u *unionFind) union(x, y string) {
	rx, ry := u.find(x), u.find(y)
	if rx != ry {
		u.parent[rx] = ry
	}
}

// DedupJob is a periodic vector-cluster dedup pass. With interval <= 0, Run
// is a no-op (the function returns immediately).
type DedupJob struct {
	store    store.Store
	embedder embed.Embedder
	metrics  store.Metrics
	log      *slog.Logger
	interval time.Duration
	opts     DedupOptions
}

// NewDedupJob builds a DedupJob that calls Dedup(opts) every interval.
// interval <= 0 disables the job.
func NewDedupJob(st store.Store, emb embed.Embedder, m store.Metrics, log *slog.Logger,
	interval time.Duration, opts DedupOptions) *DedupJob {
	return &DedupJob{
		store:    st,
		embedder: emb,
		metrics:  m,
		log:      log,
		interval: interval,
		opts:     opts,
	}
}

// Run loops on a ticker until ctx is cancelled. It runs one pass immediately
// and again on every tick. It is a no-op if the job was built with
// interval <= 0.
func (d *DedupJob) Run(ctx context.Context) {
	if d.interval <= 0 || d.opts.Similarity < 0 {
		return
	}
	d.pass(ctx)
	t := time.NewTicker(d.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.pass(ctx)
		}
	}
}

func (d *DedupJob) pass(ctx context.Context) {
	opts := d.opts
	opts.Log = d.log
	opts.Now = time.Now().UTC()
	rep, err := Dedup(ctx, d.store, d.embedder, opts)
	if err != nil {
		d.log.WarnContext(ctx, "dedup pass failed", "error", err)
		return
	}
	if rep.Tombstoned > 0 {
		d.metrics.DedupTombstoned(rep.Tombstoned)
	}
}
