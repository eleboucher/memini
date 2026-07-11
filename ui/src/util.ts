import type { Memory, Tier } from './types'

const TIER_COLOR: Record<Tier, string> = {
  working: 'var(--tier-working)',
  episodic: 'var(--tier-episodic)',
  semantic: 'var(--tier-semantic)',
  procedural: 'var(--tier-procedural)',
}

export function tierColor(t: Tier): string {
  return TIER_COLOR[t] ?? 'var(--muted)'
}

// One-line meaning of each tier (and its lifetime), surfaced as legend tooltips.
const TIER_DESC: Record<Tier, string> = {
  working: 'Default intake — raw scratch (72h TTL).',
  episodic: 'Summaries of what happened in a session (30-day TTL).',
  semantic: 'Durable extracted facts — what I know (never expires).',
  procedural: 'Workflows and how-to knowledge (never expires).',
}

export function tierDesc(t: Tier): string {
  return TIER_DESC[t] ?? ''
}

// Typed-extraction classes (decision/preference/problem) and their chart colors.
export const MEMORY_TYPES = ['decision', 'preference', 'problem'] as const

const MEMORY_TYPE_COLOR: Record<string, string> = {
  decision: 'var(--tier-semantic)',
  preference: 'var(--tier-procedural)',
  problem: 'var(--tier-working)',
}

export function memoryTypeColor(t: string): string {
  return MEMORY_TYPE_COLOR[t] ?? 'var(--muted)'
}

// memoryType reads the typed-extraction class stamped in a memory's metadata, or
// undefined when it carries none.
export function memoryType(m: Memory): string | undefined {
  const mt = (m.metadata as Record<string, unknown> | undefined)?.memory_type
  return typeof mt === 'string' && mt ? mt : undefined
}

// isAutoTiered reports whether the write-time marker classifier chose this
// memory's tier (metadata.tier_classified=marker) — machine-curated durability
// the user may want to audit.
export function isAutoTiered(m: Memory): boolean {
  return (m.metadata as Record<string, unknown> | undefined)?.tier_classified === 'marker'
}

// promotedFrom returns the episodic source ID when this fact was produced by
// the promotion pass, or undefined for a direct write.
export function promotedFrom(m: Memory): string | undefined {
  const src = (m.metadata as Record<string, unknown> | undefined)?.promoted_from
  return typeof src === 'string' && src ? src : undefined
}

// relTime renders a compact, human relative timestamp ("3h", "2d", "just now").
export function relTime(iso?: string): string {
  if (!iso) return '—'
  const t = new Date(iso).getTime()
  if (Number.isNaN(t)) return '—'
  const s = Math.max(0, (Date.now() - t) / 1000)
  if (s < 45) return 'just now'
  const m = s / 60
  if (m < 60) return `${Math.round(m)}m ago`
  const h = m / 60
  if (h < 24) return `${Math.round(h)}h ago`
  const d = h / 24
  if (d < 30) return `${Math.round(d)}d ago`
  const mo = d / 30
  if (mo < 12) return `${Math.round(mo)}mo ago`
  return `${Math.round(mo / 12)}y ago`
}

export function fmtDate(iso?: string): string {
  if (!iso) return '—'
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return '—'
  return d.toLocaleString(undefined, {
    year: 'numeric',
    month: 'short',
    day: 'numeric',
    hour: '2-digit',
    minute: '2-digit',
  })
}

export function shortId(id: string): string {
  return id.length > 12 ? id.slice(0, 8) : id
}

export function num(n: number): string {
  return n.toLocaleString()
}

// ---- Namespace hierarchy -----------------------------------------------
// Namespaces are '/'-separated paths ("acme/phoenix/api"). These helpers turn
// a flat list of namespace strings into the tree implied by those paths, so
// every view that groups namespaces (Projects, the namespace selector, the
// Graph namespace mode) nests consistently at every depth instead of only
// splitting on the first segment.

export interface NsNode {
  ns: string // full path, e.g. "acme/phoenix/api"
  leaf: boolean // true when `ns` itself appeared in the input list (holds memories)
  children: NsNode[]
}

// nsTree builds the forest implied by `namespaces`: a node exists for every
// path prefix that either appears in the list or is an ancestor of one that
// does (e.g. "acme/phoenix/api" implies synthetic "acme" and "acme/phoenix"
// nodes when those exact namespaces hold no memories of their own — leaf is
// false for those). Roots and each node's children are sorted alphabetically.
export function nsTree(namespaces: string[]): NsNode[] {
  const byNs = new Map<string, NsNode>()
  const roots: NsNode[] = []

  const getOrCreate = (ns: string): NsNode => {
    let node = byNs.get(ns)
    if (node) return node
    node = { ns, leaf: false, children: [] }
    byNs.set(ns, node)
    const slash = ns.lastIndexOf('/')
    if (slash === -1) {
      roots.push(node)
    } else {
      getOrCreate(ns.slice(0, slash)).children.push(node)
    }
    return node
  }

  for (const ns of namespaces) getOrCreate(ns).leaf = true

  const sortRec = (nodes: NsNode[]) => {
    nodes.sort((a, b) => a.ns.localeCompare(b.ns))
    for (const n of nodes) sortRec(n.children)
  }
  sortRec(roots)

  return roots
}

// ancestorsOf returns every path-prefix ancestor of `ns`, root-first (e.g.
// "acme/phoenix/api" -> ["acme", "acme/phoenix"]). Empty for a top-level
// namespace or the empty string.
export function ancestorsOf(ns: string): string[] {
  if (!ns) return []
  const parts = ns.split('/')
  const out: string[] = []
  for (let i = 1; i < parts.length; i++) out.push(parts.slice(0, i).join('/'))
  return out
}

// depth is the number of path separators in `ns` (0 for a top-level
// namespace, 2 for "acme/phoenix/api").
export function depth(ns: string): number {
  return ns ? ns.split('/').length - 1 : 0
}

// rootOf returns the top-level (depth-0) segment of `ns`, e.g.
// "acme/phoenix/api" -> "acme"; a namespace with no separator is its own root.
export function rootOf(ns: string): string {
  const i = ns.indexOf('/')
  return i === -1 ? ns : ns.slice(0, i)
}

// fromLabel renders a ScoredMemory/BriefingItem `from` provenance string as a
// short chip label. Per the schema, the server sends a bare namespace name
// for both the "ancestor" and "home" read-set origins — the UI tells them
// apart by checking whether that name is a path-ancestor of the namespace
// the producing request actually ran under (an ancestor cascades in by path
// prefix; home doesn't), and otherwise labels it "personal". "link:" and
// "call:" values are already prefixed by the server and just get a
// friendlier separator.
//
// `queriedNamespace` MUST be the namespace captured when the results were
// fetched, never the live namespace signal: results outlive a selector
// switch, and disambiguating a stale hit against the new active namespace
// would silently flip an ancestor label to "personal" (or vice versa)
// without any refetch.
export function fromLabel(from: string, queriedNamespace: string): string {
  if (from.startsWith('link:')) return `link: ${from.slice(5)}`
  if (from.startsWith('call:')) return `call: ${from.slice(5)}`
  if (ancestorsOf(queriedNamespace).includes(from)) return from
  return 'personal'
}
