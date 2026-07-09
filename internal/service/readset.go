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
// and is never dropped by the post-expansion clamp. At most one ListNamespaces
// call is made — shared by subtree expansion and every "/*" pattern — and none
// when neither is present.
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
// durable-only merge legs — the single global namespace and the configured
// read-namespaces list (MEMINI_READ_NAMESPACES / WithReadNamespaces) —
// mirroring the pre-read-set addGlobalNamespace exactly, for each: skipped
// when unset, already present (primary, a subtree member, or an earlier merge
// leg — widest tiers win, never narrowed), or the request's tier filter
// admits no durable tier. An entry ending in "/*" expands to itself plus every
// namespace nested under it. All "/*" expansion (subtree and read-namespaces
// alike) shares one lazy ListNamespaces call.
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

	gt := durableTiers(sc.reqTiers)
	if len(gt) == 0 {
		// No durable tier is admitted, so every merge leg below would search
		// nothing — skip them all without an unnecessary ListNamespaces call.
		return entries, nil
	}
	// addDurable merges ns as a durable-only leg unless it's already present
	// (primary, a subtree member, or an earlier leg), which always keeps the
	// wider (nil-tiers) entry rather than narrowing it.
	addDurable := func(ns string) {
		if !containsNamespace(entries, ns) {
			entries = append(entries, scopeEntry{ns: ns, tiers: gt})
		}
	}

	if s.globalNamespace != "" {
		addDurable(s.globalNamespace)
	}

	for _, rn := range s.readNamespaces {
		base, isSubtree := strings.CutSuffix(rn, "/*")
		addDurable(base)
		if !isSubtree {
			continue
		}
		list, err := listAll()
		if err != nil {
			return nil, fmt.Errorf("read-set: list namespaces: %w", err)
		}
		prefix := base + "/"
		for _, n := range list {
			if n != base && strings.HasPrefix(n, prefix) {
				addDurable(n)
			}
		}
	}

	return entries, nil
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
// nested under it (mirrors subtreeNamespaces); every entry — including one
// that happens to name the global namespace — keeps nil tiers, i.e. the
// request's own tier filter, not the default merge's durable-only override.
func (s *Service) resolveExplicitReadSet(ctx context.Context, raw []string) ([]scopeEntry, error) {
	if len(raw) > 16 {
		return nil, invalidInputf("read-set: %d namespaces exceeds the 16 entry cap", len(raw))
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
		ns, isSubtree := strings.CutSuffix(r, "/*")
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

// containsNamespace reports whether entries already holds ns, regardless of
// its tier override.
func containsNamespace(entries []scopeEntry, ns string) bool {
	for _, e := range entries {
		if e.ns == ns {
			return true
		}
	}
	return false
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
