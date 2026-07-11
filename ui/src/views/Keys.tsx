import { useState } from 'preact/hooks'
import { api } from '../api'
import { useAsync } from '../hooks'
import type { ApiKey } from '../types'
import { Loading, ErrorBanner, Empty } from '../components/States'
import { IconKey, IconTrash, IconRefresh, IconCopy, IconCheck } from '../icons'
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
  const { data, error, loading } = useAsync(() => api.listKeys(), [nonce])
  const keys = data ?? []

  const reload = () => {
    setMutErr(null)
    setNonce((n) => n + 1)
  }

  // The server's 403 body is a fixed, distinctive message (see
  // internal/api/rest/apikeys.go's requireAdminOrDev) — matching on it lets
  // this view show a purpose-built empty state instead of a raw error banner.
  if (error?.includes('admin key required')) {
    return (
      <div class="view">
        <Empty
          title="Admin key required"
          hint="Managing API keys needs the server's admin key. Set it under Settings (API token), then reload this page."
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
  onChanged: () => void
  onError: (e: string | null) => void
  onSecret: (secret: string) => void
}

function KeyRow({ k, onChanged, onError, onSecret }: RowProps) {
  const [armed, setArmed] = useState(false) // delete confirmation (two-click, MemoryDrawer's pattern)
  const [busy, setBusy] = useState(false)
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

  const rotate = async () => {
    setBusy(true)
    onError(null)
    try {
      const rotated = await api.rotateKey(k.name)
      onSecret(rotated.secret)
    } catch (e) {
      onError(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy(false)
    }
  }

  return (
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
      {isFile ? (
        <span class="chip" title="Managed via MEMINI_API_KEYS_FILE — edit the file to change this key">
          declarative
        </span>
      ) : (
        <>
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
  )
}

function CreateKeyForm({
  onCreated,
  onError,
}: {
  onCreated: (name: string, secret: string) => void
  onError: (e: string | null) => void
}) {
  const [name, setName] = useState('')
  const [home, setHome] = useState('')
  const [defaultNS, setDefaultNS] = useState('')
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
      })
      setName('')
      setHome('')
      setDefaultNS('')
      onCreated(created.name, created.secret)
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
      <button class="btn primary" type="submit" disabled={busy || !name.trim()}>
        {busy && <span class="spinner" style={{ width: '14px', height: '14px' }} />}
        <IconKey />
        Create key
      </button>
    </form>
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
