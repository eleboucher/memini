# Access control for a team

You have a shared memini and more than one person, and you want to answer three
questions cleanly: who is this caller, who can manage keys, and who can see the
admin UI as an admin. This guide walks a full team setup end to end. It is the
task-oriented companion to [api-keys.md](../api-keys.md), which is the reference
for every mechanism it uses.

The shape is always the same:

1. A **break-glass** `MEMINI_API_KEY` in a secret, set once and rarely touched.
2. **One admin key per human** who administers the server.
3. **Non-admin keys** for every agent, CI runner, and integration.
4. Keys managed either imperatively (the CLI or the UI against the store) or
   declaratively (a `MEMINI_API_KEYS_FILE` in your GitOps repo).

Admin is a per-key attribute, orthogonal to whether a key is human or a bot. The
whole point of this guide is that "admin" means "can manage keys and server
defaults", not "is a person", and you assign it deliberately.

## Step 1: the break-glass env key

Set `MEMINI_API_KEY` once, from a secret, and treat it as the credential you
reach for only when named-key administration has gone wrong (nobody left with
admin, a botched keys file, a first-key lockout). It authenticates with no
principal, so the self-guard can never lock it out, and it is the recovery path
that always works.

Generate a strong value and store it wherever your other secrets live. On
Kubernetes with the bundled chart, reference your own Secret (the chart creates
none):

```yaml
# charts/memini/values.yaml
app:
  env:
    MEMINI_API_KEY:
      valueFrom:
        secretKeyRef:
          name: memini-auth
          key: api-key
```

```sh
kubectl create secret generic memini-auth \
  --from-literal=api-key="$(openssl rand -hex 32)"
```

You do not strictly have to run one: a deployment can live entirely on named
admin keys minted through the CLI. But keeping a break-glass key (or reliable
CLI access to the store) is what turns a lockout into a five-second fix rather
than a rebuild. Do not hand this key to agents or people for day-to-day use;
issue them their own named keys below.

## Step 2: one admin key per human

Every person who administers the server gets their own named key with
`admin: true`. Their name lands in `metadata.author` on anything they write, the
activity log shows who granted or revoked what, and you can rotate or revoke one
human without touching anyone else. There are three ways to mint one; pick
whichever fits how you run the server.

