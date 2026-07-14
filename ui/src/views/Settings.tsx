import { apiToken, baseUrl, identity, namespaceHeader, refresh } from '../store'

// Settings edits the connection details the SPA uses to reach memini (persisted
// to localStorage) and shows the current session. The API token is no longer
// edited here — it is set once at the Login gate; this view instead reports who
// that credential authenticates as and offers a Logout that clears it.
export function Settings() {
  const id = identity.value
  const who = id?.key_name ?? (id?.authenticated ? 'admin key' : 'dev mode (no auth configured)')

  const logout = () => {
    // Clearing both drops the AuthGate back to Login: apiToken stops
    // authenticating requests, identity=null flips the gate.
    apiToken.value = ''
    identity.value = null
  }

  return (
    <div class="view stagger">
      <div class="panel panel-pad" style={{ maxWidth: '560px', marginBottom: '16px' }}>
        <div class="section-h">
          <h2>Session</h2>
          <span class="hint">the credential this browser is signed in with</span>
        </div>
        <div class="kv" style={{ gridTemplateColumns: '140px 1fr', marginBottom: '16px' }}>
          <span class="key">Signed in as</span>
          <span class="val mono" style={{ display: 'flex', alignItems: 'center', gap: '8px' }}>
            {who}
            {id?.admin && <span class="chip on">admin</span>}
          </span>
        </div>
        <button class="btn" onClick={logout}>
          Log out
        </button>
      </div>

      <div class="panel panel-pad" style={{ maxWidth: '560px' }}>
        <div class="section-h">
          <h2>Connection</h2>
          <span class="hint">stored locally in this browser</span>
        </div>

        <label class="field">
          <span class="lbl">API base URL</span>
          <input
            class="input"
            placeholder="(same origin)"
            value={baseUrl.value}
            onInput={(e) => (baseUrl.value = (e.target as HTMLInputElement).value)}
          />
          <span class="desc">Leave empty to use the host serving this page. Set it to target a remote memini.</span>
        </label>

        <label class="field">
          <span class="lbl">Namespace header</span>
          <input
            class="input mono"
            value={namespaceHeader.value}
            onInput={(e) => (namespaceHeader.value = (e.target as HTMLInputElement).value)}
          />
          <span class="desc">
            The server reads the namespace from the <code>X-Memini-Namespace</code> header; leave this as the default unless you proxy under a different name.
          </span>
        </label>

        <button class="btn primary" onClick={() => refresh()}>
          Apply &amp; reload data
        </button>
      </div>
    </div>
  )
}
