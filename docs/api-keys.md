# API keys

memini supports multiple named API keys, each optionally bound to a **home**
namespace (identity) and a **default** namespace (context), instead of a
single shared bearer token. This doc covers what a key is, how the two
namespace bindings differ, the secret lifecycle, the declarative file format,
bootstrap and lockout behavior, and attribution. It's orthogonal to
[scopes.md](scopes.md) (how a namespace's read set is composed once a
request's namespace/home are resolved — a key's bindings are one of the
inputs to that resolution) and to [tiers.md](tiers.md) (what a memory's tier
means, unaffected by which key wrote it).

## Keys are identity, not authorization

A memini API key answers "who is this caller" (a name, plus optional home and
default namespace bindings) — it does **not** scope what that caller can read
or write. Any valid key — admin, named, or file-sourced — can read or write
any namespace, the same as the single shared `MEMINI_API_KEY` token always
could. Binding a key to a home namespace makes memini use that namespace by
default for `visibility:"personal"` writes and the read cascade's home leg;
it is a convenience default, not a fence. If you need hard namespace
isolation between tenants, that's a deployment-topology decision (separate
memini instances/stores), not something an API key's bindings enforce.

## Three kinds of key

| Kind  | Configured via                                                     | Mutable via API/CLI?                                  | Typical use                                                     |
| ----- | ------------------------------------------------------------------ | ----------------------------------------------------- | --------------------------------------------------------------- |
| Admin | `MEMINI_API_KEY` env var                                           | n/a (one shared value)                                | Server operator; the only key that can manage other keys        |
| Named | `memini key add` / `POST /v1/keys`, stored in the `api_keys` table | Yes — add/rotate/disable/delete                       | Per-person or per-integration credentials, imperatively managed |
| File  | `MEMINI_API_KEYS_FILE` (declarative YAML), loaded once at boot     | No — immutable via the API; edit the file and restart | GitOps-managed fleets, SOPS-encrypted secrets                   |

A request authenticates against these in order: admin key first (constant-time
compare), then the file keys (by hash), then the table (by hash). A key found
in the file or table but disabled is rejected outright — it never falls
through to a lower-precedence check. See `internal/apiauth.Config.Authenticate`
for the exact precedence and edge cases (e.g. what happens with no token
presented at all).

## Home binding vs default namespace: identity vs context

Two fields can be set per key — `home` and `default_namespace` — and they
resolve with **opposite precedence** against the request's own headers. This
asymmetry is deliberate, not an oversight:

| Binding             | Header               | Precedence when both are set           | Why                                                                                                                                                |
| ------------------- | -------------------- | -------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------- |
| `default_namespace` | `X-Memini-Namespace` | **header wins**                        | Namespace resolution is _context_ — the caller picks it per request; the key's default only fills in when the caller sent nothing.                 |
| `home`              | `X-Memini-Home`      | **key's home wins, header is ignored** | Home resolution is _identity_ — a bound key's home is who the caller is, so a client sending a different (or stale) home header can't override it. |

A bound key doesn't reject a conflicting `X-Memini-Home` header as an error —
it silently ignores it and logs the mismatch once at debug level (`X-Memini-Home
ignored: request key is bound to a home namespace`). Sending the header is
harmless; it just never takes effect for a key with `home` set. An **unbound**
key (no `home` configured) falls through to ordinary header-driven resolution,
identical to the admin key.

**Worked example.** Key `kit` is created with `home: personal/kit` and
`default_namespace: acme`. A client sends `X-Memini-Namespace: acme/phoenix`
and `X-Memini-Home: personal/someone-else`:

| Field     | Resolves to    | Why                                                                 |
| --------- | -------------- | ------------------------------------------------------------------- |
| namespace | `acme/phoenix` | header present → header wins over the key's `default_namespace`     |
| home      | `personal/kit` | key is bound → key's `home` wins, `X-Memini-Home` header is ignored |

