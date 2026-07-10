import { useState } from 'preact/hooks'
import { api } from '../api'
import { namespace, route, refreshNonce, refresh } from '../store'
import { useAsync } from '../hooks'
import { TIERS, type Stats } from '../types'
import { tierColor, relTime, num } from '../util'
import { Loading, ErrorBanner, Empty } from '../components/States'
import { NamespaceDrawer } from '../components/NamespaceDrawer'
import { IconTrash, IconSettings } from '../icons'

// NO_TENANT groups flat (no-slash) namespaces, which have no tenant segment.
const NO_TENANT = '(no tenant)'

interface Project {
  name: string // full stored namespace, e.g. "work/memini/reviewer"
  stats: Stats | null
}

interface Tenant {
  tenant: string // display label for the k8s-style namespace box
  total: number // memories summed across the tenant's projects
  projects: { name: string; label: string; stats: Stats | null }[]
}

// groupByTenant buckets namespaces by their first path segment (the tenant),
// modeled as k8s namespaces; each namespace under it becomes a "pod" labeled by
// the remaining path. A flat (no-slash) namespace that equals an existing
// tenant (e.g. "work" alongside "work/memini") joins that tenant's box as a
// "(root)" pod rather than the NO_TENANT bucket; a flat namespace with no
// matching tenant falls under NO_TENANT. Tenants and pods are sorted
// alphabetically, with NO_TENANT last.
function groupByTenant(projects: Project[]): Tenant[] {
  // First pass: the set of tenants that have at least one hierarchical member,
  // so a bare "work" can be recognized as that tenant's root.
  const hierarchicalTenants = new Set<string>()
  for (const p of projects) {
    const slash = p.name.indexOf('/')
    if (slash !== -1) hierarchicalTenants.add(p.name.slice(0, slash))
  }

  const byTenant = new Map<string, Tenant>()
  for (const p of projects) {
    const slash = p.name.indexOf('/')
    let tenant: string
    let label: string
    if (slash !== -1) {
      tenant = p.name.slice(0, slash)
      label = p.name.slice(slash + 1)
    } else if (hierarchicalTenants.has(p.name)) {
      tenant = p.name // the tenant root itself holds memories directly
      label = '(root)'
    } else {
      tenant = NO_TENANT
      label = p.name
    }
    let t = byTenant.get(tenant)
    if (!t) {
      t = { tenant, total: 0, projects: [] }
      byTenant.set(tenant, t)
    }
    t.total += p.stats?.total ?? 0
    t.projects.push({ name: p.name, label, stats: p.stats })
  }
  const tenants = [...byTenant.values()]
  tenants.sort((a, b) => {
    if (a.tenant === NO_TENANT) return 1
    if (b.tenant === NO_TENANT) return -1
    return a.tenant.localeCompare(b.tenant)
  })
  for (const t of tenants) t.projects.sort((a, b) => a.label.localeCompare(b.label))
  return tenants
}

export function Projects() {
  const { data, error, loading } = useAsync(async () => {
    const names = await api.namespaces()
    const stats = await Promise.all(names.map((n) => api.statsFor(n).catch(() => null)))
    return names.map((name, i) => ({ name, stats: stats[i] }))
  }, [refreshNonce.value])

  if (loading && !data) return <div class="view"><Loading /></div>
  if (error) return <div class="view"><ErrorBanner message={error} /></div>

  const projects = data ?? []
  const tenants = groupByTenant(projects)

  return (
    <div class="view">
      {projects.length === 0 ? (
        <Empty title="No projects" hint="Namespaces appear here once they hold memories." />
      ) : (
        <div class="tenant-list stagger">
          {tenants.map((t) => (
            <TenantBox key={t.tenant} tenant={t} />
          ))}
        </div>
      )}
    </div>
  )
}

function TenantBox({ tenant }: { tenant: Tenant }) {
  const open = (ns: string) => {
    namespace.value = ns
    route.value = 'overview'
  }
  return (
    <section class="tenant-box">
      <div class="tenant-head">
        <span class="tenant-name">{tenant.tenant}</span>
        <span class="tenant-count">
          <span class="v">{num(tenant.total)}</span> memories · {tenant.projects.length}{' '}
          {tenant.projects.length === 1 ? 'project' : 'projects'}
        </span>
      </div>
      <div class="pod-grid">
        {tenant.projects.map((p) => (
          <Pod key={p.name} name={p.name} label={p.label} stats={p.stats} onOpen={() => open(p.name)} />
        ))}
      </div>
    </section>
  )
}

function Pod({
  name,
  label,
  stats,
  onOpen,
}: {
  name: string
  label: string
  stats: Stats | null
  onOpen: () => void
}) {
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
    <div
      class="panel pod"
      role="button"
      tabIndex={0}
      onClick={onOpen}
      onKeyDown={(e) => {
        if (e.key === 'Enter') onOpen()
      }}
    >
      <div class="pod-head">
        <div class="pod-name" title={name}>{label}</div>
        <div class="pod-actions">
          <button
            class="icon-btn"
            aria-label={`Manage ${name}`}
            title="Move or split this namespace"
            onClick={manage}
          >
            <IconSettings />
          </button>
          <button
            class={`icon-btn pod-del-btn ${armed ? 'danger-on' : ''}`}
            aria-label={armed ? 'Confirm delete' : `Delete ${name}`}
            onClick={del}
            disabled={deleting}
          >
            <IconTrash />
          </button>
        </div>
      </div>
      {armed && (
        <div class="banner err" role="status">
          Click trash again to delete all memories in "{name}".
        </div>
      )}
      <div class="pod-count">
        <span class="v">{num(stats?.total ?? 0)}</span> memories
      </div>
      <TierBar stats={stats} />
      <div class="pod-foot">{stats?.last_write_at ? `updated ${relTime(stats.last_write_at)}` : '—'}</div>
      {managing && (
        // Preact portals bubble events through the vdom tree, so stop clicks and
        // keys from the drawer reaching the pod's open/keydown handlers.
        <div onClick={(e) => e.stopPropagation()} onKeyDown={(e) => e.stopPropagation()}>
          <NamespaceDrawer name={name} onClose={() => setManaging(false)} />
        </div>
      )}
    </div>
  )
}

function TierBar({ stats }: { stats: Stats | null }) {
  const total = stats?.total ?? 0
  if (total === 0) return <div class="pod-bar empty-bar" />
  return (
    <div class="pod-bar">
      {TIERS.map((t) => {
        const n = stats?.by_tier[t] ?? 0
        if (n === 0) return null
        return <span key={t} style={{ flexGrow: n, background: tierColor(t) }} title={`${t}: ${n}`} />
      })}
    </div>
  )
}
