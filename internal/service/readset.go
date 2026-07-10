package service

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/eleboucher/memini/internal/httputil"
	"github.com/eleboucher/memini/internal/memory"
)

// scopeEntry is one namespace in a read-set with an optional tier restriction.
type scopeEntry struct {
	ns    string
	tiers []memory.Tier // nil = use the request's tier filter; non-nil = override
}

// readScope carries the scope-control inputs a read accepts.
type readScope struct {
	primary  string        // request namespace (always included, always first)
	explicit []string      // per-call namespaces; non-empty REPLACES the default read set
	subtree  bool          // legacy scope=subtree on primary
	reqTiers []memory.Tier // the request's tier filter (empty = all tiers)
}

// resolveReadSet computes the namespaces (and any per-namespace tier override)
// a read operation sees. Two mutually exclusive paths:
//
//   - explicit (sc.explicit non-empty): the read set becomes exactly those
//     entries — validated, "/*" expanded, deduped. No global merge, no subtree
//     of primary; explicit means explicit.
//   - default (sc.explicit empty): primary, optionally its subtree, plus the
//     global namespace contributing durable tiers only — the merge
//     addGlobalNamespace performed before this mechanism existed.
//
// The primary namespace, when present in the result, is always ordered first
// and is never dropped by the post-expansion clamp; on the default path, a
// configured global namespace is likewise clamp-proof, front-ordered right
// after primary, but only when the set actually exceeds the cap (see
// promoteGlobal; under-cap sets keep global last, preserving tie-break
// order). At most one ListNamespaces call is made — shared by subtree
// expansion and every "/*" pattern — and none when neither is present.
func (s *Service) resolveReadSet(ctx context.Context, sc readScope) ([]scopeEntry, error) {
	var entries []scopeEntry
	var err error
	if len(sc.explicit) > 0 {
		entries, err = s.resolveExplicitReadSet(ctx, sc.explicit)
	} else {
		entries, err = s.resolveDefaultReadSet(ctx, sc)
	}
	if err != nil {
		return nil, err
	}
	entries = primaryFirst(entries, sc.primary)
	return clampReadSet(ctx, entries), nil
}

// resolveDefaultReadSet builds primary (+ subtree, when requested) plus the
// durable-only merge legs — the single global namespace and the tenant's
// shared namespace (see tenantSharedNamespace) — mirroring the pre-read-set
// addGlobalNamespace exactly, for each: skipped when unset, already present
// (primary, a subtree member, or an earlier merge leg — widest tiers win,
// never narrowed), or the request's tier filter admits no durable tier.
// Widest-tier-wins always holds: an incoming nil (full) tier override widens
// an existing narrower one in place, but a narrower incoming override never
// displaces an existing nil one.
func (s *Service) resolveDefaultReadSet(ctx context.Context, sc readScope) ([]scopeEntry, error) {
	var all []string
	var listed bool
	listAll := func() ([]string, error) {
		if !listed {
			var err error
			all, err = s.store.ListNamespaces(ctx)
			listed = true
			if err != nil {
				return nil, err
			}
		}
		return all, nil
	}

	names := []string{sc.primary}
	if sc.subtree {
		list, err := listAll()
		if err != nil {
			return nil, err
		}
		names = subtreeFrom(list, sc.primary)
	}
	entries := make([]scopeEntry, len(names))
	for i, n := range names {
		entries[i] = scopeEntry{ns: n}
	}

	// addEntry merges ns into entries with the given tier override. When ns is
	// already present (primary, a subtree member, or an earlier merge leg),
	// the wider entry always wins: an incoming nil (full) tier override
	// widens an existing narrower one in place, but a narrower incoming
	// override never displaces an existing nil one.
	addEntry := func(ns string, tiers []memory.Tier) {
		for i := range entries {
			if entries[i].ns == ns {
				if tiers == nil {
					entries[i].tiers = nil
				}
				return
			}
		}
		entries = append(entries, scopeEntry{ns: ns, tiers: tiers})
	}

	gt := durableTiers(sc.reqTiers)

	if len(gt) == 0 {
		// No durable tier is admitted, so the durable-only merge legs would
		// search nothing — skip them without an unnecessary ListNamespaces call.
		return entries, nil
	}

	if s.globalNamespace != "" {
		addEntry(s.globalNamespace, gt)
	}

	// The tenant-shared merge is opt-in (WithTenantShared / MEMINI_TENANT_SHARED),
	// so a namespace stays isolated by default even when it has a tenant segment.
	ts := ""
	if s.tenantShared {
		ts = tenantSharedNamespace(sc.primary)
		if ts != "" {
			addEntry(ts, gt)
		}
	}

	return promoteProtected(entries, s.globalNamespace, ts), nil
}

// tenantSharedLeaf is the conventional shared namespace nested directly under
// a tenant root: facts in work/_shared are readable (durable tiers only) from
// every work/... namespace, the tenant-wide layer between a project's own
// namespace and the global one. The name matches the layout README.md
// recommends.
const tenantSharedLeaf = "_shared"

// tenantSharedNamespace derives the tenant-shared namespace primary
// implicitly reads: the first path segment plus "/_shared" ("work/memini" →
// "work/_shared"). Empty — no merge — when primary has no tenant segment (no
// "/"), so untenanted flat namespaces keep their exact pre-tenant behavior,
// and when primary IS the tenant's shared namespace itself.
func tenantSharedNamespace(primary string) string {
	tenant, _, ok := strings.Cut(primary, "/")
	if !ok || tenant == "" {
		return ""
	}
	ts := tenant + "/" + tenantSharedLeaf
	if ts == primary {
		return ""
	}
	return ts
}

