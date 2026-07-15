import { useEffect, useRef, useState } from 'preact/hooks'
import { api } from '../api'
import { namespace, refresh, refreshNonce } from '../store'
import { useAsync } from '../hooks'
import { nsTree, type NsNode } from '../util'
import { IconChevron, IconCheck } from '../icons'

// NamespaceSelect switches the active namespace. The chosen value is sent as the
// namespace header on every API call; empty ('') means "All namespaces" (reads
// aggregate across namespaces). Refetches the list on refresh so namespaces
// created while the app is open appear without a reload. Namespaces render as
// an indented tree (nsTree) rather than a flat list, so a namespace like
// "acme/phoenix/api" appears nested under "acme/phoenix" nested under "acme"
// — matching how Namespaces groups them — instead of one long flat option list.
export function NamespaceSelect() {
  const [open, setOpen] = useState(false)
  const { data } = useAsync(() => api.namespaces(), [refreshNonce.value])
  const list = data ?? []
  const tree = nsTree(list)
  const current = namespace.value || 'All namespaces'
  const menuRef = useRef<HTMLDivElement>(null)
  // Collapsed node paths, local to this popover (not persisted — it's a small
  // navigation aid, not a durable view preference like Namespaces' boxes).
  const [collapsed, setCollapsed] = useState<Set<string>>(new Set())

  const pick = (ns: string) => {
    namespace.value = ns
    setOpen(false)
    refresh()
  }

  const toggleCollapse = (ns: string) => {
    setCollapsed((prev) => {
      const next = new Set(prev)
      if (next.has(ns)) next.delete(ns)
      else next.add(ns)
      return next
    })
  }

  // Keyboard: roving focus through options, Escape closes.
  useEffect(() => {
    if (!open) return
    menuRef.current?.querySelector<HTMLElement>('[role="menuitemradio"]')?.focus()
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') return setOpen(false)
      if (e.key !== 'ArrowDown' && e.key !== 'ArrowUp') return
      e.preventDefault()
      const items = Array.from(menuRef.current?.querySelectorAll<HTMLElement>('[role="menuitemradio"]') ?? [])
      const i = items.indexOf(document.activeElement as HTMLElement)
      const next = e.key === 'ArrowDown' ? i + 1 : i - 1
      items[(next + items.length) % items.length]?.focus()
    }
    document.addEventListener('keydown', onKey)
    return () => document.removeEventListener('keydown', onKey)
  }, [open])

  return (
    <div class="ns-select">
      <button
        class="ns-trigger"
        onClick={() => setOpen((v) => !v)}
        aria-haspopup="menu"
        aria-expanded={open}
        aria-label={`Active namespace: ${current}`}
      >
        <span class="status-dot ok" aria-hidden="true" />
        <span class="ns-name">{current}</span>
        <IconChevron aria-hidden="true" />
      </button>
      {open && (
        <>
          <div style={{ position: 'fixed', inset: 0, zIndex: 29 }} onClick={() => setOpen(false)} />
          <div class="menu tree" role="menu" ref={menuRef}>
            <button
              type="button"
              role="menuitemradio"
              aria-checked={namespace.value === ''}
              class={`opt ${namespace.value === '' ? 'active' : ''}`}
              onClick={() => pick('')}
            >
              <span class="ck">{namespace.value === '' && <IconCheck aria-hidden="true" />}</span>
              All namespaces
            </button>
            {tree.map((n) => (
              <NsRow
                key={n.ns}
                node={n}
                depth={0}
                current={namespace.value}
                collapsed={collapsed}
                onToggle={toggleCollapse}
                onPick={pick}
              />
            ))}
            {list.length === 0 && (
              <div class="opt" style={{ color: 'var(--muted)' }}>
                no other namespaces yet
              </div>
            )}
          </div>
        </>
      )}
    </div>
  )
}

function NsRow({
  node,
  depth,
  current,
  collapsed,
  onToggle,
  onPick,
}: {
  node: NsNode
  depth: number
  current: string
  collapsed: Set<string>
  onToggle: (ns: string) => void
  onPick: (ns: string) => void
}) {
  const isCollapsed = collapsed.has(node.ns)
  const hasChildren = node.children.length > 0
  const active = node.ns === current
  const label = node.ns.slice(node.ns.lastIndexOf('/') + 1)

  return (
    <>
      <div class="tree-row" style={{ paddingLeft: `${depth * 16}px` }}>
        {hasChildren ? (
          <button
            type="button"
            class={`tree-chevron${isCollapsed ? ' collapsed' : ''}`}
            aria-label={`${isCollapsed ? 'Expand' : 'Collapse'} ${node.ns}`}
            onClick={(e) => {
              e.stopPropagation()
              onToggle(node.ns)
            }}
          >
            <IconChevron />
          </button>
        ) : (
          <span class="tree-spacer" aria-hidden="true" />
        )}
        {node.leaf ? (
          <button
            type="button"
            role="menuitemradio"
            aria-checked={active}
            class={`opt ${active ? 'active' : ''}`}
            onClick={() => onPick(node.ns)}
          >
            <span class="ck">{active && <IconCheck aria-hidden="true" />}</span>
            {label}
          </button>
        ) : (
          // A synthetic path-prefix node (e.g. "acme/phoenix" when only
          // "acme/phoenix/api" holds memories) — a group label, not a
          // selectable namespace. It keeps the empty check slot so its label
          // lands on the same x as a leaf's at the same depth; without it the
          // group's label sits 22px left and nesting reads wrong.
          <span class="opt opt-group">
            <span class="ck" aria-hidden="true" />
            {label}
          </span>
        )}
      </div>
      {hasChildren &&
        !isCollapsed &&
        node.children.map((c) => (
          <NsRow
            key={c.ns}
            node={c}
            depth={depth + 1}
            current={current}
            collapsed={collapsed}
            onToggle={onToggle}
            onPick={onPick}
          />
        ))}
    </>
  )
}
