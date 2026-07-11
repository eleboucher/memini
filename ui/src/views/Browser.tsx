import { useState } from 'preact/hooks'
import { api, isAllProjects } from '../api'
import { namespace, refreshNonce } from '../store'
import { useAsync } from '../hooks'
import type { Level, Memory, SortKey, SortOrder, Tier } from '../types'
import { MemoryCard } from '../components/MemoryCard'
import { MemoryDrawer } from '../components/MemoryDrawer'
import { TierFilter } from '../components/TierFilter'
import { MetaFilter } from '../components/MetaFilter'
import { MemoryTypeFilter } from '../components/MemoryTypeFilter'
import { LevelFilter } from '../components/LevelFilter'
import { RecencyFilter } from '../components/RecencyFilter'
import { NamespaceFilter } from '../components/NamespaceFilter'
import { SortControl } from '../components/SortControl'
import { Loading, ErrorBanner, Empty } from '../components/States'

export function Browser() {
  const [tiers, setTiers] = useState<Tier[]>([])
  const [levels, setLevels] = useState<Level[]>([])
  const [memoryTypes, setMemoryTypes] = useState<string[]>([])
  const [tags, setTags] = useState<string[]>([])
  const [metadata, setMetadata] = useState<Record<string, string>>({})
  const [createdAfter, setCreatedAfter] = useState<string | undefined>()
  const [accessedAfter, setAccessedAfter] = useState<string | undefined>()
  const [namespaces, setNamespaces] = useState<string[]>([])
  const [sort, setSort] = useState<SortKey>('created_at')
  const [order, setOrder] = useState<SortOrder>('desc')
  const [includeExpired, setExpired] = useState(false)
  const [includeSuperseded, setSuperseded] = useState(false)
  const [open, setOpen] = useState<Memory | null>(null)

  const { data, error, loading } = useAsync(
    () =>
      api.list({
        tiers,
        levels,
        memoryTypes,
        tags,
        metadata,
        createdAfter,
        accessedAfter,
        namespaces,
        sort,
        order,
        includeExpired,
        includeSuperseded,
        limit: 500,
      }),
    [
      namespace.value,
      tiers.join(','),
      levels.join(','),
      memoryTypes.join(','),
      tags.join(','),
      JSON.stringify(metadata),
      createdAfter,
      accessedAfter,
      namespaces.join(','),
      sort,
      order,
      includeExpired,
      includeSuperseded,
      refreshNonce.value,
    ],
  )
  const memories = data ?? []
  const showNs = isAllProjects()
  // Cap the column for a lone result so it doesn't stretch full-width.
  const sparse = memories.length === 1

  return (
    <>
      <div class={`view browse ${sparse ? 'sparse' : ''}`}>
        <div class="toolbar">
          <TierFilter selected={tiers} onChange={setTiers} />
          <MemoryTypeFilter selected={memoryTypes} onChange={setMemoryTypes} />
          <LevelFilter selected={levels} onChange={setLevels} />
        </div>
        <div class="toolbar">
          <MetaFilter
            onChange={(t, m) => {
              setTags(t)
              setMetadata(m)
            }}
          />
          <RecencyFilter
            onChange={(c, a) => {
              setCreatedAfter(c)
              setAccessedAfter(a)
            }}
          />
          {showNs && <NamespaceFilter selected={namespaces} onChange={setNamespaces} />}
          <SortControl
            sort={sort}
            order={order}
            onChange={(s, o) => {
              setSort(s)
              setOrder(o)
            }}
          />
          <span class="grow" />
          <label class="chip" style={{ cursor: 'pointer', gap: '6px' }}>
            <input
              type="checkbox"
              checked={includeExpired}
              onChange={(e) => setExpired((e.target as HTMLInputElement).checked)}
            />
            expired
          </label>
          <label class="chip" style={{ cursor: 'pointer', gap: '6px' }}>
            <input
              type="checkbox"
              checked={includeSuperseded}
              onChange={(e) => setSuperseded((e.target as HTMLInputElement).checked)}
            />
            superseded
          </label>
          <span class="chip mono">{memories.length} shown</span>
        </div>

        {error && <ErrorBanner message={error} />}
        {loading && !data ? (
          <Loading />
        ) : memories.length === 0 ? (
          <Empty title="No memories" />
        ) : (
          <div class="mem-list">
            {memories.map((m) => (
              <MemoryCard key={m.id} memory={m} onOpen={setOpen} showNamespace={showNs} />
            ))}
          </div>
        )}
      </div>

      {open && <MemoryDrawer memory={open} onClose={() => setOpen(null)} />}
    </>
  )
}
