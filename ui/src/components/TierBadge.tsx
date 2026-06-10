import type { Tier } from '../types'

export function TierBadge({ tier }: { tier: Tier }) {
  return <span class={`tier ${tier}`}>{tier}</span>
}
