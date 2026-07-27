# API keys

memini supports multiple named API keys, each optionally bound to a **home**
namespace (identity) and a **default** namespace (context), instead of a
single shared bearer token. This doc covers what a key is, the **admin
attribute** that gates key management, the **read-only attribute** that refuses
every write, how the two namespace bindings differ, the secret lifecycle, the
declarative file format, bootstrap and lockout behavior, and attribution. If you
are setting up a team from scratch, the task-oriented
[access control guide](guides/access-control.md) walks the whole thing end to
end; this page is the reference it builds on. It's orthogonal to
[scopes.md](scopes.md) (how a namespace's read set is composed once a
request's namespace/home are resolved — a key's bindings are one of the
inputs to that resolution) and to [tiers.md](tiers.md) (what a memory's tier
means, unaffected by which key wrote it).

## Keys are identity, not isolation

A memini API key answers "who is this caller" (a name, plus optional home and
default namespace bindings) — it does **not** scope which namespaces that caller
can reach. Any valid key — admin, named, or file-sourced — can read **any**
namespace, the same as the single shared `MEMINI_API_KEY` token always could.
Binding a key to a home namespace makes memini use that namespace by default for
`visibility:"personal"` writes and the read cascade's home leg; it is a
convenience default, not a fence. If you need hard namespace isolation between
teams, that's a deployment-topology decision (separate memini instances/stores),
not something an API key's bindings enforce.

A key carries exactly **two** authorization bits, both deliberately narrow, and
both orthogonal to the namespace bindings above:

| Bit         | What it changes                                                                                                             | Default |
| ----------- | --------------------------------------------------------------------------------------------------------------------------- | ------- |
| `admin`     | Unlocks key management (`/v1/keys` CRUD) and the server-wide settings defaults (`/v1/settings/defaults`), and nothing else. | `false` |
| `read_only` | Refuses **every** mutating request, across both the REST and MCP surfaces. Reads are untouched.                             | `false` |

