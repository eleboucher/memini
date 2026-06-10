import { TIERS, type Tier } from '../types'
import { TierBadge } from './TierBadge'

interface Props {
  selected: Tier[]
  onChange: (tiers: Tier[]) => void
}

// TierFilter is a row of toggleable tier badges. Empty selection = all tiers.
export function TierFilter({ selected, onChange }: Props) {
  const toggle = (t: Tier) => {
    onChange(selected.includes(t) ? selected.filter((x) => x !== t) : [...selected, t])
  }
  return (
    <div class="tier-filter" role="group" aria-label="Filter by tier">
      {TIERS.map((t) => {
        const on = selected.length === 0 || selected.includes(t)
        return (
          <button
            key={t}
            type="button"
            class={`tier-toggle ${on ? 'on' : ''}`}
            aria-pressed={on}
            aria-label={`${t} tier${on ? '' : ' (hidden)'}`}
            onClick={() => toggle(t)}
          >
            <TierBadge tier={t} />
          </button>
        )
      })}
    </div>
  )
}
