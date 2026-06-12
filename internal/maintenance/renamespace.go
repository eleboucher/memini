package maintenance

import (
	"context"
	"sort"
	"strings"

	"github.com/eleboucher/memini/internal/store"
)

// DefaultSplitKeys are the metadata keys, in priority order, that Split groups a
// namespace by. import_source_namespace is stamped by the importer when a merge
// discarded the source namespace, so it recovers a botched `--merge-into` import
// exactly; the rest cover the scope fields the mem0/agentmemory/mnemory adapters
// preserve (user_id/agent_id/run_id/project).
var DefaultSplitKeys = []string{"import_source_namespace", "user_id", "agent_id", "run_id", "project"}

// RenamespaceReport summarizes a Move or Split.
type RenamespaceReport struct {
	Moved   int            `json:"moved"`
	Targets map[string]int `json:"targets,omitempty"` // memories moved into each destination
	Skipped int            `json:"skipped"`           // left in place (no grouping key, or already in place)
	DryRun  bool           `json:"dry_run"`
}

// listAll returns every memory in a namespace, including superseded and expired
// ones, so a recovery move relocates the whole namespace (not just live rows).
func listAll(ctx context.Context, st store.Store, ns string) ([]string, []map[string]any, error) {
	mems, err := st.List(ctx, ns, store.Filter{IncludeSuperseded: true, IncludeExpired: true}, 0)
	if err != nil {
		return nil, nil, err
	}
	ids := make([]string, len(mems))
	metas := make([]map[string]any, len(mems))
	for i, m := range mems {
		ids[i] = m.ID
		metas[i] = m.Metadata
	}
	return ids, metas, nil
}

// Move relocates every memory in fromNS to toNS. A no-op when fromNS == toNS.
func Move(ctx context.Context, st store.Store, fromNS, toNS string, dryRun bool) (RenamespaceReport, error) {
	rep := RenamespaceReport{DryRun: dryRun, Targets: map[string]int{}}
	if fromNS == toNS {
		return rep, nil
	}
	ids, _, err := listAll(ctx, st, fromNS)
	if err != nil {
		return rep, err
	}
	if len(ids) == 0 {
		return rep, nil
	}
	if dryRun {
		rep.Moved = len(ids)
		rep.Targets[toNS] = len(ids)
		return rep, nil
	}
	n, err := st.Reassign(ctx, fromNS, ids, toNS)
	if err != nil {
		return rep, err
	}
	rep.Moved = int(n)
	rep.Targets[toNS] = int(n)
	return rep, nil
}

// Split regroups a namespace by metadata, moving each record to the namespace
// named by the first of byKeys it carries. Records with no grouping key (or
// whose key equals fromNS) stay put and are counted as skipped. Pass nil byKeys
// to use DefaultSplitKeys. This is the recovery path for a store whose imports
// were collapsed into one pool.
func Split(ctx context.Context, st store.Store, fromNS string, byKeys []string, dryRun bool) (RenamespaceReport, error) {
	if len(byKeys) == 0 {
		byKeys = DefaultSplitKeys
	}
	rep := RenamespaceReport{DryRun: dryRun, Targets: map[string]int{}}

	ids, metas, err := listAll(ctx, st, fromNS)
	if err != nil {
		return rep, err
	}
	groups := map[string][]string{}
	for i, meta := range metas {
		target := firstMetaValue(meta, byKeys)
		if target == "" || target == fromNS {
			rep.Skipped++
			continue
		}
		groups[target] = append(groups[target], ids[i])
	}

	// Deterministic order so dry-run and apply report the same way.
	for _, target := range sortedKeys(groups) {
		gids := groups[target]
		if dryRun {
			rep.Targets[target] = len(gids)
			rep.Moved += len(gids)
			continue
		}
		n, err := st.Reassign(ctx, fromNS, gids, target)
		if err != nil {
			return rep, err
		}
		rep.Targets[target] = int(n)
		rep.Moved += int(n)
	}
	return rep, nil
}

// firstMetaValue returns the first non-empty, trimmed string value among keys.
func firstMetaValue(meta map[string]any, keys []string) string {
	if meta == nil {
		return ""
	}
	for _, k := range keys {
		if s, ok := meta[k].(string); ok {
			if v := strings.TrimSpace(s); v != "" {
				return v
			}
		}
	}
	return ""
}

func sortedKeys(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
