import type { Memory } from '../types'
import { TierBadge } from './TierBadge'
import { MemoryTypeBadge } from './MemoryTypeBadge'
import { fromLabel, isAutoTiered, memoryType, promotedFrom, relTime } from '../util'

interface Props {
  memory: Memory
  score?: number
  onOpen: (m: Memory) => void
  showNamespace?: boolean
  // Read-set provenance beyond memory.namespace (ScoredMemory.from /
  // BriefingItem.from) — omitted for a primary-namespace hit.
  from?: string
  // The namespace the fetch that produced `from` ran under, captured at fetch
  // time by the owning view — NOT the live namespace signal, which can move
  // (selector switch) while these results are still on screen and would
  // silently relabel a stale ancestor hit as "personal" (or vice versa).
  fromNs?: string
}

export function MemoryCard({ memory: m, score, onOpen, showNamespace, from, fromNs }: Props) {
  return (
    <button type="button" class={`mem panel ${m.tier}`} onClick={() => onOpen(m)}>
      <div class="mem-head">
        <TierBadge tier={m.tier} />
        <MemoryTypeBadge type={memoryType(m)} />
        {showNamespace && m.namespace && (
          <span class="chip" title="Namespace">{m.namespace}</span>
        )}
        {from && (
          <span class="chip" title={`In scope via ${from}`}>
            {fromLabel(from, fromNs ?? '')}
          </span>
        )}
        {isAutoTiered(m) && (
          <span class="chip" title="Tier chosen by the write-time classifier — worth a glance">
            auto-tiered
          </span>
        )}
        {promotedFrom(m) && (
          <span class="chip" title="Distilled from a frequently-recalled short-term memory">
            promoted
          </span>
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
        {m.confidence != null && (
          <span
            class="imp"
            title={`confidence ${m.confidence.toFixed(2)} — grows each time the fact is re-observed`}
          >
            conf
            <span class="imp-bar">
              <i style={{ width: `${Math.round(Math.min(1, Math.max(0, m.confidence)) * 100)}%` }} />
            </span>
          </span>
        )}
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
