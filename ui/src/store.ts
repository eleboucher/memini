import { signal, effect } from '@preact/signals'

export type Route = 'projects' | 'overview' | 'browse' | 'search' | 'graph' | 'health' | 'settings'
export type Theme = 'ink' | 'ivory'

// Persisted connection settings. baseUrl is empty by default — the UI is served
// by the same memini binary it talks to, so relative requests hit the right
// host. Override it to point the UI at a remote memini.
interface Persisted {
  baseUrl: string
  token: string
  namespace: string
  namespaceHeader: string
  theme: Theme
}

const KEY = 'memini.settings'

const defaults: Persisted = {
  baseUrl: '',
  token: '',
  namespace: '',
  namespaceHeader: 'X-Memini-Namespace',
  theme: 'ink',
}

function load(): Persisted {
  try {
    const raw = localStorage.getItem(KEY)
    return raw ? { ...defaults, ...JSON.parse(raw) } : { ...defaults }
  } catch {
    return { ...defaults }
  }
}

const initial = load()

// Routing is a single signal — no History API, so no deep links / back-button
// / shareable URLs, and a reload always lands on the default view. Deliberate
// simplicity for a single-operator admin panel; switch to hash routing if that
// changes. Default landing is the Overview in "All projects" mode (ns === '').
export const route = signal<Route>('overview')
export const baseUrl = signal(initial.baseUrl)
export const token = signal(initial.token)
export const namespace = signal(initial.namespace)
export const namespaceHeader = signal(initial.namespaceHeader)
export const theme = signal<Theme>(initial.theme)

// A monotonically increasing nonce views watch to force a refetch.
export const refreshNonce = signal(0)
export function refresh() {
  refreshNonce.value++
}

// Persist settings and reflect the theme on <html> whenever they change.
effect(() => {
  const data: Persisted = {
    baseUrl: baseUrl.value,
    token: token.value,
    namespace: namespace.value,
    namespaceHeader: namespaceHeader.value,
    theme: theme.value,
  }
  try {
    localStorage.setItem(KEY, JSON.stringify(data))
  } catch {
    /* storage unavailable — settings just won't persist */
  }
  document.documentElement.dataset.theme = theme.value
})
