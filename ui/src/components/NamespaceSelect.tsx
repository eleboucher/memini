import { useEffect, useRef, useState } from 'preact/hooks'
import { api } from '../api'
import { namespace, refresh, refreshNonce } from '../store'
import { useAsync } from '../hooks'
import { IconChevron, IconCheck } from '../icons'

// NamespaceSelect switches the active tenant. The chosen value is sent as the
// namespace header on every API call; empty ('') means "All projects" (reads
// aggregate across namespaces). Refetches the list on refresh so namespaces
// created while the app is open appear without a reload.
export function NamespaceSelect() {
  const [open, setOpen] = useState(false)
  const { data } = useAsync(() => api.namespaces(), [refreshNonce.value])
  const list = data ?? []
  const current = namespace.value || 'All projects'
  const options = ['', ...list]
  const menuRef = useRef<HTMLDivElement>(null)

  const pick = (ns: string) => {
    namespace.value = ns
    setOpen(false)
    refresh()
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
        aria-label={`Active project: ${current}`}
      >
        <span class="status-dot ok" aria-hidden="true" />
        <span class="ns-name">{current}</span>
        <IconChevron aria-hidden="true" />
      </button>
      {open && (
        <>
          <div style={{ position: 'fixed', inset: 0, zIndex: 29 }} onClick={() => setOpen(false)} />
          <div class="menu" role="menu" ref={menuRef}>
            {options.map((ns) => {
              const active = ns === namespace.value
              return (
                <button
                  key={ns || '__all__'}
                  type="button"
                  role="menuitemradio"
                  aria-checked={active}
                  class={`opt ${active ? 'active' : ''}`}
                  onClick={() => pick(ns)}
                >
                  <span class="ck">{active && <IconCheck aria-hidden="true" />}</span>
                  {ns || 'All projects'}
                </button>
              )
            })}
            {list.length === 0 && (
              <div class="opt" style={{ color: 'var(--muted)' }}>
                no other projects yet
              </div>
            )}
          </div>
        </>
      )}
    </div>
  )
}
