import { useEffect, useState } from 'preact/hooks'
import { api } from '../api'
import { identity, refreshNonce } from '../store'
import { useAsync } from '../hooks'
import { SETTINGS_CATALOG } from '../settings-catalog.gen'
import { SettingsEditor, formatSettingValue } from '../components/SettingsEditor'
import { Loading, ErrorBanner, Empty } from '../components/States'
import { IconTrash } from '../icons'
import { parsePinKey, fmtDate } from '../util'
import type {
  ApiKey,
  CallerIdentity,
  ClientSettings,
  HandshakeRequest,
  HandshakeResponse,
  Pin,
} from '../types'

// Config is the server-side configuration surface (Phase 8): every
// ClientSettings field, seeable with description + effective value +
// provenance, plus editing of the layers that produce it (the server's
// global defaults, and — in Keys.tsx — per-key overrides) and the two
// pieces of machinery that feed namespace resolution (pins, and the
// handshake itself). Four independent tabs, no sub-routes — app.tsx keeps
// every route single-segment (see its NAV comment), so tab state is local.
type Tab = 'settings' | 'defaults' | 'pins' | 'preview'

const TABS: { id: Tab; label: string }[] = [
  { id: 'settings', label: 'Settings' },
  { id: 'defaults', label: 'Defaults' },
  { id: 'pins', label: 'Pins' },
  { id: 'preview', label: 'Preview' },
]

export function Config() {
  const [tab, setTab] = useState<Tab>('settings')
  return (
    <div class="view">
      <div class="tabs" role="tablist" aria-label="Config sections">
        {TABS.map((t) => (
          <button
            key={t.id}
            type="button"
            role="tab"
            id={`config-tab-${t.id}`}
            aria-selected={tab === t.id}
            aria-controls={`config-panel-${t.id}`}
            class={`tab ${tab === t.id ? 'active' : ''}`}
            onClick={() => setTab(t.id)}
          >
            {t.label}
          </button>
        ))}
      </div>
      {/* Only the active panel is ever mounted, so a single tabpanel region
          suffices: its id/labelledby track the active tab, completing the
          pairing each tab's aria-controls points at. */}
      <div role="tabpanel" id={`config-panel-${tab}`} aria-labelledby={`config-tab-${tab}`}>
        {tab === 'settings' && <SettingsTab />}
        {tab === 'defaults' && <DefaultsTab />}
        {tab === 'pins' && <PinsTab />}
        {tab === 'preview' && <PreviewTab />}
      </div>
    </div>
  )
}

// ============================================================================
// Tab 1: Settings — the visibility centerpiece. Every catalog field, its
// effective value, and where it came from, for the caller's own credential
// (GET /v1/self) or — admin-only — any other key, merged client-side.
// ============================================================================

const PROV_LABEL: Record<string, string> = {
  default: 'built-in',
  global: 'server default',
  key: 'per-key',
  defaults: 'defaults',
}

function ProvenanceBadge({ source }: { source?: string }) {
  const s = source ?? 'default'
  return (
    <span class={`prov ${s}`} title={PROV_LABEL[s] ?? s}>
      {PROV_LABEL[s] ?? s}
    </span>
  )
}

// SettingsTable renders the read-only visibility table shared by the
// Settings tab (self / another key) and the Preview tab (a handshake
// result) — same catalog-driven row list, different data source.
function SettingsTable({
  settings,
  sources,
  computedBy,
  note,
}: {
  settings: ClientSettings
  sources: Record<string, string>
  computedBy: 'server' | 'client'
  note?: string | null
}) {
  const flat = settings as unknown as Record<string, unknown>
  return (
    <div class="panel panel-pad">
      <div class="section-h">
        <h2>Effective settings</h2>
        <span class="hint">
          {computedBy === 'server'
            ? 'server-computed — full built-in → global → per-key layering'
            : 'client-merged — a per-key blob applied over GET /v1/settings/defaults'}
        </span>
      </div>
      {note && (
        <div class="hint" style={{ marginBottom: '14px' }}>
          {note}
        </div>
      )}
      <div class="mem-list">
        {SETTINGS_CATALOG.map((f) => (
          <div class="panel panel-pad setrow" key={f.key}>
            <div class="setrow-main">
              <div class="val mono">{f.key}</div>
              <div class="hint">{f.description}</div>
            </div>
            <span class="val mono setrow-value">{formatSettingValue(f, flat[f.key])}</span>
            <ProvenanceBadge source={sources[f.key]} />
          </div>
        ))}
      </div>
    </div>
  )
}

