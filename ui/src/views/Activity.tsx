import { useCallback, useEffect, useState } from 'preact/hooks'
import { api, ApiError, isAllNamespaces } from '../api'
import { identity, namespace, refreshNonce } from '../store'
import { EVENT_KINDS, type ActivityEvent, type EventKind, type Memory, type Tier } from '../types'
import { MemoryDrawer } from '../components/MemoryDrawer'
import { TierBadge } from '../components/TierBadge'
import { TierFilter } from '../components/TierFilter'
import { NamespaceFilter } from '../components/NamespaceFilter'
import { Loading, ErrorBanner, Empty } from '../components/States'
import { relTime, fmtDate } from '../util'

const KINDS_KEY = 'memini.activity.kinds'
const PAGE = 40

// Hidden kinds persist: a briefing fires on every session start, so the feed is
// mostly briefings unless you say otherwise — and having to say so on every
// visit would be its own annoyance.
function loadHiddenKinds(): EventKind[] {
  try {
    const raw = localStorage.getItem(KINDS_KEY)
    if (!raw) return []
    const parsed = JSON.parse(raw) as unknown
    if (!Array.isArray(parsed)) return []
    return parsed.filter((k): k is EventKind => EVENT_KINDS.includes(k as EventKind))
  } catch {
    return []
  }
}

// headline renders the one-line "what happened" for an event.
function headline(ev: ActivityEvent): string {
  const n = ev.memories?.length ?? 0
  switch (ev.kind) {
    case 'recall': {
      // "Served" counts only what the recall returned — floored hits are logged
      // for visibility (rendered dimmed below) but were never served.
      const servedMems = (ev.memories ?? []).filter((m) => !m.filtered)
      const served = servedMems.length
      if (served === 0) {
        // With floored rows dimmed below, "found nothing" would contradict
        // them; surface the floored count instead of an empty verdict.
        const floored = ev.memories?.filter((m) => m.filtered).length ?? 0
        return floored > 0 ? `served 0 · ${floored} floored` : 'found nothing'
      }
      // When an injection-telemetry report covered this serve, say what
      // actually reached model context; a serve nothing reported on keeps the
      // old wording — absent means unknown, not zero. Floored rows are outside
      // the join: they were never served, so a report cannot cover them.
      const known = servedMems.filter((m) => typeof m.injected === 'boolean')
      if (known.length > 0) {
        const inj = known.filter((m) => m.injected).length
        return `served ${served} → injected ${inj}`
      }
      return `served ${served} ${served === 1 ? 'memory' : 'memories'}`
    }
    case 'inject':
      return n === 0 ? 'reported suppressed injections' : `injected ${n} ${n === 1 ? 'memory' : 'memories'}`
    case 'briefing':
      return `session briefing · ${n} ${n === 1 ? 'memory' : 'memories'}`
    case 'get':
      return 'opened'
    case 'remember':
      return 'remembered'
    case 'update':
      return 'updated'
    case 'forget':
      return 'forgot'
    case 'supersede':
      return 'superseded'
    case 'pin':
      return 'pinned a project namespace'
    case 'unpin':
      return 'removed a project pin'
    case 'settings':
      return 'changed settings'
    default:
      return ev.kind
  }
}

// actorLabel renders who performed an event. A named key shows its name; the
// admin env key and dev-mode requests have no name, so they render by kind; a
// legacy row (no kind at all) renders nothing — attribution predates it.
function actorLabel(ev: ActivityEvent): string | null {
  if (ev.actor) return ev.actor
  switch (ev.actor_kind) {
    case 'env':
      return 'admin key'
    case 'none':
      return 'open access'
    default:
      return null
  }
}

