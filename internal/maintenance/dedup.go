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

	// A single requested namespace (the one-shot API path) propagates its
	// error so the caller sees a 5xx. A store-wide pass over many namespaces is
	// best-effort instead: dedupNamespace calls the external embedder, so a
	// namespace whose content trips it would otherwise abort every later
	// namespace on every tick. We log and skip it rather than poison the pass.
	scoped := len(namespaces) == 1
	for _, ns := range namespaces {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		nsRep, err := dedupNamespace(ctx, st, emb, ns, opts)
		if err != nil {
			if scoped || ctx.Err() != nil {
				return rep, err
			}
			log.WarnContext(ctx, "dedup namespace failed; skipping", "namespace", ns, "error", err)
			continue
		}
		if nsRep.memoriesSeen == 0 {
			continue // too few memories to cluster; not a processed namespace
		}
		rep.Namespaces++
		rep.MemoriesSeen += nsRep.memoriesSeen
		rep.ClustersFound += nsRep.clustersFound
		rep.Tombstoned += nsRep.tombstoned
		rep.Actions = append(rep.Actions, nsRep.actions...)
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

// nsDedup is one namespace's contribution to a dedup pass. memoriesSeen is 0
// when the namespace held too few live memories to cluster.
type nsDedup struct {
	memoriesSeen  int
	clustersFound int
	tombstoned    int
	actions       []ClusterAction
}

// dedupNamespace clusters one namespace and tombstones the non-representative
// members of each cluster. It performs the embed + per-anchor vector-search
// fan-out, so it is the unit a store-wide pass isolates failures to.
func dedupNamespace(ctx context.Context, st store.Store, emb embed.Embedder, ns string, opts DedupOptions) (nsDedup, error) {
	var res nsDedup
	f := store.Filter{Tiers: opts.Tiers, Now: opts.Now}
	mems, err := st.List(ctx, ns, f, 0)
	if err != nil {
		return res, err
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
		return res, nil
	}
	res.memoriesSeen = len(mems)

	texts := make([]string, len(mems))
	for i, m := range mems {
		texts[i] = m.Content
	}
	vecs, err := emb.Embed(ctx, texts)
	if err != nil {
		return res, err
	}

	// Union-find over the similarity graph: two memories with cosine score >=
	// opts.Similarity share a root. Processing order doesn't matter — the
	// closure is computed transitively, then components are resolved once.
	u := newUnionFind(idsOf(mems))
	for i, anchor := range mems {
		cands, err := st.VectorSearch(ctx, ns, vecs[i], f, opts.NeighboursPerAnchor)
		if err != nil {
			return res, err
		}
		for _, c := range cands {
			if c.Memory.ID == anchor.ID {
				continue
			}
			if c.Score < opts.Similarity {
				break // best-first; further results are lower-scored
			}
			if _, ok := u.parent[c.Memory.ID]; !ok {
				// Result not in our current namespace snapshot (e.g. another
				// tier dropped by the filter). Skip rather than corrupt the
				// DSU with an unknown id.
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
		res.actions = append(res.actions, ClusterAction{
			RepresentativeID: keep.ID,
			TombstonedIDs:    idsOf(rest),
			Size:             len(comp),
		})
		res.clustersFound++

		if opts.DryRun {
			res.tombstoned += len(rest)
			continue
		}
		for _, m := range rest {
			if err := st.SetSuperseded(ctx, ns, m.ID, keep.ID); err != nil {
				if errors.Is(err, store.ErrNotFound) {
					continue // raced with a concurrent write/delete
				}
				return res, err
			}
			res.tombstoned++
		}
	}
	return res, nil
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
	// Heal any supersession chain this pass created (a former representative
	// re-superseded into a newer one leaves its dependents pointing at a
	// tombstone), so no cluster is left without a recallable head.
	if rrep, err := RepairSupersession(ctx, d.store, d.opts.Namespaces, false, d.log); err != nil {
		d.log.WarnContext(ctx, "supersession repair failed", "error", err)
	} else if rrep.Restored > 0 {
		d.log.InfoContext(ctx, "repaired stranded memories", "restored", rrep.Restored, "namespaces", rrep.Namespaces)
	}
}
