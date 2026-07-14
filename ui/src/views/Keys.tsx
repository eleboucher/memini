import { useState } from 'preact/hooks'
import { api } from '../api'
import { apiToken, identity } from '../store'
import { useAsync } from '../hooks'
import type { ApiKey } from '../types'
import { Loading, ErrorBanner, Empty } from '../components/States'
import { SettingsEditor } from '../components/SettingsEditor'
import { IconKey, IconTrash, IconRefresh, IconCopy, IconCheck, IconChevron } from '../icons'
import { fmtDate } from '../util'

// Keys manages the REST API-key surface (K3b): a list of every key — both
// table-managed (source=db, mutable here) and declaratively managed via
// MEMINI_API_KEYS_FILE (source=file, read-only "declarative" rows) — plus a
// create form and per-row rotate/enable-disable/delete actions. The server
// gates every /v1/keys operation to the admin key (or dev/bootstrap mode
// with auth disabled entirely); a named API key gets 403, rendered here as
// a dedicated empty state rather than a generic error banner.
export function Keys() {
  const [nonce, setNonce] = useState(0)
  const [mutErr, setMutErr] = useState<string | null>(null)
  const [secret, setSecret] = useState<{ name: string; secret: string; verb: 'created' | 'rotated' } | null>(null)
  // A non-admin named key can't manage keys (the server 403s every /v1/keys
  // call). We know that up front from identity.admin — so skip the fetch
  // entirely and render a purpose-built locked state instead of firing a
  // request just to catch its 403. admin===undefined (identity not yet
  // resolved) optimistically attempts the fetch; admin===true and dev mode
  // both proceed.
  const nonAdmin = identity.value?.admin === false
  const { data, error, loading } = useAsync(
    () => (nonAdmin ? Promise.resolve<ApiKey[]>([]) : api.listKeys()),
    [nonce],
  )
  const keys = data ?? []
  // Fetched once for the whole list (not per-row): the per-key settings
  // editor shows the resolved global defaults as its placeholder for an
  // unset field (what that field would actually inherit), rather than just
  // the catalog's built-in — this view is already admin-gated the same way
  // GET /v1/settings/defaults is, so it's expected to succeed whenever this
  // page renders at all. Best-effort: a failure here just falls back to the
  // catalog's built-in placeholder (SettingsEditor's default when none is
  // supplied), not a page-level error.
  const globalDefaults = useAsync(
    () => (nonAdmin ? Promise.resolve(null) : api.getSettingsDefaults()),
    [nonce],
  )

  const reload = () => {
    setMutErr(null)
    setNonce((n) => n + 1)
  }

  // Non-admin session: identity already told us this credential can't reach the
  // key surface, so render a locked state naming the key instead of a raw 403.
  if (nonAdmin) {
    const who = identity.value?.key_name
    return (
      <div class="view">
        <Empty
          title="Admin access required"
          hint={`Signed in as ${who ? `"${who}"` : 'this key'}, which is not an admin key. Managing API keys needs an admin credential — the admin env key (MEMINI_API_KEY) or an API key with admin=true. Sign in with one under UI settings, or mint one with the memini CLI (memini key add --admin).`}
        />
      </div>
    )
  }

  return (
    <div class="view stagger">
      <div class="panel panel-pad">
        <div class="section-h">
          <h2>API keys</h2>
          <span class="hint">
            table-managed keys (mutable here) plus MEMINI_API_KEYS_FILE entries (declarative, read-only)
          </span>
        </div>
        {loading && !data && <Loading />}
        {error && <ErrorBanner message={error} />}
        {mutErr && <ErrorBanner message={mutErr} />}
        {data && keys.length === 0 && <Empty title="No API keys yet" hint="Create one below." />}
        {keys.length > 0 && (
          <div class="mem-list" style={{ marginBottom: '16px' }}>
            {keys.map((k) => (
              <KeyRow
                key={k.name}
                k={k}
                globalDefaults={globalDefaults.data as unknown as Record<string, unknown> | undefined}
                onChanged={reload}
                onError={setMutErr}
                onSecret={(s) => setSecret({ name: k.name, secret: s, verb: 'rotated' })}
              />
            ))}
          </div>
        )}
        <CreateKeyForm
          onCreated={(name, s) => {
            setSecret({ name, secret: s, verb: 'created' })
            // Dev-mode bootstrap: creating the first key turns auth on
            // immediately (see createApiKey's spec note), so the reload() below
            // would 401 with no credential — tearing down this view and the
            // one-time secret modal with it. Adopt the new secret first, then
            // refresh identity from the now-authenticated session.
            if (identity.value && !identity.value.authenticated) {
              apiToken.value = s
              api
                .self()
                .then((r) => (identity.value = r.identity))
                .catch(() => {})
            }
            reload()
          }}
          onError={setMutErr}
        />
      </div>
      {secret && (
        <SecretModal name={secret.name} secret={secret.secret} verb={secret.verb} onClose={() => setSecret(null)} />
      )}
    </div>
  )
}

