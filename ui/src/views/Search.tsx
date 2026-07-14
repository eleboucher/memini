import { useEffect, useState } from 'preact/hooks'
import { api, isAllNamespaces } from '../api'
import { namespace } from '../store'
import type { Memory, Scored, Tier } from '../types'
import { MemoryCard } from '../components/MemoryCard'
import { MemoryDrawer } from '../components/MemoryDrawer'
import { TierFilter } from '../components/TierFilter'
import { MetaFilter } from '../components/MetaFilter'
import { Empty, ErrorBanner } from '../components/States'
import { IconSearch } from '../icons'

export function Search() {
  const [q, setQ] = useState('')
  const [tiers, setTiers] = useState<Tier[]>([])
  const [tags, setTags] = useState<string[]>([])
  const [metadata, setMetadata] = useState<Record<string, string>>({})
  const [results, setResults] = useState<Scored[] | null>(null)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [open, setOpen] = useState<Memory | null>(null)
  const [openFrom, setOpenFrom] = useState<string | undefined>(undefined)
  // The namespace the current `results` were fetched under, captured at fetch
  // time. `from` provenance labels disambiguate against THIS (fromLabel), not
  // the live namespace signal — the signal can move while results are still
  // on screen, which would silently relabel a stale ancestor hit "personal".
  const [queriedNs, setQueriedNs] = useState('')
  const [took, setTook] = useState(0)

  const showNs = isAllNamespaces()

  // A namespace switch invalidates the on-screen results outright (they were
  // answered under a different scope) — clear them rather than letting a
  // stale list linger under the new selection.
  useEffect(() => {
    setResults(null)
    setError(null)
    setOpen(null)
    setOpenFrom(undefined)
  }, [namespace.value])

  const run = async (e?: Event) => {
    e?.preventDefault()
    if (!q.trim() || loading) return // guard double-submit (Enter) racing
    setLoading(true)
    setError(null)
    // Snapshot the active namespace before the await: it's what this query
    // actually runs under, and what its results' provenance is relative to.
    const ns = namespace.value
    const t0 = performance.now()
    try {
      const r = await api.search(q.trim(), { tiers, tags, metadata, limit: 30 })
      setResults(r)
      setQueriedNs(ns)
      setTook(performance.now() - t0)
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err))
      setResults(null)
    } finally {
      setLoading(false)
    }
  }

  return (
    <>
      <div class="view">
        <form class="toolbar" onSubmit={run}>
          <div class="grow" style={{ position: 'relative' }}>
            <span
              aria-hidden="true"
              style={{
                position: 'absolute',
                left: '12px',
                top: '50%',
                transform: 'translateY(-50%)',
                color: 'var(--muted)',
                width: '16px',
                height: '16px',
              }}
            >
              <IconSearch />
            </span>
            <input
              class="input"
              style={{ paddingLeft: '38px' }}
              placeholder="Search memories"
              aria-label="Search memories"
              value={q}
              autofocus
              onInput={(e) => setQ((e.target as HTMLInputElement).value)}
            />
          </div>
          <TierFilter selected={tiers} onChange={setTiers} />
          <MetaFilter
            onChange={(t, m) => {
              setTags(t)
              setMetadata(m)
            }}
          />
          <button class="btn primary" type="submit" disabled={loading || !q.trim()}>
            {loading ? <span class="spinner" style={{ width: '14px', height: '14px' }} /> : <IconSearch />}
            Recall
          </button>
        </form>

        {error && <ErrorBanner message={error} />}

        {results !== null && (
          <>
            <div class="section-h">
              <h2>{results.length} results</h2>
              <span class="hint mono">{took.toFixed(0)}ms</span>
            </div>
            {results.length === 0 ? (
              <Empty title="No matches" />
            ) : (
              <div class="mem-list">
                {results.map((r) => (
                  <MemoryCard
                    key={r.memory.id}
                    memory={r.memory}
                    score={r.score}
                    from={r.from}
                    fromNs={queriedNs}
                    onOpen={(m) => {
                      setOpen(m)
                      setOpenFrom(r.from)
                    }}
                    showNamespace={showNs}
                  />
                ))}
              </div>
            )}
          </>
        )}

        {results === null && !error && <div class="empty"><div class="big">Search memories</div></div>}
      </div>

      {open && <MemoryDrawer memory={open} from={openFrom} fromNs={queriedNs} onClose={() => setOpen(null)} />}
    </>
  )
}
