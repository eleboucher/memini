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
import { zoom as d3zoom, zoomIdentity } from 'd3-zoom'
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
  for (const ids of byTag.values()) {
    if (ids.length < 2) continue
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

    const width = host.clientWidth
    const height = host.clientHeight
    const { nodes, links } = build(memories)

    const svg = select(host).append('svg').attr('viewBox', `0 0 ${width} ${height}`)

    // Arrowhead for supersession edges.
    svg
      .append('defs')
      .append('marker')
      .attr('id', 'arrow')
      .attr('viewBox', '0 -5 10 10')
      .attr('refX', 16)
      .attr('markerWidth', 6)
      .attr('markerHeight', 6)
      .attr('orient', 'auto')
      .append('path')
      .attr('d', 'M0,-4L9,0L0,4')
      .style('fill', 'var(--ember)')

    const g = svg.append('g')

    const link = g
      .append('g')
      .selectAll('line')
      .data(links)
      .join('line')
      .style('stroke', (d) => (d.kind === 'supersede' ? 'var(--ember)' : 'var(--line-strong)'))
      .style('stroke-width', (d) => (d.kind === 'supersede' ? 1.6 : 1))
      .style('stroke-dasharray', (d) => (d.kind === 'tag' ? '2 4' : 'none'))
      .style('opacity', (d) => (d.kind === 'supersede' ? 0.85 : 0.4))
      .attr('marker-end', (d) => (d.kind === 'supersede' ? 'url(#arrow)' : null))

    const node = g
      .append('g')
      .selectAll<SVGGElement, GNode>('g')
      .data(nodes)
      .join('g')
      .style('cursor', 'pointer')
      .on('click', (_e, d) => {
        const m = byId.get(d.id)
        if (m) openRef.current(m)
      })

    node
      .append('circle')
      .attr('r', (d) => d.r)
      .style('fill', (d) => tierColor(d.tier))
      .style('fill-opacity', (d) => (d.superseded ? 0.25 : 0.9))
      .style('stroke', (d) => tierColor(d.tier))
      .style('stroke-width', 1.5)
      .style('stroke-dasharray', (d) => (d.superseded ? '2 2' : 'none'))

    node
      .append('text')
      .attr('class', 'node-label')
      .attr('x', (d) => d.r + 4)
      .attr('y', 4)
      .text((d) => d.label)
      .style('display', nodes.length > 80 ? 'none' : 'inline')

    node.append('title').text((d) => `${d.tier} · ${d.label}`)

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
      .on('tick', () => {
        link
          .attr('x1', (d) => (d.source as GNode).x!)
          .attr('y1', (d) => (d.source as GNode).y!)
          .attr('x2', (d) => (d.target as GNode).x!)
          .attr('y2', (d) => (d.target as GNode).y!)
        node.attr('transform', (d) => `translate(${d.x},${d.y})`)
      })

    node.call(
      d3drag<SVGGElement, GNode>()
        .on('start', (event, d) => {
          if (!event.active) sim.alphaTarget(0.3).restart()
          d.fx = d.x
          d.fy = d.y
        })
        .on('drag', (event, d) => {
          d.fx = event.x
          d.fy = event.y
        })
        .on('end', (event, d) => {
          if (!event.active) sim.alphaTarget(0)
          d.fx = null
          d.fy = null
        }),
    )

    const zoomB = d3zoom<SVGSVGElement, unknown>()
      .scaleExtent([0.2, 4])
      .on('zoom', (event) => g.attr('transform', event.transform.toString()))
    svg.call(zoomB).call(zoomB.transform, zoomIdentity)

    return () => {
      sim.stop()
      select(host).select('svg').remove()
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
