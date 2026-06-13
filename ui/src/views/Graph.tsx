import { useEffect, useRef, useState } from 'preact/hooks'
import {
  forceCenter,
  forceCollide,
  forceLink,
  forceManyBody,
  forceSimulation,
  type SimulationNodeDatum,
} from 'd3-force'
import { select } from 'd3-selection'
import { zoom as d3zoom, zoomIdentity, type ZoomTransform } from 'd3-zoom'
import { drag as d3drag } from 'd3-drag'
import { api } from '../api'
import { namespace, refreshNonce } from '../store'
import { useAsync } from '../hooks'
import type { Memory, Tier } from '../types'
import { MemoryDrawer } from '../components/MemoryDrawer'
import { Loading, ErrorBanner, Empty } from '../components/States'
import { TIERS } from '../types'
import { tierColor } from '../util'

interface GNode extends SimulationNodeDatum {
  id: string
  tier: Tier
  label: string
  r: number
  superseded: boolean
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

    let width = host.clientWidth
    let height = host.clientHeight
    const { nodes, links } = build(memories)
    const tagLinks = links.filter((l) => l.kind === 'tag')
    const supersedeLinks = links.filter((l) => l.kind === 'supersede')

    // Resolve CSS custom properties to concrete colors once — canvas fillStyle /
    // strokeStyle can't consume `var(--x)`. (Theme switches recolor on refresh.)
    const cssVar = (name: string) => getComputedStyle(host).getPropertyValue(name).trim()
    const tierHex: Record<Tier, string> = {
      working: cssVar('--tier-working'),
      episodic: cssVar('--tier-episodic'),
      semantic: cssVar('--tier-semantic'),
      procedural: cssVar('--tier-procedural'),
    }
    const colors = {
      tier: (t: Tier) => tierHex[t] || cssVar('--muted') || '#888',
      ember: cssVar('--ember') || '#e07a3f',
      tag: cssVar('--line-strong') || '#ccc',
      label: cssVar('--text-dim') || '#888',
    }
    const labelFont = `10px ${cssVar('--font-ui') || 'sans-serif'}`
    const showLabels = nodes.length <= 80

    const dpr = window.devicePixelRatio || 1
    const canvas = document.createElement('canvas')
    host.appendChild(canvas)
    const ctx = canvas.getContext('2d')!

    const sizeCanvas = () => {
      canvas.width = Math.round(width * dpr)
      canvas.height = Math.round(height * dpr)
    }
    sizeCanvas()

    let transform: ZoomTransform = zoomIdentity
    let hovered: GNode | null = null

    // drawArrow draws a small filled arrowhead at the target end of a supersede
    // edge, offset clear of the target node's radius.
    const drawArrow = (sx: number, sy: number, tx: number, ty: number, tr: number) => {
      const a = Math.atan2(ty - sy, tx - sx)
      const tipX = tx - Math.cos(a) * (tr + 2)
      const tipY = ty - Math.sin(a) * (tr + 2)
      const size = 6
      ctx.beginPath()
      ctx.moveTo(tipX, tipY)
      ctx.lineTo(tipX - Math.cos(a - 0.4) * size, tipY - Math.sin(a - 0.4) * size)
      ctx.lineTo(tipX - Math.cos(a + 0.4) * size, tipY - Math.sin(a + 0.4) * size)
      ctx.closePath()
      ctx.fillStyle = colors.ember
      ctx.fill()
    }

    const draw = () => {
      ctx.setTransform(dpr, 0, 0, dpr, 0, 0)
      ctx.clearRect(0, 0, width, height)
      ctx.translate(transform.x, transform.y)
      ctx.scale(transform.k, transform.k)

      // One path per link kind — a single stroke() beats one per edge.
      ctx.globalAlpha = 0.4
      ctx.strokeStyle = colors.tag
      ctx.lineWidth = 1
      ctx.setLineDash([2, 4])
      ctx.beginPath()
      for (const l of tagLinks) {
        const s = l.source as GNode
        const t = l.target as GNode
        ctx.moveTo(s.x!, s.y!)
        ctx.lineTo(t.x!, t.y!)
      }
      ctx.stroke()
      ctx.setLineDash([])

      ctx.globalAlpha = 0.85
      ctx.strokeStyle = colors.ember
      ctx.lineWidth = 1.6
      ctx.beginPath()
      for (const l of supersedeLinks) {
        const s = l.source as GNode
        const t = l.target as GNode
        ctx.moveTo(s.x!, s.y!)
        ctx.lineTo(t.x!, t.y!)
      }
      ctx.stroke()
      for (const l of supersedeLinks) {
        const s = l.source as GNode
        const t = l.target as GNode
        drawArrow(s.x!, s.y!, t.x!, t.y!, t.r)
      }

      // Live then superseded, so the dash state is set per group, not per node.
      ctx.lineWidth = 1.5
      for (const superseded of [false, true]) {
        ctx.setLineDash(superseded ? [2, 2] : [])
        for (const n of nodes) {
          if (n.superseded !== superseded) continue
          const c = colors.tier(n.tier)
          ctx.beginPath()
          ctx.arc(n.x!, n.y!, n.r, 0, Math.PI * 2)
          ctx.globalAlpha = superseded ? 0.25 : 0.9
          ctx.fillStyle = c
          ctx.fill()
          ctx.globalAlpha = 1
          ctx.strokeStyle = c
          ctx.stroke()
        }
      }
      ctx.setLineDash([])

      // Inline labels (only on smaller graphs, matching the SVG behavior).
      if (showLabels) {
        ctx.globalAlpha = 1
        ctx.fillStyle = colors.label
        ctx.font = labelFont
        ctx.textBaseline = 'middle'
        for (const n of nodes) ctx.fillText(n.label, n.x! + n.r + 4, n.y!)
      }

      // Hover tooltip in screen space so it stays legible at any zoom.
      if (hovered) {
        ctx.setTransform(dpr, 0, 0, dpr, 0, 0)
        const text = `${hovered.tier} · ${hovered.label}`
        ctx.font = labelFont
        const tw = ctx.measureText(text).width
        const px = transform.applyX(hovered.x!) + hovered.r * transform.k + 8
        const py = transform.applyY(hovered.y!)
        ctx.globalAlpha = 0.92
        ctx.fillStyle = cssVar('--surface') || '#000'
        ctx.strokeStyle = colors.tag
        ctx.lineWidth = 1
        ctx.beginPath()
        ctx.rect(px - 6, py - 11, tw + 12, 22)
        ctx.fill()
        ctx.stroke()
        ctx.globalAlpha = 1
        ctx.fillStyle = colors.label
        ctx.textBaseline = 'middle'
        ctx.fillText(text, px, py)
      }
      ctx.globalAlpha = 1
    }