If that same client instead sends no `X-Memini-Namespace` header at all, the
namespace resolves to the key's `default_namespace`, `acme`.

## Secret lifecycle

- **Shown once.** Creating or rotating a key returns the plaintext secret
  exactly once, in that response body (CLI stdout, or the REST/UI create/rotate
  response). It is never stored and cannot be recovered or redisplayed —
  only its SHA-256 hash is persisted (`store.APIKey.Hash`).
- **Rotation preserves unspecified fields.** Re-running `memini key add
<name>` (or `POST /v1/keys/{name}/rotate`) against an existing name
  generates a fresh secret and hash but keeps `CreatedAt`, `home`,
  `default_namespace`, and `Disabled` exactly as they were, unless a flag/field
  is explicitly passed to change it. A key disabled during incident response
  is never silently re-enabled by a later rotation — `--disabled=false` (or
  `disabled: false` in the PATCH body) must be explicit.
  `PATCH /v1/keys/{name}` follows the same preserve-unspecified contract for
  updating `home`/`default_namespace`/`disabled` without rotating the secret.
- **File keys are immutable via the API.** A key sourced from
  `MEMINI_API_KEYS_FILE` can't be rotated, disabled, or deleted through the CLI
  or REST/UI — those calls 409. The file is the source of truth; edit it and
  restart the server.

## Declarative file: `MEMINI_API_KEYS_FILE`

Set `MEMINI_API_KEYS_FILE` to a path and memini loads it once at boot as a set
of keys with a fixed identity — good for GitOps-managed fleets where keys
should be reviewable in a pull request rather than minted ad hoc. There is no
live reload: a change to the file takes effect on the next restart (a GitOps
rollout typically restarts the pod on every file change anyway).

```yaml
keys:
  # A key identified by a pre-computed hash. Use this form when the secret
  # itself lives elsewhere and should never touch this file in plaintext.
  - name: kit
    hash: "b9f195c5cc7ef6afadbfbc42892ad47d3b24c6bc94bb510c4564a90a14e8b799" # sha256 of the secret
    home: personal/kit
    default_namespace: acme

  # A key identified by a plaintext secret. Allowed because the file itself
  # is the secret store — e.g. SOPS-encrypted at rest in a GitOps repo, only
  # ever decrypted onto the pod's filesystem at deploy time. memini hashes it
  # once at load and never keeps the plaintext in memory afterward.
  - name: ci
    secret: "correct-horse-battery-staple"
    disabled: false

  # disabled: true keeps a name+entry around (e.g. mid-rotation) without it
  # authenticating anything.
  - name: retired-bot
    secret: "no-longer-valid"
    disabled: true
```

Each entry needs a unique `name` and **exactly one** of `hash` or `secret`;
`home`/`default_namespace`/`disabled` are optional. Validation is **fail-loud**
at boot: malformed YAML, a missing name, both or neither of `hash`/`secret`, a
hash that isn't 32 bytes of hex, a duplicate name, two entries sharing a
secret, or an invalid `home`/`default_namespace` all refuse to start the
server, naming the offending entry (never echoing the hash or secret itself).

If a file entry shares a name with an existing `api_keys` table row, the file
entry wins at auth time (file is checked before the table) — the server logs
a **shadow warning** at boot listing which table keys are shadowed this way,
so an operator isn't left wondering why a table-managed key stopped
authenticating.

