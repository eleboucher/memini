import { useEffect, useRef, useState } from 'preact/hooks'
import ForceGraph from 'force-graph'
import { forceCollide } from 'd3-force'
import { api } from '../api'
import { namespace, refresh, refreshNonce } from '../store'
import { useAsync } from '../hooks'
import type { Memory, NamespaceLink, Tier } from '../types'
import { MemoryDrawer } from '../components/MemoryDrawer'
import { Loading, ErrorBanner, Empty } from '../components/States'
import { TIERS } from '../types'
import { tierColor, rootOf, ancestorsOf, depth as nsDepth } from '../util'

interface GNode {
  id: string
  tier: Tier
  label: string
  r: number
  superseded: boolean
  x?: number
  y?: number
}
interface GLink {
  source: string | GNode
  target: string | GNode
  kind: 'supersede' | 'tag'
}

// build derives a node-link graph from memories: supersession is a directed
// edge (old → replacement); shared tags create faint undirected affinity edges.
function build(memories: Memory[]): { nodes: GNode[]; links: GLink[] } {
  const present = new Set(memories.map((m) => m.id))
  const nodes: GNode[] = memories.map((m) => ({
    id: m.id,
    tier: m.tier,
    label: (m.summary || m.content).slice(0, 40),
    r: 7 + Math.min(1, Math.max(0, m.importance)) * 9 + Math.min(6, Math.log1p(m.access_count) * 2),
    superseded: !!m.superseded_by,
  }))

  const links: GLink[] = []
  const seen = new Set<string>()
  const add = (a: string, b: string, kind: GLink['kind']) => {
    const key = kind + (a < b ? a + b : b + a)
    if (seen.has(key) || a === b) return
    seen.add(key)
    links.push({ source: a, target: b, kind })
  }

  for (const m of memories) {
    if (m.superseded_by && present.has(m.superseded_by)) add(m.id, m.superseded_by, 'supersede')
  }

  // Tag co-occurrence — star-link within each tag group to avoid hairballs.
  const byTag = new Map<string, string[]>()
  for (const m of memories) {
    for (const t of m.tags ?? []) {
      const arr = byTag.get(t) ?? []
      arr.push(m.id)
      byTag.set(t, arr)
    }
  }
  // Skip catch-all tags: a huge star of edges is a hairball and the dominant cost.
  const MAX_TAG_GROUP = 40
  for (const ids of byTag.values()) {
    if (ids.length < 2 || ids.length > MAX_TAG_GROUP) continue
    for (let i = 1; i < ids.length; i++) add(ids[0], ids[i], 'tag')
  }

  return { nodes, links }
}

// ---- Namespace mode ---------------------------------------------------
// Nodes are namespaces (sized by memory count, colored by depth); edges are
// the implicit parent->child cascade plus explicit namespace links.

interface NNode {
  id: string // full namespace path
  label: string // last path segment
  depth: number
  total: number
  r: number
  x?: number
  y?: number
}
interface NLink {
  source: string | NNode
  target: string | NNode
  kind: 'cascade' | 'link'
}

interface NsGraphData {
  names: string[]
  totals: Map<string, number>
  links: NamespaceLink[]
}

// buildNamespaceGraph derives the namespace-level node-link graph: one node
// per namespace (sized by its own live memory count, colored by nesting
// depth), a directed cascade edge from each namespace to its immediate
// parent (mirroring the ancestor read-set leg every namespace inherits from),
// and a directed edge per stored namespace link (a durable-tier read edge
// that isn't implied by nesting).
function buildNamespaceGraph(data: NsGraphData): { nodes: NNode[]; links: NLink[] } {
  const present = new Set(data.names)
  const nodes: NNode[] = data.names.map((ns) => {
    const total = data.totals.get(ns) ?? 0
    return {
      id: ns,
      label: ns.slice(ns.lastIndexOf('/') + 1),
      depth: nsDepth(ns),
      total,
      r: 7 + Math.min(26, Math.log1p(total) * 6),
    }
  })

  const links: NLink[] = []
  for (const ns of data.names) {
    const ancestors = ancestorsOf(ns)
    const parent = ancestors[ancestors.length - 1]
    if (parent && present.has(parent)) links.push({ source: parent, target: ns, kind: 'cascade' })
  }
  const seenLinks = new Set<string>()
  for (const l of data.links) {
    if (!present.has(l.src) || !present.has(l.dst)) continue
    const key = `${l.src}->${l.dst}`
    if (seenLinks.has(key)) continue
    seenLinks.add(key)
    links.push({ source: l.src, target: l.dst, kind: 'link' })
  }
  return { nodes, links }
}

// DEPTH_COLOR_VARS cycles the existing tier ramp by nesting depth (0 = root)
// instead of tier, so namespace-mode nodes reuse the same theme-aware palette
// as everything else rather than inventing new colors.
const DEPTH_COLOR_VARS = ['--tier-working', '--tier-episodic', '--tier-semantic', '--tier-procedural']

