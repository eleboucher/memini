import { LEVELS, type Level } from '../types'

interface Props {
  selected: Level[]
  onChange: (levels: Level[]) => void
}

// LevelFilter toggles derivation level — explicit (the user said it) vs deduced
// (the model distilled it). Empty selection = both.
export function LevelFilter({ selected, onChange }: Props) {
  const toggle = (l: Level) => {
    onChange(selected.includes(l) ? selected.filter((x) => x !== l) : [...selected, l])
  }
  return (
    <div class="tier-filter" role="group" aria-label="Filter by level">
      {LEVELS.map((l) => {
        const on = selected.length === 0 || selected.includes(l)
        return (
          <button
            key={l}
            type="button"
            class={`tier-toggle ${on ? 'on' : ''}`}
            aria-pressed={on}
            aria-label={`${l} memories${on ? '' : ' (hidden)'}`}
            onClick={() => toggle(l)}
          >
            <span class={`chip level ${l}`}>{l}</span>
          </button>
        )
      })}
    </div>
  )
}