// detailChips renders the kind-specific "why"/"what" of an event as small chips:
// a recall's source, a write's outcome (tier + auto-supersede/reinforce/merge
// hint), and a config event's target (pinned keys/note, changed settings layer).
function detailChips(ev: ActivityEvent): { label: string; title?: string; warn?: boolean }[] {
  const d = (ev.detail ?? {}) as Record<string, unknown>
  const chips: { label: string; title?: string; warn?: boolean }[] = []
  const str = (v: unknown): string | null => (typeof v === 'string' && v ? v : null)

  if (ev.kind === 'recall') {
    // The source is the recall's "why". "api" is the unremarkable default, so
    // it is left off — a chip on every direct search would be noise.
    const source = str(d.source)
    if (source && source !== 'api') chips.push({ label: source, title: 'What triggered this recall' })
    // How many candidates a cooldown pass held back (exclude_ids): surfaces that
    // already injected a memory this session ask the server to drop it.
    const excluded = typeof d.excluded_count === 'number' ? d.excluded_count : 0
    if (excluded > 0) {
      chips.push({
        label: `${excluded} excluded · already injected`,
        title: 'Candidates dropped because a surface already injected them this session',
      })
    }
  }

  if (ev.kind === 'inject') {
    // The beacon's "what": which hook surface reported, what its local gates
    // held back, and the client's own token estimate.
    const surface = str(d.surface)
    if (surface) chips.push({ label: surface, title: 'Hook surface that reported this injection' })
    const sup = (d.suppressed ?? {}) as Record<string, unknown>
    for (const [reason, count] of Object.entries(sup)) {
      if (typeof count === 'number' && count > 0) {
        chips.push({ label: `−${count} ${reason}`, title: `Held back client-side: ${reason}` })
      }
    }
    if (typeof d.injected_tokens_est === 'number' && d.injected_tokens_est > 0) {
      chips.push({ label: `~${d.injected_tokens_est} tok`, title: 'Client-estimated tokens injected into context' })
    }
  }

  if (ev.kind === 'remember' || ev.kind === 'update') {
    const tier = str(d.tier)
    if (tier) chips.push({ label: tier, title: 'Tier this write landed in' })
    if (str(d.auto_superseded)) {
      chips.push({ label: 'auto-superseded', title: `Replaced ${str(d.auto_superseded)}`, warn: true })
    }
    if (d.reinforced === true) chips.push({ label: 'reinforced', title: 'Folded into an existing memory' })
    if (d.merge_hint === true) chips.push({ label: 'merge hint', title: 'A near-duplicate was flagged' })
  }

  if (ev.kind === 'pin' || ev.kind === 'unpin') {
    const keys = Array.isArray(d.keys) ? (d.keys as unknown[]).filter((k): k is string => typeof k === 'string') : []
    for (const k of keys) chips.push({ label: k, title: 'Pin key' })
    const note = str(d.note)
    if (note) chips.push({ label: note, title: 'Pin note' })
  }

  if (ev.kind === 'settings') {
    const target = str(d.key_name)
    if (target) chips.push({ label: target, title: 'Key whose settings changed' })
    const layer = str(d.layer)
    if (layer) chips.push({ label: layer, title: 'Settings layer' })
    if (typeof d.admin === 'boolean') {
      chips.push({ label: d.admin ? 'admin granted' : 'admin revoked', title: 'Admin capability change', warn: true })
    }
  }

  return chips
}

const WINDOWS: { label: string; hours: number }[] = [
  { label: 'Any time', hours: 0 },
  { label: 'Last hour', hours: 1 },
  { label: 'Last 24h', hours: 24 },
  { label: 'Last 7d', hours: 24 * 7 },
  { label: 'Last 30d', hours: 24 * 30 },
]

