import { signal, effect } from '@preact/signals'
import type { CallerIdentity } from './types'

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

export const baseUrl = signal(initial.baseUrl)
export const namespace = signal(initial.namespace)
export const namespaceHeader = signal(initial.namespaceHeader)
// Bearer token sent as Authorization when non-empty. The operator pastes it
// once at the Login gate (verified against GET /v1/self before it is adopted)
// and it persists here unconditionally. Required when the server has
// MEMINI_API_KEY set (bearer-gates /v1, /mcp, /metrics).
export const apiToken = signal(initial.apiToken)
export const theme = signal<Theme>(initial.theme)

// Who the current apiToken authenticates as, per GET /v1/self. null means
// "unverified" — the AuthGate shows Login until it resolves (or after a
// mid-session 401 in api.ts clears it). authenticated=false is dev mode (no
// auth configured); admin drives the admin-gated views' locked states.
export const identity = signal<CallerIdentity | null>(null)

// Set true when a request 401s WHILE a session was live (api.ts, only when it
// clears a previously non-null identity) — the key this browser was signed in
// with got revoked or rotated out from under it. The Login gate reads it to
// explain the bounce ("your session ended") instead of showing a bare form;
// cleared on the next successful sign-in. Not persisted: a full reload starts
// fresh and, if the stored token is now dead, surfaces that as a startup error
// instead.
export const sessionEnded = signal(false)

// A monotonically increasing nonce views watch to force a refetch.
export const refreshNonce = signal(0)
export function refresh() {
  refreshNonce.value++
}

// The server's last non-fatal advisory (the X-Memini-Warning response header) —
// e.g. a home namespace header it overrode because the API key is bound to a
// home of its own. Set on every response, so it clears itself once the
// condition goes away. The shell renders it as a banner: the request succeeded,
// but not quite as asked, and that is worth seeing.
export const serverWarning = signal('')

// Persist settings and reflect the theme on <html> whenever they change.
effect(() => {
  const data: Persisted = {
    baseUrl: baseUrl.value,
    namespace: namespace.value,
    namespaceHeader: namespaceHeader.value,
    apiToken: apiToken.value,
    theme: theme.value,
  }
  try {
    localStorage.setItem(KEY, JSON.stringify(data))
  } catch {
    /* storage unavailable — settings just won't persist */
  }
  document.documentElement.dataset.theme = theme.value
})
