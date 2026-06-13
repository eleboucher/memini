import { route, theme, type Route } from './store'
import { NamespaceSelect } from './components/NamespaceSelect'
import { Projects } from './views/Projects'
import { Dashboard } from './views/Dashboard'
import { Browser } from './views/Browser'
import { Search } from './views/Search'
import { Graph } from './views/Graph'
import { Health } from './views/Health'
import { Settings } from './views/Settings'
import {
  IconProjects,
  IconOverview,
  IconBrowse,
  IconSearch,
  IconGraph,
  IconHealth,
  IconSettings,
  IconMoon,
  IconSun,
  LogoMark,
} from './icons'
import type { JSX } from 'preact'

const NAV: { id: Route; label: string; Icon: (p: JSX.SVGAttributes<SVGSVGElement>) => JSX.Element }[] = [
  { id: 'projects', label: 'Projects', Icon: IconProjects },
  { id: 'overview', label: 'Overview', Icon: IconOverview },
  { id: 'browse', label: 'Browse', Icon: IconBrowse },
  { id: 'search', label: 'Search', Icon: IconSearch },
  { id: 'graph', label: 'Graph', Icon: IconGraph },
  { id: 'health', label: 'Health', Icon: IconHealth },
  { id: 'settings', label: 'Settings', Icon: IconSettings },
]

const TITLES: Record<Route, string> = {
  projects: 'Projects',
  overview: 'Overview',
  browse: 'Memory browser',
  search: 'Recall',
  graph: 'Relationship graph',
  health: 'Health & fsck',
  settings: 'Settings',
}

function View() {
  switch (route.value) {
    case 'projects':
      return <Projects />
    case 'overview':
      return <Dashboard />
    case 'browse':
      return <Browser />
    case 'search':
      return <Search />
    case 'graph':
      return <Graph />
    case 'health':
      return <Health />
    case 'settings':
      return <Settings />
  }
}

export function App() {
  return (
    <div class="shell">
      <nav class="sidebar" aria-label="Primary">
        <div class="brand">
          <LogoMark class="brand-logo" />
          <span class="mark">memini</span>
          <span class="dot" aria-hidden="true" />
        </div>
        {NAV.map(({ id, label, Icon }) => (
          <button
            key={id}
            type="button"
            class={`nav-item ${route.value === id ? 'active' : ''}`}
            aria-current={route.value === id ? 'page' : undefined}
            title={label}
            onClick={() => (route.value = id)}
          >
            <Icon class="nav-icon" aria-hidden="true" />
            <span class="label">{label}</span>
          </button>
        ))}
        <div class="spacer" />
      </nav>

      <main class="main">
        <header class="topbar">
          <h1 class="title">{TITLES[route.value]}</h1>
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
          <View />
        </div>
      </main>
    </div>
  )
}
