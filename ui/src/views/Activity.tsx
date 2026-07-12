import { useCallback, useEffect, useState } from 'preact/hooks'
import { api, ApiError, isAllProjects } from '../api'
import { namespace, refreshNonce } from '../store'
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
    case 'recall':
      return n === 0 ? 'found nothing' : `served ${n} ${n === 1 ? 'memory' : 'memories'}`
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
    default:
      return ev.kind
  }
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
    [kindsKey, tiersKey, text, hours, nsKey],
  )

  // Reset to the first page whenever the scope or any filter changes. The text
  // box is debounced so a fetch doesn't fire on every keystroke.
  useEffect(() => {
    const t = setTimeout(() => void load(), text ? 250 : 0)
    return () => clearTimeout(t)
  }, [namespace.value, kindsKey, tiersKey, text, hours, nsKey, refreshNonce.value])

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

  const showNs = isAllProjects()

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
                        class="act-mem"
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
