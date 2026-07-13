#!/usr/bin/env node
// Extracts api/openapi.yaml's ClientSettings schema into a small, ordered
// catalog — [{ key, type, default, enum?, min?, description }] — that the
// Settings UI renders as its row list (label + description + provenance),
// so every server-known field is visible even before the UI has bespoke
// copy for it, and help text can't drift from the spec.
//
// Parses YAML with js-yaml rather than adding a direct dependency: it is
// already pulled in transitively by openapi-typescript's own dependency
// tree (@redocly/openapi-core -> js-yaml) and npm hoists it to top-level
// node_modules, so it's resolvable here without a package.json change. If
// that ever stops being true (a dep-tree change drops js-yaml), this
// import fails loudly at `npm run gen-api` time rather than silently
// emitting a stale/wrong catalog — acceptable for a devDependency-only
// build step. Re-vendor a minimal extractor here if that ever happens.
import { readFileSync, writeFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { dirname, join } from 'node:path'
import { load } from 'js-yaml'

const here = dirname(fileURLToPath(import.meta.url))
const specPath = join(here, '../../api/openapi.yaml')
const outPath = join(here, '../src/settings-catalog.gen.ts')

const spec = load(readFileSync(specPath, 'utf8'))
const props = spec?.components?.schemas?.ClientSettings?.properties
if (!props || typeof props !== 'object') {
  console.error(
    'gen-catalog: components.schemas.ClientSettings.properties not found in api/openapi.yaml — spec shape changed, update this script',
  )
  process.exit(1)
}

function cleanDescription(d) {
  if (typeof d !== 'string') return ''
  // YAML folded/literal blocks can leave embedded newlines; collapse to one line.
  return d.replace(/\s+/g, ' ').trim()
}

const entries = Object.entries(props).map(([key, def]) => {
  /** @type {{key: string, type: string, default: unknown, enum?: string[], min?: number, description: string}} */
  const entry = {
    key,
    type: def.type,
    default: def.default,
    description: cleanDescription(def.description),
  }
  // Enum values live on the field itself for scalar strings (namespace_scope)
  // or on `items` for an array-of-enum field (inject_labels).
  if (def.type === 'array' && Array.isArray(def.items?.enum)) {
    entry.enum = def.items.enum
  } else if (Array.isArray(def.enum)) {
    entry.enum = def.enum
  }
  if (typeof def.minimum === 'number') {
    entry.min = def.minimum
  }
  return entry
})

const header = `// GENERATED FILE — do not hand-edit.
// Produced by \`npm run gen-api\` (ui/scripts/gen-catalog.mjs) from
// api/openapi.yaml's ClientSettings schema, in spec order. Carries the
// field-level help text, built-in defaults, enums, and minimums that the
// openapi-typescript output (api-schema.gen.ts) doesn't preserve — the
// Config view's Settings tab renders this catalog as its row list so every
// server-known ClientSettings field is visible, described, and labeled with
// its built-in default even before the UI has bespoke copy for it.

export interface SettingsCatalogEntry {
  key: string
  type: 'boolean' | 'integer' | 'number' | 'string' | 'array'
  default: unknown
  enum?: string[]
  min?: number
  description: string
}

export const SETTINGS_CATALOG: SettingsCatalogEntry[] = ${JSON.stringify(entries, null, 2)}
`

writeFileSync(outPath, header)
console.log(`gen-catalog: wrote ${entries.length} ClientSettings fields to ${outPath}`)
