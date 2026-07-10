import { signal, effect } from '@preact/signals'

export type Theme = 'ink' | 'ivory'

// Persisted connection settings. baseUrl is empty by default — the UI is served
// by the same memini binary it talks to, so relative requests hit the right
// host. Override it to point the UI at a remote memini.
interface Persisted {
  baseUrl: string
  namespace: string
  namespaceHeader: string
  apiToken: string
  theme: Theme
}

const KEY = 'memini.settings'

const defaults: Persisted = {
  baseUrl: '',
  namespace: '',
  namespaceHeader: 'X-Memini-Namespace',
  apiToken: '',
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

// The server injects MEMINI_API_KEY into the shell as <meta name="memini-token">
// (same-origin only). Read it so the UI authenticates without the operator
// pasting a token. A persisted token (set in Settings, e.g. for a remote
// baseUrl) takes precedence.
function injectedToken(): string {
  const m = document.querySelector('meta[name="memini-token"]')
  return m?.getAttribute('content') ?? ''
}

export const baseUrl = signal(initial.baseUrl)
export const namespace = signal(initial.namespace)
export const namespaceHeader = signal(initial.namespaceHeader)
// Bearer token sent as Authorization when non-empty. Required when the server
// has MEMINI_API_KEY set (bearer-gates /v1, /mcp, /metrics).
export const apiToken = signal(initial.apiToken || injectedToken())
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
    namespace: namespace.value,
    namespaceHeader: namespaceHeader.value,
    // Don't persist the server-injected token: leaving it blank lets a rotated
    // MEMINI_API_KEY take effect on reload. Only a manual override is stored.
    apiToken: apiToken.value === injectedToken() ? '' : apiToken.value,
    theme: theme.value,
  }
  try {
    localStorage.setItem(KEY, JSON.stringify(data))
  } catch {
    /* storage unavailable — settings just won't persist */
  }
  document.documentElement.dataset.theme = theme.value
})