    const sim = forceSimulation<GNode>(nodes)
      .force(
        'link',
        forceLink<GNode, GLink>(links)
          .id((d) => d.id)
          .distance((d) => (d.kind === 'supersede' ? 60 : 90))
          .strength((d) => (d.kind === 'supersede' ? 0.6 : 0.15)),
      )
      // distanceMax bounds the charge to nearby nodes; otherwise every node
      // repels every other each tick, the dominant cost on large graphs.
      .force('charge', forceManyBody().strength(-220).distanceMax(400))
      .force('center', forceCenter(width / 2, height / 2))
      .force('collide', forceCollide<GNode>().radius((d) => d.r + 6))
      // Settle in fewer ticks so the per-tick repaint stops sooner.
      .alphaDecay(0.045)
      .on('tick', draw)

    // findNode hit-tests in graph space (pointer coords inverted through zoom).
    const findNode = (px: number, py: number): GNode | null => {
      const gx = transform.invertX(px)
      const gy = transform.invertY(py)
      const n = sim.find(gx, gy)
      if (!n) return null
      return Math.hypot(n.x! - gx, n.y! - gy) <= n.r + 2 ? n : null
    }

    const sel = select(canvas)

    // Node drag. A non-null subject claims the gesture (stopping zoom-pan); over
    // empty space the subject is null, so the gesture falls through to zoom.
    // d3-drag suppresses the trailing click after a real drag, so opening the
    // drawer lives in a separate click handler below.
    sel.call(
      d3drag<HTMLCanvasElement, unknown>()
        .container(canvas)
        .subject((event) => findNode(event.x, event.y) ?? undefined)
        .on('start', (event) => {
          if (!event.active) sim.alphaTarget(0.3).restart()
          const d = event.subject as GNode
          d.fx = transform.invertX(event.x)
          d.fy = transform.invertY(event.y)
        })
        .on('drag', (event) => {
          const d = event.subject as GNode
          d.fx = transform.invertX(event.x)
          d.fy = transform.invertY(event.y)
        })
        .on('end', (event) => {
          if (!event.active) sim.alphaTarget(0)
          const d = event.subject as GNode
          d.fx = null
          d.fy = null
        }),
    )

    const onClick = (e: MouseEvent) => {
      const rect = canvas.getBoundingClientRect()
      const hit = findNode(e.clientX - rect.left, e.clientY - rect.top)
      if (!hit) return
      const m = byId.get(hit.id)
      if (m) openRef.current(m)
    }
    canvas.addEventListener('click', onClick)

    sel.call(
      d3zoom<HTMLCanvasElement, unknown>()
        .scaleExtent([0.2, 4])
        .on('zoom', (event) => {
          transform = event.transform
          draw()
        }),
    )

    // Hover: hit-test on move, update tooltip + cursor, repaint only on change.
    const onMove = (e: PointerEvent) => {
      const rect = canvas.getBoundingClientRect()
      const hit = findNode(e.clientX - rect.left, e.clientY - rect.top)
      if (hit !== hovered) {
        hovered = hit
        canvas.style.cursor = hit ? 'pointer' : 'grab'
        draw()
      }
    }
    const onLeave = () => {
      if (hovered) {
        hovered = null
        canvas.style.cursor = 'grab'
        draw()
      }
    }
    canvas.addEventListener('pointermove', onMove)
    canvas.addEventListener('pointerleave', onLeave)

    // Keep the backing store in sync with the panel size (height is viewport-based).
    const ro = new ResizeObserver(() => {
      width = host.clientWidth
      height = host.clientHeight
      sizeCanvas()
      sim.force('center', forceCenter(width / 2, height / 2))
      sim.alpha(0.1).restart()
      draw()
    })
    ro.observe(host)

    return () => {
      ro.disconnect()
      sim.stop()
      canvas.removeEventListener('pointermove', onMove)
      canvas.removeEventListener('pointerleave', onLeave)
      canvas.removeEventListener('click', onClick)
      canvas.remove()
    }
    // Rebuild on data / namespace / refresh; byId & memories derive from data,
    // so they need no separate dep. (No ESLint runs here; this is a note.)
  }, [data, namespace.value, refreshNonce.value])

  return (
    <div class="view" style={{ maxWidth: 'none' }}>
      {error && <ErrorBanner message={error} />}
      {loading && !data ? (
        <Loading />
      ) : memories.length === 0 ? (
        <Empty title="No memories" />
      ) : (
        <div class="panel graph-wrap" ref={hostRef}>
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