function IdentityPanel({ identity, nonNamed }: { identity: CallerIdentity; nonNamed: boolean }) {
  return (
    <div class="panel panel-pad" style={{ marginBottom: '16px' }}>
      <div class="section-h">
        <h2>Identity</h2>
      </div>
      <div class="kv" style={{ gridTemplateColumns: '160px 1fr' }}>
        <span class="key">Key name</span>
        <span class="val mono">{identity.key_name ?? '(admin key / dev mode — no named principal)'}</span>
        <span class="key">Home</span>
        <span class="val mono">{identity.home || 'unbound — personal writes need a home'}</span>
        <span class="key">Default namespace</span>
        <span class="val mono">{identity.default_namespace || '—'}</span>
      </div>
      {nonNamed && (
        <div class="hint" style={{ marginTop: '12px' }}>
          Authenticated with the admin credential (or dev mode) — no per-key layer applies to this
          identity; the settings below are built-ins plus any server global defaults only.
        </div>
      )}
    </div>
  )
}

function SettingsTab() {
  const self = useAsync(() => api.self(), [refreshNonce.value])
  // identity.admin is the authoritative admin signal (env key, dev mode, or a
  // named key with admin=true); the key-picker below is admin-only, so skip its
  // fetch entirely for a non-admin rather than firing a request just to 403.
  const isAdmin = identity.value?.admin === true
  const keysAsync = useAsync(
    () => (isAdmin ? api.listKeys() : Promise.resolve<ApiKey[]>([])),
    [refreshNonce.value],
  )
  const [pickedKey, setPickedKey] = useState('')

  const defaultsForMerge = useAsync(
    () => (pickedKey ? api.getSettingsDefaults() : Promise.resolve(null)),
    [pickedKey],
  )

  if (self.loading && !self.data) return <Loading />
  if (self.error) return <ErrorBanner message={self.error} />
  if (!self.data) return null

  const selfIdentity = self.data.identity
  const pickedRow = pickedKey ? (keysAsync.data ?? []).find((k) => k.name === pickedKey) : undefined
  const showingKey = Boolean(pickedKey && pickedRow)
  const mergeReady = !showingKey || Boolean(defaultsForMerge.data)

  let settings: ClientSettings = self.data.settings
  let sources: Record<string, string> = self.data.settings_sources
  let computedBy: 'server' | 'client' = 'server'
  let note: string | null = null

  if (showingKey && mergeReady) {
    const base = defaultsForMerge.data as unknown as Record<string, unknown>
    const explicit = (pickedRow!.settings ?? {}) as Record<string, unknown>
    const merged: Record<string, unknown> = { ...base, ...explicit }
    const mergedSources: Record<string, string> = {}
    for (const f of SETTINGS_CATALOG) {
      mergedSources[f.key] = Object.prototype.hasOwnProperty.call(explicit, f.key) ? 'key' : 'defaults'
    }
    settings = merged as unknown as ClientSettings
    sources = mergedSources
    computedBy = 'client'
    note = `Client-merged: "${pickedKey}"'s explicit settings blob applied over GET /v1/settings/defaults. "defaults" below may be the built-in or an admin-set global override — only /v1/self and /v1/handshake distinguish the two, and only for the credential making the request.`
  }

  return (
    <>
      <IdentityPanel identity={selfIdentity} nonNamed={!selfIdentity.key_name} />
      {isAdmin && (
        <div
          class="panel panel-pad"
          style={{ marginBottom: '16px', display: 'flex', gap: '10px', alignItems: 'center', flexWrap: 'wrap' }}
        >
          <span class="lbl" style={{ marginBottom: 0 }}>
            Inspect settings for
          </span>
          <select
            class="select"
            style={{ width: 'auto' }}
            value={pickedKey}
            onChange={(e) => setPickedKey((e.target as HTMLSelectElement).value)}
          >
            <option value="">This credential (self)</option>
            {(keysAsync.data ?? []).map((k) => (
              <option key={k.name} value={k.name}>
                {k.name}
              </option>
            ))}
          </select>
        </div>
      )}
      {showingKey && defaultsForMerge.error ? (
        // The defaults fetch failed — show the error alone. Falling through to
        // the spinner below would leave a permanent "Merging…" (mergeReady
        // stays false without data), and the table would show self's settings,
        // not the inspected key's.
        <ErrorBanner message={defaultsForMerge.error} />
      ) : showingKey && !mergeReady ? (
        <Loading label="Merging…" />
      ) : (
        <SettingsTable settings={settings} sources={sources} computedBy={computedBy} note={note} />
      )}
    </>
  )
}

