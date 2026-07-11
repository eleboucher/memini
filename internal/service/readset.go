package service

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/eleboucher/memini/internal/httputil"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/store"
)

// scopeEntry is one namespace in a read-set with an optional tier restriction.
type scopeEntry struct {
	ns    string
	tiers []memory.Tier // nil = use the request's tier filter; non-nil = override
}

// readScope carries the scope-control inputs a read accepts.
type readScope struct {
	primary  string        // request namespace (always included, always first)
	home     string        // caller's personal namespace (X-Memini-Home); "" = no home leg
	explicit []string      // per-call namespaces; non-empty REPLACES the default read set
	subtree  bool          // legacy scope=subtree on primary, or Scope "everywhere"
	bare     bool          // Scope "project": primary only, skip ancestors/home/links
	reqTiers []memory.Tier // the request's tier filter (empty = all tiers)
}

// resolveReadSet computes the namespaces (and any per-namespace tier override)
// a read operation sees. Two mutually exclusive paths:
//
//   - explicit (sc.explicit non-empty): the read set becomes exactly those
//     entries — validated, "/*" expanded, deduped. No cascade merge, no
//     subtree of primary; explicit means explicit, and it wins over sc.bare/
//     sc.subtree/Scope regardless of value (gap G8).
//   - default (sc.explicit empty): primary, optionally its subtree, plus the
//     cascade legs — ancestors, home, and links — contributing durable tiers
//     only. sc.bare skips the cascade legs entirely (primary/subtree only).
//
// The primary namespace, when present in the result, is always ordered first
// and is never dropped by the post-expansion clamp; on the default path, the
// caller's home namespace is likewise clamp-proof (see promoteProtected),
// front-ordered right after primary, but only when the set actually exceeds
// the cap — under-cap sets keep their natural cascade order (ancestors, then
// home, then links), preserving nearest-first tie-break behavior. At most one
// ListNamespaces call is made — shared by subtree expansion and every "/*"
// pattern — and none when neither is present.
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

// ancestorsOf returns every proper path prefix of ns, nearest first:
// "acme/phoenix/api" -> ["acme/phoenix", "acme"]. A flat namespace (no "/")
// returns nil. Nearest-first ordering is load-bearing: FuseScores breaks
// score ties by first-seen order across namespaces (search/fusion.go:53-68),
// so closer ancestors must be appended before farther ones.
func ancestorsOf(ns string) []string {
	var out []string
	for i := strings.LastIndexByte(ns, '/'); i > 0; i = strings.LastIndexByte(ns[:i], '/') {
		out = append(out, ns[:i])
	}
	return out
}

// intersectDurableTiers restricts a namespace link's tier override to the
// durable set gt: an empty link tiers list means "the full durable set" (gt
// itself, unrestricted beyond the global durable-only rule) — checked by
// length, not nil, because store.NamespaceLink documents nil as that
// sentinel but a JSON-backed driver round-trips a nil slice as an empty one
// (see sqlitevec's tiersOrEmpty/OrEmptySlice convention used store-wide), so
// len(linkTiers) == 0 must mean the same thing regardless of nilness. A
// non-empty override is intersected with gt, which may yield an empty
// (non-durable) result when the link only lists non-durable tiers
// (episodic/working) — the global tier rule (only semantic/procedural cross
// a namespace boundary) always wins over a link's own configuration.
func intersectDurableTiers(linkTiers, gt []memory.Tier) []memory.Tier {
	if len(linkTiers) == 0 {
		return gt
	}
	out := make([]memory.Tier, 0, len(gt))
	for _, t := range gt {
		if slices.Contains(linkTiers, t) {
			out = append(out, t)
		}
	}
	return out
}

// resolveDefaultReadSet builds primary (+ subtree, when requested) plus, on
// the non-bare path, the cascade legs in order — ancestors (nearest first),
// home, then stored links — each contributing durable tiers only (see
// intersectDurableTiers for links). For each leg: skipped when unset,
// already present (primary, a subtree member, or an earlier cascade leg —
// widest tiers win, never narrowed), or the request's tier filter admits no
// durable tier. Widest-tier-wins always holds: an incoming nil (full) tier
// override widens an existing narrower one in place, but a narrower incoming
// override never displaces an existing nil one.
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

	if sc.bare {
		// Scope "project": primary (+ subtree) only, no cascade.
		return entries, nil
	}

	// addEntry merges ns into entries with the given tier override. When ns is
	// already present (primary, a subtree member, or an earlier cascade leg),
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
		// No durable tier is admitted, so the cascade legs would search
		// nothing — skip them without an unnecessary ListNamespaces/ListLinks
		// call.
		return entries, nil
	}

	// Ancestors: every proper path prefix of primary, nearest first.
	for _, a := range ancestorsOf(sc.primary) {
		addEntry(a, gt)
	}

	// Home: the caller's personal namespace, when configured.
	if sc.home != "" {
		addEntry(sc.home, gt)
	}

	// Links: stored one-hop read edges from primary. Optional capability —
	// degrade gracefully (no links leg) against a store that predates
	// LinkStore.
	if ls, ok := s.store.(store.LinkStore); ok {
		links, err := ls.ListLinks(ctx, sc.primary)
		if err != nil {
			return nil, fmt.Errorf("read-set: list links: %w", err)
		}
		for _, l := range links {
			tiers := intersectDurableTiers(l.Tiers, gt)
			if len(tiers) == 0 {
				// The link's own tier override admits no durable tier; the
				// global tier rule means it contributes nothing.
				continue
			}
			addEntry(l.Dst, tiers)
		}
	}

	return promoteProtected(entries, sc.home), nil
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
// nested under it (mirrors subtreeFrom); every entry keeps nil tiers, i.e.
// the request's own tier filter, not the default cascade's durable-only
// override.
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

// promoteProtected moves each protected namespace's entry (when present, and
// not already primary) to immediately after primary, in the priority order
// given, so the post-expansion clamp (which keeps the front of the slice)
// never drops it out from behind a large subtree/link expansion. It only
// reorders when the clamp will actually fire (len > readSetMaxEntries): entry
// order is observable beyond the clamp (FuseScores breaks exact score ties by
// first-seen order across namespaces), so an under-cap read set keeps the
// cascade's natural order (ancestors, then home, then links), preserving
// nearest-first tie-break behavior exactly. Over the cap the reorder is safe:
// addEntry above only ever widens an existing entry, never narrows it, so
// moving an entry earlier cannot change its tier access, only guarantee it
// survives the clamp (and an over-cap set is new behavior with no prior
// ordering to preserve). A no-op when no protected namespace is present or
// the set is within the cap.
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

// parseScope maps the client-facing Scope string (RecallInput.Scope,
// BriefingOpts.Scope) to the readScope bare/subtree flags it contributes.
// This mapping lives at the request boundary, not inside resolveReadSet:
// Scope is a convenience name for a bare/subtree combination, not a
// read-set concept of its own.
//
//   - "" or "full" (default): the cascade (ancestors + home + links), no
//     subtree.
//   - "project": primary only (bare), no cascade.
//   - "everywhere": the cascade, plus subtree.
//
// Any other value is a caller error, listing the valid options.
func parseScope(scope string) (bare, subtree bool, err error) {
	switch scope {
	case "", "full":
		return false, false, nil
	case "project":
		return true, false, nil
	case "everywhere":
		return false, true, nil
	default:
		return false, false, invalidInputf(
			"scope: %q is not valid (want \"project\", \"full\", or \"everywhere\")", scope)
	}
}
