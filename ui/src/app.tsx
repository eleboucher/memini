import { useEffect, useState } from 'preact/hooks'
import { LocationProvider, Router, Route, useLocation } from 'preact-iso'
import { apiToken, identity, readOnlySession, serverWarning, theme } from './store'
import { verifyToken, ApiError } from './api'
import { Login } from './views/Login'
import { NamespaceSelect } from './components/NamespaceSelect'
import { Namespaces } from './views/Namespaces'
import { Dashboard } from './views/Dashboard'
import { Browser } from './views/Browser'
import { Search } from './views/Search'
import { Activity } from './views/Activity'
import { Graph } from './views/Graph'
import { Health } from './views/Health'
import { Scopes } from './views/Scopes'
import { Keys } from './views/Keys'
import { Config } from './views/Config'
import { Settings } from './views/Settings'
import {
  IconNamespaces,
  IconOverview,
  IconBrowse,
  IconSearch,
  IconActivity,
  IconGraph,
  IconHealth,
  IconScopes,
  IconKey,
  IconConfig,
  IconSettings,
  IconMoon,
  IconSun,
  LogoMark,
} from './icons'
import type { AnyComponent, JSX } from 'preact'

// The nav and the routes are one table: path → view. Overview is "/" so a bare
// load lands there (in "All namespaces" mode, ns === ''). LocationProvider
// intercepts clicks on the <a> links for client-side navigation, so the back/
// forward buttons, shareable deep links, and reloads all resolve to the right
// view (the server serves index.html on unknown paths — see internal/api/ui).
//
// Keep every path single-segment. Vite builds with `base: './'` (relative asset
// URLs), which resolves correctly only when a deep-link reload is one level deep
// (/browse → ./assets → /assets). A nested route like /memories/:id would make
// a reload fetch /memories/assets/* and 404 — switch vite base to '/' first.
// The namespaces landing page lived at /projects until the terminology
// cleanup; keep old bookmarks and deep links working by redirecting.
function LegacyProjectsRedirect() {
  const { route } = useLocation()
  useEffect(() => route('/namespaces', true), [route])
  return null
}

const NAV: {
  path: string
  label: string
  title: string
  Icon: (p: JSX.SVGAttributes<SVGSVGElement>) => JSX.Element
  component: AnyComponent
}[] = [
  { path: '/namespaces', label: 'Namespaces', title: 'Namespaces', Icon: IconNamespaces, component: Namespaces },
  { path: '/', label: 'Overview', title: 'Overview', Icon: IconOverview, component: Dashboard },
  { path: '/browse', label: 'Browse', title: 'Memory browser', Icon: IconBrowse, component: Browser },
  { path: '/search', label: 'Search', title: 'Recall', Icon: IconSearch, component: Search },
  { path: '/activity', label: 'Activity', title: 'Recent activity', Icon: IconActivity, component: Activity },
  { path: '/graph', label: 'Graph', title: 'Relationship graph', Icon: IconGraph, component: Graph },
  { path: '/scopes', label: 'Read set', title: 'Read set & links', Icon: IconScopes, component: Scopes },
  { path: '/keys', label: 'Keys', title: 'API keys', Icon: IconKey, component: Keys },
  { path: '/config', label: 'Config', title: 'Server config', Icon: IconConfig, component: Config },
  { path: '/health', label: 'Health', title: 'Health & fsck', Icon: IconHealth, component: Health },
  { path: '/settings', label: 'UI settings', title: 'UI settings', Icon: IconSettings, component: Settings },
]

export function App() {
  return (
    <LocationProvider>
      <AuthGate />
    </LocationProvider>
  )
}