**From the UI.** Sign in with an admin credential, open **Keys**, fill in the
create form, and leave the **admin** checkbox checked. The new secret is shown
exactly once. In dev mode (a fresh server with no auth configured yet) the
checkbox defaults to checked precisely so your first key is an admin; see
[Step 6](#step-6-what-each-role-sees).

**From curl**, as any existing admin (the env key here):

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

**From the CLI**, which talks to the store directly and needs no running server
or env key at all (this is how a no-env-key deployment mints its first admin):

```console
$ memini key add robin --home personal/robin --default-namespace acme --admin
Secret (save this now — it is not stored and cannot be shown again):
k9f2c8a1b3d47e6a0c1d2e3f4a5b6c7d8e9f0a1b2c3d4e5f6a7b8c9d0e1f2a3b4

NAME   HOME            DEFAULT NS  CREATED               DISABLED  ADMIN
robin  personal/robin  acme        2026-07-13T18:22:04Z  false     true
```

Each human then points their agent at the shared server with their **own** key,
not the break-glass key:

```sh
# client (robin's agent host)
export MEMINI_BASE_URL=https://memini.internal.example
export MEMINI_API_KEY=<robin's named admin key>
```

Because robin's key is bound to `personal/robin` server-side, robin's personal
memories follow them across every repo without exporting `MEMINI_HOME` anywhere
(a bound key's home wins over the header; see
[api-keys.md](../api-keys.md#home-binding-vs-default-namespace-identity-vs-context)).

## Step 3: non-admin keys for agents and CI

Everything that is not a human administrator gets a **non-admin** key. A CI
runner, a per-agent worker, an integration gateway: they read and write memories
all day, and they have no business reaching `/v1/keys` or the server defaults.
`admin` defaults to `false`, so you get this by simply not asking for admin:

```console
$ memini key add ci --default-namespace acme
$ memini key add reviewer-bot --default-namespace acme/phoenix
```

A non-admin key is still a full read/write credential for any namespace (a key
is [identity, not authorization](../api-keys.md#keys-are-identity-not-authorization)).
What it cannot do is manage other keys. If it tries, it gets a verbatim `403`:

```json
{
  "error": "admin credential required: this endpoint needs the admin env key (MEMINI_API_KEY) or an API key with admin=true"
}
```

Two per-key bindings are worth setting on these, both covered in full in
[api-keys.md](../api-keys.md):

- **`default_namespace`** gives an agent a landing namespace when it sends no
  `X-Memini-Namespace` header (the header always wins when present).
- **Per-key `settings`** pin behavior for that one credential. A CI runner that
  should not pollute memory with captured turns, for example:

  ```console
  $ curl -sS -X PATCH http://localhost:8080/v1/keys/ci \
      -H "Authorization: Bearer $MEMINI_API_KEY" \
      -H 'Content-Type: application/json' \
      -d '{"settings": {"capture_turns": false, "recall_limit": 5}}'
  ```

## Step 4: the GitOps file-keys pattern

If you manage config declaratively, put the keys in a
`MEMINI_API_KEYS_FILE` checked into your manifests repo, SOPS-encrypted at rest.
This is the reviewable form: a key change is a pull request, not an ad-hoc API
call. One admin key per human, non-admin for the bots, admin carried as the
`admin: true` bool:

```yaml
# api-keys.yaml, referenced by MEMINI_API_KEYS_FILE. SOPS-encrypt at rest.
keys:
  # Humans: admin, so they can manage keys and edit server defaults from the UI
  # or curl without ever reaching for the break-glass env key.
  - name: robin
    secret: "correct-horse-battery-staple"
    home: personal/robin
    default_namespace: acme
    admin: true
  - name: kit
    hash: "b9f195c5cc7ef6afadbfbc42892ad47d3b24c6bc94bb510c4564a90a14e8b799"
    home: personal/kit
    default_namespace: acme
    admin: true

  # Bots: NOT admin. Read and write memories, cannot touch /v1/keys.
  - name: ci
    secret: "another-secret-entirely"
    default_namespace: acme
  - name: reviewer-bot
    secret: "a-third-secret"
    default_namespace: acme/phoenix
    settings:
      capture_turns: false
      recall_limit: 5
```

The file is loaded once at boot with fail-loud validation (a duplicate name, a
bad hash, an invalid namespace, and the server refuses to start naming the
offending entry). There is no live reload, which suits GitOps: a change to the
file restarts the pod anyway. File keys are **immutable at runtime**, so every
`/v1/keys` mutation against one returns `409`; you change a file key (including
its admin state) by editing the file and rolling the deployment.

Mount the file from a Secret and point the server at it. With the bundled chart,
add a `secret`-type persistence volume and set the env var:

```yaml
# charts/memini/values.yaml
app:
  env:
    MEMINI_API_KEYS_FILE: /etc/memini/api-keys.yaml
persistence:
  api-keys:
    enabled: true
    type: secret
    name: memini-api-keys # your SOPS-decrypted Secret, key: api-keys.yaml
    globalMounts:
      - path: /etc/memini/api-keys.yaml
        subPath: api-keys.yaml
        readOnly: true
```

```sh
# the Secret this mounts (in practice created by your SOPS/GitOps pipeline)
kubectl create secret generic memini-api-keys \
  --from-file=api-keys.yaml=./api-keys.yaml
```

For Compose, mount the file and set the same variable:

```yaml
# compose.yaml
services:
  memini:
    environment:
      MEMINI_API_KEYS_FILE: /etc/memini/api-keys.yaml
    volumes:
      - ./api-keys.yaml:/etc/memini/api-keys.yaml:ro
```

A file entry that shares a name with a table row wins at auth time (file is
checked before the table), and the server logs a shadow warning at boot so an
operator is not left wondering why a table-managed key stopped authenticating.

## Step 5: the rotation ceremony

Rotation generates a fresh secret and invalidates the old one immediately, while
preserving everything else about the key, including its admin state. It is how
you cycle a credential on a schedule or after a suspected leak.

**Rotating someone else's key** is the ordinary case. As an admin:

```console
$ curl -sS -X POST http://localhost:8080/v1/keys/ci/rotate \
    -H "Authorization: Bearer $MEMINI_API_KEY"
```

The response carries the new `secret` once. Hand it to the owner, who updates
their `MEMINI_API_KEY`. The old secret stops working the instant the rotate
returns, so coordinate the swap or expect a gap.

**Rotating your own key** is deliberately allowed (unlike demoting, disabling, or
deleting yourself, which the self-guard blocks). Rotation is a credential
refresh handed back to the prover of the old secret, not a loss of capability,
so admin and every binding carry across:

```console
$ curl -sS -X POST http://localhost:8080/v1/keys/robin/rotate \
    -H "Authorization: Bearer $ROBIN_SECRET"
```

The catch: the old secret is dead the moment this returns, so whatever you are
using has to adopt the new `secret` for its **next** request. In the admin UI
this is seamless; it swaps the session token before any refetch, so rotating the
key you are signed in with keeps you signed in and just shows you the new secret
once. A script has to capture the returned `secret` and use it going forward, or
its very next call 401s.

**File keys rotate by editing the file**, not through this API (the rotate call
409s a file key). Change the `secret`/`hash` in `api-keys.yaml` and roll the
deployment.

## Step 6: what each role sees

The admin UI stays fully navigable for everyone; the admin-gated views render
purpose-built locked states for non-admins rather than hiding. Three roles:

- **Admin (a named admin key, or the env key).** Full access. **Keys** lists and
  manages every key; **Config** edits server defaults; the create form offers
  the admin checkbox and per-row grant/revoke. `identity.admin` is `true`.

- **Non-admin (a named non-admin key).** Everything except the two admin
  surfaces. **Keys** and **Config**'s defaults render a locked state naming the
  signed-in key and pointing at how to get admin (sign in with an admin key, or
  mint one with `memini key add --admin`). Reads, search, activity, scopes, and
  the health tools all work. `identity.admin` is `false`.

- **Dev mode (no auth configured).** A fresh server with no `MEMINI_API_KEY`,
  no keys, and no keys file treats every request as an unauthenticated admin, so
  you can bootstrap. The top bar shows a persistent amber **no auth** chip, and
  the Keys create form defaults the admin checkbox to checked (with an inline
  warning if you uncheck it, because a non-admin first key locks this browser
  out the moment auth turns on). Create your first admin key here and auth turns
  on immediately.

Sign-in is a one-time paste per browser: the token is verified against
`GET /v1/self`, then kept in that browser's `localStorage`. If the key it runs
on is later revoked or rotated out from under the session, the next request 401s
and the UI drops back to the sign-in screen with a "your session ended" notice.
Logout (under **UI settings** > Session) clears the stored token. See
[web-ui.md](../operations/web-ui.md) for the full login flow and the
`localStorage` caveat.

## When it goes wrong

The self-guard stops a single admin from locking itself out in one call, but it
cannot stop two admins from demoting each other down to zero named admins, nor
stop you from editing the last `admin: true` out of the keys file. If `/v1/keys`
starts 403ing every named key, recover with the break-glass env key or the CLI:

```console
$ curl -sS -X PATCH http://localhost:8080/v1/keys/robin \
    -H "Authorization: Bearer $MEMINI_API_KEY" -d '{"admin": true}'

# or, with no env key at all, against the store directly:
$ memini key add robin --admin
```

The same recovery covers the first-key lockout (a non-admin first key created in
dev mode with no env key). Both are the deliberate fail-closed design: key
management is never a surface a non-admin credential can grant itself back into.
See [api-keys.md](../api-keys.md#the-last-admin-footgun-and-recovery) for the
full model.
