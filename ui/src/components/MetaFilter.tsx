import { useState } from 'preact/hooks'

interface Props {
  onChange: (tags: string[], metadata: Record<string, string>) => void
}

// parseTags splits a free-text input into tags on commas/whitespace.
function parseTags(s: string): string[] {
  return s
    .split(/[,\s]+/)
    .map((t) => t.trim())
    .filter(Boolean)
}

// parseMeta reads comma-separated key=value pairs into a metadata filter map,
// splitting each pair on the first '=' so values may contain '='.
function parseMeta(s: string): Record<string, string> {
  const out: Record<string, string> = {}
  for (const part of s.split(',')) {
    const i = part.indexOf('=')
    if (i <= 0) continue
    const k = part.slice(0, i).trim()
    if (k) out[k] = part.slice(i + 1).trim()
  }
  return out
}

// MetaFilter is a pair of free-text inputs for the tag and metadata filters.
// onInput keeps the field in sync; onChange (blur/Enter) commits the parsed
// filters to the parent, so the list isn't re-queried on every keystroke.
export function MetaFilter({ onChange }: Props) {
  const [tagsRaw, setTagsRaw] = useState('')
  const [metaRaw, setMetaRaw] = useState('')
  const commit = (tags: string, meta: string) => onChange(parseTags(tags), parseMeta(meta))
  return (
    <>
      <input
        class="input filter-input"
        placeholder="tags"
        aria-label="Filter by tags (comma-separated)"
        value={tagsRaw}
        onInput={(e) => setTagsRaw((e.target as HTMLInputElement).value)}
        onChange={(e) => commit((e.target as HTMLInputElement).value, metaRaw)}
      />
      <input
        class="input filter-input"
        placeholder="meta key=value"
        aria-label="Filter by metadata (key=value, comma-separated)"
        value={metaRaw}
        onInput={(e) => setMetaRaw((e.target as HTMLInputElement).value)}
        onChange={(e) => commit(tagsRaw, (e.target as HTMLInputElement).value)}
      />
    </>
  )
}