interface RowProps {
  k: ApiKey
  globalDefaults?: Record<string, unknown>
  onChanged: () => void
  onError: (e: string | null) => void
  onSecret: (secret: string) => void
}

function KeyRow({ k, globalDefaults, onChanged, onError, onSecret }: RowProps) {
  const [armed, setArmed] = useState(false) // delete confirmation (two-click, MemoryDrawer's pattern)
  const [busy, setBusy] = useState(false)
  const [expanded, setExpanded] = useState(false)
  const isFile = k.source === 'file'

  const del = async () => {
    if (!armed) {
      setArmed(true)
      setTimeout(() => setArmed(false), 3000)
      return
    }
    setBusy(true)
    onError(null)
    try {
      await api.deleteKey(k.name)
      onChanged()
    } catch (e) {
      onError(e instanceof Error ? e.message : String(e))
      setBusy(false)
      setArmed(false)
    }
  }

  const toggleDisabled = async () => {
    setBusy(true)
    onError(null)
    try {
      await api.updateKey(k.name, { disabled: !k.disabled })
      onChanged()
    } catch (e) {
      onError(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy(false)
    }
  }

  const toggleAdmin = async () => {
    setBusy(true)
    onError(null)
    try {
      // Revoking one's own admin (or the server's other self-guard cases)
      // returns 409 — surfaced through the shared error path, same as any
      // other mutation failure.
      await api.updateKey(k.name, { admin: !k.admin })
      onChanged()
    } catch (e) {
      onError(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy(false)
    }
  }

  const rotate = async () => {
    setBusy(true)
    onError(null)
    try {
      const rotated = await api.rotateKey(k.name)
      // Rotating the key THIS session authenticates with invalidates the old
      // secret immediately server-side. Adopt the new one before anything
      // refetches, or the next request 401s and the AuthGate unmounts this
      // view — taking the one-time secret modal with it.
      if (rotated.name === identity.value?.key_name) apiToken.value = rotated.secret
      onSecret(rotated.secret)
    } catch (e) {
      onError(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy(false)
    }
  }

  return (
    <>
      <div class="panel panel-pad" style={{ display: 'flex', alignItems: 'center', gap: '10px', flexWrap: 'wrap' }}>
        <span class="val mono" style={{ flex: '1 1 140px', overflowWrap: 'anywhere' }}>
          {k.name}
        </span>
        <span class="hint mono" style={{ flex: '1 1 120px' }}>
          {k.home || '—'}
        </span>
        <span class="hint mono" style={{ flex: '1 1 120px' }}>
          {k.default_namespace || '—'}
        </span>
        <span class="hint" style={{ flex: '1 1 140px' }}>
          {fmtDate(k.created_at)}
        </span>
        <span class="chip" style={k.disabled ? { borderColor: '#ff8a6a', color: '#ff8a6a' } : undefined}>
          {k.disabled ? 'disabled' : 'enabled'}
        </span>
        {k.admin && (
          <span class="chip" style={{ borderColor: 'var(--tier-semantic)', color: 'var(--tier-semantic)' }} title="Admin key — may manage keys and server defaults">
            admin
          </span>
        )}
        <button
          class="icon-btn"
          aria-label={expanded ? `Collapse settings for ${k.name}` : `Edit settings for ${k.name}`}
          title="Per-key settings override"
          onClick={() => setExpanded((v) => !v)}
        >
          <IconChevron style={{ transform: expanded ? 'rotate(180deg)' : undefined, transition: 'transform 0.15s var(--ease)' }} />
        </button>
        {isFile ? (
          <span class="chip" title="Managed via MEMINI_API_KEYS_FILE — edit the file to change this key">
            declarative
          </span>
        ) : (
          <>
            <button
              class="btn ghost"
              type="button"
              aria-label={k.admin ? `Revoke admin from ${k.name}` : `Grant admin to ${k.name}`}
              title={k.admin ? 'Revoke admin capability' : 'Grant admin capability'}
              onClick={toggleAdmin}
              disabled={busy}
            >
              {k.admin ? 'Revoke admin' : 'Grant admin'}
            </button>
            <button
              class="icon-btn"
              aria-label={k.disabled ? `Enable ${k.name}` : `Disable ${k.name}`}
              title={k.disabled ? 'Enable' : 'Disable'}
              onClick={toggleDisabled}
              disabled={busy}
            >
              <IconCheck />
            </button>
            <button class="icon-btn" aria-label={`Rotate secret for ${k.name}`} title="Rotate secret" onClick={rotate} disabled={busy}>
              <IconRefresh />
            </button>
            <button
              class={`icon-btn ${armed ? 'danger-on' : ''}`}
              aria-label={armed ? `Confirm delete ${k.name}` : `Delete ${k.name}`}
              title={armed ? 'Click again to confirm' : 'Delete'}
              onClick={del}
              disabled={busy}
            >
              <IconTrash />
            </button>
          </>
        )}
      </div>
      {expanded && <KeySettingsPanel apiKey={k} globalDefaults={globalDefaults} onChanged={onChanged} onError={onError} />}
    </>
  )
}

// KeySettingsPanel is the per-key settings editor (catalog-driven, via
// SettingsEditor): `apiKey.settings` is already the explicit override blob
// (fields present here are exactly the fields this key overrides — nothing
// to infer, unlike Config's Defaults tab), edited via PATCH /v1/keys/{name}.
// A source=file key gets a read-only render: the API 409s any write to it,
// so there's no form to offer, only its current declared blob.
function KeySettingsPanel({
  apiKey,
  globalDefaults,
  onChanged,
  onError,
}: {
  apiKey: ApiKey
  globalDefaults?: Record<string, unknown>
  onChanged: () => void
  onError: (e: string | null) => void
}) {
  const isFile = apiKey.source === 'file'
  const [explicit, setExplicit] = useState<Record<string, unknown>>({ ...(apiKey.settings ?? {}) })
  const [saving, setSaving] = useState(false)
  const [saved, setSaved] = useState(false)
  const [err, setErr] = useState<string | null>(null)

  const setField = (key: string, value: unknown) => {
    setExplicit((prev) => ({ ...prev, [key]: value }))
    setSaved(false)
  }
  const resetField = (key: string) => {
    setExplicit((prev) => {
      const next = { ...prev }
      delete next[key]
      return next
    })
    setSaved(false)
  }

  const save = async (e: Event) => {
    e.preventDefault()
    setSaving(true)
    setErr(null)
    try {
      await api.updateKey(apiKey.name, { settings: explicit })
      setSaved(true)
      onChanged()
    } catch (e2) {
      const msg = e2 instanceof Error ? e2.message : String(e2)
      setErr(msg)
      onError(msg)
    } finally {
      setSaving(false)
    }
  }

  return (
    <div class="panel panel-pad" style={{ marginTop: '-6px' }}>
      <div class="section-h">
        <h2 style={{ fontSize: '14px' }}>Settings override — {apiKey.name}</h2>
        <span class="hint">
          {isFile
            ? 'managed by the api-keys file — read-only here'
            : 'only explicitly-set fields are stored; everything else inherits the server defaults'}
        </span>
      </div>
      {isFile && (
        <div class="hint" style={{ marginBottom: '12px' }}>
          This key is declared in <code>MEMINI_API_KEYS_FILE</code>. Edit the file to change its settings —
          the API rejects writes to a file-sourced key with 409.
        </div>
      )}
      {err && <ErrorBanner message={err} />}
      <form onSubmit={save}>
        <SettingsEditor values={explicit} placeholders={globalDefaults} onSet={setField} onReset={resetField} readOnly={isFile} />
        {!isFile && (
          <div style={{ display: 'flex', gap: '10px', alignItems: 'center', marginTop: '16px' }}>
            <button class="btn primary" type="submit" disabled={saving}>
              {saving && <span class="spinner" style={{ width: '14px', height: '14px' }} />}
              Save settings
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

function CreateKeyForm({
  onCreated,
  onError,
}: {
  onCreated: (name: string, secret: string) => void
  onError: (e: string | null) => void
}) {
  // Dev-mode bootstrap: no auth is configured yet, so this is very likely the
  // FIRST key, and creating it turns auth on. A non-admin first key would lock
  // this browser out of every admin view the moment it does — so default the
  // admin box to checked here, and warn loudly if the operator unchecks it.
  // Once auth is on (authenticated=true), this is an ordinary admin minting a
  // key, and non-admin is the sensible default, so the box starts unchecked.
  const bootstrapping = identity.value != null && !identity.value.authenticated
  const [name, setName] = useState('')
  const [home, setHome] = useState('')
  const [defaultNS, setDefaultNS] = useState('')
  const [admin, setAdmin] = useState(bootstrapping)
  const [busy, setBusy] = useState(false)

  const submit = async (e: Event) => {
    e.preventDefault()
    const target = name.trim()
    if (!target || busy) return
    setBusy(true)
    onError(null)
    try {
      const created = await api.createKey({
        name: target,
        home: home.trim() || undefined,
        default_namespace: defaultNS.trim() || undefined,
        disabled: false,
        admin,
      })
      setName('')
      setHome('')
      setDefaultNS('')
      setAdmin(false)
      onCreated(created.name, created.secret)
    } catch (err) {
      onError(err instanceof Error ? err.message : String(err))
    } finally {
      setBusy(false)
    }
  }

  return (
    <>
    <form onSubmit={submit} style={{ display: 'flex', gap: '8px', flexWrap: 'wrap', alignItems: 'center' }}>
      <input
        class="input"
        placeholder="name, e.g. ci-bot"
        value={name}
        onInput={(e) => setName((e.target as HTMLInputElement).value)}
        style={{ flex: '1 1 160px' }}
      />
      <input
        class="input"
        placeholder="home namespace (optional)"
        value={home}
        onInput={(e) => setHome((e.target as HTMLInputElement).value)}
        style={{ flex: '1 1 160px' }}
      />
      <input
        class="input"
        placeholder="default namespace (optional)"
        value={defaultNS}
        onInput={(e) => setDefaultNS((e.target as HTMLInputElement).value)}
        style={{ flex: '1 1 160px' }}
      />
      <label
        class="hint"
        title="Grant this key the admin capability (manage keys and server defaults, like MEMINI_API_KEY)"
        style={{ display: 'flex', alignItems: 'center', gap: '6px', flex: '0 0 auto', cursor: 'pointer' }}
      >
        <input type="checkbox" checked={admin} onChange={(e) => setAdmin((e.target as HTMLInputElement).checked)} />
        admin
      </label>
      <button class="btn primary" type="submit" disabled={busy || !name.trim()}>
        {busy && <span class="spinner" style={{ width: '14px', height: '14px' }} />}
        <IconKey />
        Create key
      </button>
    </form>
    {bootstrapping && !admin && (
      <div class="banner warn" role="status" style={{ marginTop: '8px' }}>
        Creating a non-admin first key locks this browser out of the admin views (Keys, Config,
        server defaults) the moment auth turns on. Recovery then needs the <code>MEMINI_API_KEY</code>{' '}
        env key or the <code>memini key</code> CLI. Leave <strong>admin</strong> checked unless you
        have one of those on hand.
      </div>
    )}
    </>
  )
}

// SecretModal shows a freshly generated secret exactly once, matching the
// CLI's "save this now" warning: the server never stores or re-displays it.
function SecretModal({
  name,
  secret,
  verb,
  onClose,
}: {
  name: string
  secret: string
  verb: 'created' | 'rotated'
  onClose: () => void
}) {
  const [copied, setCopied] = useState(false)

  const copy = () => {
    Promise.resolve(navigator.clipboard?.writeText(secret))
      .then(() => {
        setCopied(true)
        setTimeout(() => setCopied(false), 1200)
      })
      .catch(() => {
        /* clipboard unavailable; the secret is still visible to copy by hand */
      })
  }

  return (
    <>
      <div class="scrim" onClick={onClose} />
      <div
        class="panel panel-pad"
        role="dialog"
        aria-modal="true"
        aria-label={`Secret for ${name}`}
        style={{
          position: 'fixed',
          top: '50%',
          left: '50%',
          transform: 'translate(-50%, -50%)',
          zIndex: 50,
          width: 'min(520px, 90vw)',
        }}
      >
        <div class="section-h">
          <h2>
            Key {verb}: {name}
          </h2>
        </div>
        <div class="banner err" role="alert" style={{ marginBottom: '12px' }}>
          This secret will not be shown again — save it now.
        </div>
        <div style={{ display: 'flex', gap: '8px', marginBottom: '16px' }}>
          <input class="input mono" readOnly value={secret} onFocus={(e) => (e.target as HTMLInputElement).select()} />
          <button class="btn" type="button" onClick={copy} aria-label={copied ? 'Copied' : 'Copy secret'}>
            {copied ? <IconCheck /> : <IconCopy />}
            {copied ? 'Copied' : 'Copy'}
          </button>
        </div>
        <button class="btn primary" type="button" onClick={onClose}>
          Done
        </button>
      </div>
    </>
  )
}