// ============================================================================
// Tab 2: Defaults — the server's global override layer. GET is always fully
// resolved with no field-level provenance of its own, so "is this field
// currently overridden globally" is inferred once at load (divergence from
// the catalog's built-in default) and tracked from there as an explicit
// "touched fields" draft — the same partial-payload discipline PUT expects.
// ============================================================================

function DefaultsTab() {
  const [nonce, setNonce] = useState(0)
  // The defaults layer is admin-gated. identity.admin tells us up front, so a
  // non-admin session skips the fetch and lands on the locked state below
  // rather than round-tripping to a 403.
  const nonAdmin = identity.value?.admin === false
  const { data, error, loading } = useAsync(
    () => (nonAdmin ? Promise.resolve(null) : api.getSettingsDefaults()),
    [nonce],
  )
  const [explicit, setExplicit] = useState<Record<string, unknown> | null>(null)
  const [saving, setSaving] = useState(false)
  const [saveErr, setSaveErr] = useState<string | null>(null)
  const [saved, setSaved] = useState(false)

  useEffect(() => {
    if (!data) return
    setExplicit((prev) => {
      if (prev !== null) return prev
      const resolved = data as unknown as Record<string, unknown>
      const initial: Record<string, unknown> = {}
      for (const f of SETTINGS_CATALOG) {
        if (JSON.stringify(resolved[f.key]) !== JSON.stringify(f.default)) initial[f.key] = resolved[f.key]
      }
      return initial
    })
    // Only ever derives the initial draft once (guarded by `prev !== null`
    // above) — a background refetch after saving must not clobber whatever
    // the admin is mid-edit on.
  }, [data])

  if (nonAdmin) {
    const who = identity.value?.key_name
    return (
      <Empty
        title="Admin access required"
        hint={`Signed in as ${who ? `"${who}"` : 'this key'}, which is not an admin key. Viewing or editing the server's global default settings needs an admin credential — the admin env key (MEMINI_API_KEY) or an API key with admin=true.`}
      />
    )
  }
  if (loading && !data) return <Loading />
  if (error) return <ErrorBanner message={error} />
  if (!data || explicit === null) return null

  const readOnly = data.managed_by === 'env'
  const resolved = data as unknown as Record<string, unknown>
  const placeholders: Record<string, unknown> = {}
  for (const f of SETTINGS_CATALOG) placeholders[f.key] = resolved[f.key]

  const setField = (key: string, value: unknown) => {
    setExplicit((prev) => ({ ...(prev ?? {}), [key]: value }))
    setSaved(false)
  }
  const resetField = (key: string) => {
    setExplicit((prev) => {
      const next = { ...(prev ?? {}) }
      delete next[key]
      return next
    })
    setSaved(false)
  }

  const save = async (e: Event) => {
    e.preventDefault()
    setSaving(true)
    setSaveErr(null)
    try {
      await api.putSettingsDefaults(explicit)
      setSaved(true)
      setNonce((n) => n + 1) // background refresh; `explicit` already reflects what was just saved
    } catch (err) {
      setSaveErr(err instanceof Error ? err.message : String(err))
    } finally {
      setSaving(false)
    }
  }

  return (
    <div class="panel panel-pad">
      <div class="section-h">
        <h2>Global defaults</h2>
        <span class="hint">
          every key inherits these absent its own per-key override — a field left unset here falls
          back to the built-in
        </span>
      </div>
      {readOnly && (
        <div class="banner warn" role="status">
          Managed by <code>MEMINI_CLIENT_DEFAULTS</code> on the server — this layer is read-only here.
        </div>
      )}
      {saveErr && <ErrorBanner message={saveErr} />}
      <form onSubmit={save}>
        <SettingsEditor values={explicit} placeholders={placeholders} onSet={setField} onReset={resetField} readOnly={readOnly} />
        {!readOnly && (
          <div style={{ display: 'flex', gap: '10px', alignItems: 'center', marginTop: '16px' }}>
            <button class="btn primary" type="submit" disabled={saving}>
              {saving && <span class="spinner" style={{ width: '14px', height: '14px' }} />}
              Save global defaults
            </button>
            {saved && (
              <span class="hint" style={{ color: 'var(--ok)' }}>
                Saved.
              </span>
            )}
          </div>
        )}
      </form>
    </div>
  )
}

