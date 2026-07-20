import type { Tier } from '../types'

export function TierBadge({ tier }: { tier: Tier }) {
  // An activity row can carry no tier: the snapshot columns are stamped at write
  // time, and a row whose writer had nothing to snapshot (a pre-hydration inject
  // event, or an id no serve covered) round-trips as "". Rendering that anyway
  // produced a stray dot with no label — `.tier` leaves --tc unset without a
  // tier modifier, so the pill lost its colours but `.tier::before` still
  // painted. Nothing is the honest rendering of nothing.
  if (!tier) return null
  return <span class={`tier ${tier}`}>{tier}</span>
}
