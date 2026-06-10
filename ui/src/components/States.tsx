// Shared loading / error / empty blocks, so every view renders these the same
// way instead of hand-rolling the markup.

export function Loading({ label = 'Loading…' }: { label?: string }) {
  return (
    <div class="loading" role="status" aria-live="polite">
      <span class="spinner" aria-hidden="true" /> {label}
    </div>
  )
}

export function ErrorBanner({ message }: { message: string }) {
  return (
    <div class="banner err" role="alert">
      {message}
    </div>
  )
}

export function Empty({ title, hint }: { title: string; hint?: string }) {
  return (
    <div class="panel empty">
      <div class="big">{title}</div>
      {hint && <div>{hint}</div>}
    </div>
  )
}
