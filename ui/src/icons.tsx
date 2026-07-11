/** Minimal stroked icon set (Lucide-style geometry), sized via CSS. */
import type { JSX } from 'preact'

type P = JSX.SVGAttributes<SVGSVGElement>

const base = (children: JSX.Element | JSX.Element[]) => (props: P) => (
  <svg
    viewBox="0 0 24 24"
    fill="none"
    stroke="currentColor"
    stroke-width="1.8"
    stroke-linecap="round"
    stroke-linejoin="round"
    aria-hidden="true"
    focusable="false"
    {...props}
  >
    {children}
  </svg>
)

export const IconOverview = base([
  <path d="M12 3 2 8l10 5 10-5-10-5Z" />,
  <path d="m2 13 10 5 10-5" />,
  <path d="m2 18 10 5 10-5" opacity="0.5" />,
])
export const IconBrowse = base([
  <path d="M4 6h16M4 12h16M4 18h10" />,
])
export const IconSearch = base([<circle cx="11" cy="11" r="7" />, <path d="m21 21-4.3-4.3" />])
export const IconGraph = base([
  <circle cx="6" cy="7" r="2.5" />,
  <circle cx="18" cy="6" r="2.5" />,
  <circle cx="13" cy="17" r="2.5" />,
  <path d="m8.2 8.4 3 6.5M15.8 7.6 14 14.7M8.4 6.6 15.5 6" opacity="0.7" />,
])
export const IconHealth = base([<path d="M3 12h4l2 6 4-14 2 8h6" />])
export const IconSettings = base([
  <path d="M4 6h10M18 6h2M4 12h2M10 12h10M4 18h7M15 18h5" />,
  <circle cx="16" cy="6" r="2" />,
  <circle cx="8" cy="12" r="2" />,
  <circle cx="13" cy="18" r="2" />,
])
export const IconProjects = base([
  <rect x="3" y="3" width="7" height="7" rx="1.5" />,
  <rect x="14" y="3" width="7" height="7" rx="1.5" />,
  <rect x="3" y="14" width="7" height="7" rx="1.5" />,
  <rect x="14" y="14" width="7" height="7" rx="1.5" />,
])
export const IconChevron = base([<path d="m6 9 6 6 6-6" />])
export const IconScopes = base([
  <circle cx="9.5" cy="12" r="6.5" />,
  <circle cx="15.5" cy="12" r="6.5" opacity="0.6" />,
])
export const IconClose = base([<path d="M18 6 6 18M6 6l12 12" />])
export const IconTrash = base([
  <path d="M3 6h18M8 6V4h8v2M6 6l1 14h10l1-14" />,
])
export const IconCopy = base([
  <rect x="9" y="9" width="12" height="12" rx="2" />,
  <path d="M5 15V5a2 2 0 0 1 2-2h10" />,
])
export const IconRefresh = base([
  <path d="M3 12a9 9 0 0 1 15-6.7L21 8M21 3v5h-5" />,
  <path d="M21 12a9 9 0 0 1-15 6.7L3 16M3 21v-5h5" />,
])
export const IconCheck = base([<path d="m20 6-11 11-5-5" />])
export const IconMoon = base([<path d="M21 12.8A9 9 0 1 1 11.2 3a7 7 0 0 0 9.8 9.8Z" />])
export const IconSun = base([
  <circle cx="12" cy="12" r="4" />,
  <path d="M12 2v2M12 20v2M2 12h2M20 12h2M5 5l1.5 1.5M17.5 17.5 19 19M19 5l-1.5 1.5M6.5 17.5 5 19" />,
])

/** Brand mark. Filled (not stroked); colours pull from the theme's logo and tier vars. */
export const LogoMark = (props: P) => (
  <svg viewBox="0 0 48 48" aria-hidden="true" focusable="false" {...props}>
    <rect x="0.5" y="0.5" width="47" height="47" rx="13" fill="var(--logo-bg)" stroke="var(--line)" />
    <circle cx="24" cy="12" r="5" fill="var(--logo-dot)" />
    <rect x="12" y="22" width="24" height="4.6" rx="2.3" fill="var(--tier-working)" />
    <rect x="12" y="29.4" width="24" height="4.6" rx="2.3" fill="var(--tier-episodic)" />
    <rect x="13.5" y="36.8" width="21" height="4.6" rx="2.3" fill="var(--tier-semantic)" />
  </svg>
)
