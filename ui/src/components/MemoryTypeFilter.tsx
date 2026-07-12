import { MEMORY_TYPES, type MemoryType } from '../types'
import { MemoryTypeBadge } from './MemoryTypeBadge'

interface Props {
  selected: string[]
  onChange: (types: string[]) => void
}

// MemoryTypeFilter is a row of toggleable memory-type badges, matching
// TierFilter. Empty selection = all types (including untyped memories, which no
// selection ever matches).
export function MemoryTypeFilter({ selected, onChange }: Props) {
  const toggle = (t: MemoryType) => {
    onChange(selected.includes(t) ? selected.filter((x) => x !== t) : [...selected, t])
  }
  return (
    <div class="tier-filter" role="group" aria-label="Filter by memory type">
      {MEMORY_TYPES.map((t) => {
        const on = selected.length === 0 || selected.includes(t)
        return (
          <button
            key={t}
            type="button"
            class={`tier-toggle ${on ? 'on' : ''}`}
            aria-pressed={on}
            aria-label={`${t} memories${on ? '' : ' (hidden)'}`}
            onClick={() => toggle(t)}
          >
            <MemoryTypeBadge type={t} />
          </button>
        )
      })}
    </div>
  )
}
