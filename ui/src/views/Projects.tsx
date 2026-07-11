import { useState } from 'preact/hooks'
import { useLocation } from 'preact-iso'
import { api } from '../api'
import { namespace, refreshNonce, refresh } from '../store'
import { useAsync } from '../hooks'
import { TIERS, type Stats } from '../types'
import { tierColor, relTime, num, nsTree, type NsNode } from '../util'
import { Loading, ErrorBanner, Empty } from '../components/States'
import { NamespaceDrawer } from '../components/NamespaceDrawer'
import { IconTrash, IconSettings, IconChevron } from '../icons'

// Collapsed namespace boxes persist across reloads (keyed by the box's full
// namespace path, so collapsing "acme/phoenix" doesn't also collapse "acme")
// so navigation stays where the user left it.
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
  name: string // full stored namespace, e.g. "acme/phoenix/api"
  stats: Stats | null
}

// ProjectNode augments the plain nsTree() shape with the stats for `ns` (when
// it holds memories of its own) and a recursive `total` across the whole
// subtree, so a box's header can show an aggregate count without re-walking
// its children on every render.
interface ProjectNode {
  ns: string
  label: string // the last path segment ("api" for "acme/phoenix/api")
  leaf: boolean
  stats: Stats | null
  total: number
  children: ProjectNode[]
}

// projectLeaf is the last path segment of a namespace, or the whole name when
// it has none. Dropping a pod onto a box re-homes it as "<destNs>/<leaf>" —
// i.e. it becomes a direct child of the box it was dropped on, regardless of
// how deep it used to sit under its old parent.
function projectLeaf(name: string): string {
  const slash = name.lastIndexOf('/')
  return slash === -1 ? name : name.slice(slash + 1)
}

// dropTarget computes the namespace a pod becomes when dropped on destNs.
// Returns "" when the drop is a no-op (dropped on its own direct parent).
function dropTarget(draggedName: string, destNs: string): string {
  const target = `${destNs}/${projectLeaf(draggedName)}`
  return target === draggedName ? '' : target
}

// buildTree turns the flat project list into the namespace hierarchy (via
// nsTree), attaching each node's own stats (when it's a real, leaf namespace)
// and a recursive memory total across its subtree.
function buildTree(projects: Project[]): ProjectNode[] {
  const statsByNs = new Map(projects.map((p) => [p.name, p.stats]))
  const attach = (n: NsNode): ProjectNode => {
    const children = n.children.map(attach)
    const stats = n.leaf ? (statsByNs.get(n.ns) ?? null) : null
    const total = (stats?.total ?? 0) + children.reduce((s, c) => s + c.total, 0)
    const slash = n.ns.lastIndexOf('/')
    return { ns: n.ns, label: slash === -1 ? n.ns : n.ns.slice(slash + 1), leaf: n.leaf, stats, total, children }
  }
  return nsTree(projects.map((p) => p.name)).map(attach)
}

// countProjects is the number of real (leaf) namespaces in a node's subtree,
// including the node itself.
function countProjects(node: ProjectNode): number {
  return (node.leaf ? 1 : 0) + node.children.reduce((s, c) => s + countProjects(c), 0)
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

  // The Move/Split drawer is lifted here (not per-pod) so a drop on any box
  // can open it for the dragged pod, which may live in a different box.
  const [manage, setManage] = useState<ManageTarget | null>(null)

  if (loading && !data) return <div class="view"><Loading /></div>
  if (error) return <div class="view"><ErrorBanner message={error} /></div>

  const projects = data ?? []
  const tree = buildTree(projects)

  // Dropping a pod onto a box opens the Move drawer pre-filled with the
  // derived target, so the drag lands on a dry-run-first confirmation rather
  // than a silent bulk move. A no-op target (own parent) is ignored.
  const onDropPod = (draggedName: string, destNs: string) => {
    const target = dropTarget(draggedName, destNs)
    if (target) setManage({ name: draggedName, moveTo: target })
  }

  return (
    <div class="view">
      {projects.length === 0 ? (
        <Empty title="No projects" hint="Namespaces appear here once they hold memories." />
      ) : (
        <div class="tenant-list stagger">
          {tree.map((n) => (
            <NsBox key={n.ns} node={n} onManage={(name) => setManage({ name })} onDropPod={onDropPod} />
          ))}
        </div>
      )}
      {manage && (
        <NamespaceDrawer name={manage.name} initialMoveTo={manage.moveTo} onClose={() => setManage(null)} />
      )}
    </div>
  )
}

