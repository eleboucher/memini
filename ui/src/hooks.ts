import { useEffect, useState } from 'preact/hooks'
import { ApiError } from './api'

export interface AsyncState<T> {
  data: T | null
  error: string | null
  loading: boolean
}

// useAsync runs `fn` whenever any dep changes (and on mount), exposing
// loading/error/data. Errors are normalized to a string. A `live` flag drops
// stale responses so an out-of-order resolution can't overwrite newer state.
// Manual refresh is driven by the global `refreshNonce` signal (passed via
// deps), so there is no separate reload handle here.
export function useAsync<T>(fn: () => Promise<T>, deps: unknown[]): AsyncState<T> {
  const [data, setData] = useState<T | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [loading, setLoading] = useState(true)

  useEffect(() => {
    let live = true
    setLoading(true)
    setError(null)
    fn()
      .then((d) => live && setData(d))
      .catch((e) => {
        if (!live) return
        setData(null)
        setError(e instanceof ApiError ? e.message : String(e))
      })
      .finally(() => live && setLoading(false))
    return () => {
      live = false
    }
    // `fn` is intentionally excluded from deps: callers pass an inline closure
    // and list its real inputs in `deps`. (No ESLint runs here; this is a note.)
  }, deps)

  return { data, error, loading }
}
