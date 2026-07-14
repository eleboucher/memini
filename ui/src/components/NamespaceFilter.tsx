import { useState } from 'preact/hooks'
import { api } from '../api'
import { useAsync } from '../hooks'

interface Props {
  selected: string[]
  onChange: (namespaces: string[]) => void
}

// NamespaceFilter narrows an "All namespaces" listing to a subset of namespaces
// without touching the global namespace picker — the picker switches which
// namespace you're working in, this only narrows what the aggregate shows.
// Rendered only in All-namespaces mode (the caller gates it); a scoped listing is
// already restricted by the namespace header.
export function NamespaceFilter({ selected, onChange }: Props) {
  const [open, setOpen] = useState(false)
  const { data } = useAsync(() => api.namespaces(), [])
  const names = data ?? []

  const toggle = (n: string) => {
    onChange(selected.includes(n) ? selected.filter((x) => x !== n) : [...selected, n])
  }

  const label =
    selected.length === 0
      ? 'All namespaces'
      : selected.length === 1
        ? selected[0]
        : `${selected.length} namespaces`

  return (
    <div class="ns-filter">
      <button
        type="button"
        class={`chip ${selected.length ? 'on' : ''}`}
        aria-expanded={open}
        aria-label="Filter by namespace"
        onClick={() => setOpen(!open)}
      >
        {label} ▾
      </button>
      {open && (
        <div class="ns-filter-menu panel" role="group" aria-label="Namespaces">
          {names.length === 0 && <span class="muted">No namespaces</span>}
          {names.map((n) => (
            <label key={n} class="ns-filter-item">
              <input
                type="checkbox"
                checked={selected.includes(n)}
                onChange={() => toggle(n)}
              />
              <span class="mono">{n}</span>
            </label>
          ))}
          {selected.length > 0 && (
            <button type="button" class="chip" onClick={() => onChange([])}>
              Clear
            </button>
          )}
        </div>
      )}
    </div>
  )
}
