import { api } from '../api'
import { namespace, refreshNonce } from '../store'
import { useAsync } from '../hooks'
import { TIERS, type Tier } from '../types'
import { tierColor, memoryTypeColor, relTime, num, MEMORY_TYPES } from '../util'
import { Loading, ErrorBanner } from '../components/States'

export function Dashboard() {
  const { data: stats, error, loading } = useAsync(
    () => api.stats(),
    [namespace.value, refreshNonce.value],
  )

  if (loading && !stats) return <Loading />
  if (error) return <ErrorBanner message={error} />
  if (!stats) return null

  const byTier = (t: Tier) => stats.by_tier[t] ?? 0
  const max = Math.max(1, stats.total)
  const byType = stats.by_memory_type ?? {}
  const typedTotal = MEMORY_TYPES.reduce((sum, t) => sum + (byType[t] ?? 0), 0)

  return (
    <div class="view stagger">
      <div class="stat-grid">
        <div class="panel stat">
          <div class="k">Memories</div>
          <div class="v">{num(stats.total)}</div>
        </div>
        <div class="panel stat">
          <div class="k">Recalls</div>
          <div class="v">{num(stats.total_accesses)}</div>
        </div>
        <div class="panel stat">
          <div class="k">Avg importance</div>
          <div class="v">
            <span class="accent">{(stats.avg_importance ?? 0).toFixed(2)}</span>
          </div>
        </div>
        <div class="panel stat">
          <div class="k">Last write</div>
          <div class="v" style={{ fontSize: '22px', paddingTop: '8px' }}>
            {relTime(stats.last_write_at ?? undefined)}
          </div>
          <div class="sub">
            {stats.expired} expired · {stats.superseded} superseded
          </div>
        </div>
      </div>

      <div class="panel panel-pad strata">
        <div class="section-h">
          <h2>Tiers</h2>
        </div>
        {stats.total === 0 ? (
          <div class="empty">
            <div class="big">No memories</div>
          </div>
        ) : (
          <>
            <div class="strata-bar">
              {TIERS.map((t) => {
                const n = byTier(t)
                if (n === 0) return null
                return (
                  <div
                    key={t}
                    class="strata-seg"
                    style={{ flexGrow: n, background: tierColor(t) }}
                    title={`${t}: ${n}`}
                  >
                    {n / max > 0.12 && <span class="seg-label">{t}</span>}
                  </div>
                )
              })}
            </div>
            <div class="strata-legend">
              {TIERS.map((t) => (
                <div class="item" key={t}>
                  <span class="sw" style={{ background: tierColor(t) }} />
                  {t} <span class="n">{num(byTier(t))}</span>
                </div>
              ))}
            </div>
          </>
        )}
      </div>

      {typedTotal > 0 && (
        <div class="panel panel-pad strata">
          <div class="section-h">
            <h2>Typed extractions</h2>
          </div>
          <div class="strata-bar">
            {MEMORY_TYPES.map((t) => {
              const n = byType[t] ?? 0
              if (n === 0) return null
              return (
                <div
                  key={t}
                  class="strata-seg"
                  style={{ flexGrow: n, background: memoryTypeColor(t) }}
                  title={`${t}: ${n}`}
                >
                  {n / typedTotal > 0.12 && <span class="seg-label">{t}</span>}
                </div>
              )
            })}
          </div>
          <div class="strata-legend">
            {MEMORY_TYPES.map((t) => (
              <div class="item" key={t}>
                <span class="sw" style={{ background: memoryTypeColor(t) }} />
                {t} <span class="n">{num(byType[t] ?? 0)}</span>
              </div>
            ))}
          </div>
        </div>
      )}
    </div>
  )
}
