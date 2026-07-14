import { apiToken, baseUrl, identity, namespace, namespaceHeader, serverWarning, sessionEnded } from './store'
import type {
  ActivityResponse,
  ApiKeysResponse,
  ApiKeyWithSecret,
  ClientSettings,
  CreateApiKeyRequest,
  DedupReport,
  DedupRequest,
  EventKind,
  FsckReport,
  HandshakeRequest,
  HandshakeResponse,
  Level,
  ListResponse,
  Memory,
  ProjectMapDeleteRequest,
  ProjectMapEntry,
  ProjectMapListResponse,
  ProjectMapPutRequest,
  SelfResponse,
  SettingsDefaultsResponse,
  SortKey,
  SortOrder,
  NamespaceLink,
  NamespaceLinksResponse,
  NamespacesResponse,
  ReadSetResponse,
  RenamespaceReport,
  Scored,
  SearchResponse,
  Stats,
  Tier,
  UpdateApiKeyBody,
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

  // The server announces non-fatal overrides here — most notably an
  // X-Memini-Home header it ignored because the API key is bound to a home
  // namespace of its own. Every request passes through this function, so
  // capturing it once surfaces it no matter which view made the call.
  serverWarning.value = res.headers.get('X-Memini-Warning') ?? ''

  if (res.status === 204) return undefined as T
  const text = await res.text()
  const data = text ? safeParse(text) : undefined
  if (!res.ok) {
    const msg = (data as { error?: string } | undefined)?.error ?? res.statusText
    // A 401 means the credential this session runs on stopped authenticating —
    // rotated or revoked out from under us. Clearing identity bounces the app
    // back to the Login gate (see app.tsx's AuthGate) instead of leaving every
    // view stuck on an error banner. Do this before throwing so the throw's
    // own handler still sees the message. Flag sessionEnded only when a live
    // session is actually being torn down (identity was non-null) so the Login
    // gate can say "your session ended" rather than showing a bare form.
    if (res.status === 401) {
      if (identity.value !== null) sessionEnded.value = true
      identity.value = null
    }
    throw new ApiError(res.status, msg)
  }
  return data as T
}

// verifyToken probes GET /v1/self with a *candidate* token, deliberately
// bypassing the global apiToken/localStorage: a bad paste must never become the
// stored credential. The Login gate calls this, then adopts the token into
// apiToken only once this resolves. An empty token sends no Authorization
// header — the dev-mode probe, where the server answers 200 with
// identity.authenticated=false.
export async function verifyToken(token: string): Promise<SelfResponse> {
  const h: Record<string, string> = {}
  if (token) h['Authorization'] = `Bearer ${token}`
  let res: Response
  try {
    res = await fetch(baseUrl.value + '/v1/self', { method: 'GET', headers: h })
  } catch (e) {
    throw new ApiError(0, `network error: ${(e as Error).message}`)
  }
  const text = await res.text()
  const data = text ? safeParse(text) : undefined
  if (!res.ok) {
    const msg = (data as { error?: string } | undefined)?.error ?? res.statusText
    throw new ApiError(res.status, msg)
  }
  return data as SelfResponse
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
  levels?: Level[]
  tags?: string[]
  metadata?: Record<string, string>
  memoryTypes?: string[]
  createdAfter?: string
  accessedAfter?: string
  includeExpired?: boolean
  includeSuperseded?: boolean
  sort?: SortKey
  order?: SortOrder
  limit?: number
  // namespaces narrows an "All projects" listing; ignored when a namespace is
  // active (scopedList doesn't send it — the header already scopes the request).
  namespaces?: string[]
}

// appendFilters adds the shared filter/sort query params to a list request.
// Sorting is server-side: with limit capping the response, sorting in the
// browser would only ever reorder whichever rows the server happened to return.
function appendFilters(q: URLSearchParams, p: ListParams) {
  p.tiers?.forEach((t) => q.append('tier', t))
  p.levels?.forEach((l) => q.append('level', l))
  p.tags?.forEach((t) => q.append('tag', t))
  p.memoryTypes?.forEach((t) => q.append('memory_type', t))
  Object.entries(p.metadata ?? {}).forEach(([k, v]) => q.append('meta', `${k}=${v}`))
  if (p.createdAfter) q.set('created_after', p.createdAfter)
  if (p.accessedAfter) q.set('accessed_after', p.accessedAfter)
  if (p.includeExpired) q.set('include_expired', 'true')
  if (p.includeSuperseded) q.set('include_superseded', 'true')
  if (p.sort) q.set('sort', p.sort)
  if (p.order) q.set('order', p.order)
  if (p.limit) q.set('limit', String(p.limit))
}

export interface ActivityParams {
  kinds?: EventKind[]
  tiers?: Tier[]
  text?: string
  actor?: string
  since?: string
  namespaces?: string[]
  limit?: number
  before?: string
}

