import { useState } from 'preact/hooks'
import { api, isAllProjects } from '../api'
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
  const [took, setTook] = useState(0)

  const showNs = isAllProjects()

  const run = async (e?: Event) => {
    e?.preventDefault()
    if (!q.trim() || loading) return // guard double-submit (Enter) racing
    setLoading(true)
    setError(null)
    const t0 = performance.now()
    try {
      const r = await api.search(q.trim(), { tiers, tags, metadata, limit: 30 })
      setResults(r)
      setTook(performance.now() - t0)
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err))
      setResults(null)
    } finally {
      setLoading(false)
    }
  }

  return (
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
                  onOpen={setOpen}
                  showNamespace={showNs}
                />
              ))}
            </div>
          )}
        </>
      )}

      {results === null && !error && <div class="empty"><div class="big">Search memories</div></div>}

      {open && <MemoryDrawer memory={open} onClose={() => setOpen(null)} />}
    </div>
  )
}
