import { baseUrl, token, namespaceHeader, refresh } from '../store'

// Settings edits the connection details the SPA uses to reach memini. They are
// persisted to localStorage. The static UI is public; the bearer token entered
// here is what authorizes the /v1 API calls when MEMINI_API_KEY is set.
export function Settings() {
  return (
    <div class="view">
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
          <span class="lbl">Bearer token</span>
          <input
            class="input mono"
            type="password"
            placeholder="MEMINI_API_KEY (if configured)"
            value={token.value}
            onInput={(e) => (token.value = (e.target as HTMLInputElement).value)}
          />
          <span class="desc">Sent as <code>Authorization: Bearer …</code> on every request.</span>
        </label>

        <label class="field">
          <span class="lbl">Namespace header</span>
          <input
            class="input mono"
            value={namespaceHeader.value}
            onInput={(e) => (namespaceHeader.value = (e.target as HTMLInputElement).value)}
          />
          <span class="desc">
            Must match the server's <code>MEMINI_NAMESPACE_HEADER</code> (default <code>X-Memini-Namespace</code>).
          </span>
        </label>

        <button class="btn primary" onClick={() => refresh()}>
          Apply &amp; reload data
        </button>
      </div>
    </div>
  )
}
