package maintenance

import (
	"context"
	"os"
	"sort"
	"strings"

	"github.com/eleboucher/memini/internal/embed"
	"github.com/eleboucher/memini/internal/store"
)

// sharedSegment is the exact path segment the old scope model used to mark a
// tenant's shared pool: "<tenant>/_shared", merged read-only into every
// sibling under <tenant>. The new model (ancestor cascade + per-person home
// namespaces) has no such node; MigrateScopes folds it back into its parent.
const sharedSegment = "_shared"

// envGlobalNamespace is the deleted (T12) config knob's env var name. Read
// directly via os.Getenv rather than config.Config.GlobalNamespace so this
// still detects an operator's stale export after T12 deletes the field.
const envGlobalNamespace = "MEMINI_GLOBAL_NAMESPACE"

// ScopesOptions configures one MigrateScopes pass.
type ScopesOptions struct {
	// DryRun reports what would move (and what would be deduped) without
	// writing anything.
	DryRun bool
	// Embedder powers the post-merge dedup pass (gap G14). Required whenever
	// a merge actually happens and DryRun is false; unused in dry-run mode,
	// where no dedup pass runs.
	Embedder embed.Embedder
	// Dedup configures the post-merge dedup pass run against each merge
	// target. Namespaces and DryRun are overridden per call; the caller
	// controls Similarity/MinClusterSize/Tiers/etc. Zero value uses Dedup's
	// own defaults.
	Dedup DedupOptions
}

// ScopeMerge reports one `<t>/_shared` -> `<t>` merge and the dedup pass run
// against the target afterward.
type ScopeMerge struct {
	From            string `json:"from"`
	To              string `json:"to"`
	Moved           int    `json:"moved"`
	DedupClusters   int    `json:"dedup_clusters,omitempty"`
	DedupTombstoned int    `json:"dedup_tombstoned,omitempty"`
}

// ScopesReport summarizes one MigrateScopes pass.
type ScopesReport struct {
	Merges []ScopeMerge `json:"merges,omitempty"`
	// BareShared lists top-level namespaces literally named "_shared" (no
	// tenant prefix): there is no parent to merge into, so these are left
	// untouched. Reported rather than silently skipped since the operator
	// likely wants it repointed by hand (a home namespace, or a link source).
	BareShared []string `json:"bare_shared,omitempty"`
	// GlobalNamespaceEnv is the raw value of MEMINI_GLOBAL_NAMESPACE when set
	// at run time. Its presence is only ever reported, never acted on: the
	// caller (CLI) prints adoption instructions instead of a silent rewrite.
	GlobalNamespaceEnv string `json:"global_namespace_env,omitempty"`
	DryRun             bool   `json:"dry_run"`
}

// MigrateScopes moves the old shared-scope model's data into the new
// ancestor-cascade shape: every namespace literally named "<prefix>/_shared"
// is merged into "<prefix>" via Move (which already rewrites link endpoints),
// followed by a dedup pass scoped to the target namespace (gap G14 — Move
// relocates by unique ID with no content dedup, so a merge can duplicate
// facts already present in the target). A namespace named just "_shared"
// (no prefix) has no parent to merge into and is left untouched, reported in
// BareShared. Idempotent: once no "<prefix>/_shared" namespace holds any
// memories, a re-run finds nothing to do.
func MigrateScopes(ctx context.Context, st store.Store, opts ScopesOptions) (ScopesReport, error) {
	rep := ScopesReport{DryRun: opts.DryRun}
	if v := os.Getenv(envGlobalNamespace); v != "" {
		rep.GlobalNamespaceEnv = v
	}

	names, err := st.ListNamespaces(ctx)
	if err != nil {
		return rep, err
	}
	sort.Strings(names)

	for _, ns := range names {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		seg, parent, ok := splitShared(ns)
		if !ok {
			continue
		}
		if parent == "" {
			rep.BareShared = append(rep.BareShared, seg)
			continue
		}
		merge, err := mergeShared(ctx, st, ns, parent, opts)
		if err != nil {
			return rep, err
		}
		rep.Merges = append(rep.Merges, merge)
	}
	return rep, nil
}

// splitShared reports whether ns's last path segment is exactly "_shared".
// When it is, seg is that segment and parent is everything before it (empty
// for a bare top-level "_shared").
func splitShared(ns string) (seg, parent string, ok bool) {
	i := strings.LastIndex(ns, "/")
	if i < 0 {
		if ns == sharedSegment {
			return ns, "", true
		}
		return "", "", false
	}
	if ns[i+1:] != sharedSegment {
		return "", "", false
	}
	return ns, ns[:i], true
}

// mergeShared moves every memory from the "<parent>/_shared" namespace into
// parent, then runs a dedup pass scoped to parent so the merge doesn't leave
// content duplicated across what used to be two namespaces.
func mergeShared(ctx context.Context, st store.Store, from, to string, opts ScopesOptions) (ScopeMerge, error) {
	mrep, err := Move(ctx, st, from, to, opts.DryRun)
	if err != nil {
		return ScopeMerge{}, err
	}
	merge := ScopeMerge{From: from, To: to, Moved: mrep.Moved}
	if opts.DryRun || mrep.Moved == 0 {
		return merge, nil
	}

	dOpts := opts.Dedup
	dOpts.Namespaces = []string{to}
	dOpts.DryRun = false
	drep, err := Dedup(ctx, st, opts.Embedder, dOpts)
	if err != nil {
		return merge, err
	}
	merge.DedupClusters = drep.ClustersFound
	merge.DedupTombstoned = drep.Tombstoned
	return merge, nil
}
