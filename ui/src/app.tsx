import { LocationProvider, Router, Route, useLocation } from 'preact-iso'
import { theme } from './store'
import { NamespaceSelect } from './components/NamespaceSelect'
import { Projects } from './views/Projects'
import { Dashboard } from './views/Dashboard'
import { Browser } from './views/Browser'
import { Search } from './views/Search'
import { Graph } from './views/Graph'
import { Health } from './views/Health'
import { Scopes } from './views/Scopes'
import { Keys } from './views/Keys'
import { Settings } from './views/Settings'
import {
  IconProjects,
  IconOverview,
  IconBrowse,
  IconSearch,
  IconGraph,
  IconHealth,
  IconScopes,
  IconKey,
  IconSettings,
  IconMoon,
  IconSun,
  LogoMark,
} from './icons'
import type { AnyComponent, JSX } from 'preact'

// The nav and the routes are one table: path → view. Overview is "/" so a bare
// load lands there (in "All projects" mode, ns === ''). LocationProvider
// intercepts clicks on the <a> links for client-side navigation, so the back/
// forward buttons, shareable deep links, and reloads all resolve to the right
// view (the server serves index.html on unknown paths — see internal/api/ui).
//
// Keep every path single-segment. Vite builds with `base: './'` (relative asset
// URLs), which resolves correctly only when a deep-link reload is one level deep
// (/browse → ./assets → /assets). A nested route like /memories/:id would make
// a reload fetch /memories/assets/* and 404 — switch vite base to '/' first.
const NAV: {
  path: string
  label: string
  title: string
  Icon: (p: JSX.SVGAttributes<SVGSVGElement>) => JSX.Element
  component: AnyComponent
}[] = [
  { path: '/projects', label: 'Projects', title: 'Projects', Icon: IconProjects, component: Projects },
  { path: '/', label: 'Overview', title: 'Overview', Icon: IconOverview, component: Dashboard },
  { path: '/browse', label: 'Browse', title: 'Memory browser', Icon: IconBrowse, component: Browser },
  { path: '/search', label: 'Search', title: 'Recall', Icon: IconSearch, component: Search },
  { path: '/graph', label: 'Graph', title: 'Relationship graph', Icon: IconGraph, component: Graph },
  { path: '/scopes', label: 'Scopes', title: 'Namespace scopes & links', Icon: IconScopes, component: Scopes },
  { path: '/keys', label: 'Keys', title: 'API keys', Icon: IconKey, component: Keys },
  { path: '/health', label: 'Health', title: 'Health & fsck', Icon: IconHealth, component: Health },
  { path: '/settings', label: 'Settings', title: 'Settings', Icon: IconSettings, component: Settings },
]

export function App() {
  return (
    <LocationProvider>
      <Shell />
    </LocationProvider>
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
          <Router>
            {NAV.map(({ path: to, component }) => (
              <Route key={to} path={to} component={component} />
            ))}
            <Route default component={Dashboard} />
          </Router>
        </div>
      </main>
    </div>
  )
}
