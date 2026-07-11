import { useState } from 'preact/hooks'
import { api } from '../api'
import { namespace, refreshNonce } from '../store'
import { useAsync } from '../hooks'
import type { NamespaceLink, ReadSetEntryItem, ReadSetOrigin, Tier } from '../types'
import { Loading, ErrorBanner, Empty } from '../components/States'
import { IconTrash } from '../icons'

// Only durable tiers ever cross a namespace boundary (episodic/working never
// do, in either direction) — the link form only offers those two.
const LINK_TIERS: Tier[] = ['semantic', 'procedural']

const ORIGIN_COLOR: Record<ReadSetOrigin, string> = {
  primary: 'var(--ember)',
  ancestor: 'var(--tier-working)',
  home: 'var(--tier-episodic)',
  link: 'var(--tier-semantic)',
  call: 'var(--tier-procedural)',
}

const ORIGIN_DESC: Record<ReadSetOrigin, string> = {
  primary: 'The active namespace itself.',
  ancestor: 'A path-prefix cascade leg (a parent namespace).',
  home: "The caller's personal namespace (X-Memini-Home).",
  link: 'A stored namespace link — durable tiers only.',
  call: 'An explicit per-call namespace.',
}

// Scopes surfaces the two pieces of the read-set model that aren't visible
// anywhere else in the UI: what a recall on the active namespace actually
// draws from (and why), and the namespace links that make up part of that.
export function Scopes() {
  return (
    <div class="view grid-2 stagger">
      <ReadSetPanel />
      <LinksPanel />
    </div>
  )
}

function ReadSetPanel() {
  const { data, error, loading } = useAsync(() => api.readSet(), [namespace.value, refreshNonce.value])
  const entries = data?.entries ?? []

  return (
    <div class="panel panel-pad">
      <div class="section-h">
        <h2>Effective read-set</h2>
        <span class="hint">
          namespaces a recall on "{namespace.value || '(default)'}" draws from, and why
        </span>
      </div>
      {loading && !data && <Loading />}
      {error && <ErrorBanner message={error} />}
      {data && entries.length === 0 && <Empty title="No entries" />}
      {entries.length > 0 && (
        <div class="mem-list">
          {entries.map((e) => (
            <ReadSetRow key={`${e.namespace}-${e.origin}`} entry={e} />
          ))}
        </div>
      )}
    </div>
  )
}

function ReadSetRow({ entry }: { entry: ReadSetEntryItem }) {
  const color = ORIGIN_COLOR[entry.origin]
  return (
    <div class="panel panel-pad" style={{ display: 'flex', alignItems: 'center', gap: '10px' }}>
      <span class="val mono" style={{ flex: 1, overflowWrap: 'anywhere' }}>
        {entry.namespace}
      </span>
      <span class="chip" title={ORIGIN_DESC[entry.origin]} style={{ borderColor: color, color }}>
        {entry.origin}
      </span>
      <span class="hint mono">{entry.tiers?.length ? entry.tiers.join(', ') : 'all tiers'}</span>
    </div>
  )
}

// LinksPanel manages outgoing namespace links from the active namespace: a
// list with per-row delete, and an add form (dst + tier restriction + note).
// Mutations refetch the list; errors from either the load or a mutation
// render through the same ErrorBanner.
function LinksPanel() {
  const [nonce, setNonce] = useState(0)
  const [mutErr, setMutErr] = useState<string | null>(null)
  const { data, error, loading } = useAsync(() => api.links(), [namespace.value, refreshNonce.value, nonce])
  const links = data ?? []

  const reload = () => {
    setMutErr(null)
    setNonce((n) => n + 1)
  }

  const del = async (dst: string) => {
    setMutErr(null)
    try {
      await api.deleteLink(dst)
      reload()
    } catch (e) {
      setMutErr(e instanceof Error ? e.message : String(e))
    }
  }

  return (
    <div class="panel panel-pad">
      <div class="section-h">
        <h2>Namespace links</h2>
        <span class="hint">
          durable-tier read edges from "{namespace.value || '(default)'}" to other namespaces
        </span>
      </div>
      {loading && !data && <Loading />}
      {error && <ErrorBanner message={error} />}
      {mutErr && <ErrorBanner message={mutErr} />}
      {data && links.length === 0 && <Empty title="No links" hint="Add one below." />}
      {links.length > 0 && (
        <div class="mem-list" style={{ marginBottom: '16px' }}>
          {links.map((l) => (
            <LinkRow key={l.dst} link={l} onDelete={() => del(l.dst)} />
          ))}
        </div>
      )}
      <AddLinkForm onAdded={reload} onError={setMutErr} />
    </div>
  )
}

function LinkRow({ link, onDelete }: { link: NamespaceLink; onDelete: () => void }) {
  return (
    <div class="panel panel-pad" style={{ display: 'flex', alignItems: 'center', gap: '10px' }}>
      <span class="val mono" style={{ flex: 1, overflowWrap: 'anywhere' }}>
        {link.dst}
      </span>
      <span class="hint mono">{link.tiers?.length ? link.tiers.join(', ') : 'semantic, procedural'}</span>
      {link.note && <span class="hint">{link.note}</span>}
      <button class="icon-btn" aria-label={`Delete link to ${link.dst}`} onClick={onDelete}>
        <IconTrash />
      </button>
    </div>
  )
}

function AddLinkForm({ onAdded, onError }: { onAdded: () => void; onError: (e: string | null) => void }) {
  const [dst, setDst] = useState('')
  const [tiers, setTiers] = useState<Tier[]>([])
  const [note, setNote] = useState('')
  const [busy, setBusy] = useState(false)

  const toggleTier = (t: Tier) => {
    setTiers((prev) => (prev.includes(t) ? prev.filter((x) => x !== t) : [...prev, t]))
  }

  const submit = async (e: Event) => {
    e.preventDefault()
    const target = dst.trim()
    if (!target || busy) return
    setBusy(true)
    onError(null)
    try {
      await api.addLink(target, tiers.length ? tiers : undefined, note.trim() || undefined)
      setDst('')
      setTiers([])
      setNote('')
      onAdded()
    } catch (err) {
      onError(err instanceof Error ? err.message : String(err))
    } finally {
      setBusy(false)
    }
  }

  return (
    <form onSubmit={submit} style={{ display: 'flex', gap: '8px', flexWrap: 'wrap', alignItems: 'center' }}>
      <input
        class="input"
        placeholder="target namespace, e.g. shared/golang"
        value={dst}
        onInput={(e) => setDst((e.target as HTMLInputElement).value)}
        style={{ flex: 1, minWidth: '160px' }}
      />
      <div style={{ display: 'flex', gap: '10px' }}>
        {LINK_TIERS.map((t) => (
          <label key={t} class="hint" style={{ display: 'flex', gap: '4px', alignItems: 'center', cursor: 'pointer' }}>
            <input type="checkbox" checked={tiers.includes(t)} onChange={() => toggleTier(t)} />
            {t}
          </label>
        ))}
      </div>
      <input
        class="input"
        placeholder="note (optional)"
        value={note}
        onInput={(e) => setNote((e.target as HTMLInputElement).value)}
        style={{ flex: 1, minWidth: '140px' }}
      />
      <button class="btn primary" type="submit" disabled={busy || !dst.trim()}>
        {busy && <span class="spinner" style={{ width: '14px', height: '14px' }} />}
        Add link
      </button>
    </form>
  )
}
