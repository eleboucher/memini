import { useEffect, useRef, useState } from 'preact/hooks'
import { createPortal } from 'preact/compat'
import type { Memory } from '../types'
import { api } from '../api'
import { refresh } from '../store'
import { TierBadge } from './TierBadge'
import { MemoryTypeBadge } from './MemoryTypeBadge'
import { fmtDate, isAutoTiered, memoryType, promotedFrom, relTime } from '../util'
import { IconClose, IconCopy, IconCheck, IconTrash } from '../icons'

interface Props {
  memory: Memory
  onClose: () => void
  wide?: boolean
}

export function MemoryDrawer({ memory: m, onClose, wide }: Props) {
  const [copied, setCopied] = useState(false)
  const [armed, setArmed] = useState(false) // delete confirmation (two-click)
  const [deleting, setDeleting] = useState(false)
  const [err, setErr] = useState<string | null>(null)

  const drawerRef = useRef<HTMLElement>(null)
  const closeRef = useRef<HTMLButtonElement>(null)

  // Modal behavior: focus the drawer on open, trap Tab inside it, close on
  // Escape, and restore focus to whatever was focused before.
  useEffect(() => {
    const prev = document.activeElement as HTMLElement | null
    // preventScroll: the drawer starts off-screen (translateX(100%)); scrolling to
    // the focused child would jump the page during the slide-in.
    closeRef.current?.focus({ preventScroll: true })

    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') {
        onClose()
        return
      }
      if (e.key !== 'Tab') return
      const root = drawerRef.current
      if (!root) return
      const f = root.querySelectorAll<HTMLElement>(
        'button, [href], input, textarea, [tabindex]:not([tabindex="-1"])',
      )
      if (f.length === 0) return
      const first = f[0]
      const last = f[f.length - 1]
      if (e.shiftKey && document.activeElement === first) {
        e.preventDefault()
        last.focus()
      } else if (!e.shiftKey && document.activeElement === last) {
        e.preventDefault()
        first.focus()
      }
    }
    document.addEventListener('keydown', onKey)
    return () => {
      document.removeEventListener('keydown', onKey)
      prev?.focus?.()
    }
  }, [onClose])

  const copyId = () => {
    Promise.resolve(navigator.clipboard?.writeText(m.id))
      .then(() => {
        setCopied(true)
        setTimeout(() => setCopied(false), 1200)
      })
      .catch(() => setErr('Could not copy to clipboard'))
  }

  const del = async () => {
    if (!armed) {
      setArmed(true)
      setTimeout(() => setArmed(false), 3000)
      return
    }
    setDeleting(true)
    setErr(null)
    try {
      // Scope the delete to the memory's own namespace — in "All projects" mode
      // the active namespace is empty and would otherwise hit the wrong tenant.
      await api.remove(m.id, m.namespace || undefined)
      refresh()
      onClose()
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e))
      setDeleting(false)
      setArmed(false)
    }
  }

  const metaEntries = m.metadata ? Object.entries(m.metadata) : []

  return createPortal(
    <>
      <div class="scrim" onClick={onClose} />
      <aside class={`drawer${wide ? ' wide' : ''}`} role="dialog" aria-modal="true" aria-label="Memory detail" ref={drawerRef}>
        <div class="drawer-head">
          <TierBadge tier={m.tier} />
          <MemoryTypeBadge type={memoryType(m)} />
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
          <button class="icon-btn" aria-label={copied ? 'Copied' : 'Copy ID'} onClick={copyId}>
            {copied ? <IconCheck /> : <IconCopy />}
          </button>
          <button
            class={`icon-btn ${armed ? 'danger-on' : ''}`}
            aria-label={armed ? 'Confirm delete' : 'Forget memory'}
            onClick={del}
            disabled={deleting}
          >
            <IconTrash />
          </button>
          <button class="icon-btn" aria-label="Close" onClick={onClose} ref={closeRef}>
            <IconClose />
          </button>
        </div>
        <div class="drawer-body">
          {err && <div class="banner err" role="alert">{err}</div>}
          {armed && !err && (
            <div class="banner err" role="status">
              Click the trash again to permanently forget this memory.
            </div>
          )}

          {m.summary && (
            <>
              <h4>Summary</h4>
              <div class="prose">{m.summary}</div>
            </>
          )}

          <h4>Content</h4>
          <div class="prose">{m.content}</div>

          {m.tags && m.tags.length > 0 && (
            <>
              <h4>Tags</h4>
              <div class="tags">
                {m.tags.map((t) => (
                  <span class="chip" key={t}>
                    #{t}
                  </span>
                ))}
              </div>
            </>
          )}

          <h4>Attributes</h4>
          <div class="kv">
            <span class="key">id</span>
            <span class="val">{m.id}</span>
            <span class="key">namespace</span>
            <span class="val">{m.namespace}</span>
            <span class="key">importance</span>
            <span class="val">{(m.importance ?? 0).toFixed(3)}</span>
            {m.confidence != null && (
              <>
                <span class="key">confidence</span>
                <span class="val" title="Seeded at 0.40; grows each time the fact is re-observed, decays when unused">
                  {m.confidence.toFixed(2)}
                  {m.confidence > 0.4 ? ' · corroborated' : ''}
                </span>
              </>
            )}
            <span class="key">access_count</span>
            <span class="val">{m.access_count}</span>
            <span class="key">created</span>
            <span class="val">{fmtDate(m.created_at)}</span>
            <span class="key">updated</span>
            <span class="val">{fmtDate(m.updated_at)}</span>
            <span class="key">last_seen</span>
            <span class="val">
              {fmtDate(m.last_accessed_at)} · {relTime(m.last_accessed_at)}
            </span>
            <span class="key">expires</span>
            <span class="val">{m.expires_at ? fmtDate(m.expires_at) : 'never'}</span>
            {m.superseded_by && (
              <>
                <span class="key">superseded_by</span>
                <span class="val">{m.superseded_by}</span>
              </>
            )}
            {promotedFrom(m) && (
              <>
                <span class="key">promoted_from</span>
                <span class="val">{promotedFrom(m)}</span>
              </>
            )}
            {m.valid_to && (
              <>
                <span class="key">valid</span>
                <span class="val">
                  {m.valid_from ? fmtDate(m.valid_from) : '—'} → {fmtDate(m.valid_to)}
                </span>
              </>
            )}
          </div>

          {metaEntries.length > 0 && (
            <>
              <h4>Metadata</h4>
              <pre class="json">{JSON.stringify(m.metadata, null, 2)}</pre>
            </>
          )}
        </div>
      </aside>
    </>,
    document.body,
  )
}
