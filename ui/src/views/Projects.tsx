import { useState } from 'preact/hooks'
import { api } from '../api'
import { namespace, route, refreshNonce, refresh } from '../store'
import { useAsync } from '../hooks'
import { TIERS, type Stats } from '../types'
import { tierColor, relTime, num } from '../util'
import { Loading, ErrorBanner, Empty } from '../components/States'
import { NamespaceDrawer } from '../components/NamespaceDrawer'
import { IconTrash, IconSettings, IconChevron } from '../icons'

// NO_TENANT groups flat (no-slash) namespaces, which have no tenant segment.
const NO_TENANT = '(no tenant)'

// Collapsed tenant boxes persist across reloads so navigation stays where the
// user left it.
const COLLAPSE_KEY = 'memini.collapsedTenants'
function loadCollapsed(): Set<string> {
  try {
    const raw = localStorage.getItem(COLLAPSE_KEY)
    return new Set(raw ? (JSON.parse(raw) as string[]) : [])
  } catch {
    return new Set()
  }
}
function saveCollapsed(set: Set<string>) {
  try {
    localStorage.setItem(COLLAPSE_KEY, JSON.stringify([...set]))
  } catch {
    /* best-effort */
  }
}

// DRAG_MIME carries the dragged pod's full namespace between dragstart and drop.
const DRAG_MIME = 'application/x-memini-namespace'

interface Project {
  name: string // full stored namespace, e.g. "work/memini/reviewer"
  stats: Stats | null
}

interface Tenant {
  tenant: string // display label for the k8s-style namespace box
  total: number // memories summed across the tenant's projects
  projects: { name: string; label: string; stats: Stats | null }[]
}

// projectLeaf is the part of a namespace after its tenant segment (the first
// slash), or the whole name when it has none. Dropping a pod onto a tenant box
// re-homes it as "<destTenant>/<leaf>", preserving any sub-path.
function projectLeaf(name: string): string {
  const slash = name.indexOf('/')
  return slash === -1 ? name : name.slice(slash + 1)
}

