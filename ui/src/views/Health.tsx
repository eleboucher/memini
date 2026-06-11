import { useState } from 'preact/hooks'
import { api } from '../api'
import { namespace, refresh } from '../store'
import type { DedupReport, FsckReport } from '../types'
import { Empty, ErrorBanner } from '../components/States'
import { IconRefresh, IconCheck } from '../icons'

// Health runs the server-side fsck consistency sweep: purge expired, enforce
// the short-term cap, and audit for duplicate clusters. It mutates state, so
// it's gated behind an explicit button rather than auto-running.
export function Health() {
  const [report, setReport] = useState<FsckReport | null>(null)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const run = async () => {
    setLoading(true)
    setError(null)
    try {
      const r = await api.fsck()
      setReport(r)
      refresh()
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setLoading(false)
    }
  }

  const dupes = report?.duplicate_groups ?? []

  return (
    <div class="view">
      <div class="panel panel-pad" style={{ marginBottom: '20px' }}>
        <div class="section-h">
          <h2>fsck</h2>
          <span class="hint">purges expired, enforces the short-term cap, groups duplicates — mutates the store</span>
        </div>
        <button class="btn primary" onClick={run} disabled={loading}>
          {loading ? <span class="spinner" style={{ width: '14px', height: '14px' }} /> : <IconRefresh />}
          Run fsck
        </button>
      </div>

      {error && <ErrorBanner message={error} />}

      {report && (
        <>
          <div class="stat-grid" style={{ gridTemplateColumns: 'repeat(3,1fr)' }}>
            <div class="panel stat">
              <div class="k">Expired purged</div>
              <div class="v">{report.expired_purged}</div>
            </div>
            <div class="panel stat">
              <div class="k">Short-term evicted</div>
              <div class="v">{report.short_term_evicted}</div>
            </div>
            <div class="panel stat">
              <div class="k">Namespaces swept</div>
              <div class="v">{report.namespaces}</div>
            </div>
          </div>

          <div class="panel panel-pad">
            <div class="section-h">
              <h2>Duplicate clusters</h2>
              <span class="hint mono">{dupes.length} groups</span>
            </div>
            {dupes.length === 0 ? (
              <div class="empty" style={{ padding: '34px' }}>
                <div class="big" style={{ display: 'flex', gap: '8px', justifyContent: 'center', alignItems: 'center' }}>
                  <span style={{ color: 'var(--ok)', width: '22px', height: '22px' }}>
                    <IconCheck />
                  </span>
                  None
                </div>
              </div>
            ) : (
              <div class="mem-list">
                {dupes.map((ids, i) => (
                  <div class="panel panel-pad" key={i}>
                    <div class="mem-meta" style={{ marginTop: 0 }}>
                      <span class="chip">group {i + 1}</span>
                      <span>{ids.length} near-duplicates</span>
                    </div>
                    <div class="kv" style={{ marginTop: '10px', gridTemplateColumns: '1fr' }}>
                      {ids.map((id) => (
                        <span class="val mono" key={id}>
                          {id}
                        </span>
                      ))}
                    </div>
                  </div>
                ))}
              </div>
            )}
          </div>
        </>
      )}

      {!report && !error && <Empty title="Not yet run" />}

      <Dedup />
    </div>
  )
}

// Dedup collapses near-duplicate memories into one representative per cluster,
// tombstoning the rest (reversibly). It scopes to the active project, or runs
// store-wide in "All projects" mode. Dry-run previews clusters without writing.
function Dedup() {
  const [report, setReport] = useState<DedupReport | null>(null)
  const [similarity, setSimilarity] = useState(0.85)
  const [dryRun, setDryRun] = useState(true)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const scope = namespace.value ? `project "${namespace.value}"` : 'all projects'

  const run = async () => {
    setLoading(true)
    setError(null)
    try {
      const r = await api.dedup({ similarity, dryRun })
      setReport(r)
      if (!dryRun && r.tombstoned > 0) refresh()
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setLoading(false)
    }
  }

  const actions = report?.actions ?? []

  return (
    <>
      <div class="panel panel-pad" style={{ margin: '20px 0' }}>
        <div class="section-h">
          <h2>dedup</h2>
          <span class="hint">
            collapses near-duplicate memories in {scope} — tombstones are reversible
          </span>
        </div>
        <div style={{ display: 'flex', gap: '16px', alignItems: 'center', flexWrap: 'wrap' }}>
          <label class="hint" style={{ display: 'flex', gap: '6px', alignItems: 'center' }}>
            similarity
            <input
              type="number"
              min={0}
              max={1}
              step={0.05}
              value={similarity}
              onInput={(e) => setSimilarity(Number((e.target as HTMLInputElement).value))}
              style={{ width: '72px' }}
            />
          </label>
          <label class="hint" style={{ display: 'flex', gap: '6px', alignItems: 'center' }}>
            <input type="checkbox" checked={dryRun} onChange={(e) => setDryRun((e.target as HTMLInputElement).checked)} />
            dry run
          </label>
          <button class="btn primary" onClick={run} disabled={loading}>
            {loading ? <span class="spinner" style={{ width: '14px', height: '14px' }} /> : <IconRefresh />}
            Run dedup
          </button>
        </div>
      </div>

      {error && <ErrorBanner message={error} />}

      {report && (
        <>
          <div class="stat-grid" style={{ gridTemplateColumns: 'repeat(3,1fr)' }}>
            <div class="panel stat">
              <div class="k">Namespaces</div>
              <div class="v">{report.namespaces}</div>
            </div>
            <div class="panel stat">
              <div class="k">Clusters found</div>
              <div class="v">{report.clusters_found}</div>
            </div>
            <div class="panel stat">
              <div class="k">{report.dry_run ? 'Would tombstone' : 'Tombstoned'}</div>
              <div class="v">{report.tombstoned}</div>
            </div>
          </div>

          <div class="panel panel-pad">
            <div class="section-h">
              <h2>Clusters</h2>
              <span class="hint mono">
                {actions.length} {report.dry_run ? '(dry run — nothing written)' : ''}
              </span>
            </div>
            {actions.length === 0 ? (
              <div class="empty" style={{ padding: '34px' }}>
                <div class="big" style={{ display: 'flex', gap: '8px', justifyContent: 'center', alignItems: 'center' }}>
                  <span style={{ color: 'var(--ok)', width: '22px', height: '22px' }}>
                    <IconCheck />
                  </span>
                  None
                </div>
              </div>
            ) : (
              <div class="mem-list">
                {actions.map((a) => (
                  <div class="panel panel-pad" key={a.representative_id}>
                    <div class="mem-meta" style={{ marginTop: 0 }}>
                      <span class="chip">keep</span>
                      <span class="val mono">{a.representative_id}</span>
                      <span>· {a.tombstoned_ids.length} tombstoned</span>
                    </div>
                    <div class="kv" style={{ marginTop: '10px', gridTemplateColumns: '1fr' }}>
                      {a.tombstoned_ids.map((id) => (
                        <span class="val mono" key={id} style={{ opacity: 0.6 }}>
                          {id}
                        </span>
                      ))}
                    </div>
                  </div>
                ))}
              </div>
            )}
          </div>
        </>
      )}
    </>
  )
}
