import { useEffect, useRef, useState } from 'preact/hooks'
import { createPortal } from 'preact/compat'
import { api } from '../api'
import { namespace, refresh } from '../store'
import type { RenamespaceReport } from '../types'
import { IconClose, IconRefresh } from '../icons'

interface Props {
  name: string
  onClose: () => void
  // Pre-fills the Move target (set when the drawer is opened by dropping a pod
  // onto a namespace box), so the drag lands on a dry-run-first confirmation
  // rather than a silent bulk move.
  initialMoveTo?: string
}

// NamespaceDrawer manages one namespace's memories: relocate the whole
// namespace (move) or regroup it by metadata (split). Both preview with a
// dry-run first, then apply; the server backs both with Store.Reassign.
export function NamespaceDrawer({ name, onClose, initialMoveTo }: Props) {
  const drawerRef = useRef<HTMLElement>(null)
  const closeRef = useRef<HTMLButtonElement>(null)

  useEffect(() => {
    const prev = document.activeElement as HTMLElement | null
    closeRef.current?.focus({ preventScroll: true })
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose()
    }
    document.addEventListener('keydown', onKey)
    return () => {
      document.removeEventListener('keydown', onKey)
      prev?.focus?.()
    }
  }, [onClose])

  return createPortal(
    <>
      <div class="scrim" onClick={onClose} />
      <aside class="drawer" role="dialog" aria-modal="true" aria-label={`Manage ${name}`} ref={drawerRef}>
        <div class="drawer-head">
          <span class="chip">{name}</span>
          <span class="grow" />
          <button class="icon-btn" aria-label="Close" onClick={onClose} ref={closeRef}>
            <IconClose />
          </button>
        </div>
        <div class="drawer-body">
          <MoveSection name={name} onDone={onClose} initialTo={initialMoveTo} />
          <SplitSection name={name} onDone={onClose} />
        </div>
      </aside>
    </>,
    document.body,
  )
}

// MoveSection relocates every memory in `name` to a target namespace.
function MoveSection({ name, onDone, initialTo }: { name: string; onDone: () => void; initialTo?: string }) {
  const [to, setTo] = useState(initialTo ?? '')
  const [report, setReport] = useState<RenamespaceReport | null>(null)
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState<string | null>(null)

  const run = async (dryRun: boolean) => {
    setBusy(true)
    setErr(null)
    try {
      const r = await api.moveNamespace(name, to.trim(), dryRun)
      setReport(r)
      if (!dryRun) {
        // The source namespace no longer exists; drop the active selection if it
        // was pointed at it, then refresh the namespace list.
        if (namespace.value === name) namespace.value = ''
        refresh()
        onDone()
      }
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy(false)
    }
  }

  const canRun = to.trim().length > 0 && !busy

  return (
    <section>
      <h4>Move</h4>
      <div class="hint" style={{ marginBottom: '10px' }}>
        Relocate every memory in "{name}" to another namespace. If the target
        already exists, the two merge (no content dedup — run Health → dedup
        after to collapse duplicates).
      </div>
      <input
        class="input"
        type="text"
        placeholder="target namespace, e.g. work/archive"
        value={to}
        onInput={(e) => setTo((e.target as HTMLInputElement).value)}
        style={{ width: '100%', marginBottom: '10px' }}
      />
      <div style={{ display: 'flex', gap: '8px' }}>
        <button class="btn" onClick={() => run(true)} disabled={!canRun}>
          {busy ? <span class="spinner" style={{ width: '14px', height: '14px' }} /> : <IconRefresh />}
          Preview
        </button>
        <button class="btn primary" onClick={() => run(false)} disabled={!canRun}>
          Move
        </button>
      </div>
      {err && <div class="banner err" role="alert" style={{ marginTop: '10px' }}>{err}</div>}
      {report && <ReportView report={report} />}
    </section>
  )
}

// SplitSection regroups a namespace by metadata keys, moving each record to the
// namespace its first matching key names.
function SplitSection({ name, onDone }: { name: string; onDone: () => void }) {
  const [keys, setKeys] = useState('')
  const [report, setReport] = useState<RenamespaceReport | null>(null)
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState<string | null>(null)

  const run = async (dryRun: boolean) => {
    setBusy(true)
    setErr(null)
    try {
      const by = keys
        .split(',')
        .map((k) => k.trim())
        .filter(Boolean)
      const r = await api.splitNamespace(name, by, dryRun)
      setReport(r)
      if (!dryRun) {
        refresh()
        onDone()
      }
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy(false)
    }
  }

  return (
    <section style={{ marginTop: '28px' }}>
      <h4>Split</h4>
      <div class="hint" style={{ marginBottom: '10px' }}>
        Regroup "{name}" by metadata keys. Blank uses the defaults
        (import_source_namespace, user_id, agent_id, run_id, project).
      </div>
      <input
        class="input"
        type="text"
        placeholder="metadata keys, comma-separated (optional)"
        value={keys}
        onInput={(e) => setKeys((e.target as HTMLInputElement).value)}
        style={{ width: '100%', marginBottom: '10px' }}
      />
      <div style={{ display: 'flex', gap: '8px' }}>
        <button class="btn" onClick={() => run(true)} disabled={busy}>
          {busy ? <span class="spinner" style={{ width: '14px', height: '14px' }} /> : <IconRefresh />}
          Preview
        </button>
        <button class="btn primary" onClick={() => run(false)} disabled={busy}>
          Split
        </button>
      </div>
      {err && <div class="banner err" role="alert" style={{ marginTop: '10px' }}>{err}</div>}
      {report && <ReportView report={report} />}
    </section>
  )
}

// ReportView renders a move/split RenamespaceReport: the moved/skipped counts
// and the per-destination breakdown, flagged as a dry run when applicable.
function ReportView({ report }: { report: RenamespaceReport }) {
  const targets = Object.entries(report.targets ?? {})
  return (
    <div class="banner" role="status" style={{ marginTop: '10px' }}>
      <div>
        {report.dry_run ? 'Would move' : 'Moved'} <strong>{report.moved ?? 0}</strong>
        {report.skipped ? ` · ${report.skipped} left in place` : ''}
        {report.dry_run ? ' (dry run — nothing written)' : ''}
      </div>
      {targets.length > 0 && (
        <div class="kv" style={{ marginTop: '8px', gridTemplateColumns: '1fr auto' }}>
          {targets.map(([ns, n]) => (
            <>
              <span class="key" key={`${ns}-k`}>{ns}</span>
              <span class="val mono" key={`${ns}-v`}>{n}</span>
            </>
          ))}
        </div>
      )}
    </div>
  )
}
