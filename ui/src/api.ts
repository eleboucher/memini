import { apiToken, baseUrl, namespace, namespaceHeader } from './store'
import type {
  DedupReport,
  DedupRequest,
  FsckReport,
  ListResponse,
  Memory,
  NamespacesResponse,
  RenamespaceReport,
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
  // The server bearer-gates /v1, /mcp and /metrics when MEMINI_API_KEY is set;
  // send the configured token so the dashboard works against that setup.
  if (apiToken.value) h['Authorization'] = `Bearer ${apiToken.value}`
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
  tags?: string[]
  metadata?: Record<string, string>
  includeExpired?: boolean
  includeSuperseded?: boolean
  limit?: number
}

// appendFilters adds the shared tier/tag/meta query params to a list request.
function appendFilters(q: URLSearchParams, p: ListParams) {
  p.tiers?.forEach((t) => q.append('tier', t))
  p.tags?.forEach((t) => q.append('tag', t))
  Object.entries(p.metadata ?? {}).forEach(([k, v]) => q.append('meta', `${k}=${v}`))
  if (p.includeExpired) q.set('include_expired', 'true')
  if (p.includeSuperseded) q.set('include_superseded', 'true')
  if (p.limit) q.set('limit', String(p.limit))
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
  appendFilters(q, p)
  const qs = q.toString()
  return req<ListResponse>('GET', '/v1/memories' + (qs ? `?${qs}` : ''), undefined, ns).then(
    (r) => r.memories ?? [],
  )
}

function scopedSearch(query: string, opts: SearchOpts, ns?: string) {
  return req<SearchResponse>(
    'POST',
    '/v1/search',
    {
      query,
      tiers: opts.tiers?.length ? opts.tiers : undefined,
      tags: opts.tags?.length ? opts.tags : undefined,
      metadata: opts.metadata && Object.keys(opts.metadata).length ? opts.metadata : undefined,
      limit: opts.limit ?? 20,
    },
    ns,
  ).then((r) => r.results ?? [])
}

interface SearchOpts {
  tiers?: Tier[]
  tags?: string[]
  metadata?: Record<string, string>
  limit?: number
}

// ---- "All projects" aggregation -------------------------------------------
// stats/list aggregate server-side via all_namespaces=true: one request, with
// the server merging namespaces and applying limit as a single global cap. (No
// namespace header is sent in "All projects" mode, so the server spans tenants.)
// search still fans out and merges client-side — see searchAll.

function statsAll(): Promise<Stats> {
  return req<Stats>('GET', '/v1/stats?all_namespaces=true')
}

function listAll(p: ListParams): Promise<Memory[]> {
  const q = new URLSearchParams()
  appendFilters(q, p)
  q.set('all_namespaces', 'true')
  return req<ListResponse>('GET', '/v1/memories?' + q.toString()).then((r) => r.memories ?? [])
}

// NOTE: searchAll still fans out one request per namespace and merges in the
// browser. Fine for interactive, limit-bounded recall; if namespace counts make
// this slow, add an all_namespaces aggregate to /v1/search too.
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

  // The namespace is sent in the X-Memini-Namespace header (the `ns` arg), not
  // the URL path, so hierarchical names like "work/memini" need no %2F encoding
  // — which some proxies reject/normalize before the request reaches memini.

  deleteNamespace: (name: string) =>
    req<{ deleted: number }>('DELETE', '/v1/namespaces', undefined, name),

  // moveNamespace relocates every memory in `name` to `to`. dryRun previews the
  // count without moving. The server normalizes `to` and rejects "*" or a
  // target equal to the source with 400.
  moveNamespace: (name: string, to: string, dryRun = false) =>
    req<RenamespaceReport>('POST', '/v1/namespaces/move', { to, dry_run: dryRun }, name),

  // splitNamespace regroups `name` by metadata keys (default keys when `by` is
  // empty), moving each record to the namespace its first matching key names.
  // dryRun previews the grouping without moving.
  splitNamespace: (name: string, by: string[], dryRun = false) =>
    req<RenamespaceReport>(
      'POST',
      '/v1/namespaces/split',
      { by: by.length ? by : undefined, dry_run: dryRun },
      name,
    ),

  // reassignMemory moves a single memory to `to`. It must scope to the memory's
  // own namespace (like remove) so "All projects" mode targets the right record
  // rather than the server default; the source namespace is the request header.
  reassignMemory: (id: string, to: string, ns?: string) =>
    req<{ moved: number }>(
      'POST',
      `/v1/memories/${encodeURIComponent(id)}/reassign`,
      { to },
      ns,
    ),

  get: (id: string, ns?: string) => req<Memory>('GET', `/v1/memories/${encodeURIComponent(id)}`, undefined, ns),

  // remove must scope to the memory's own namespace: in "All projects" mode the
  // active namespace is empty, so without this the server would fall back to
  // its default namespace and delete the wrong record (or 404).
  remove: (id: string, ns?: string) =>
    req<void>('DELETE', `/v1/memories/${encodeURIComponent(id)}`, undefined, ns),

  fsck: () => req<FsckReport>('POST', '/v1/fsck'),

  // dedup collapses near-duplicate memories. It scopes to the active namespace
  // via the request header; in "All projects" mode there's no active namespace,
  // so it runs store-wide. dryRun previews the clusters without tombstoning.
  dedup: (opts: { similarity?: number; dryRun?: boolean } = {}) => {
    const body: DedupRequest = {}
    if (opts.similarity != null) body.similarity = opts.similarity
    if (opts.dryRun != null) body.dry_run = opts.dryRun
    if (isAllProjects()) body.all_namespaces = true
    return req<DedupReport>('POST', '/v1/dedup', body)
  },
}