// ============================================================================
// Tab 3: Pins — explicit project→namespace overrides (GET/PUT/DELETE
// /v1/pins). A pin beats every other namespace_source at handshake time.
// ============================================================================

function PinsTab() {
  const [nonce, setNonce] = useState(0)
  const [mutErr, setMutErr] = useState<string | null>(null)
  const { data, error, loading } = useAsync(() => api.listPins(), [nonce])
  const pins = data ?? []

  const reload = () => {
    setMutErr(null)
    setNonce((n) => n + 1)
  }

  return (
    <div class="panel panel-pad">
      <div class="section-h">
        <h2>Explicit pins</h2>
        <span class="hint">project → namespace overrides; a pin beats every other namespace_source</span>
      </div>
      {loading && !data && <Loading />}
      {error && <ErrorBanner message={error} />}
      {mutErr && <ErrorBanner message={mutErr} />}
      {data && pins.length === 0 && <Empty title="No pins yet" hint="Add one below." />}
      {pins.length > 0 && (
        <div class="mem-list" style={{ marginBottom: '16px' }}>
          {pins.map((p) => (
            <PinRow key={p.key} pin={p} onChanged={reload} onError={setMutErr} />
          ))}
        </div>
      )}
      <AddPinForm onAdded={reload} onError={setMutErr} />
    </div>
  )
}

function PinRow({
  pin,
  onChanged,
  onError,
}: {
  pin: Pin
  onChanged: () => void
  onError: (e: string | null) => void
}) {
  const [editing, setEditing] = useState(false)
  const [namespaceVal, setNamespaceVal] = useState(pin.namespace)
  const [note, setNote] = useState(pin.note ?? '')
  const [armed, setArmed] = useState(false)
  const [busy, setBusy] = useState(false)
  const facts = parsePinKey(pin.key)

  const save = async (e: Event) => {
    e.preventDefault()
    const ns = namespaceVal.trim()
    if (!ns || busy) return
    setBusy(true)
    onError(null)
    try {
      await api.putPin({ ...facts, namespace: ns, note: note.trim() || undefined })
      setEditing(false)
      onChanged()
    } catch (err) {
      onError(err instanceof Error ? err.message : String(err))
    } finally {
      setBusy(false)
    }
  }

  const del = async () => {
    if (!armed) {
      setArmed(true)
      setTimeout(() => setArmed(false), 3000)
      return
    }
    setBusy(true)
    onError(null)
    try {
      await api.deletePin(facts)
      onChanged()
    } catch (err) {
      onError(err instanceof Error ? err.message : String(err))
      setBusy(false)
      setArmed(false)
    }
  }

  if (editing) {
    return (
      <form
        class="panel panel-pad"
        onSubmit={save}
        style={{ display: 'flex', gap: '8px', flexWrap: 'wrap', alignItems: 'center' }}
      >
        <span class="hint mono" style={{ flex: '1 1 200px', overflowWrap: 'anywhere' }}>
          {pin.key}
        </span>
        <input
          class="input"
          value={namespaceVal}
          onInput={(e) => setNamespaceVal((e.target as HTMLInputElement).value)}
          style={{ flex: '1 1 160px' }}
        />
        <input
          class="input"
          placeholder="note (optional)"
          value={note}
          onInput={(e) => setNote((e.target as HTMLInputElement).value)}
          style={{ flex: '1 1 160px' }}
        />
        <button class="btn primary" type="submit" disabled={busy || !namespaceVal.trim()}>
          Save
        </button>
        <button class="btn ghost" type="button" onClick={() => setEditing(false)} disabled={busy}>
          Cancel
        </button>
      </form>
    )
  }

  return (
    <div class="panel panel-pad" style={{ display: 'flex', alignItems: 'center', gap: '10px', flexWrap: 'wrap' }}>
      <span class="hint mono" style={{ flex: '1 1 220px', overflowWrap: 'anywhere' }}>
        {pin.key}
      </span>
      <span class="val mono" style={{ flex: '1 1 160px', overflowWrap: 'anywhere' }}>
        {pin.namespace}
      </span>
      <span class="hint" style={{ flex: '1 1 140px' }}>
        {pin.note || '—'}
      </span>
      <span class="hint mono" style={{ flex: '1 1 100px' }}>
        {pin.created_by ?? '—'}
      </span>
      <span class="hint" style={{ flex: '1 1 140px' }}>
        {fmtDate(pin.updated_at)}
      </span>
      <button class="btn ghost" type="button" onClick={() => setEditing(true)} disabled={busy}>
        Edit
      </button>
      <button
        class={`icon-btn ${armed ? 'danger-on' : ''}`}
        aria-label={armed ? `Confirm delete pin ${pin.key}` : `Delete pin ${pin.key}`}
        title={armed ? 'Click again to confirm' : 'Delete'}
        onClick={del}
        disabled={busy}
      >
        <IconTrash />
      </button>
    </div>
  )
}