// NsBox renders one namespace level as a k8s-style box: its own memories (if
// any) as a "(root)" pod, flat children as pods in the same grid, and any
// child that itself has children as a nested NsBox — so a namespace like
// "acme/phoenix/api" renders nested under "acme/phoenix" nested under "acme"
// rather than as a flat pod under "acme".
function NsBox({
  node,
  onManage,
  onDropPod,
}: {
  node: ProjectNode
  onManage: (name: string) => void
  onDropPod: (draggedName: string, destNs: string) => void
}) {
  const [dragOver, setDragOver] = useState(false)
  const [collapsed, setCollapsed] = useState(() => loadCollapsed().has(node.ns))
  const { route: navigate } = useLocation()

  const open = (ns: string) => {
    namespace.value = ns
    navigate('/')
  }

  const toggle = () => {
    const set = loadCollapsed()
    const next = !collapsed
    if (next) set.add(node.ns)
    else set.delete(node.ns)
    saveCollapsed(set)
    setCollapsed(next)
  }

  // Children with no further children render as pods in this box's grid;
  // children that themselves have children recurse into nested boxes.
  const podChildren = node.children.filter((c) => c.children.length === 0)
  const boxChildren = node.children.filter((c) => c.children.length > 0)
  const projectCount = countProjects(node)

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
      onDragEnter={(e) => {
        // Stop the enter from also bubbling to an ancestor box, so only the
        // deepest (most specific) box under the pointer highlights.
        e.stopPropagation()
        setDragOver(true)
      }}
      onDragLeave={(e) => {
        // Only clear when the pointer actually leaves the box, not when it
        // crosses into a child element.
        if (!e.currentTarget.contains(e.relatedTarget as Node | null)) setDragOver(false)
      }}
      onDrop={(e) => {
        e.preventDefault()
        // Stop the drop from also firing on an ancestor box's onDrop — only
        // the box actually under the pointer should re-home the pod.
        e.stopPropagation()
        setDragOver(false)
        const dragged = e.dataTransfer?.getData(DRAG_MIME)
        if (dragged) onDropPod(dragged, node.ns)
      }}
    >
      <button
        class="tenant-head"
        aria-expanded={!collapsed}
        aria-label={`${collapsed ? 'Expand' : 'Collapse'} ${node.ns}`}
        onClick={toggle}
      >
        <span class={`tenant-chevron${collapsed ? ' collapsed' : ''}`} aria-hidden="true">
          <IconChevron />
        </span>
        <span class="tenant-name">{node.label}</span>
        <span class="tenant-count">
          <span class="v">{num(node.total)}</span> memories · {projectCount}{' '}
          {projectCount === 1 ? 'project' : 'projects'}
        </span>
      </button>
      {!collapsed && (node.leaf || podChildren.length > 0) && (
        <div class="pod-grid">
          {node.leaf && (
            <Pod name={node.ns} label="(root)" stats={node.stats} onOpen={() => open(node.ns)} onManage={onManage} />
          )}
          {podChildren.map((c) => (
            <Pod key={c.ns} name={c.ns} label={c.label} stats={c.stats} onOpen={() => open(c.ns)} onManage={onManage} />
          ))}
        </div>
      )}
      {!collapsed && boxChildren.length > 0 && (
        <div class="tenant-list nested">
          {boxChildren.map((c) => (
            <NsBox key={c.ns} node={c} onManage={onManage} onDropPod={onDropPod} />
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
