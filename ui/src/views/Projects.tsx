import { api } from '../api'
import { namespace, route, refreshNonce } from '../store'
import { useAsync } from '../hooks'
import { TIERS, type Stats } from '../types'
import { tierColor, relTime, num } from '../util'
import { Loading, ErrorBanner, Empty } from '../components/States'

// Projects is the landing view: every namespace holding memories, with a count
// and tier mini-bar, so you see the whole picture before drilling into one.
export function Projects() {
  const { data, error, loading } = useAsync(async () => {
    const names = await api.namespaces()
    const stats = await Promise.all(names.map((n) => api.statsFor(n).catch(() => null)))
    return names.map((name, i) => ({ name, stats: stats[i] }))
  }, [refreshNonce.value])

  const open = (ns: string) => {
    namespace.value = ns
    route.value = 'overview'
  }

  if (loading && !data) return <div class="view"><Loading /></div>
  if (error) return <div class="view"><ErrorBanner message={error} /></div>

  const projects = data ?? []

  return (
    <div class="view">
      {projects.length === 0 ? (
        <Empty title="No projects" hint="Namespaces appear here once they hold memories." />
      ) : (
        <div class="project-grid stagger">
          {projects.map((p) => (
            <button class="panel project-card" key={p.name} onClick={() => open(p.name)}>
              <div class="project-name">{p.name}</div>
              <div class="project-count">
                <span class="v">{num(p.stats?.total ?? 0)}</span> memories
              </div>
              <TierBar stats={p.stats} />
              <div class="project-foot">
                {p.stats?.last_write_at ? `updated ${relTime(p.stats.last_write_at)}` : '—'}
              </div>
            </button>
          ))}
        </div>
      )}
    </div>
  )
}

function TierBar({ stats }: { stats: Stats | null }) {
  const total = stats?.total ?? 0
  if (total === 0) return <div class="project-bar empty-bar" />
  return (
    <div class="project-bar">
      {TIERS.map((t) => {
        const n = stats?.by_tier[t] ?? 0
        if (n === 0) return null
        return <span key={t} style={{ flexGrow: n, background: tierColor(t) }} title={`${t}: ${n}`} />
      })}
    </div>
  )
}
