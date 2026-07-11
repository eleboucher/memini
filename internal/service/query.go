package service

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/eleboucher/memini/internal/maintenance"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/store"
)

// ListInput selects a slice of a namespace's memories for browsing. The zero
// value (besides Namespace) lists all live memories, newest store order.
type ListInput struct {
	Namespace string
	Tiers     []memory.Tier
	// Levels restricts the listing to memories whose derivation level matches one
	// of the listed values; empty means no level constraint.
	Levels []memory.Level
	// Tags narrows the listing to memories carrying every listed tag (AND).
	Tags []string
	// Metadata narrows the listing to memories whose top-level metadata contains
	// each listed key=value string pair (AND).
	Metadata          map[string]string
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
		Levels:            in.Levels,
		Tags:              in.Tags,
		Metadata:          in.Metadata,
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
	Namespace    string              `json:"namespace"`
	Total        int                 `json:"total"`                    // live memories (excludes expired/superseded)
	ByTier       map[memory.Tier]int `json:"by_tier"`                  // live count per tier
	ByMemoryType map[string]int      `json:"by_memory_type,omitempty"` // live count per metadata.memory_type (typed extractions)
	Expired      int                 `json:"expired"`                  // past-TTL, not yet swept
	Superseded   int                 `json:"superseded"`               // contradiction-tombstoned
	// uncorroborated durable debris (confidence below the demote floor); unbounded
	// by short-term caps, so a growing value signals reclaimable bloat
	LowConfidenceDurable int        `json:"low_confidence_durable"`
	TotalAccesses        int        `json:"total_accesses"`
	AvgImportance        float64    `json:"avg_importance"`
	LastWriteAt          *time.Time `json:"last_write_at,omitempty"`
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

	st := Stats{Namespace: namespace, ByTier: map[memory.Tier]int{}, ByMemoryType: map[string]int{}}
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
			if mt, ok := m.Metadata["memory_type"].(string); ok && mt != "" {
				st.ByMemoryType[mt]++
			}
			if m.Tier.Term() == memory.LongTerm && m.EffectiveConfidence(now) < memory.ConfidenceDemoteFloor {
				st.LowConfidenceDurable++
			}
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
	merged := Stats{ByTier: map[memory.Tier]int{}, ByMemoryType: map[string]int{}}
	var importanceWeighted float64
	for _, ns := range names {
		st, err := s.Stats(ctx, ns)
		if err != nil {
			return Stats{}, err
		}
		merged.Total += st.Total
		merged.Expired += st.Expired
		merged.Superseded += st.Superseded
		merged.LowConfidenceDurable += st.LowConfidenceDurable
		merged.TotalAccesses += st.TotalAccesses
		// Weight by live total so the merged average isn't skewed by empty or
		// tombstone-only namespaces.
		importanceWeighted += st.AvgImportance * float64(st.Total)
		for tier, n := range st.ByTier {
			merged.ByTier[tier] += n
		}
		for mt, n := range st.ByMemoryType {
			merged.ByMemoryType[mt] += n
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

// Briefing is a layered session-start summary of a namespace: the most durable
// facts and procedures, the most recent episodic activity, and pinned memories.
type Briefing struct {
	Namespace string `json:"namespace"`
	// ScopeHeader is a one-line, human-readable summary of the read-set this
	// briefing drew from: the primary namespace, then each cascade leg that
	// actually contributed durable memories (nearest ancestor first, home
	// last), then a "+K link(s)" suffix counting contributing links. See
	// scopeHeader for the exact format and edge-case decisions.
	ScopeHeader string           `json:"scope_header,omitempty"`
	Facts       []*memory.Memory `json:"facts,omitempty"`      // semantic, highest-retention first
	Procedures  []*memory.Memory `json:"procedures,omitempty"` // procedural, highest-retention first
	Recent      []*memory.Memory `json:"recent,omitempty"`     // episodic, newest first
	Pinned      []*memory.Memory `json:"pinned,omitempty"`     // tagged pinned, any tier
	// Children summarizes the direct child namespaces (one segment deeper)
	// under the primary namespace, each aggregating its whole subtree —
	// most-recent write first, capped at childRollupMaxChildren. Empty at a
	// leaf namespace.
	Children []ChildSummary `json:"children,omitempty"`
	// ChildrenTruncated is the number of direct children omitted by the
	// childRollupMaxChildren cap (0 when everything fit). The REST wire shape
	// (T6) has no dedicated field for it, so renderers surface it themselves
	// (MCP appends an "… and N more" note; REST returns just the capped array).
	ChildrenTruncated int `json:"children_truncated,omitempty"`
}

// ChildSummary is one direct-child rollup entry in a Briefing: the child
// namespace, its all-tier live memory count, and small pinned/recent-durable
// highlight sets (each capped at childRollupPerSection). All figures aggregate
// the child's entire subtree, so a leaf-heavy tree (memories only in
// grandchildren) still surfaces at the interior node.
type ChildSummary struct {
	NS     string           `json:"namespace"`
	Total  int              `json:"total"`
	Pinned []*memory.Memory `json:"pinned,omitempty"`
	Recent []*memory.Memory `json:"recent,omitempty"`
}

const (
	// childRollupMaxChildren caps how many direct children a briefing rolls up
	// (gap G9): the most recently written win, the rest are counted in
	// Briefing.ChildrenTruncated — so a wide tenant root can't balloon
	// briefing cost or token size.
	childRollupMaxChildren = 10
	// childRollupPerSection caps each child's pinned and recent highlight
	// lists, keeping the rollup a glanceable index rather than a briefing of
	// its own.
	childRollupPerSection = 3
)

// BriefingOpts sets per-section caps for a Briefing. A nil field falls back
// to DefaultPerSection (5); a pointer to 0 explicitly disables the section so
// callers can opt sections out without rebalancing the others. Section caps are
// independent: pinned memories count against Pinned and never against
// Facts/Procedures/Recent, so an operator can keep a small durable
// "top-of-mind" set always-injected while still capping the per-section recall.
type BriefingOpts struct {
	Pinned     *int
	Facts      *int
	Procedures *int
	Recent     *int
	// Namespaces, when non-empty, REPLACES the default read set (namespace,
	// subtree, ancestors, home, and links) with exactly these namespaces —
	// same replace-not-extend semantics as RecallInput.Namespaces. Each is
	// read with all tiers.
	Namespaces []string
	// Subtree expands the briefing to namespace and every namespace nested
	// under it, same semantics as RecallInput.Subtree. Ignored when Namespaces
	// is set.
	Subtree bool
	// Home is the caller's personal namespace, merged read-only into the
	// default read set — durable tiers only. See RecallInput.Home.
	Home string
	// Scope selects the read-set shape, same semantics as RecallInput.Scope:
	// "" or "full" (default), "project" (bare), or "everywhere" (+ subtree).
	// Ignored when Namespaces is set. An unrecognized value is an
	// invalid-input error.
	Scope string
	// ReadSet (output-only) is set to the resolved read-set this briefing
	// drew from, with per-leg origin — same out-param pattern as
	// RecallInput.ReadSet. The caller passes the address of a local slice;
	// nil disables reporting.
	ReadSet *[]ReadSetEntry
}

// DefaultPerSection is the briefing cap applied to any section whose dedicated
// opt is nil. It mirrors the historical "per_section=N" default so callers that
// don't pass per-section options see the same behavior.
const DefaultPerSection = 5

// Briefing builds a session-start briefing for a namespace: up to Facts
// semantic facts (ranked by DurableScore), Procedures procedural how-tos,
// Recent episodic entries (newest first), and Pinned pinned memories (any tier).
// Each opt is a pointer so nil falls back to DefaultPerSection and a pointer
// to 0 disables the section. It is a cheap, query-less read for hooks to
// inject context when a session opens.
func (s *Service) Briefing(ctx context.Context, namespace string, opts BriefingOpts) (Briefing, error) {
	resolve := func(p *int) int {
		if p == nil {
			return DefaultPerSection
		}
		return *p
	}
	pinnedN := resolve(opts.Pinned)
	factsN := resolve(opts.Facts)
	procsN := resolve(opts.Procedures)
	recentN := resolve(opts.Recent)
	now := s.now()
	bare, scopeSubtree, err := parseScope(opts.Scope)
	if err != nil {
		return Briefing{}, err
	}
	entries, err := s.resolveReadSet(ctx, readScope{
		primary:  namespace,
		home:     opts.Home,
		explicit: opts.Namespaces,
		subtree:  opts.Subtree || scopeSubtree,
		bare:     bare,
	})
	if err != nil {
		return Briefing{}, err
	}
	if opts.ReadSet != nil {
		*opts.ReadSet = toReadSetEntries(entries)
	}
	b := Briefing{Namespace: namespace}
	var facts, procs, recent []*memory.Memory
	// bucket sorts a memory into the briefing's sections. A durable-only entry
	// (an ancestor/home/link cascade leg; explicit entries are never
	// durable-only) contributes semantic/procedural facts only — no
	// episodic/working — so shared cross-namespace context surfaces without
	// dragging ancestor/home/link chatter into every namespace's briefing.
	bucket := func(m *memory.Memory, durableOnly bool) {
		switch m.Tier {
		case memory.TierSemantic:
			facts = append(facts, m)
		case memory.TierProcedural:
			procs = append(procs, m)
		case memory.TierEpisodic:
			if durableOnly {
				return
			}
			recent = append(recent, m)
		default:
			if durableOnly {
				return
			}
		}
		if slices.Contains(m.Tags, maintenance.PinnedTag) {
			b.Pinned = append(b.Pinned, m)
		}
	}
	// durableByNS tallies, per read-set namespace, the durable (semantic/
	// procedural) memories that leg contributed to THIS briefing — the counts
	// the scope header renders. Tallied here, in the same loop that feeds the
	// sections, so header numbers always match what was actually fetched.
	durableByNS := make(map[string]int, len(entries))
	for _, e := range entries {
		// Push the entry's tier override into the List filter so a
		// durable-only entry never loads episodic/working rows in the first
		// place, rather than fetching and discarding them in bucket.
		mems, err := s.store.List(ctx, e.ns, store.Filter{Now: now, Tiers: e.tiers}, 0)
		if err != nil {
			return Briefing{}, err
		}
		for _, m := range mems {
			if m.Tier.Term() == memory.LongTerm {
				durableByNS[e.ns]++
			}
			bucket(m, e.tiers != nil)
		}
	}
	b.ScopeHeader = scopeHeader(namespace, entries, durableByNS)
	// Rank durable sections by DurableScore (no recency decay), scored once per
	// memory rather than inside the comparator.
	byDurable := func(ms []*memory.Memory) {
		score := make(map[string]float64, len(ms))
		for _, m := range ms {
			score[m.ID] = m.DurableScore(now)
		}
		sort.SliceStable(ms, func(i, j int) bool { return score[ms[i].ID] > score[ms[j].ID] })
	}
	byDurable(facts)
	byDurable(procs)
	sort.SliceStable(recent, func(i, j int) bool { return recent[i].CreatedAt.After(recent[j].CreatedAt) })

	// Pinned sort: by DurableScore first, then created_at desc — so the always
	// injected "top-of-mind" set keeps stable ordering as new pinned memories
	// land (pinned memories are exempt from demotion, but a fresh pin can still
	// shadow an older one with lower durability).
	sortPinned(b.Pinned, now)

	b.Facts = topN(facts, factsN)
	b.Procedures = topN(procs, procsN)
	b.Recent = topN(recent, recentN)
	b.Pinned = topN(b.Pinned, pinnedN)

	b.Children, b.ChildrenTruncated, err = s.childRollup(ctx, namespace, now)
	if err != nil {
		return Briefing{}, err
	}
	return b, nil
}

// sortPinned orders a pinned set by DurableScore desc, then created_at desc —
// the briefing's "top-of-mind" ordering, shared by the main Pinned section and
// each child rollup's pinned highlights.
func sortPinned(ms []*memory.Memory, now time.Time) {
	sort.SliceStable(ms, func(i, j int) bool {
		di := ms[i].DurableScore(now)
		dj := ms[j].DurableScore(now)
		if di != dj {
			return di > dj
		}
		return ms[i].CreatedAt.After(ms[j].CreatedAt)
	})
}

// scopeHeader renders Briefing.ScopeHeader from the resolved read-set entries
// (their recorded origins — never re-derived from strings) and the per-leg
// durable contribution counts tallied in the briefing's fetch loop. Format:
//
//	Scope: acme/phoenix/api ← acme/phoenix(3) ← acme(4) ← personal(2), +1 link
//
// Edge decisions (pinned by tests):
//   - The primary namespace is always shown, even with no legs — a flat
//     namespace renders just "Scope: acme".
//   - A leg that contributed zero durable memories to THIS briefing is
//     omitted (keeps the header short); only primary is unconditional.
//   - Home renders as its namespace name, after the ancestors (entries keep
//     cascade order: ancestors nearest-first, then home).
//   - Links collapse into a "+K link(s)" suffix counting only links that
//     contributed; no suffix when none did.
//   - Explicit per-call namespaces (origin "call") are not rendered: an
//     explicit read-set is already self-described by the caller, so the
//     header shows just the primary.
func scopeHeader(primary string, entries []scopeEntry, durable map[string]int) string {
	var sb strings.Builder
	sb.WriteString("Scope: ")
	sb.WriteString(primary)
	links := 0
	for _, e := range entries {
		n := durable[e.ns]
		if n == 0 {
			continue
		}
		switch e.origin {
		case OriginAncestor, OriginHome:
			fmt.Fprintf(&sb, " ← %s(%d)", e.ns, n)
		case OriginLink:
			links++
		}
	}
	if links > 0 {
		fmt.Fprintf(&sb, ", +%d link", links)
		if links > 1 {
			sb.WriteString("s")
		}
	}
	return sb.String()
}

// childRollup builds Briefing.Children: one ChildSummary per DIRECT child
// namespace (one segment deeper than primary), each aggregating its entire
// subtree — so leaf-heavy trees (memories only in grandchildren) still
// surface at the interior node. Children are ordered by most-recent write
// (name asc on ties) and capped at childRollupMaxChildren, returning the
// omitted count. A leaf namespace returns (nil, 0, nil).
//
// Cost: one ListNamespaces (cheap SELECT DISTINCT) plus one store.List per
// namespace strictly under primary — the same per-namespace full-list shape
// Briefing's own subtree path, Stats, and StatsAll already use. Total is an
// all-tier count and the children sort needs every child's last write, so
// small-limit queries can't replace the full list until the store grows a
// count/max aggregate; the wire stays bounded regardless via the per-section
// (3) and per-children (10) caps.
func (s *Service) childRollup(ctx context.Context, primary string, now time.Time) ([]ChildSummary, int, error) {
	all, err := s.store.ListNamespaces(ctx)
	if err != nil {
		return nil, 0, err
	}
	prefix := primary + "/"
	type childAgg struct {
		total     int
		lastWrite time.Time
		pinned    []*memory.Memory
		durable   []*memory.Memory
	}
	aggs := map[string]*childAgg{}
	for _, ns := range all {
		if !strings.HasPrefix(ns, prefix) {
			continue
		}
		seg := ns[len(prefix):]
		if i := strings.IndexByte(seg, '/'); i >= 0 {
			seg = seg[:i]
		}
		child := prefix + seg
		a := aggs[child]
		if a == nil {
			a = &childAgg{}
			aggs[child] = a
		}
		mems, err := s.store.List(ctx, ns, store.Filter{Now: now}, 0)
		if err != nil {
			return nil, 0, err
		}
		for _, m := range mems {
			a.total++
			if m.CreatedAt.After(a.lastWrite) {
				a.lastWrite = m.CreatedAt
			}
			if slices.Contains(m.Tags, maintenance.PinnedTag) {
				a.pinned = append(a.pinned, m)
			}
			if m.Tier.Term() == memory.LongTerm {
				a.durable = append(a.durable, m)
			}
		}
	}
	if len(aggs) == 0 {
		return nil, 0, nil
	}
	names := make([]string, 0, len(aggs))
	for n := range aggs {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		wi, wj := aggs[names[i]].lastWrite, aggs[names[j]].lastWrite
		if !wi.Equal(wj) {
			return wi.After(wj)
		}
		return names[i] < names[j]
	})
	truncated := 0
	if len(names) > childRollupMaxChildren {
		truncated = len(names) - childRollupMaxChildren
		names = names[:childRollupMaxChildren]
	}
	out := make([]ChildSummary, len(names))
	for i, n := range names {
		a := aggs[n]
		sortPinned(a.pinned, now)
		sort.SliceStable(a.durable, func(x, y int) bool { return a.durable[x].CreatedAt.After(a.durable[y].CreatedAt) })
		out[i] = ChildSummary{
			NS:     n,
			Total:  a.total,
			Pinned: topN(a.pinned, childRollupPerSection),
			Recent: topN(a.durable, childRollupPerSection),
		}
	}
	return out, truncated, nil
}

func topN(ms []*memory.Memory, n int) []*memory.Memory {
	if len(ms) > n {
		return ms[:n]
	}
	return ms
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