For GitOps: check the file into your manifests repo with secrets encrypted at
rest (e.g. [SOPS](https://github.com/getsops/sops)), decrypted only onto the
pod's filesystem at deploy time. Prefer the `hash:` form when a secret is
minted and distributed by tooling that never needs the plaintext to reach the
manifests repo at all.

## Bootstrapping and the lockout

With **no `MEMINI_API_KEY` set** and **no keys yet** (an empty `api_keys`
table and no `MEMINI_API_KEYS_FILE`), the server runs in dev/bootstrap mode:
every request is allowed unauthenticated, including `/v1/keys` itself. This is
the intended way to create your first key from the UI: open the **Keys**
view, create a key, and auth is enforced immediately afterward (no cache lag —
see [Revocation lag](#revocation-lag) below for the one caveat).

> [!WARNING]
> **Post-bootstrap lockout.** Once that first key exists and no
> `MEMINI_API_KEY` (admin key) is configured, `/v1/keys` becomes unreachable
> through REST or the UI: any named key gets a 403 ("admin key required"),
> and a request with no token at all is now rejected too, since the table is
> no longer empty. There is no way back into key management through the API
> at that point. This is fail-closed by design — key management is
> deliberately an admin-only surface, never something a named credential can
> grant itself. Avoid it by either setting `MEMINI_API_KEY` before or
> immediately after creating the first key, or by managing keys entirely
> through the `memini key` CLI (which talks to the store directly and is
> never gated by this rule) if you don't want to run an admin key at all.

## Attribution

A memory written by a request authenticated with a **named** key (table or
file) stamps that key's name into `metadata.author`, unless the caller
already set `metadata.author` explicitly on the write — the stamp only fills
an absence, it never overwrites an explicit value. The **admin** key
authenticates with no principal at all, so it never stamps an author. This
applies uniformly across the REST and MCP write paths.

## Knobs

| Knob                          | Default | Description                                                                                                                                                                                                                                                                        |
| ----------------------------- | ------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `MEMINI_API_KEY`              | —       | The **admin/bootstrap key**: a bearer token that authenticates with no principal at all, required as the highest-precedence check, and the only credential that can manage other keys via `/v1/keys` (see [Bootstrapping](#bootstrapping-and-the-lockout)). Also gates `/metrics`. |
| `MEMINI_API_KEYS_FILE`        | —       | Path to a declarative YAML file of named keys, loaded once at boot (fail-loud validation). See [Declarative file](#declarative-file-memini_api_keys_file).                                                                                                                         |
| `home` (per-key)              | unbound | The key's bound personal namespace — identity, overrides `X-Memini-Home`. See [Home binding vs default namespace](#home-binding-vs-default-namespace-identity-vs-context).                                                                                                         |
| `default_namespace` (per-key) | unset   | The namespace applied when the request sends no `X-Memini-Namespace` header — context, the header always wins. Same section as above.                                                                                                                                              |
| `disabled` (per-key)          | `false` | Rejects the key outright at auth time without deleting it (e.g. incident response, mid-rotation holding pattern).                                                                                                                                                                  |

## Revocation lag

Disabling, rotating, or deleting a specific **already-existing** key takes
effect immediately, on every server process, regardless of who made the
change (CLI or REST/UI) — the per-request credential lookup
(`GetAPIKeyByHash`) is a live, uncached query.

The lag that exists is narrower: whether auth is enforced **at all** in
dev/bootstrap mode depends on a cached "does the `api_keys` table hold any
row" reading, refreshed at most every **10 seconds**
(`apiauth.Config`'s `keyTableCacheTTL`). REST/UI mutations invalidate this
cache immediately after a write (`apiauth.Config.Invalidate()`), so creating
the first key (or deleting the last one) through the running server itself
takes effect right away. The gap is the **CLI-vs-server process** case:
running `memini key add <name>` (creating the very first key) or
`memini key rm <name>` (deleting the last remaining one) from the CLI against
a store a separate, already-running server process also serves has no way to
signal that server's in-memory cache — so that server can take up to 10
seconds to start enforcing auth (after the first key) or to relax back to
dev mode (after the last key is removed). This is an accepted tradeoff
(avoiding a table query on every unauthenticated request in the common,
no-keys-yet case), not a bug — restart the server, or drive the mutation
through its own REST API/UI instead of the CLI, if you need the transition
to be instantaneous.
