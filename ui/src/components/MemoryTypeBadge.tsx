// MemoryTypeBadge renders the typed-extraction class (decision/preference/
// problem) a memory carries, or nothing when it has none. Only known classes
// render, so an arbitrary metadata.memory_type can't inject CSS classes or show
// an unstyled badge.
const KNOWN = new Set(['decision', 'preference', 'problem'])

export function MemoryTypeBadge({ type }: { type?: string }) {
  if (!type || !KNOWN.has(type)) return null
  return (
    <span class={`mtype ${type}`} title="Typed extraction">
      {type}
    </span>
  )
}