// subtreeFrom filters all to root and every namespace nested under it (root +
// "root/..."), the set a subtree read searches. Pure — callers fetch all via
// the shared lazy ListNamespaces call.
func subtreeFrom(all []string, root string) []string {
	prefix := root + "/"
	out := []string{root}
	for _, ns := range all {
		if ns != root && strings.HasPrefix(ns, prefix) {
			out = append(out, ns)
		}
	}
	return out
}

// resolveExplicitReadSet validates and expands a per-call namespace list into
// the read set it becomes verbatim, in first-occurrence order. An entry
// ending in "/*" expands to the bare namespace plus every namespace strictly
// nested under it (mirrors subtreeFrom); every entry — including one
// that happens to name the global namespace — keeps nil tiers, i.e. the
// request's own tier filter, not the default merge's durable-only override.
func (s *Service) resolveExplicitReadSet(ctx context.Context, raw []string) ([]scopeEntry, error) {
	if len(raw) > readSetMaxExplicit {
		return nil, invalidInputf("read-set: %d namespaces exceeds the %d entry cap", len(raw), readSetMaxExplicit)
	}

	// All "/*" patterns share one ListNamespaces call, fetched lazily so a
	// request with no pattern never scans.
	var all []string
	var listed bool
	listAll := func() ([]string, error) {
		if !listed {
			var err error
			all, err = s.store.ListNamespaces(ctx)
			listed = true
			if err != nil {
				return nil, err
			}
		}
		return all, nil
	}

	var entries []scopeEntry
	seen := make(map[string]bool, len(raw))
	add := func(ns string) {
		if !seen[ns] {
			seen[ns] = true
			entries = append(entries, scopeEntry{ns: ns})
		}
	}
	for _, r := range raw {
		// Normalize before matching: entries are compared literally against
		// stored namespaces, so an untrimmed " work" or "work/" would silently
		// match nothing. The pattern suffix is cut first so a bare "/*" still
		// fails validation (empty base) instead of collapsing to a literal "*".
		ns, isSubtree := strings.CutSuffix(strings.TrimSpace(r), "/*")
		ns = httputil.NormalizeNamespace(ns)
		if err := httputil.ValidateNamespace(ns); err != nil {
			return nil, invalidInputf("read-set: invalid namespace %q: %v", r, err)
		}
		add(ns)
		if !isSubtree {
			continue
		}
		names, err := listAll()
		if err != nil {
			return nil, fmt.Errorf("read-set: list namespaces: %w", err)
		}
		prefix := ns + "/"
		for _, n := range names {
			if n != ns && strings.HasPrefix(n, prefix) {
				add(n)
			}
		}
	}
	return entries, nil
}

// promoteGlobal moves the global namespace's entry (when present, and not
// already primary) to immediately after primary, so the post-expansion clamp
// (which keeps the front of the slice) never drops them out from behind a
// large subtree/pattern expansion. The protected namespaces are the
// durable-only merge legs — the global namespace and the tenant-shared
// namespace — front-ordered in the priority order given. It only reorders when
// the clamp will actually fire (len > readSetMaxEntries): entry order is
// observable beyond the clamp (FuseScores breaks exact score ties by first-seen
// order across namespaces), so an under-cap read set keeps these legs in their
// traditional trailing position, preserving pre-read-set tie-break behavior
// exactly. Over the cap the reorder is safe: addEntry above only ever widens an
// existing entry, never narrows it, so moving an entry earlier cannot change its
// tier access, only guarantee it survives the clamp (and an over-cap set is new
// behavior with no prior ordering to preserve). A no-op when no protected
// namespace is present or the set is within the cap.
func promoteProtected(entries []scopeEntry, protected ...string) []scopeEntry {
	if len(entries) <= readSetMaxEntries || len(entries) == 0 {
		return entries
	}
	// entries[0] is primary (kept first). Collect the protected legs in
	// priority order, skipping empties, primary itself, and any not present.
	front := []scopeEntry{entries[0]}
	taken := map[int]bool{0: true}
	for _, ns := range protected {
		if ns == "" || ns == entries[0].ns {
			continue
		}
		for i := 1; i < len(entries); i++ {
			if !taken[i] && entries[i].ns == ns {
				front = append(front, entries[i])
				taken[i] = true
				break
			}
		}
	}
	if len(front) == 1 {
		return entries // nothing to promote beyond primary
	}
	out := make([]scopeEntry, 0, len(entries))
	out = append(out, front...)
	for i := 1; i < len(entries); i++ {
		if !taken[i] {
			out = append(out, entries[i])
		}
	}
	return out
}

// primaryFirst moves primary's entry to the front when present, preserving
// the relative order of the rest. A no-op when primary is absent (the
// explicit path never force-adds it) or already first.
func primaryFirst(entries []scopeEntry, primary string) []scopeEntry {
	idx := -1
	for i, e := range entries {
		if e.ns == primary {
			idx = i
			break
		}
	}
	if idx <= 0 {
		return entries
	}
	out := make([]scopeEntry, 0, len(entries))
	out = append(out, entries[idx])
	out = append(out, entries[:idx]...)
	out = append(out, entries[idx+1:]...)
	return out
}

// clampReadSet caps a resolved read-set at readSetMaxEntries, keeping the
// front of the slice — primary first, by primaryFirst above — and warning
// with the dropped count. A no-op under the cap.
func clampReadSet(ctx context.Context, entries []scopeEntry) []scopeEntry {
	if len(entries) <= readSetMaxEntries {
		return entries
	}
	dropped := len(entries) - readSetMaxEntries
	slog.WarnContext(ctx, "read-set: clamped after expansion", "kept", readSetMaxEntries, "dropped", dropped)
	return entries[:readSetMaxEntries]
}