// dropTarget computes the namespace a pod becomes when dropped on destTenant.
// The NO_TENANT bucket un-tenants it (bare leaf). Returns "" when the drop is a
// no-op (same namespace, i.e. dropped on its own tenant).
function dropTarget(draggedName: string, destTenant: string): string {
  const leaf = projectLeaf(draggedName)
  const target = destTenant === NO_TENANT ? leaf : `${destTenant}/${leaf}`
  return target === draggedName ? '' : target
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

// ManageTarget is the namespace whose Move/Split drawer is open, optionally with
// a pre-filled Move target (set when opened by a drag-drop).
interface ManageTarget {
  name: string
  moveTo?: string
}

export function Projects() {
  const { data, error, loading } = useAsync(async () => {
    const names = await api.namespaces()
    const stats = await Promise.all(names.map((n) => api.statsFor(n).catch(() => null)))
    return names.map((name, i) => ({ name, stats: stats[i] }))
  }, [refreshNonce.value])

  // The Move/Split drawer is lifted here (not per-pod) so a drop on any tenant
  // box can open it for the dragged pod, which may live in a different box.
  const [manage, setManage] = useState<ManageTarget | null>(null)

  if (loading && !data) return <div class="view"><Loading /></div>
  if (error) return <div class="view"><ErrorBanner message={error} /></div>

  const projects = data ?? []
  const tenants = groupByTenant(projects)

  // Dropping a pod onto a tenant box opens the Move drawer pre-filled with the
  // derived target, so the drag lands on a dry-run-first confirmation rather
  // than a silent bulk move. A no-op target (own tenant) is ignored.
  const onDropPod = (draggedName: string, destTenant: string) => {
    const target = dropTarget(draggedName, destTenant)
    if (target) setManage({ name: draggedName, moveTo: target })
  }

  return (
    <div class="view">
      {projects.length === 0 ? (
        <Empty title="No projects" hint="Namespaces appear here once they hold memories." />
      ) : (
        <div class="tenant-list stagger">
          {tenants.map((t) => (
            <TenantBox key={t.tenant} tenant={t} onManage={(name) => setManage({ name })} onDropPod={onDropPod} />
          ))}
        </div>
      )}
      {manage && (
        <NamespaceDrawer name={manage.name} initialMoveTo={manage.moveTo} onClose={() => setManage(null)} />
      )}
    </div>
  )
}

function TenantBox({
  tenant,
  onManage,
  onDropPod,
}: {
  tenant: Tenant
  onManage: (name: string) => void
  onDropPod: (draggedName: string, destTenant: string) => void
}) {
  const [dragOver, setDragOver] = useState(false)
  const [collapsed, setCollapsed] = useState(() => loadCollapsed().has(tenant.tenant))

  const open = (ns: string) => {
    namespace.value = ns
    route.value = 'overview'
  }

  const toggle = () => {
    const set = loadCollapsed()
    const next = !collapsed
    if (next) set.add(tenant.tenant)
    else set.delete(tenant.tenant)
    saveCollapsed(set)
    setCollapsed(next)
  }

  return (
    <section
      class={`tenant-box${dragOver ? ' drop-target' : ''}${collapsed ? ' collapsed' : ''}`}
      onDragOver={(e) => {
        // preventDefault marks this a valid drop target; without it onDrop never fires.
        if (e.dataTransfer?.types.includes(DRAG_MIME)) {
          e.preventDefault()
          e.dataTransfer.dropEffect = 'move'
        }
      }}
      onDragEnter={() => setDragOver(true)}
      onDragLeave={(e) => {
        // Only clear when the pointer actually leaves the box, not when it
        // crosses into a child element.
        if (!e.currentTarget.contains(e.relatedTarget as Node | null)) setDragOver(false)
      }}
      onDrop={(e) => {
        e.preventDefault()
        setDragOver(false)
        const dragged = e.dataTransfer?.getData(DRAG_MIME)
        if (dragged) onDropPod(dragged, tenant.tenant)
      }}
    >
      <button
        class="tenant-head"
        aria-expanded={!collapsed}
        aria-label={`${collapsed ? 'Expand' : 'Collapse'} ${tenant.tenant}`}
        onClick={toggle}
      >
        <span class={`tenant-chevron${collapsed ? ' collapsed' : ''}`} aria-hidden="true">
          <IconChevron />
        </span>
        <span class="tenant-name">{tenant.tenant}</span>
        <span class="tenant-count">
          <span class="v">{num(tenant.total)}</span> memories · {tenant.projects.length}{' '}
          {tenant.projects.length === 1 ? 'project' : 'projects'}
        </span>
      </button>
      {!collapsed && (
        <div class="pod-grid">
          {tenant.projects.map((p) => (
            <Pod key={p.name} name={p.name} label={p.label} stats={p.stats} onOpen={() => open(p.name)} onManage={onManage} />
          ))}
        </div>
      )}
    </section>
  )
}

function Pod({
  name,
  label,
  stats,
  onOpen,
  onManage,
}: {
  name: string
  label: string
  stats: Stats | null
  onOpen: () => void
  onManage: (name: string) => void
}) {
  const [armed, setArmed] = useState(false)
  const [deleting, setDeleting] = useState(false)
  const [dragging, setDragging] = useState(false)

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
      class={`panel pod${dragging ? ' dragging' : ''}`}
      role="button"
      tabIndex={0}
      draggable
      onDragStart={(e) => {
        e.dataTransfer?.setData(DRAG_MIME, name)
        if (e.dataTransfer) e.dataTransfer.effectAllowed = 'move'
        setDragging(true)
      }}
      onDragEnd={() => setDragging(false)}
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
            onClick={(e) => {
              e.stopPropagation()
              onManage(name)
            }}
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