// activity fetches a page of the activity feed. Every filter is applied
// server-side so paging stays correct — dropping events client-side would make
// a page of N events arrive as fewer than N and misreport how much is left.
function fetchActivity(p: ActivityParams): Promise<ActivityResponse> {
  const q = new URLSearchParams()
  p.kinds?.forEach((k) => q.append('kind', k))
  p.tiers?.forEach((t) => q.append('tier', t))
  if (p.text) q.set('q', p.text)
  if (p.actor) q.set('actor', p.actor)
  if (p.since) q.set('since', p.since)
  if (p.limit) q.set('limit', String(p.limit))
  if (p.before) q.set('before', p.before)
  if (isAllProjects()) {
    q.set('all_namespaces', 'true')
    p.namespaces?.forEach((n) => q.append('namespace', n))
  }
  const qs = q.toString()
  return req<ActivityResponse>('GET', '/v1/activity' + (qs ? `?${qs}` : ''))
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
      // The recall's "why": this search came from the web UI. Recorded on the
      // activity event so the feed distinguishes a human's UI search from an
      // agent's pretool/mcp recall.
      source: 'ui',
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
  // Narrowing the aggregate happens server-side: the global limit is applied
  // under the sort, so filtering the response here could starve a selected
  // namespace whose rows fell outside the cap.
  p.namespaces?.forEach((n) => q.append('namespace', n))
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

  // activity is namespace-aware via the header, or aggregates in All-projects
  // mode; see fetchActivity.
  activity: (p: ActivityParams = {}) => fetchActivity(p),

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

  // readSet resolves the effective read-set (namespace/origin/tiers) for the
  // request namespace — header-scoped, like scopedStats. `ns` overrides the
  // active selection (used by the Graph namespace mode to probe every
  // namespace in turn, not just the active one).
  readSet: (ns?: string) => req<ReadSetResponse>('GET', '/v1/namespaces/readset', undefined, ns),

  // links lists outgoing namespace links (durable-tier read edges) from the
  // request namespace.
  links: (ns?: string) =>
    req<NamespaceLinksResponse>('GET', '/v1/links', undefined, ns).then((r) => r.links ?? []),

  // addLink creates or replaces (idempotent on dst) a link from the active
  // namespace to `dst`. Omitting tiers falls back to the durable default
  // (semantic, procedural) — non-durable tiers never cross a link regardless.
  addLink: (dst: string, tiers?: Tier[], note?: string, ns?: string) =>
    req<NamespaceLink>('POST', '/v1/links', { dst, tiers, note }, ns),

  // deleteLink removes the link to `dst`. The server accepts dst via query or
  // body; sending it in the URL keeps this a plain DELETE with no body.
  deleteLink: (dst: string, ns?: string) =>
    req<void>('DELETE', `/v1/links?dst=${encodeURIComponent(dst)}`, undefined, ns),

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

  // --- API key management (K3b) -----------------------------------------
  // /v1/keys is not namespace-scoped (global, like /v1/namespaces); ns is
  // left to default so the active-namespace header still rides along
  // harmlessly (the server ignores it here), matching listNamespaces below.

  listKeys: () => req<ApiKeysResponse>('GET', '/v1/keys').then((r) => r.keys ?? []),

  createKey: (body: CreateApiKeyRequest) => req<ApiKeyWithSecret>('POST', '/v1/keys', body),

  updateKey: (name: string, body: UpdateApiKeyBody) =>
    req<ApiKeysResponse['keys'][number]>('PATCH', `/v1/keys/${encodeURIComponent(name)}`, body),

  deleteKey: (name: string) => req<void>('DELETE', `/v1/keys/${encodeURIComponent(name)}`),

  rotateKey: (name: string) => req<ApiKeyWithSecret>('POST', `/v1/keys/${encodeURIComponent(name)}/rotate`),

  // --- Config view (Phase 8): identity/settings/pins/handshake ------------

  // self is a lighter-weight refresh than handshake for a client that
  // already has a resolved namespace: identity + fully-merged settings +
  // per-field provenance for the request's own credential, no project
  // facts to resend. The Settings tab's default (no key picked) view.
  self: () => req<SelfResponse>('GET', '/v1/self'),

  // handshake resolves namespace/identity/settings from client-supplied
  // project facts — the same call every real client makes on startup. The
  // Preview tab uses it as a "why did I get this namespace?" debugger.
  handshake: (body: HandshakeRequest) => req<HandshakeResponse>('POST', '/v1/handshake', body),

  // getSettingsDefaults reads the server's global default layer (fully
  // resolved, plus managed_by so the UI can render an env-managed layer
  // read-only). Admin-gated — a named key gets 403.
  getSettingsDefaults: () => req<SettingsDefaultsResponse>('GET', '/v1/settings/defaults'),

  // putSettingsDefaults replaces the global layer wholesale: fields present
  // become the new global override, fields omitted stop being overridden
  // (revert to the built-in default) — not a merge with whatever was
  // previously stored. Only explicitly-touched fields should be included.
  putSettingsDefaults: (body: Partial<ClientSettings>) =>
    req<ClientSettings>('PUT', '/v1/settings/defaults', body),

  listPins: () => req<ProjectMapListResponse>('GET', '/v1/pins').then((r) => r.entries ?? []),

  putPin: (body: ProjectMapPutRequest) => req<ProjectMapEntry>('PUT', '/v1/pins', body),

  // deletePin identifies the pin to remove by remote_url/toplevel_path in
  // the body (there's no synthetic ID) — parsePinKey (util.ts) recovers
  // those facts from a listed entry's combined `key` string.
  deletePin: (body: ProjectMapDeleteRequest) => req<void>('DELETE', '/v1/pins', body),
}