export function Activity() {
  const [hidden, setHidden] = useState<EventKind[]>(loadHiddenKinds)
  const [tiers, setTiers] = useState<Tier[]>([])
  const [text, setText] = useState('')
  const [actor, setActor] = useState('')
  const [keyNames, setKeyNames] = useState<string[]>([])
  const [hours, setHours] = useState(0)
  const [namespaces, setNamespaces] = useState<string[]>([])
  const [events, setEvents] = useState<ActivityEvent[]>([])
  const [cursor, setCursor] = useState<string | undefined>()
  const [hasMore, setHasMore] = useState(false)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [open, setOpen] = useState<Memory | null>(null)

  const kinds = EVENT_KINDS.filter((k) => !hidden.includes(k))
  const kindsKey = kinds.join(',')
  const tiersKey = tiers.join(',')
  const nsKey = namespaces.join(',')

  // Every filter goes to the server rather than narrowing the response, so a
  // page of N events really is N events and "load more" stays meaningful.
  // `since` is recomputed per fetch: pinning the instant into state would make
  // "last hour" mean the hour before you picked it, drifting as you sit there.
  const load = useCallback(
    async (before?: string) => {
      setLoading(true)
      setError(null)
      try {
        const res = await api.activity({
          kinds,
          tiers,
          text: text.trim() || undefined,
          actor: actor.trim() || undefined,
          since: hours > 0 ? new Date(Date.now() - hours * 3600_000).toISOString() : undefined,
          namespaces,
          limit: PAGE,
          before,
        })
        const page = res.events ?? []
        setEvents((prev) => (before ? [...prev, ...page] : page))
        setCursor(res.next_cursor)
        setHasMore(Boolean(res.has_more))
      } catch (e) {
        setError(e instanceof ApiError ? e.message : String(e))
        if (!before) setEvents([])
      } finally {
        setLoading(false)
      }
    },
    [kindsKey, tiersKey, text, actor, hours, nsKey],
  )

  // Reset to the first page whenever the scope or any filter changes. The free-
  // text boxes (query, actor) are debounced so a fetch doesn't fire on every
  // keystroke.
  useEffect(() => {
    const t = setTimeout(() => void load(), text || actor ? 250 : 0)
    return () => clearTimeout(t)
  }, [namespace.value, kindsKey, tiersKey, text, actor, hours, nsKey, refreshNonce.value])

  // Admins get a datalist of key names for the actor filter, so it autocompletes
  // instead of forcing exact recall of a key's name.
  useEffect(() => {
    if (!identity.value?.admin) {
      setKeyNames([])
      return
    }
    let live = true
    api
      .listKeys()
      .then((keys) => {
        if (live) setKeyNames(keys.map((k) => k.name))
      })
      .catch(() => {
        // The datalist is a convenience; a plain input still works without it.
      })
    return () => {
      live = false
    }
  }, [identity.value?.admin])

  const toggleKind = (k: EventKind) => {
    const next = hidden.includes(k) ? hidden.filter((x) => x !== k) : [...hidden, k]
    setHidden(next)
    try {
      localStorage.setItem(KINDS_KEY, JSON.stringify(next))
    } catch {
      // A private-mode browser with no storage still filters, just not durably.
    }
  }

  const openMemory = async (id: string, ns: string) => {
    try {
      setOpen(await api.get(id, ns))
    } catch {
      // The memory is gone (a forget event, most likely). The row still renders
      // from its snapshot; there is simply nothing to open.
    }
  }

  const showNs = isAllNamespaces()

  return (
    <>
      <div class="view activity">
        <div class="toolbar">
          <div class="tier-filter" role="group" aria-label="Filter by event kind">
            {EVENT_KINDS.map((k) => {
              const on = !hidden.includes(k)
              return (
                <button
                  key={k}
                  type="button"
                  class={`tier-toggle ${on ? 'on' : ''}`}
                  aria-pressed={on}
                  aria-label={`${k} events${on ? '' : ' (hidden)'}`}
                  onClick={() => toggleKind(k)}
                >
                  <span class={`chip evkind ${k}`}>{k}</span>
                </button>
              )
            })}
          </div>
        </div>
        <div class="toolbar">
          {/* Tier here means "events that touched a memory of this tier" — the
              whole event comes back, so its counts stay honest. */}
          <TierFilter selected={tiers} onChange={setTiers} />
          <input
            class="chip act-search"
            type="search"
            placeholder="Filter by query or memory…"
            aria-label="Filter activity by text"
            value={text}
            onInput={(e) => setText((e.target as HTMLInputElement).value)}
          />
          <input
            class="chip act-actor"
            type="search"
            list={keyNames.length ? 'activity-actors' : undefined}
            placeholder="Filter by actor (key name)…"
            aria-label="Filter activity by actor"
            value={actor}
            onInput={(e) => setActor((e.target as HTMLInputElement).value)}
          />
          {keyNames.length > 0 && (
            <datalist id="activity-actors">
              {keyNames.map((n) => (
                <option key={n} value={n} />
              ))}
            </datalist>
          )}
          <select
            class="chip"
            aria-label="Time window"
            value={String(hours)}
            onChange={(e) => setHours(Number((e.target as HTMLSelectElement).value))}
          >
            {WINDOWS.map((w) => (
              <option key={w.hours} value={String(w.hours)}>
                {w.label}
              </option>
            ))}
          </select>
          {showNs && <NamespaceFilter selected={namespaces} onChange={setNamespaces} />}
          <span class="grow" />
          <span class="chip mono">{events.length} shown</span>
        </div>

        {error && <ErrorBanner message={error} />}
        {loading && events.length === 0 ? (
          <Loading />
        ) : events.length === 0 ? (
          <Empty title="No activity yet" />
        ) : (
          <div class="panel act-list">
            {events.map((ev) => (
              <div key={ev.op_id} class="act-event">
                <div class="act-head">
                  <span class={`chip evkind ${ev.kind}`}>{ev.kind}</span>
                  {ev.query ? (
                    <span class="act-query">“{ev.query}”</span>
                  ) : (
                    <span class="act-what">{headline(ev)}</span>
                  )}
                  {ev.query && <span class="muted">{headline(ev)}</span>}
                  {typeof ev.detail?.degraded === 'string' && (
                    <span class="chip warn" title="Recall fell back to keyword-only search">
                      degraded: {ev.detail.degraded}
                    </span>
                  )}
                  {detailChips(ev).map((c, i) => (
                    <span key={i} class={`chip ${c.warn ? 'warn' : ''}`} title={c.title}>
                      {c.label}
                    </span>
                  ))}
                  {(() => {
                    const who = actorLabel(ev)
                    return who ? (
                      <span class="chip act-actor-chip" title="Who performed this operation">
                        {who}
                      </span>
                    ) : null
                  })()}
                  {showNs && <span class="chip mono">{ev.namespace}</span>}
                  <span class="grow" />
                  <span class="muted" title={fmtDate(ev.time)}>
                    {relTime(ev.time)}
                  </span>
                </div>

                {(ev.memories?.length ?? 0) > 0 && (
                  <div class="act-mems">
                    {ev.memories?.map((m) => (
                      <button
                        key={m.id}
                        type="button"
                        class={`act-mem${m.filtered ? ' floored' : ''}`}
                        onClick={() => openMemory(m.id, m.namespace)}
                      >
                        {m.rank ? <span class="act-rank mono">#{m.rank}</span> : <span class="act-rank" />}
                        <TierBadge tier={m.tier} />
                        <span class="act-summary">{m.summary}</span>
                        {m.section && <span class="chip">{m.section}</span>}
                        {/* The score is the "why": how well this memory matched
                            the query it was served for. */}
                        {typeof m.score === 'number' && (
                          <span class="chip mono" title="Relevance score for this query">
                            {m.score.toFixed(2)}
                          </span>
                        )}
                        {/* Served but never injected: the client's gates held
                            it back. Only reported suppression renders — an
                            absent flag means no telemetry covered this serve. */}
                        {m.injected === false && (
                          <span class="chip warn" title="Served by the server but suppressed client-side before reaching model context">
                            not injected
                          </span>
                        )}
                        {/* Floored: below the request's min_rank_score, so logged
                            for visibility but never returned. */}
                        {m.filtered === 'rank_floor' && (
                          <span
                            class="chip floored-badge"
                            title="Below this recall's min_rank_score floor — logged but not served"
                          >
                            floored
                          </span>
                        )}
                        {showNs && m.namespace !== ev.namespace && (
                          <span class="chip mono">{m.namespace}</span>
                        )}
                      </button>
                    ))}
                  </div>
                )}
              </div>
            ))}
          </div>
        )}

        {hasMore && (
          <div class="act-more">
            <button
              type="button"
              class="chip"
              disabled={loading}
              onClick={() => void load(cursor)}
            >
              {loading ? 'Loading…' : 'Load more'}
            </button>
          </div>
        )}
      </div>

      {open && <MemoryDrawer memory={open} onClose={() => setOpen(null)} />}
    </>
  )
}