function AddPinForm({ onAdded, onError }: { onAdded: () => void; onError: (e: string | null) => void }) {
  const [namespaceVal, setNamespaceVal] = useState('')
  const [remoteUrl, setRemoteUrl] = useState('')
  const [toplevelPath, setToplevelPath] = useState('')
  const [note, setNote] = useState('')
  const [busy, setBusy] = useState(false)

  const submit = async (e: Event) => {
    e.preventDefault()
    const ns = namespaceVal.trim()
    const remote = remoteUrl.trim()
    const path = toplevelPath.trim()
    if (!ns || (!remote && !path) || busy) return
    setBusy(true)
    onError(null)
    try {
      await api.putPin({
        namespace: ns,
        remote_url: remote || undefined,
        toplevel_path: path || undefined,
        note: note.trim() || undefined,
      })
      setNamespaceVal('')
      setRemoteUrl('')
      setToplevelPath('')
      setNote('')
      onAdded()
    } catch (err) {
      onError(err instanceof Error ? err.message : String(err))
    } finally {
      setBusy(false)
    }
  }

  return (
    <form onSubmit={submit} style={{ display: 'flex', gap: '8px', flexWrap: 'wrap', alignItems: 'center' }}>
      <input
        class="input"
        placeholder="namespace, e.g. acme/phoenix"
        value={namespaceVal}
        onInput={(e) => setNamespaceVal((e.target as HTMLInputElement).value)}
        style={{ flex: '1 1 160px' }}
      />
      <input
        class="input"
        placeholder="remote URL (or)"
        value={remoteUrl}
        onInput={(e) => setRemoteUrl((e.target as HTMLInputElement).value)}
        style={{ flex: '1 1 200px' }}
      />
      <input
        class="input"
        placeholder="absolute toplevel path"
        value={toplevelPath}
        onInput={(e) => setToplevelPath((e.target as HTMLInputElement).value)}
        style={{ flex: '1 1 200px' }}
      />
      <input
        class="input"
        placeholder="note (optional)"
        value={note}
        onInput={(e) => setNote((e.target as HTMLInputElement).value)}
        style={{ flex: '1 1 140px' }}
      />
      <button
        class="btn primary"
        type="submit"
        disabled={busy || !namespaceVal.trim() || (!remoteUrl.trim() && !toplevelPath.trim())}
      >
        {busy && <span class="spinner" style={{ width: '14px', height: '14px' }} />}
        Add pin
      </button>
    </form>
  )
}

// ============================================================================
// Tab 4: Preview — the "why did I get this namespace?" debugger. Sends
// project facts to POST /v1/handshake and renders exactly what a real
// client would receive: resolved namespace, which precedence rung fired,
// pin details when one matched, the read-set, and the full settings table.
// ============================================================================

