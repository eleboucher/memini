import { useState } from 'preact/hooks'

type Field = 'created' | 'accessed'

const WINDOWS: { label: string; hours: number }[] = [
  { label: 'Any time', hours: 0 },
  { label: 'Last 24h', hours: 24 },
  { label: 'Last 7d', hours: 24 * 7 },
  { label: 'Last 30d', hours: 24 * 30 },
]

interface Props {
  // onChange emits ISO instants for the created_after / accessed_after params;
  // undefined means no constraint.
  onChange: (createdAfter?: string, accessedAfter?: string) => void
}

// RecencyFilter narrows a listing to memories created (or last used) within a
// window. The cutoff is computed here and sent to the server rather than
// filtering the response, because the listing is capped under a sort: sorted by
// importance, a recent-but-unimportant memory falls outside the capped page, so
// a client-side window filter would quietly under-report.
export function RecencyFilter({ onChange }: Props) {
  const [field, setField] = useState<Field>('created')
  const [hours, setHours] = useState(0)

  const emit = (f: Field, h: number) => {
    const iso = h > 0 ? new Date(Date.now() - h * 3600_000).toISOString() : undefined
    onChange(f === 'created' ? iso : undefined, f === 'accessed' ? iso : undefined)
  }

  return (
    <div class="recency-filter" role="group" aria-label="Filter by recency">
      <select
        class="chip"
        value={field}
        aria-label="Recency field"
        onChange={(e) => {
          const f = (e.target as HTMLSelectElement).value as Field
          setField(f)
          emit(f, hours)
        }}
      >
        <option value="created">Created</option>
        <option value="accessed">Last used</option>
      </select>
      <select
        class="chip"
        value={String(hours)}
        aria-label="Recency window"
        onChange={(e) => {
          const h = Number((e.target as HTMLSelectElement).value)
          setHours(h)
          emit(field, h)
        }}
      >
        {WINDOWS.map((w) => (
          <option key={w.hours} value={String(w.hours)}>
            {w.label}
          </option>
        ))}
      </select>
    </div>
  )
}
