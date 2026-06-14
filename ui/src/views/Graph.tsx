import { useEffect, useRef, useState } from 'preact/hooks'
import ForceGraph from 'force-graph'
import { forceCollide } from 'd3-force'
import { api } from '../api'
import { namespace, refreshNonce } from '../store'
import { useAsync } from '../hooks'
import type { Memory, Tier } from '../types'
import { MemoryDrawer } from '../components/MemoryDrawer'
import { Loading, ErrorBanner, Empty } from '../components/States'
import { TIERS } from '../types'
import { tierColor } from '../util'

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

export function Graph() {
  const hostRef = useRef<HTMLDivElement>(null)
  const [open, setOpen] = useState<Memory | null>(null)
  const openRef = useRef(setOpen)
  openRef.current = setOpen

  const { data, error, loading } = useAsync(
    () => api.list({ includeSuperseded: true, limit: 600 }),
    [namespace.value, refreshNonce.value],
  )
  const memories = data ?? []
  const byId = new Map(memories.map((m) => [m.id, m]))

  useEffect(() => {
    const host = hostRef.current
    if (!host || memories.length === 0) return

    const { nodes, links } = build(memories)

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
    // Rebuild on data / namespace / refresh; byId & memories derive from data.
  }, [data, namespace.value, refreshNonce.value])

  return (
    <div class="view" style={{ maxWidth: 'none' }}>
      {error && <ErrorBanner message={error} />}
      {loading && !data ? (
        <Loading />
      ) : memories.length === 0 ? (
        <Empty title="No memories" />
      ) : (
        <div class="panel graph-wrap">
          <div class="graph-canvas" ref={hostRef} />
          <div class="graph-hint">{memories.length} nodes · scroll to zoom · drag to pin</div>
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
        </div>
      )}
      {open && <MemoryDrawer memory={open} onClose={() => setOpen(null)} />}
    </div>
  )
}
