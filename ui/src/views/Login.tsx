import { useState } from 'preact/hooks'
import { apiToken, baseUrl, identity } from '../store'
import { verifyToken, ApiError } from '../api'
import { LogoMark, IconChevron, IconKey } from '../icons'

// Login is the pre-Shell auth gate (rendered by app.tsx's AuthGate, NOT a
// route — it stays out of the single-segment NAV table). The operator pastes an
// API key; it is verified against GET /v1/self before it ever touches the
// persisted apiToken, so a bad paste leaves the stored credential untouched. On
// success the token is adopted and identity is set, which flips the AuthGate to
// render the Shell. The collapsed Advanced section carries the API base URL —
// Settings is unreachable pre-login, so a remote target has to be set here.
export function Login({ initialError }: { initialError?: string | null }) {
  const [token, setToken] = useState('')
  const [err, setErr] = useState<string | null>(initialError ?? null)
  const [busy, setBusy] = useState(false)
  const [advanced, setAdvanced] = useState(false)

  const submit = async (e: Event) => {
    e.preventDefault()
    if (busy) return
    setBusy(true)
    setErr(null)
    try {
      const res = await verifyToken(token)
      // Adopt only after the server accepted the token — the input is preserved
      // on failure so a typo is a one-character fix, not a full re-paste.
      apiToken.value = token
      identity.value = res.identity
    } catch (e2) {
      setErr(e2 instanceof ApiError ? e2.message : String(e2))
    } finally {
      setBusy(false)
    }
  }

  return (
    <div class="login-screen">
      <form class="panel panel-pad login-panel" onSubmit={submit}>
        <div class="login-brand">
          <LogoMark class="brand-logo" />
          <span class="mark">memini</span>
        </div>
        <div class="section-h">
          <h2>Sign in</h2>
          <span class="hint">paste an API key to connect</span>
        </div>

        {err && <div class="banner err" role="alert">{err}</div>}

        <label class="field">
          <span class="lbl">API key</span>
          <input
            class="input mono"
            type="password"
            autoComplete="off"
            autoFocus
            placeholder="mk_…"
            value={token}
            onInput={(e) => setToken((e.target as HTMLInputElement).value)}
          />
          <span class="desc">
            Sent as <code>Authorization: Bearer …</code> and stored in this browser. Verified before
            it is saved.
          </span>
        </label>

        <button class="btn primary" type="submit" disabled={busy} style={{ width: '100%', justifyContent: 'center' }}>
          {busy && <span class="spinner" style={{ width: '14px', height: '14px' }} />}
          <IconKey />
          Sign in
        </button>

        <button
          type="button"
          class="login-adv-toggle"
          aria-expanded={advanced}
          onClick={() => setAdvanced((v) => !v)}
        >
          <IconChevron style={{ transform: advanced ? 'rotate(180deg)' : undefined, transition: 'transform 0.15s var(--ease)' }} />
          Advanced
        </button>
        {advanced && (
          <label class="field" style={{ marginTop: '12px', marginBottom: 0 }}>
            <span class="lbl">API base URL</span>
            <input
              class="input"
              placeholder="(same origin)"
              value={baseUrl.value}
              onInput={(e) => (baseUrl.value = (e.target as HTMLInputElement).value)}
            />
            <span class="desc">Leave empty to use the host serving this page. Set it to target a remote memini.</span>
          </label>
        )}
      </form>
    </div>
  )
}
