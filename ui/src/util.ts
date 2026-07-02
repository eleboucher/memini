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
  working: 'Raw, short-lived observations — session scratch (24h TTL).',
  episodic: 'Summaries of what happened in a session (90-day TTL).',
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
