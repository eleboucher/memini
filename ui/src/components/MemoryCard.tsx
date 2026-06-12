import type { Memory } from '../types'
import { TierBadge } from './TierBadge'
import { MemoryTypeBadge } from './MemoryTypeBadge'
import { memoryType, relTime } from '../util'

interface Props {
  memory: Memory
  score?: number
  onOpen: (m: Memory) => void
  showNamespace?: boolean
}

export function MemoryCard({ memory: m, score, onOpen, showNamespace }: Props) {
  return (
    <button type="button" class={`mem panel ${m.tier}`} onClick={() => onOpen(m)}>
      <div class="mem-head">
        <TierBadge tier={m.tier} />
        <MemoryTypeBadge type={memoryType(m)} />
        {showNamespace && m.namespace && (
          <span class="chip" title="Project">{m.namespace}</span>
        )}
        <span class="grow" />
        {score !== undefined && <span class="mem-score">{score.toFixed(3)}</span>}
        {m.superseded_by && <span class="chip" title="Superseded by a newer memory">superseded</span>}
      </div>
      <div class="mem-body clamp">{m.summary || m.content}</div>
      <div class="mem-meta">
        <span class="imp" title={`importance ${m.importance.toFixed(2)}`}>
          imp
          <span class="imp-bar">
            <i style={{ width: `${Math.round(Math.min(1, Math.max(0, m.importance)) * 100)}%` }} />
          </span>
        </span>
        <span title="recall count">×{m.access_count}</span>
        <span title="last accessed">{relTime(m.last_accessed_at)}</span>
        {m.tags && m.tags.length > 0 && (
          <span class="tags">
            {m.tags.slice(0, 4).map((t) => (
              <span class="chip" key={t}>
                #{t}
              </span>
            ))}
            {m.tags.length > 4 && <span class="chip">+{m.tags.length - 4}</span>}
          </span>
        )}
      </div>
    </button>
  )
}