They are independent, and the combination is meaningful: an `admin` +
`read_only` key is an auditor who can enumerate keys and inspect server defaults
but change neither. See [The admin attribute](#the-admin-attribute) and
[The read-only attribute](#the-read-only-attribute) for the full models.

> [!WARNING]
> `read_only` bounds what a key can **change**, not what it can **see**. A
> read-only key can still send any `X-Memini-Namespace` it likes and read every
> namespace on the server. For an unattended agent running against an untrusted
> branch, that reading surface is the thing to think about — read-only removes
> the risk of it corrupting your memory, not the risk of it seeing something.
> Restricting what a credential can read is a deployment-topology decision, the
> same as team isolation above.

## Three kinds of key

A credential comes from one of three **sources**. The source decides how a key
is created and whether it can change at runtime. The two authorization bits are
separate attributes, orthogonal to the source: a named key or a file key can be
an admin and/or read-only, and the env key is always admin and never read-only.

| Kind              | Configured via                                                     | Mutable via API/CLI?                                 | Admin?                   | Read-only?                   | Typical use                                                                   |
| ----------------- | ------------------------------------------------------------------ | ---------------------------------------------------- | ------------------------ | ---------------------------- | ----------------------------------------------------------------------------- |
| Env (break-glass) | `MEMINI_API_KEY` env var                                           | n/a (one shared value)                               | Always                   | Never                        | The operator's always-on recovery credential; authenticates with no principal |
| Named             | `memini key add` / `POST /v1/keys`, stored in the `api_keys` table | Yes: add/rotate/disable/delete, plus flip either bit | Optional (`--admin`)     | Optional (`--read-only`)     | Per-person or per-integration credentials, imperatively managed               |
| File              | `MEMINI_API_KEYS_FILE` (declarative YAML), loaded once at boot     | No: immutable via the API; edit the file and restart | Optional (`admin: true`) | Optional (`read_only: true`) | GitOps-managed fleets, SOPS-encrypted secrets                                 |

The env key is no longer the _only_ credential that can manage other keys: any
key with the admin attribute can. It stays special in one way that earns the
"break-glass" name: it authenticates with no principal at all, so the
self-guard (below) can never lock it out, and it is the one credential that
always works when every named admin has managed to demote itself. It also gates
two operator surfaces that named admin keys do **not** unlock: `/metrics` and
verbose `/healthz` (see [The metrics/healthz asymmetry](#the-metricshealthz-asymmetry)).

A request authenticates against these in order: env key first (constant-time
compare), then the file keys (by hash), then the table (by hash). A key found
in the file or table but disabled is rejected outright — it never falls
through to a lower-precedence check. See `internal/apiauth.Config.Authenticate`
for the exact precedence and edge cases (e.g. what happens with no token
presented at all).

## The admin attribute

Every key is either an **admin** or it is not. Admin is a boolean the server
tracks per key (`store.APIKey.Admin`, `ApiKey.admin` on the wire), and it gates
exactly two surfaces:

- **`/v1/keys` CRUD** (list, create, update, rotate, delete): managing other
  keys, including granting and revoking admin.
- **`/v1/settings/defaults`** (GET and PUT): the server-wide behavior defaults
  layer (see [env-vars.md](reference/env-vars.md#4-behavior-settings-layered-server-data)).

Three classes of caller pass that gate: the env `MEMINI_API_KEY`, dev/bootstrap
mode (no auth configured at all), and a named or file key with `admin: true`.
Everything else is a non-admin, and hitting either surface returns a verbatim
`403`:

```json
{
  "error": "admin credential required: this endpoint needs the admin env key (MEMINI_API_KEY) or an API key with admin=true"
}
```

That string is deliberately different from the old `"admin key required"` so any
out-of-tree tooling that matched the previous text fails loudly rather than
silently mis-classifying a response. Nothing else changes for a non-admin key:
it still reads and writes any namespace exactly as before (a key is
[identity, not isolation](#keys-are-identity-not-isolation)), unless it also
carries [read_only](#the-read-only-attribute).

### Checking whether the current key is an admin

`GET /v1/self` reports the caller's effective admin capability in
`identity.admin`. A named admin key sees:

```json
{
  "identity": {
    "authenticated": true,
    "admin": true,
    "key_name": "robin",
    "home": "personal/robin"
  },
  "settings": { "...": "fully resolved, every field present" },
  "settings_sources": { "...": "per-field default/global/key provenance" }
}
```

Dev mode (no auth configured) reports `"authenticated": false, "admin": true`
with no `key_name`; the env break-glass key reports `"authenticated": true,
"admin": true` with no `key_name` (neither authenticates as a named principal).
A non-admin named key reports `"admin": false`, and that is exactly what the
admin UI reads to render its locked states instead of probing a 403.

### Creating an admin key

Pass `"admin": true` when creating the key. As the env key (or any admin):

```console
$ curl -sS -X POST http://localhost:8080/v1/keys \
    -H "Authorization: Bearer $MEMINI_API_KEY" \
    -H 'Content-Type: application/json' \
    -d '{"name": "robin", "home": "personal/robin", "default_namespace": "acme", "admin": true}'
```

```json
{
  "name": "robin",
  "home": "personal/robin",
  "default_namespace": "acme",
  "created_at": "2026-07-13T18:22:04Z",
  "disabled": false,
  "admin": true,
  "source": "db",
  "secret": "k9f2c8a1b3d47e6a0c1d2e3f4a5b6c7d8e9f0a1b2c3d4e5f6a7b8c9d0e1f2a3b4"
}
```

The `secret` is shown exactly once, here (see [Secret lifecycle](#secret-lifecycle)).
`"admin": false` (or omitting `admin`) creates a plain key.

The CLI mints one against the store directly, which is how a deployment with no
env key at all still gets its first admin:

```console
$ memini key add robin --home personal/robin --default-namespace acme --admin
Secret (save this now — it is not stored and cannot be shown again):
k9f2c8a1b3d47e6a0c1d2e3f4a5b6c7d8e9f0a1b2c3d4e5f6a7b8c9d0e1f2a3b4

NAME   HOME            DEFAULT NS  CREATED               DISABLED  ADMIN
robin  personal/robin  acme        2026-07-13T18:22:04Z  false     true
```

### Granting and revoking admin on an existing key

`PATCH /v1/keys/{name}` with just `{"admin": true}` promotes an existing key;
`{"admin": false}` demotes it. Omitting the field leaves the current capability
untouched (the same preserve-unspecified contract as `home`/`disabled`; see
[Secret lifecycle](#secret-lifecycle)). Neither rotates the secret.

```console
$ curl -sS -X PATCH http://localhost:8080/v1/keys/ci-bot \
    -H "Authorization: Bearer $MEMINI_API_KEY" \
    -H 'Content-Type: application/json' \
    -d '{"admin": true}'
```

```json
{
  "name": "ci-bot",
  "default_namespace": "acme",
  "created_at": "2026-06-01T09:15:00Z",
  "disabled": false,
  "admin": true,
  "source": "db"
}
```

Revoking is the mirror image, `{"admin": false}`. On the CLI, a rotation carries
the flag explicitly: `memini key add ci-bot --admin=false` demotes while
generating a fresh secret; a bare `memini key add ci-bot` rotates and
**preserves** whatever admin state the key already had (see
[Secret lifecycle](#secret-lifecycle)).

### The self-guard

An admin can lock the whole surface against everyone in a single request by
demoting, disabling, or deleting its own key. The server refuses that one move.
A **named** admin key acting on **itself** cannot:

- demote itself (`PATCH {"admin": false}` on its own name),
- disable itself (`PATCH {"disabled": true}` on its own name), or
- delete itself (`DELETE` on its own name).

Each returns a `409` naming the escape hatch. Demote or disable:

```console
$ curl -sS -X PATCH http://localhost:8080/v1/keys/robin \
    -H "Authorization: Bearer $ROBIN_SECRET" \
    -d '{"admin": false}'
```

```json
{
  "error": "api key \"robin\" cannot demote or disable itself; use the admin env key (MEMINI_API_KEY) or another admin key"
}
```

Delete:

```console
$ curl -sS -X DELETE http://localhost:8080/v1/keys/robin \
    -H "Authorization: Bearer $ROBIN_SECRET"
```

```json
{
  "error": "api key \"robin\" cannot delete itself; use the admin env key (MEMINI_API_KEY) or another admin key"
}
```

The guard is narrow on purpose. The env key and dev mode authenticate with no
principal, so "self" never matches them: they are the break-glass escape the
409 points back at. And a named admin may still demote, disable, or delete a
**different** admin key. That leaves the last-admin footgun open across two
keys (below), which is a recoverable state, not one the guard prevents.

### Rotate-self is allowed

Rotation is the one self-targeting action that is **not** blocked.
`POST /v1/keys/{name}/rotate` against your own key succeeds and returns the new
secret, because rotation is a credential refresh handed back to the prover of
the old secret, not a capability loss. Admin, home, default namespace, and
disabled state all carry across the hash swap unchanged:

```console
$ curl -sS -X POST http://localhost:8080/v1/keys/robin/rotate \
    -H "Authorization: Bearer $ROBIN_SECRET"
```

```json
{
  "name": "robin",
  "home": "personal/robin",
  "default_namespace": "acme",
  "created_at": "2026-07-13T18:22:04Z",
  "disabled": false,
  "admin": true,
  "source": "db",
  "secret": "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0c1d2e3f4a5b6c7d8e9f0a1b2"
}
```

The old secret stops authenticating immediately, so a caller rotating the key
it is signed in with must adopt the returned `secret` for its next request. The
admin UI does this for you (it swaps the session token before any refetch); a
script has to capture the new `secret` and use it going forward, or its very
next call 401s.

### The grant/revoke activity event

Minting an admin key, or flipping the flag on an existing one, is a privileged
act, so it is audited. Each grant or revoke writes an activity event (kind
`settings`, reusing the settings-change event rather than adding a new kind)
carrying the target key's name and its new state:

```json
{
  "kind": "settings",
  "detail": { "key_name": "ci-bot", "admin": true }
}
```

It appears in `GET /v1/activity` and the UI's **Activity** view. A no-op PATCH
(`admin: true` on a key that is already an admin) writes nothing; only a real
change is recorded. Creating a plain (non-admin) key writes no event either.

### A file with an admin human and non-admin agents

`MEMINI_API_KEYS_FILE` carries the attribute as an `admin: true` YAML bool,
defaulting to `false` when omitted. A realistic team file mixes one admin key
per human with non-admin keys for every agent and CI runner:

```yaml
# api-keys.yaml, referenced by MEMINI_API_KEYS_FILE. SOPS-encrypt at rest.
keys:
  # A human operator. admin: true, so robin manages keys and edits server
  # defaults from the UI or curl without ever needing the env key.
  - name: robin
    secret: "correct-horse-battery-staple"
    home: personal/robin
    default_namespace: acme
    admin: true

  # A second human, admin via a pre-computed hash so the secret never touches
  # this file in plaintext (mint it elsewhere, paste only the sha256).
  - name: kit
    hash: "b9f195c5cc7ef6afadbfbc42892ad47d3b24c6bc94bb510c4564a90a14e8b799"
    home: personal/kit
    default_namespace: acme
    admin: true

  # A CI runner. NOT an admin: it reads and writes memories but cannot reach
  # /v1/keys. admin defaults to false, so just leave it off.
  - name: ci
    secret: "another-secret-entirely"
    default_namespace: acme

  # A per-agent key, also non-admin, with its own behavior override.
  - name: reviewer-bot
    secret: "a-third-secret"
    default_namespace: acme/phoenix
    settings:
      capture_turns: false
      recall_limit: 5
```

Because file keys are immutable at runtime (every `/v1/keys` mutation against a
file key returns `409`; see [Secret lifecycle](#secret-lifecycle)), you change a
file key's admin state by editing the file and restarting, not through the API
or UI.

### The last-admin footgun and recovery

The self-guard stops a single key from locking itself out, but it cannot stop
**two** admins from demoting each other down to zero named admins, one call each.
If that happens (or if you edit the last `admin: true` out of the keys file),
`/v1/keys` is unreachable through the API and the UI: every named key is now a
non-admin, so every request 403s. Two recovery paths always work, and neither
goes through the gated surface:

1. **The env break-glass key.** If `MEMINI_API_KEY` is set, present it as the
   bearer. It authenticates with no principal, so the self-guard never applied
   to it and it can re-grant admin to any key:

   ```console
   $ curl -sS -X PATCH http://localhost:8080/v1/keys/robin \
       -H "Authorization: Bearer $MEMINI_API_KEY" -d '{"admin": true}'
   ```

2. **The `memini key` CLI.** It talks to the store directly and is never gated
   by the admin rule, so it works even with no env key configured at all:

   ```console
   $ memini key add robin --admin   # re-mints robin's secret AND re-grants admin
   ```

   (This rotates robin's secret as a side effect; if you only want to re-grant
   without a new secret, use the env key path above instead.)

This is the same fail-closed philosophy as the bootstrap lockout
([below](#bootstrapping-and-the-lockout)): key management is deliberately a
surface a non-admin credential can never grant itself back into. Keep at least
one of {an env key, CLI access to the store} on hand and the footgun is a
five-second fix.

### The metrics/healthz asymmetry

Two operator surfaces authenticate **only** against the env `MEMINI_API_KEY`,
never against a named admin key:

- **`/metrics`** on the main HTTP port (when `MEMINI_METRICS_ADDR` is empty).
- **`GET /healthz?verbose=1`**, whose per-dependency error detail is gated;
  an absent or wrong token degrades to the plain `healthz` body rather than
  erroring.

This is deliberate. Both are gated at the HTTP server layer (`bearerAuth` /
`validBearer`), which is upstream of the apiauth principal resolution and knows
only the single env key, not the `api_keys` table or the file. A named admin key
manages other keys and edits server defaults, but it does **not** pull `/metrics`
or verbose `/healthz` with its own bearer. If you run named admins and want
`/metrics`, either move it to its own unauthenticated in-cluster listener with
`MEMINI_METRICS_ADDR` (the recommended shape; see
[configuration.md](reference/configuration.md#memini_metrics_addr)) or set an env
key for the scraper to use. The verbose `/healthz` detail is likewise an
operator-only view, not something a per-person admin key is meant to unlock.

## The read-only attribute

Every key is either **read-only** or it is not. Read-only is a boolean the server
tracks per key (`store.APIKey.ReadOnly`, `ApiKey.read_only` on the wire), and it
refuses every mutating request the key makes — on **both** HTTP surfaces, REST
and MCP. Reads are entirely untouched.

The case it exists for is an unattended agent: a CI job's LLM that should recall
project context but must never write, mutate, or delete a memory. A bad write
from a CI run is invisible until it poisons someone's recall weeks later, and
nobody is watching the job when it happens.

```console
$ memini key add ci-agent --read-only
Secret (save this now — it is not stored and cannot be shown again):
7f3c…

NAME      HOME  DEFAULT NS  CREATED               DISABLED  ADMIN  READ ONLY
ci-agent  -     -           2026-07-26T22:17:29Z  false     false  true
```

A refused write returns a verbatim `403`:

```json
{
  "error": "read-only credential: API key \"ci-agent\" has read_only=true and cannot perform mutating requests"
}
```

### What counts as a read

The gate is an **allowlist**, not a denylist. A read-only key may issue:

- every `GET` and `HEAD`;
- `POST /v1/search` and `POST /v1/answer` — queries that need a request body, so
  they are POSTs despite mutating nothing (`/v1/answer` spends LLM tokens, but
  spending tokens is not mutating stored state);
- `POST /v1/handshake` — deliberately side-effect-free, and the call every client
  makes to resolve its namespace before doing anything else. Denying it would
  make a read-only credential unusable rather than merely unprivileged.

Everything else is a write, **including any endpoint added in a future version**.
That is the point of an allowlist: a new mutating endpoint is refused until
someone consciously classifies it, so forgetting over-restricts (a loud 403)
instead of silently granting write access. A spec-derived test enforces the
classification in CI.

Two consequences worth knowing:

- **Dry runs are writes.** `POST /v1/namespaces/move` and `/split` are refused
  even with `dry_run: true` — the preview posts to the mutating endpoint, so a
  read-only key cannot preview a move either.
- **`PUT /v1/self/settings` is a write.** A read-only key cannot change its own
  per-key behavior settings. An admin has to do it for it.

### Reads still leave a trace

Serving a read still bumps the memory's access counters (`store.Reinforce`,
which feeds ranking and episodic→durable promotion) and still appends to the
activity log. That is internal relevance bookkeeping, not caller-facing
mutation: **read-only bounds what the caller can change, not whether serving a
read leaves a trace.** A read-only agent's recalls still make the memories it
uses rank better, which is usually what you want.

### On the MCP surface

MCP is served on the same auth path, so the same credential behaves the same
way. The write tools — `memory_remember`, `memory_update`, `memory_forget` —
stay **listed** for a read-only session and are refused when called, with an
error result the agent can read:

```text
read-only credential: this API key has read_only=true and cannot call
"memory_remember", which modifies stored memories. Do not retry — reads
(memory_recall, memory_briefing, memory_get, memory_list, memory_history)
still work.
```

The refusal is an error tool _result_, not a protocol error, precisely so the
model sees it as output and stops retrying rather than treating it as a
transient failure.

### Clients skip writes rather than collecting 403s

`GET /v1/self` and `POST /v1/handshake` both report `identity.read_only`, so a
client learns the capability in the one call it already makes. The bundled
Claude Code / Codex plugin uses it to **skip** its capture and telemetry writes
outright. Without that, an unattended agent posts a capture every turn, eats a
403, and logs it to stderr forever — which reads to an operator as "memory is
broken" rather than "this key cannot write".

An older server omits the field. Clients must read absent as **writable**, never
as "skip", or they silently stop saving against a server that predates it.

### The self-guard

A named key cannot make **itself** read-only — `PATCH /v1/keys/<its own name>`
with `read_only: true` returns `409`:

```json
{
  "error": "api key \"robin\" cannot make itself read-only; a read-only credential cannot reach this endpoint to undo it, so use the admin env key (MEMINI_API_KEY), another admin key, or `memini key add robin --read-only`"
}
```

This is stricter than the admin self-demote guard for a reason: once the flag is
set, the read-only gate refuses `/v1/keys` itself, so the key could never lift
it. Going the other way — clearing `read_only` on itself — is allowed, because
that direction is a restoration and is only reachable while the key is still
writable.

The env key and dev mode have no principal, so "self" never matches them; they
are the escape hatch this points back at.

### Rotation preserves it

`memini key add <existing-name> --read-only` is not needed on a rotation: an
omitted `--read-only` carries the stored value forward, exactly like `--admin`
and `--disabled`. Rotating a CI credential's secret never quietly hands it write
access back. Pass `--read-only=false` explicitly to lift the restriction.

### Read-only does not imply non-admin

The two bits are independent. `--admin --read-only` mints an auditor: it can
`GET /v1/keys` and `GET /v1/settings/defaults`, and it can change neither. In the
admin UI that session gets a persistent `read-only` chip and every write control
disabled.

### The grant/revoke activity event

Imposing or lifting `read_only` writes a `settings` activity event carrying
`{key_name, read_only}`, so the feed answers "when did this credential stop being
able to write, and who did it". Deleting a read-only key deliberately writes
nothing: that removes a restriction from a credential that no longer exists, so a
`read_only: false` event would read as the opposite of what happened.

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
identical to the env key.

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
  `default_namespace`, `Disabled`, and `Admin` exactly as they were, unless a
  flag/field is explicitly passed to change it. A key disabled during incident
  response is never silently re-enabled by a later rotation, and an admin key
  is never silently demoted by one: `--disabled=false` / `--admin=false` (or
  `disabled: false` / `admin: false` in the PATCH body) must be explicit.
  `PATCH /v1/keys/{name}` follows the same preserve-unspecified contract for
  updating `home`/`default_namespace`/`disabled`/`admin`/`settings` without
  rotating the secret — see [The admin attribute](#the-admin-attribute) for the
  admin field's self-guard and [Per-key behavior settings](#per-key-behavior-settings)
  below for what `settings` does.
- **File keys are immutable via the API.** A key sourced from
  `MEMINI_API_KEYS_FILE` can't be rotated, disabled, or deleted through the CLI
  or REST/UI — those calls 409. The file is the source of truth; edit it and
  restart the server.

## Per-key behavior settings

A key's identity (`home`/`default_namespace`) is one axis; its **behavior**
(whether it captures turns, how much a briefing injects, and so on) is a
separate one, layered as built-in defaults ← the server's global defaults
← this key's own override. Three endpoints touch that per-key layer:

| Endpoint                | Who can call it                                              | What it does                                                                                                                                         |
| ----------------------- | ------------------------------------------------------------ | ---------------------------------------------------------------------------------------------------------------------------------------------------- |
| `PATCH /v1/keys/{name}` | Admin only (see [The admin attribute](#the-admin-attribute)) | Sets or clears `settings` on any table-sourced key by name, alongside `home`/`default_namespace`/`disabled`/`admin`.                                 |
| `PUT /v1/self/settings` | The key itself (a **named** key only)                        | Full-replace of the caller's own `settings`. No merge/patch variant: `GET /v1/self`, edit the result, `PUT` it back.                                 |
| `GET /v1/self`          | Any authenticated caller                                     | This key's identity plus its fully-resolved `settings` (every field present) and `settings_sources` (per-field `default`/`global`/`key` provenance). |

`PUT /v1/self/settings` is how a named key manages its own behavior without
needing an admin credential at all — the natural fit for "I want my own recall
limit lower" without touching anyone else's. A **named** admin key can still use
it (it is a named principal like any other), so admins are not forced to manage
their own settings through the defaults layer. It 403s only for the env
break-glass key and for dev mode (auth disabled): neither authenticates as a
**named** principal, so there is no "self" to update — use
`PUT /v1/settings/defaults` for the global layer instead (see [scopes.md](scopes.md) and
[env-vars.md](reference/env-vars.md#4-behavior-settings-layered-server-data)
for how the layers stack). It 409s for a `MEMINI_API_KEYS_FILE`-sourced key,
matching every other file-key mutation: that key's `settings` comes from the
file and is immutable at runtime.

A field a key never set (in the file, via `PATCH`, or via `PUT /v1/self/settings`)
keeps inheriting the server's global defaults — setting `settings: {}` (or
omitting a field on a `PUT`) is how you go back to inheriting rather than
pinning a value forever.

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
    # Optional per-key behavior override (config-handshake redesign) — the
    # same ClientSettings a named key can also set for itself via
    # PUT /v1/self/settings (see below). Unset fields keep inheriting the
    # server's global defaults.
    settings:
      session_digest: false
      recall_limit: 5

  # A key identified by a plaintext secret. Allowed because the file itself
  # is the secret store — e.g. SOPS-encrypted at rest in a GitOps repo, only
  # ever decrypted onto the pod's filesystem at deploy time. memini hashes it
  # once at load and never keeps the plaintext in memory afterward.
  - name: ci
    secret: "correct-horse-battery-staple"
    disabled: false

  # A read-only credential: it can recall everything it could before, and the
  # server refuses its every write. This is the shape to hand an unattended
  # agent — a CI job's LLM that should read project context but never change it.
  - name: ci-agent
    secret: "another-secret-from-your-secret-store"
    read_only: true

  # disabled: true keeps a name+entry around (e.g. mid-rotation) without it
  # authenticating anything.
  - name: retired-bot
    secret: "no-longer-valid"
    disabled: true
```

Each entry needs a unique `name` and **exactly one** of `hash` or `secret`;
`home`/`default_namespace`/`disabled`/`admin`/`settings` are optional (`admin`
defaults to `false`; see [The admin attribute](#the-admin-attribute)).
Validation is
**fail-loud** at boot: malformed YAML, a missing name, both or neither of
`hash`/`secret`, a hash that isn't 32 bytes of hex, a duplicate name, two
entries sharing a secret, an invalid `home`/`default_namespace`, or a
`settings` block carrying an unknown field or a value failing
`ClientSettings.Validate` all refuse to start the server, naming the
offending entry (never echoing the hash or secret itself). A file key's
`settings` is immutable at runtime through the API, same as every other field
on a file key — edit the file and restart to change it.

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
the intended way to create your first key.

**Create the first key with `admin: true`.** In dev mode there is no other
admin around to promote it afterward, so the first key has to arrive already
able to manage keys. The admin UI knows this: in dev mode its create form
**defaults the admin checkbox to checked**, and shows an inline warning if you
uncheck it. On the CLI or curl, pass the flag yourself:

```console
$ memini key add robin --home personal/robin --admin
```

The moment that key exists, auth turns on (no cache lag through the running
server itself, which invalidates its bootstrap cache on the write; see
[Revocation lag](#revocation-lag) for the one CLI-vs-server caveat). From then
on only an admin credential reaches `/v1/keys`, and robin is one.

> [!WARNING]
> **The non-admin-first-key lockout.** If you create the first key **without**
> admin, and no `MEMINI_API_KEY` is configured, `/v1/keys` becomes unreachable
> through REST and the UI: that key is a non-admin, so it gets a `403`
> (`"admin credential required: ..."`), and a request with no token is rejected
> too now that the table is non-empty. There is no way back in through the API.
> This is fail-closed by design: key management is never something a non-admin
> credential can grant itself. Recover exactly as for
> [the last-admin footgun](#the-last-admin-footgun-and-recovery) above, with the
> env break-glass key or the `memini key` CLI
> (`memini key add <name> --admin`), which talks to the store directly and is
> never gated by this rule.

The env `MEMINI_API_KEY` is the **break-glass** answer to both lockouts. Set it
and you always hold a principal-less admin credential the self-guard can never
lock out, independent of whatever named admins exist. You do not have to run
one, since a deployment can live entirely on named admin keys minted through the
CLI, but keeping one configured (or keeping CLI access to the store) is what
turns either lockout into a five-second fix rather than a rebuild.

## Attribution

A memory written by a request authenticated with a **named** key (table or
file, admin or not) stamps that key's name into `metadata.author`, unless the
caller already set `metadata.author` explicitly on the write — the stamp only
fills an absence, it never overwrites an explicit value. The env break-glass key
and dev mode authenticate with no principal at all, so they never stamp an
author. The admin _attribute_ makes no difference here: a named admin key stamps
its name exactly like any other named key. This applies uniformly across the
REST and MCP write paths.

## Knobs

| Knob                          | Default | Description                                                                                                                                                                                                                                                                                                                                                                                                                              |
| ----------------------------- | ------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `MEMINI_API_KEY`              | —       | The **break-glass admin/bootstrap key**: a bearer token that authenticates with no principal, sits at the highest-precedence check, and can manage other keys via `/v1/keys`. No longer the _only_ admin credential (any key with `admin: true` qualifies; see [The admin attribute](#the-admin-attribute)), but the one the self-guard can never lock out. Also gates `/metrics` and verbose `/healthz`, which named admin keys do not. |
| `MEMINI_API_KEYS_FILE`        | —       | Path to a declarative YAML file of named keys, loaded once at boot (fail-loud validation). See [Declarative file](#declarative-file-memini_api_keys_file).                                                                                                                                                                                                                                                                               |
| `home` (per-key)              | unbound | The key's bound personal namespace — identity, overrides `X-Memini-Home`. See [Home binding vs default namespace](#home-binding-vs-default-namespace-identity-vs-context).                                                                                                                                                                                                                                                               |
| `default_namespace` (per-key) | unset   | The namespace applied when the request sends no `X-Memini-Namespace` header — context, the header always wins. Same section as above.                                                                                                                                                                                                                                                                                                    |
| `disabled` (per-key)          | `false` | Rejects the key outright at auth time without deleting it (e.g. incident response, mid-rotation holding pattern).                                                                                                                                                                                                                                                                                                                        |
| `admin` (per-key)             | `false` | Unlocks `/v1/keys` CRUD and `/v1/settings/defaults`. See [The admin attribute](#the-admin-attribute).                                                                                                                                                                                                                                                                                                                                    |
| `read_only` (per-key)         | `false` | Refuses every mutating request across REST and MCP; reads are untouched. See [The read-only attribute](#the-read-only-attribute).                                                                                                                                                                                                                                                                                                        |

## Revocation lag

Disabling, rotating, or deleting a specific **already-existing** key takes
effect immediately, on every server process, regardless of who made the
change (CLI or REST/UI) — the per-request credential lookup
(`GetAPIKeyByHash`) is a live, uncached query.

The lag that exists is narrower: whether auth is enforced **at all** in
dev/bootstrap mode depends on a cached "does the `api_keys` table hold any
row" reading, refreshed at most every **10 seconds**
(`apiauth`'s `keyTableCacheTTL` package-level const, consulted by
`Config.tableNonEmpty` — not a `Config` field). REST/UI mutations invalidate this
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
