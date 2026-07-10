import { useState } from 'preact/hooks'
import { api } from '../api'
import { namespace, route, refreshNonce, refresh } from '../store'
import { useAsync } from '../hooks'
import { TIERS, type Stats } from '../types'
import { tierColor, relTime, num } from '../util'
import { Loading, ErrorBanner, Empty } from '../components/States'
import { NamespaceDrawer } from '../components/NamespaceDrawer'
import { IconTrash, IconSettings } from '../icons'

export function Projects() {
  const { data, error, loading } = useAsync(async () => {
    const names = await api.namespaces()
    const stats = await Promise.all(names.map((n) => api.statsFor(n).catch(() => null)))
    return names.map((name, i) => ({ name, stats: stats[i] }))
  }, [refreshNonce.value])

  const open = (ns: string) => {
    namespace.value = ns
    route.value = 'overview'
  }

  if (loading && !data) return <div class="view"><Loading /></div>
  if (error) return <div class="view"><ErrorBanner message={error} /></div>

  const projects = data ?? []

  return (
    <div class="view">
      {projects.length === 0 ? (
        <Empty title="No projects" hint="Namespaces appear here once they hold memories." />
      ) : (
        <div class="project-grid stagger">
          {projects.map((p) => (
            <ProjectCard key={p.name} name={p.name} stats={p.stats} onOpen={() => open(p.name)} />
          ))}
        </div>
      )}
    </div>
  )
}

function ProjectCard({ name, stats, onOpen }: { name: string; stats: Stats | null; onOpen: () => void }) {
  const [armed, setArmed] = useState(false)
  const [deleting, setDeleting] = useState(false)
  const [managing, setManaging] = useState(false)

  const manage = (e: Event) => {
    e.stopPropagation()
    setManaging(true)
  }

  const del = async (e: Event) => {
    e.stopPropagation()
    if (!armed) {
      setArmed(true)
      setTimeout(() => setArmed(false), 3000)
      return
    }
    setDeleting(true)
    try {
      await api.deleteNamespace(name)
      if (namespace.value === name) namespace.value = ''
      refresh()
    } catch {
      setDeleting(false)
      setArmed(false)
    }
  }

  return (
    <div class="panel project-card" role="button" tabIndex={0} onClick={onOpen} onKeyDown={(e) => { if (e.key === 'Enter') onOpen() }}>
      <div class="project-head">
        <div class="project-name">{name}</div>
        <button
          class="icon-btn"
          aria-label={`Manage ${name}`}
          title="Move or split this namespace"
          onClick={manage}
        >
          <IconSettings />
        </button>
        <button
          class={`icon-btn project-del-btn ${armed ? 'danger-on' : ''}`}
          aria-label={armed ? 'Confirm delete' : `Delete ${name}`}
          onClick={del}
          disabled={deleting}
        >
          <IconTrash />
        </button>
      </div>
      {managing && (
        // Preact portals bubble events through the vdom tree, so stop clicks and
        // keys from the drawer reaching the card's open/keydown handlers.
        <div onClick={(e) => e.stopPropagation()} onKeyDown={(e) => e.stopPropagation()}>
          <NamespaceDrawer name={name} onClose={() => setManaging(false)} />
        </div>
      )}
      {armed && (
        <div class="banner err" role="status">
          Click trash again to delete all memories in "{name}".
        </div>
      )}
      <div class="project-count">
        <span class="v">{num(stats?.total ?? 0)}</span> memories
      </div>
      <TierBar stats={stats} />
      <div class="project-foot">
        {stats?.last_write_at ? `updated ${relTime(stats.last_write_at)}` : '—'}
      </div>
    </div>
  )
}

function TierBar({ stats }: { stats: Stats | null }) {
  const total = stats?.total ?? 0
  if (total === 0) return <div class="project-bar empty-bar" />
  return (
    <div class="project-bar">
      {TIERS.map((t) => {
        const n = stats?.by_tier[t] ?? 0
        if (n === 0) return null
        return <span key={t} style={{ flexGrow: n, background: tierColor(t) }} title={`${t}: ${n}`} />
      })}
    </div>
  )
}
