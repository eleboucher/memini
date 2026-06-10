import { baseUrl, namespace, namespaceHeader } from './store'
import type {
  FsckReport,
  ListResponse,
  Memory,
  NamespacesResponse,
  Scored,
  SearchResponse,
  Stats,
  Tier,
} from './types'

export class ApiError extends Error {
  constructor(
    public status: number,
    message: string,
  ) {
    super(message)
    this.name = 'ApiError'
  }
}

function headers(extra?: Record<string, string>, ns?: string): Record<string, string> {
  const h: Record<string, string> = { ...extra }
  // An explicit ns overrides the active namespace (used to scope a request to a
  // specific project without switching the global selection).
  const effective = ns ?? namespace.value
  if (effective) h[namespaceHeader.value] = effective
  return h
}

async function req<T>(method: string, path: string, body?: unknown, ns?: string): Promise<T> {
  const init: RequestInit = {
    method,
    headers: headers(body ? { 'Content-Type': 'application/json' } : undefined, ns),
  }
  if (body) init.body = JSON.stringify(body)

  let res: Response
  try {
    res = await fetch(baseUrl.value + path, init)
  } catch (e) {
    throw new ApiError(0, `network error: ${(e as Error).message}`)
  }

  if (res.status === 204) return undefined as T
  const text = await res.text()
  const data = text ? safeParse(text) : undefined
  if (!res.ok) {
    const msg = (data as { error?: string } | undefined)?.error ?? res.statusText
    throw new ApiError(res.status, msg)
  }
  return data as T
}

function safeParse(text: string): unknown {
  try {
    return JSON.parse(text)
  } catch {
    return { error: text }
  }
}

export interface ListParams {
  tiers?: Tier[]
  includeExpired?: boolean
  includeSuperseded?: boolean
  limit?: number
}

// isAllProjects reports the "All projects" aggregate mode: the active namespace
// is unset, so reads fan out across every namespace and merge client-side.
export function isAllProjects(): boolean {
  return namespace.value === ''
}

// ---- scoped (single-namespace) requests -----------------------------------
// `ns` undefined => use the active namespace header; never reached with an
// empty active namespace because the public methods route that to aggregation.

function scopedStats(ns?: string) {
  return req<Stats>('GET', '/v1/stats', undefined, ns)
}

function scopedList(p: ListParams, ns?: string) {
  const q = new URLSearchParams()
  p.tiers?.forEach((t) => q.append('tier', t))
  if (p.includeExpired) q.set('include_expired', 'true')
  if (p.includeSuperseded) q.set('include_superseded', 'true')
  if (p.limit) q.set('limit', String(p.limit))
  const qs = q.toString()
  return req<ListResponse>('GET', '/v1/memories' + (qs ? `?${qs}` : ''), undefined, ns).then(
    (r) => r.memories ?? [],
  )
}

function scopedSearch(query: string, opts: SearchOpts, ns?: string) {
  return req<SearchResponse>(
    'POST',
    '/v1/search',
    { query, tiers: opts.tiers?.length ? opts.tiers : undefined, limit: opts.limit ?? 20 },
    ns,
  ).then((r) => r.results ?? [])
}

interface SearchOpts {
  tiers?: Tier[]
  limit?: number
}

// ---- "All projects" aggregation -------------------------------------------
// NOTE: these fan out one request per namespace and merge in the browser
// (an N+1 against the API). Fine for an admin UI over a handful of projects;
// if namespace counts grow, add server-side aggregate endpoints instead.

async function statsAll(): Promise<Stats> {
  const names = await listNamespaces()
  const per = (await Promise.all(names.map((n) => scopedStats(n).catch(() => null)))).filter(
    (s): s is Stats => s !== null,
  )
  const merged: Stats = {
    namespace: '',
    total: 0,
    by_tier: {},
    expired: 0,
    superseded: 0,
    total_accesses: 0,
    avg_importance: 0,
  }
  let importanceWeighted = 0
  for (const s of per) {
    merged.total += s.total
    merged.expired += s.expired
    merged.superseded += s.superseded
    merged.total_accesses += s.total_accesses
    // Weight by live total; skip namespaces with no live memories so a stale
    // avg_importance can't pollute the merged average.
    if (s.total > 0) importanceWeighted += s.avg_importance * s.total
    for (const t of Object.keys(s.by_tier) as Tier[]) {
      merged.by_tier[t] = (merged.by_tier[t] ?? 0) + (s.by_tier[t] ?? 0)
    }
    if (s.last_write_at && (!merged.last_write_at || newer(s.last_write_at, merged.last_write_at))) {
      merged.last_write_at = s.last_write_at
    }
  }
  merged.avg_importance = merged.total ? importanceWeighted / merged.total : 0
  return merged
}

async function listAll(p: ListParams): Promise<Memory[]> {
  const names = await listNamespaces()
  const lists = await Promise.all(names.map((n) => scopedList(p, n).catch(() => [])))
  return lists.flat()
}

async function searchAll(query: string, opts: SearchOpts): Promise<Scored[]> {
  const names = await listNamespaces()
  // Over-fetch per namespace so the merged global top-N isn't truncated by each
  // namespace's local cutoff before merging.
  const want = opts.limit ?? 20
  const perNs = Math.min(want * Math.max(1, names.length), 200)
  const all = await Promise.all(
    names.map((n) => scopedSearch(query, { ...opts, limit: perNs }, n).catch(() => [])),
  )
  return all
    .flat()
    .sort((a, b) => b.score - a.score)
    .slice(0, want)
}

// newer compares two timestamps by absolute instant (not lexically), so mixed
// timezone offsets sort correctly.
function newer(a: string, b: string): boolean {
  return new Date(a).getTime() > new Date(b).getTime()
}

function listNamespaces() {
  return req<NamespacesResponse>('GET', '/v1/namespaces').then((r) => r.namespaces ?? [])
}

export const api = {
  // Active-namespace aware: aggregate across all projects when "All projects"
  // is selected, otherwise scope to the active namespace.
  stats: () => (isAllProjects() ? statsAll() : scopedStats()),
  list: (p: ListParams = {}) => (isAllProjects() ? listAll(p) : scopedList(p)),
  search: (query: string, opts: SearchOpts = {}) =>
    isAllProjects() ? searchAll(query, opts) : scopedSearch(query, opts),

  // statsFor fetches stats for one explicit namespace, ignoring the active
  // selection. Backs the Projects landing.
  statsFor: (ns: string) => scopedStats(ns),

  namespaces: listNamespaces,

  get: (id: string, ns?: string) => req<Memory>('GET', `/v1/memories/${encodeURIComponent(id)}`, undefined, ns),

  // remove must scope to the memory's own namespace: in "All projects" mode the
  // active namespace is empty, so without this the server would fall back to
  // its default namespace and delete the wrong record (or 404).
  remove: (id: string, ns?: string) =>
    req<void>('DELETE', `/v1/memories/${encodeURIComponent(id)}`, undefined, ns),

  fsck: () => req<FsckReport>('POST', '/v1/fsck'),
}