export function Graph() {
  const hostRef = useRef<HTMLDivElement>(null)
  const [mode, setMode] = useState<'memories' | 'namespaces'>('memories')
  const [open, setOpen] = useState<Memory | null>(null)
  const openRef = useRef(setOpen)
  openRef.current = setOpen
  // '' = all tenants. Client-side filter over the loaded set (which spans
  // tenants in "All projects" mode), so switching tenants is instant.
  const [tenant, setTenant] = useState('')

  const { data, error, loading } = useAsync(
    () => api.list({ includeSuperseded: true, limit: 600 }),
    [namespace.value, refreshNonce.value],
  )
  const memories = data ?? []
  // Distinct top-level roots present in the loaded memories, for the filter
  // dropdown (e.g. "acme" for both "acme" and "acme/phoenix/api").
  const tenantList = [...new Set(memories.map((m) => rootOf(m.namespace)))].sort((a, b) => a.localeCompare(b))
  const filtered = tenant ? memories.filter((m) => rootOf(m.namespace) === tenant) : memories

  // Namespace-mode data: every namespace (not scoped to the active one — this
  // is a store-wide topology view), its live total, and its outgoing links.
  // Skips the fetch entirely while in memories mode.
  const {
    data: nsData,
    error: nsError,
    loading: nsLoading,
  } = useAsync(async (): Promise<NsGraphData | null> => {
    if (mode !== 'namespaces') return null
    const names = await api.namespaces()
    const [totals, linkLists] = await Promise.all([
      Promise.all(names.map((n) => api.statsFor(n).then((s) => s.total).catch(() => 0))),
      Promise.all(names.map((n) => api.links(n).catch(() => []))),
    ])
    return { names, totals: new Map(names.map((n, i) => [n, totals[i]])), links: linkLists.flat() }
  }, [mode, refreshNonce.value])

  // ---- Memories-mode graph ----
  useEffect(() => {
    if (mode !== 'memories') return
    const host = hostRef.current
    if (!host || filtered.length === 0) return

    const byId = new Map(filtered.map((m) => [m.id, m]))
    const { nodes, links } = build(filtered)

    // Resolve CSS custom properties to concrete colors once — canvas fill/stroke
    // can't consume `var(--x)`. (Theme switches recolor on the next refresh.)
    const cssVar = (name: string) => getComputedStyle(host).getPropertyValue(name).trim()
    const withAlpha = (hex: string, a: string) => (/^#[0-9a-f]{6}$/i.test(hex) ? hex + a : hex)
    const tierHex: Record<Tier, string> = {
      working: cssVar('--tier-working'),
      episodic: cssVar('--tier-episodic'),
      semantic: cssVar('--tier-semantic'),
      procedural: cssVar('--tier-procedural'),
    }
    const tierFill = (t: Tier) => tierHex[t] || cssVar('--muted') || '#888'
    const ember = cssVar('--ember') || '#e07a3f'
    const tagColor = withAlpha(cssVar('--line-strong') || '#ccc', '66')
    const labelColor = cssVar('--text-dim') || '#888'
    const labelFont = `10px ${cssVar('--font-ui') || 'sans-serif'}`
    const showLabels = nodes.length <= 80

    const graph = new ForceGraph<GNode, GLink>(host)
      .graphData({ nodes, links })
      .backgroundColor('rgba(0,0,0,0)')
      .width(host.clientWidth)
      .height(host.clientHeight)
      .minZoom(0.2)
      .maxZoom(4)
      .nodeId('id')
      // Hover tooltip — built via textContent so memory text can't inject HTML.
      .nodeLabel((n) => {
        const el = document.createElement('div')
        el.textContent = `${n.tier} · ${n.label}`
        return el
      })
      .nodeCanvasObjectMode(() => 'replace')
      .nodeCanvasObject((n, ctx) => {
        const c = tierFill(n.tier)
        ctx.beginPath()
        ctx.arc(n.x!, n.y!, n.r, 0, Math.PI * 2)
        ctx.globalAlpha = n.superseded ? 0.25 : 0.9
        ctx.fillStyle = c
        ctx.fill()
        ctx.globalAlpha = 1
        ctx.lineWidth = 1.5
        ctx.setLineDash(n.superseded ? [2, 2] : [])
        ctx.strokeStyle = c
        ctx.stroke()
        ctx.setLineDash([])
        if (showLabels) {
          ctx.fillStyle = labelColor
          ctx.font = labelFont
          ctx.textBaseline = 'middle'
          ctx.fillText(n.label, n.x! + n.r + 4, n.y!)
        }
      })
      // Clickable area matches the drawn circle so hit-testing follows the node.
      .nodePointerAreaPaint((n, paint, ctx) => {
        ctx.fillStyle = paint
        ctx.beginPath()
        ctx.arc(n.x!, n.y!, n.r + 2, 0, Math.PI * 2)
        ctx.fill()
      })
      .linkColor((l) => (l.kind === 'supersede' ? withAlpha(ember, 'd9') : tagColor))
      .linkWidth((l) => (l.kind === 'supersede' ? 1.6 : 1))
      .linkLineDash((l) => (l.kind === 'tag' ? [2, 4] : null))
      .linkDirectionalArrowLength((l) => (l.kind === 'supersede' ? 5 : 0))
      .linkDirectionalArrowColor(() => ember)
      .linkDirectionalArrowRelPos(1)
      // Non-zero alphaMin so the layout converges and halts (default 0 keeps it
      // drifting, making clicks chase a moving node).
      .d3AlphaDecay(0.045)
      .d3AlphaMin(0.001)
      .cooldownTime(4000)
      .onNodeClick((n) => {
        const m = byId.get(n.id)
        if (m) openRef.current(m)
      })

    const charge = graph.d3Force('charge')
    if (charge) charge.strength(-220).distanceMax(400)
    const link = graph.d3Force('link')
    if (link) {
      link.distance((l: GLink) => (l.kind === 'supersede' ? 60 : 90))
      link.strength((l: GLink) => (l.kind === 'supersede' ? 0.6 : 0.15))
    }
    graph.d3Force('collide', forceCollide<GNode>().radius((n) => n.r + 6) as never)

    const ro = new ResizeObserver(() => {
      graph.width(host.clientWidth).height(host.clientHeight)
    })
    ro.observe(host)

    return () => {
      ro.disconnect()
      graph._destructor()
      host.replaceChildren()
    }
    // Rebuild on data / namespace / refresh / tenant filter; filtered & byId
    // derive from data + tenant.
  }, [mode, data, namespace.value, refreshNonce.value, tenant])

  // ---- Namespace-mode graph ----
  useEffect(() => {
    if (mode !== 'namespaces') return
    const host = hostRef.current
    if (!host || !nsData || nsData.names.length === 0) return

    const { nodes, links } = buildNamespaceGraph(nsData)

    const cssVar = (name: string) => getComputedStyle(host).getPropertyValue(name).trim()
    const withAlpha = (hex: string, a: string) => (/^#[0-9a-f]{6}$/i.test(hex) ? hex + a : hex)
    const depthColors = DEPTH_COLOR_VARS.map((v) => cssVar(v) || cssVar('--muted') || '#888')
    const depthFill = (d: number) => depthColors[Math.min(d, depthColors.length - 1)]
    const ember = cssVar('--ember') || '#e07a3f'
    const linkColor = withAlpha(cssVar('--line-strong') || '#ccc', 'aa')
    const labelColor = cssVar('--text-dim') || '#888'
    const labelFont = `10px ${cssVar('--font-ui') || 'sans-serif'}`

    const graph = new ForceGraph<NNode, NLink>(host)
      .graphData({ nodes, links })
      .backgroundColor('rgba(0,0,0,0)')
      .width(host.clientWidth)
      .height(host.clientHeight)
      .minZoom(0.2)
      .maxZoom(4)
      .nodeId('id')
      .nodeLabel((n) => {
        const el = document.createElement('div')
        el.textContent = `${n.id} · ${n.total} memories`
        return el
      })
      .nodeCanvasObjectMode(() => 'replace')
      .nodeCanvasObject((n, ctx) => {
        const c = depthFill(n.depth)
        ctx.beginPath()
        ctx.arc(n.x!, n.y!, n.r, 0, Math.PI * 2)
        ctx.globalAlpha = 0.9
        ctx.fillStyle = c
        ctx.fill()
        ctx.globalAlpha = 1
        ctx.lineWidth = 1.5
        ctx.strokeStyle = c
        ctx.stroke()
        ctx.fillStyle = labelColor
        ctx.font = labelFont
        ctx.textBaseline = 'middle'
        ctx.fillText(n.label, n.x! + n.r + 4, n.y!)
      })
      .nodePointerAreaPaint((n, paint, ctx) => {
        ctx.fillStyle = paint
        ctx.beginPath()
        ctx.arc(n.x!, n.y!, n.r + 2, 0, Math.PI * 2)
        ctx.fill()
      })
      .linkColor((l) => (l.kind === 'link' ? ember : linkColor))
      .linkWidth((l) => (l.kind === 'link' ? 1.6 : 1.2))
      .linkLineDash((l) => (l.kind === 'link' ? [2, 4] : null))
      .linkDirectionalArrowLength(5)
      .linkDirectionalArrowColor((l) => (l.kind === 'link' ? ember : linkColor))
      .linkDirectionalArrowRelPos(1)
      .d3AlphaDecay(0.045)
      .d3AlphaMin(0.001)
      .cooldownTime(4000)
      // Selecting a namespace node makes it the active project, so the rest
      // of the UI (and the memories-mode graph) can pick it up immediately.
      .onNodeClick((n) => {
        namespace.value = n.id
        refresh()
      })

    const charge = graph.d3Force('charge')
    if (charge) charge.strength(-260).distanceMax(500)
    const link = graph.d3Force('link')
    if (link) {
      link.distance((l: NLink) => (l.kind === 'cascade' ? 70 : 110))
      link.strength((l: NLink) => (l.kind === 'cascade' ? 0.5 : 0.2))
    }
    graph.d3Force('collide', forceCollide<NNode>().radius((n) => n.r + 8) as never)

    const ro = new ResizeObserver(() => {
      graph.width(host.clientWidth).height(host.clientHeight)
    })
    ro.observe(host)

    return () => {
      ro.disconnect()
      graph._destructor()
      host.replaceChildren()
    }
  }, [mode, nsData])

  const showTenantFilter = mode === 'memories' && tenantList.length > 1
  const isLoading = mode === 'memories' ? loading && !data : nsLoading && !nsData
  const activeError = mode === 'memories' ? error : nsError
  const empty = mode === 'memories' ? memories.length === 0 : (nsData?.names.length ?? 0) === 0

  return (
    <>
      <div class="view" style={{ maxWidth: 'none' }}>
        <div class="toolbar" style={{ marginBottom: '14px' }}>
          <div class="seg-ctl">
            <button type="button" class={mode === 'memories' ? 'on' : ''} onClick={() => setMode('memories')}>
              memories
            </button>
            <button type="button" class={mode === 'namespaces' ? 'on' : ''} onClick={() => setMode('namespaces')}>
              namespaces
            </button>
          </div>
        </div>
        {activeError && <ErrorBanner message={activeError} />}
        {isLoading ? (
          <Loading />
        ) : empty ? (
          <Empty title={mode === 'memories' ? 'No memories' : 'No namespaces'} />
        ) : (
          <div class="panel graph-wrap">
            {showTenantFilter && (
              <div class="graph-controls">
                <label class="hint">
                  tenant{' '}
                  <select
                    class="input"
                    value={tenant}
                    onChange={(e) => setTenant((e.target as HTMLSelectElement).value)}
                  >
                    <option value="">all</option>
                    {tenantList.map((t) => (
                      <option key={t} value={t}>
                        {t}
                      </option>
                    ))}
                  </select>
                </label>
              </div>
            )}
            <div class="graph-canvas" ref={hostRef} />
            {mode === 'memories' ? (
              filtered.length === 0 ? (
                <div class="graph-hint">no memories in "{tenant}"</div>
              ) : (
                <div class="graph-hint">
                  {filtered.length} nodes{tenant ? ` · ${tenant}` : ''} · scroll to zoom · drag to pin
                </div>
              )
            ) : (
              <div class="graph-hint">
                {nsData?.names.length ?? 0} namespaces · click a node to switch project · scroll to zoom
              </div>
            )}
            {mode === 'memories' ? (
              <div class="graph-legend">
                {TIERS.map((t) => (
                  <div class="row" key={t}>
                    <span class="status-dot" style={{ background: tierColor(t) }} /> {t}
                  </div>
                ))}
                <div class="row" style={{ marginTop: '8px', color: 'var(--ember)' }}>
                  <span style={{ fontFamily: 'var(--font-mono)' }}>→</span> supersedes
                </div>
                <div class="row" style={{ color: 'var(--muted)' }}>
                  <span style={{ fontFamily: 'var(--font-mono)' }}>┄</span> shared tag
                </div>
              </div>
            ) : (
              <div class="graph-legend">
                <div class="row">
                  <span class="status-dot" style={{ background: 'var(--tier-working)' }} /> depth 0 (root)
                </div>
                <div class="row">
                  <span class="status-dot" style={{ background: 'var(--tier-episodic)' }} /> depth 1
                </div>
                <div class="row">
                  <span class="status-dot" style={{ background: 'var(--tier-semantic)' }} /> depth 2
                </div>
                <div class="row">
                  <span class="status-dot" style={{ background: 'var(--tier-procedural)' }} /> depth 3+
                </div>
                <div class="row" style={{ marginTop: '8px', color: 'var(--muted)' }}>
                  <span style={{ fontFamily: 'var(--font-mono)' }}>→</span> cascade (parent → child)
                </div>
                <div class="row" style={{ color: 'var(--ember)' }}>
                  <span style={{ fontFamily: 'var(--font-mono)' }}>┄→</span> link
                </div>
              </div>
            )}
          </div>
        )}
      </div>
      {open && <MemoryDrawer memory={open} onClose={() => setOpen(null)} />}
    </>
  )
}