function PreviewTab() {
  const [remoteUrl, setRemoteUrl] = useState('')
  const [toplevelPath, setToplevelPath] = useState('')
  const [toplevelBasename, setToplevelBasename] = useState('')
  const [cwdBasename, setCwdBasename] = useState('')
  const [agent, setAgent] = useState('')
  const [envNamespace, setEnvNamespace] = useState('')
  const [declaredNamespace, setDeclaredNamespace] = useState('')
  const [result, setResult] = useState<HandshakeResponse | null>(null)
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState<string | null>(null)

  const submit = async (e: Event) => {
    e.preventDefault()
    const cwd = cwdBasename.trim()
    if (!cwd || busy) return
    setBusy(true)
    setErr(null)
    try {
      const body: HandshakeRequest = {
        project: {
          remote_url: remoteUrl.trim() || undefined,
          toplevel_path: toplevelPath.trim() || undefined,
          toplevel_basename: toplevelBasename.trim() || undefined,
          cwd_basename: cwd,
          agent: agent.trim() || undefined,
          env_namespace: envNamespace.trim() || undefined,
          declared_namespace: declaredNamespace.trim() || undefined,
        },
      }
      setResult(await api.handshake(body))
    } catch (e2) {
      setErr(e2 instanceof Error ? e2.message : String(e2))
      setResult(null)
    } finally {
      setBusy(false)
    }
  }

  return (
    <div class="grid-2">
      <div class="panel panel-pad">
        <div class="section-h">
          <h2>Handshake preview</h2>
          <span class="hint">send project facts, see how the server resolves them</span>
        </div>
        <form onSubmit={submit}>
          <label class="field">
            <span class="lbl">Remote URL</span>
            <input class="input" value={remoteUrl} onInput={(e) => setRemoteUrl((e.target as HTMLInputElement).value)} />
          </label>
          <label class="field">
            <span class="lbl">Toplevel path</span>
            <input
              class="input"
              value={toplevelPath}
              onInput={(e) => setToplevelPath((e.target as HTMLInputElement).value)}
            />
          </label>
          <label class="field">
            <span class="lbl">Toplevel basename</span>
            <input
              class="input"
              value={toplevelBasename}
              onInput={(e) => setToplevelBasename((e.target as HTMLInputElement).value)}
            />
          </label>
          <label class="field">
            <span class="lbl">Cwd basename *</span>
            <input class="input" value={cwdBasename} onInput={(e) => setCwdBasename((e.target as HTMLInputElement).value)} />
            <span class="desc">The only field every caller can always supply — required here too.</span>
          </label>
          <label class="field">
            <span class="lbl">Agent</span>
            <input class="input" value={agent} onInput={(e) => setAgent((e.target as HTMLInputElement).value)} />
          </label>
          <label class="field">
            <span class="lbl">Env namespace</span>
            <input
              class="input"
              value={envNamespace}
              onInput={(e) => setEnvNamespace((e.target as HTMLInputElement).value)}
            />
            <span class="desc">Simulates the client's MEMINI_NAMESPACE.</span>
          </label>
          <label class="field">
            <span class="lbl">Declared namespace</span>
            <input
              class="input"
              value={declaredNamespace}
              onInput={(e) => setDeclaredNamespace((e.target as HTMLInputElement).value)}
            />
          </label>
          <button class="btn primary" type="submit" disabled={busy || !cwdBasename.trim()}>
            {busy && <span class="spinner" style={{ width: '14px', height: '14px' }} />}
            Run handshake
          </button>
        </form>
        {err && <ErrorBanner message={err} />}
      </div>
      <div class="panel panel-pad">
        <div class="section-h">
          <h2>Result</h2>
        </div>
        {!result && <Empty title="No result yet" hint="Run a handshake to see the resolution." />}
        {result && <HandshakeResult result={result} />}
      </div>
    </div>
  )
}

function HandshakeResult({ result }: { result: HandshakeResponse }) {
  return (
    <div>
      <div class="kv" style={{ gridTemplateColumns: '150px 1fr', marginBottom: '14px' }}>
        <span class="key">Namespace</span>
        <span class="val mono">{result.namespace}</span>
        <span class="key">Resolved via</span>
        <span class="val">
          <span class="chip on">{result.namespace_source}</span>
        </span>
        <span class="key">Identity</span>
        <span class="val mono">{result.identity.key_name ?? '(admin / dev mode)'}</span>
      </div>
      {result.pin && (
        <div class="banner warn" role="status" style={{ marginBottom: '14px' }}>
          Matched pin <span class="mono">{result.pin.key}</span>
          {result.pin.note ? ` — ${result.pin.note}` : ''}
          {result.pin.created_by ? ` (created by ${result.pin.created_by})` : ''}
        </div>
      )}
      {result.read_set.length > 0 && (
        <>
          <div class="hint" style={{ marginBottom: '8px' }}>
            Read-set
          </div>
          <div class="mem-list" style={{ marginBottom: '18px' }}>
            {result.read_set.map((e) => (
              <div
                class="panel panel-pad"
                key={`${e.namespace}-${e.origin}`}
                style={{ display: 'flex', gap: '10px', alignItems: 'center' }}
              >
                <span class="val mono" style={{ flex: 1, overflowWrap: 'anywhere' }}>
                  {e.namespace}
                </span>
                <span class="chip">{e.origin}</span>
              </div>
            ))}
          </div>
        </>
      )}
      <SettingsTable settings={result.settings} sources={result.settings_sources} computedBy="server" />
    </div>
  )
}