// AuthGate stands in front of the Shell. On mount it verifies the persisted
// token against GET /v1/self: a 200 sets identity and reveals the Shell, a 401
// or network error drops to the Login gate (with the reason surfaced only when
// there was actually a token to reject — a clean first visit shows a bare form,
// not a scary "unauthorized"). Login is component state here, deliberately not
// a route, so the single-segment route constraint (see the NAV comment) is
// untouched. identity is a signal, so a mid-session 401 (api.ts clears it)
// re-renders this and bounces back to Login without a remount dance.
function AuthGate() {
  const [checking, setChecking] = useState(true)
  const [loginError, setLoginError] = useState<string | null>(null)

  useEffect(() => {
    let live = true
    const hadToken = apiToken.value !== ''
    verifyToken(apiToken.value)
      .then((res) => {
        if (!live) return
        identity.value = res.identity
      })
      .catch((e) => {
        if (!live) return
        identity.value = null
        // Only explain the failure when a stored token was rejected; a first
        // visit with no token is just "please sign in", not an error.
        if (hadToken) setLoginError(e instanceof ApiError ? e.message : String(e))
      })
      .finally(() => live && setChecking(false))
    return () => {
      live = false
    }
  }, [])

  if (checking) return <Splash />
  if (identity.value === null) return <Login initialError={loginError} />
  return <Shell />
}

function Splash() {
  return (
    <div class="login-screen">
      <div class="loading" role="status" aria-live="polite">
        <span class="spinner" aria-hidden="true" /> Connecting…
      </div>
    </div>
  )
}

function Shell() {
  const { path } = useLocation()
  const title = NAV.find((n) => n.path === path)?.title ?? 'memini'

  return (
    <div class="shell">
      <nav class="sidebar" aria-label="Primary">
        <div class="brand">
          <LogoMark class="brand-logo" />
          <span class="mark">memini</span>
          <span class="dot" aria-hidden="true" />
        </div>
        {NAV.map(({ path: to, label, Icon }) => (
          <a
            key={to}
            href={to}
            class={`nav-item ${path === to ? 'active' : ''}`}
            aria-current={path === to ? 'page' : undefined}
            title={label}
          >
            <Icon class="nav-icon" aria-hidden="true" />
            <span class="label">{label}</span>
          </a>
        ))}
        <div class="spacer" />
      </nav>

      <main class="main">
        <header class="topbar">
          <h1 class="title">{title}</h1>
          <span class="grow" />
          {/* Dev mode: the server has no auth configured, so every caller is an
              unauthenticated admin. A subtle, persistent chip says so — the same
              amber affordance as the serverWarning banner, but it never clears. */}
          {identity.value && !identity.value.authenticated && (
            <span
              class="chip warn"
              title="No auth configured on the server — anyone who can reach it has full access. Create an admin API key to lock it down."
            >
              no auth
            </span>
          )}
          {/* The signed-in key cannot write. Persistent for the same reason the
              dev-mode chip is: it is a standing property of the session, not an
              event, and it is the one piece of context that explains why every
              write control on every view is disabled. */}
          {readOnlySession.value && (
            <span
              class="chip limited"
              title="This API key is read-only — it can read everything it could before, but the server refuses every write. Sign in with a read-write key to make changes."
            >
              read-only
            </span>
          )}
          <NamespaceSelect />
          <button
            class="icon-btn"
            aria-label={theme.value === 'ink' ? 'Switch to light theme' : 'Switch to dark theme'}
            onClick={() => (theme.value = theme.value === 'ink' ? 'ivory' : 'ink')}
          >
            {theme.value === 'ink' ? <IconSun aria-hidden="true" /> : <IconMoon aria-hidden="true" />}
          </button>
        </header>
        <div class="content">
          {/* A non-fatal advisory from the server — e.g. the home namespace it
              overrode because this API key is bound to a home of its own. The
              request succeeded, just not quite as asked, and the operator would
              otherwise be left wondering why they are seeing a different
              namespace's memories than they requested. */}
          {serverWarning.value && (
            <div class="banner warn" role="status">
              {serverWarning.value}
            </div>
          )}
          <Router>
            {NAV.map(({ path: to, component }) => (
              <Route key={to} path={to} component={component} />
            ))}
            <Route path="/projects" component={LegacyProjectsRedirect} />
            <Route default component={Dashboard} />
          </Router>
        </div>
      </main>
    </div>
  )
}
