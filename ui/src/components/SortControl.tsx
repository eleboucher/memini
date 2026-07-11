import { SORT_KEYS, type SortKey, type SortOrder } from '../types'

interface Props {
  sort: SortKey
  order: SortOrder
  onChange: (sort: SortKey, order: SortOrder) => void
}

// SortControl picks the server-side sort for a listing: a key select plus a
// direction toggle. Sorting happens on the server because the listing is capped
// — reordering the capped page in the browser would sort the wrong rows.
export function SortControl({ sort, order, onChange }: Props) {
  const label = SORT_KEYS.find((s) => s.key === sort)?.label ?? sort
  return (
    <div class="sort-control" role="group" aria-label="Sort">
      <select
        class="chip"
        value={sort}
        aria-label="Sort by"
        onChange={(e) => onChange((e.target as HTMLSelectElement).value as SortKey, order)}
      >
        {SORT_KEYS.map((s) => (
          <option key={s.key} value={s.key}>
            {s.label}
          </option>
        ))}
      </select>
      <button
        type="button"
        class="chip sort-dir"
        aria-label={`${label}, ${order === 'desc' ? 'descending' : 'ascending'}`}
        title={order === 'desc' ? 'Descending' : 'Ascending'}
        onClick={() => onChange(sort, order === 'desc' ? 'asc' : 'desc')}
      >
        {order === 'desc' ? '↓' : '↑'}
      </button>
    </div>
  )
}
