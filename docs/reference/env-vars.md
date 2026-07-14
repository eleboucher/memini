# Server variables vs client variables

memini has two sides, and both read `MEMINI_*` environment variables, but the
client side no longer reads its own `MEMINI_*` set to decide what to do on its
own — it reads a small **bootstrap** set to find and reach the server, then
lets the server hand back an identity, a namespace, and a fully-resolved set
of behavior settings on every connect. This page documents both sides: the
bootstrap variables, the four-layer client model those variables feed into,
the four names that mean different things depending on which side reads them,
and what got removed.

The **server** is the `memini` process. It reads the settings in the
[configuration reference](configuration.md), which control storage, embeddings,
retrieval and the background lifecycle.

The **client** is the plugin that runs inside your agent (the Claude Code hooks,
the opencode plugin, and so on). Its own environment surface is documented in
[`plugin/README.md`](../../plugin/README.md).

## The four layers

Every client — the Claude Code hooks, the opencode plugin, hermes, the Open
WebUI filter/tool — resolves the same four things, in the same order, the
moment it connects. Read top to bottom; each layer builds on the one above it.

### 1. Bootstrap (client-side env, read once)

The handful of variables that exist purely so the client can find the server
and prove who it is. Nothing here is server data — it never travels further
than the request the client is about to make.

| Variable                  | Purpose                                                                                                                                                                                                                                                                                                                          |
| ------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `MEMINI_BASE_URL`         | Where the server is. Defaults to `http://localhost:8080`.                                                                                                                                                                                                                                                                        |
| `MEMINI_API_KEY`          | The bearer token this client sends. Absent means an unauthenticated request.                                                                                                                                                                                                                                                     |
| `MEMINI_REQUIRE_HTTPS`    | Refuse to send `MEMINI_API_KEY` over plaintext HTTP to a non-loopback host (a hard throw, not a warning) — a client-side guard, `@memini/client`'s `assertBearerTransportSafe`.                                                                                                                                                  |
| `MEMINI_DEBUG`            | Verbose hook/client logging to stderr.                                                                                                                                                                                                                                                                                           |
| `MEMINI_AGENT`            | A per-agent suffix (e.g. `reviewer`), sent as a project fact so the server can nest a per-agent namespace segment under the resolved one.                                                                                                                                                                                        |
| `MEMINI_NAMESPACE`        | An explicit namespace, sent as a fact so the **server** can weigh it against a pin (see [Context](#3-context-namespace-resolution) below) — the client cannot make that call locally.                                                                                                                                            |
| `MEMINI_NAMESPACE_PREFIX` | A client-side override of the `namespace_prefix` behavior setting, sent as a fact. Prepended to the **derived** name (`<prefix>/<repo>`), so one credential can serve several tenants selected per shell/directory (e.g. a per-tree `.envrc`) — no pin, no second key. See [Behavior](#4-behavior-settings-layered-server-data). |
| `MEMINI_HOME`             | This caller's personal namespace, sent as `X-Memini-Home` on every request.                                                                                                                                                                                                                                                      |

Everything past this point is **server data**. The client sends what it knows
(bootstrap facts) and reads back what the server resolved; it does not derive
a namespace or a behavior setting on its own except when the server cannot be
reached at all (see each layer's "degraded" note).

### 2. Identity (per-key, resolved by the handshake)

Every request authenticates as something — the admin key, dev mode (no auth
configured), or a **named** API key (table- or file-sourced). A named key can
be bound to a `home` namespace, and that binding **wins over** whatever
`X-Memini-Home` the client sends: identity is who the caller _is_, not what it
_claims_ to be this request. An unbound key falls through to the header,
identical to the admin key. See [api-keys.md](../api-keys.md#home-binding-vs-default-namespace-identity-vs-context)
for the full asymmetry (namespace resolves the opposite way — the header wins
there, because namespace is context, not identity).

### 3. Context (namespace resolution)

The namespace a write lands in and a plain recall draws from. Resolved
**server-side**, from client-supplied facts, in this order:

```
1. pin                 <- /v1/pins, set by /memini:namespace; follows you across machines
2. MEMINI_NAMESPACE     <- sent as a fact; a pin still beats it
3. declared_namespace   <- gateway/integration callers with no meaningful cwd
4. derive               <- git remote > git toplevel > cwd basename (+ namespace_prefix / MEMINI_NAMESPACE_PREFIX, + MEMINI_AGENT)
5. key default_namespace
6. server default       <- MEMINI_DEFAULT_NAMESPACE / MEMINI_NAMESPACE (server env)
```

A **pin** is the replacement for the old per-machine override file: it is
server-side state, keyed by the project's git remote and/or toplevel path
(`PUT`/`DELETE /v1/pins`, or `/memini:namespace <ns>` / `--clear`). Because it
lives on the server rather than in a file on one machine, it follows you
across every machine you work from, and every client — hooks, MCP tools, the
Go CLI — resolves the same one. It beats `MEMINI_NAMESPACE` on purpose: an
exported `MEMINI_NAMESPACE` pins _every_ repo on the machine to one namespace,
and if the environment won, pinning one project would silently do nothing on
exactly the machines that need it most.

**There is no offline pinning.** Setting or clearing a pin needs the server
reachable, because the pin itself lives there. When it is not,
`/memini:namespace` says so and points at the one capability regression this
redesign introduces: export `MEMINI_NAMESPACE=<ns>` as a machine-local,
offline escape hatch until the server is back. It works exactly as before —
degraded resolution still honors it — it just cannot follow you to another
machine the way a pin does.

**Degraded (server unreachable):** the client falls back to `MEMINI_NAMESPACE`
if set, else local derivation from git/cwd facts alone — no pin, no key
default, no server default, because none of those exist without the server.
Every hook and command that shows this says so explicitly (`source: local-*`
or a `degraded` flag).

### 4. Behavior (settings, layered server data)

Every behavioral knob a client used to read as its own `MEMINI_*` variable —
whether to capture turns, how many memories to inject at session start, and so
on — is now **server data**, resolved fresh on every handshake:

```
built-in default
  ← global defaults      (PUT /v1/settings/defaults, or MEMINI_CLIENT_DEFAULTS server env)
    ← per-key settings    (PATCH /v1/keys/{name}, or PUT /v1/self/settings)
      ← client env var, IF SET    <- wins as a DEBUG override
```

The server resolves and returns the fully-merged result (`ClientSettings`) on
every `/v1/handshake` and `/v1/self`, with per-field provenance
(`settings_sources`: `default` / `global` / `key`). A client-side `MEMINI_*`
env var for the same knob (e.g. `MEMINI_SESSION_DIGEST`) still works, but it
is now explicitly a **local debug override**: it wins over whatever the server
resolved, for this one client, without touching the server's stored value for
anyone else. See [`plugin/README.md`](../../plugin/README.md) for the full
knob list and their wire names.

`MEMINI_CLIENT_DEFAULTS` is the GitOps-friendly counterpart to
`PUT /v1/settings/defaults`: a JSON-encoded `ClientSettings` object set as a
**server** env var, which becomes the global-defaults layer and **locks it
read-only** — `PUT /v1/settings/defaults` is refused with 409 while it is set,
so the environment is the single source of truth and cannot be silently
overridden through the API. See
[`MEMINI_CLIENT_DEFAULTS`](configuration.md#memini_client_defaults) and the
[homelab guide](../guides/homelab-team.md) for a values.yaml example.

`namespace_scope` is a server-resolved behavior setting with **no**
client-side debug override: there is no `MEMINI_NAMESPACE_SCOPE` (removed; see
below). It only takes effect through a live handshake; the degraded local
derivation path always behaves as `namespace_scope: repo`.

`namespace_prefix` is the exception: it **does** have a client override,
`MEMINI_NAMESPACE_PREFIX`, sent as a fact and honored the same way any client
env override is (it wins over the merged global/per-key value). It is prepended
to a **derived** namespace only — never to a verbatim `MEMINI_NAMESPACE` or a
gateway `declared_namespace` — giving `<prefix>/<repo>`. This is what lets a
single credential (even the admin env key, which has no per-key settings) serve
several tenants selected by directory: point a per-tree `.envrc` (or a shell
hook) at `export MEMINI_NAMESPACE_PREFIX=<tenant>` and every repo under that
tree resolves to `<tenant>/<repo>`, with the repo name derived from the git
remote so it stays stable across machines. Unlike the other two, it is also
applied in the degraded local path, so it keeps working with the server down.

## The four that mean two things

Four names are read on **both** sides, and mean different things depending on
which one reads them.

| Variable           | On the server                                                                                                                                                                                                                                                                               | On the client                                                                                                                                                                                      |
| ------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `MEMINI_API_KEY`   | The **break-glass** admin/bootstrap credential the server accepts. Setting it turns authentication on. No longer the only admin (named/file keys can carry `admin: true`), but the one that always works and cannot lock itself out; see [api-keys.md](../api-keys.md#the-admin-attribute). | The bearer token the client **sends**. Setting it does not turn anything on.                                                                                                                       |
| `MEMINI_NAMESPACE` | The **server default** — the last-resort fallback when nothing else in [context resolution](#3-context-namespace-resolution) applies (no pin, no header, no derivable facts, no key default).                                                                                               | A fact the client **sends**, so the server can weigh it against a pin. It no longer wins outright on its own — a pin beats it.                                                                     |
| `MEMINI_HOME`      | Read only by `memini mcp` (stdio has no headers, so there is nowhere else to get it).                                                                                                                                                                                                       | The caller's personal namespace, sent as `X-Memini-Home` — unless the authenticating key is bound to a `home`, which wins instead (see [Identity](#2-identity-per-key-resolved-by-the-handshake)). |
| `MEMINI_AGENT`     | Nothing directly — but the value the client sends as a project fact is what the server's own derivation nests under the resolved namespace.                                                                                                                                                 | Sent as a fact for the server to apply; also applied locally in the degraded fallback path when there is no server to ask.                                                                         |

The practical consequence of the overlap: exporting one of these in your shell
configures **both** the server you start from that shell and the agent you
start from it. That is usually what you want on a laptop, where the two are
the same machine and the same person. It is usually wrong on a shared server,
where `MEMINI_API_KEY` on the server means "demand this token" and on a
developer's machine means "send this token". Setting the same value in both
places is how you end up sharing one credential across a team instead of
issuing [named keys](../api-keys.md).

## Removed variables (warn-and-ignore)

Four client-side variables were retired by this redesign. They are **silently
ignored everywhere except one place**: `SessionStart` checks for them and, if
any is set, prints a single combined stderr line —

```
[memini] ignored removed env vars: MEMINI_URL, MEMINI_NAMESPACE_SCOPE (see docs/reference/env-vars.md)
```

— once per session-start, not on every hook (the hot-path hooks stay silent so
this never adds noise to a tool call).

| Variable                 | What it used to do                                               | Now                                                                                                                                                         |
| ------------------------ | ---------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `MEMINI_URL`             | Alias for `MEMINI_BASE_URL`.                                     | Removed. Set `MEMINI_BASE_URL`.                                                                                                                             |
| `MEMINI_TOKEN`           | Alias for `MEMINI_API_KEY`.                                      | Removed. Set `MEMINI_API_KEY`.                                                                                                                              |
| `MEMINI_MCP_URL`         | Documented as an MCP-endpoint override; never actually wired in. | Removed outright. The MCP endpoint is always `${MEMINI_BASE_URL}/mcp`.                                                                                      |
| `MEMINI_NAMESPACE_SCOPE` | Client-side `repo` vs `owner-repo` derivation choice.            | Moved server-side: the `namespace_scope` behavior setting (see [Behavior](#4-behavior-settings-layered-server-data)). No client env override exists for it. |

## Retired local state: overrides.json and config.json's tenantRoots

Two client-side files predate this redesign and are retired in favor of
server-side state:

- **`~/.config/memini/overrides.json`** — the old per-machine namespace
  override, keyed by git toplevel path. Replaced by **pins** (see
  [Context](#3-context-namespace-resolution)). Migration is automatic: the
  first time a project's handshake succeeds and reports no pin,
  `SessionStart` reads a matching entry (never writes or clears the file) and
  `PUT`s it to `/v1/pins`, printing one stderr line on success. For every
  project on a machine at once, run `/memini:namespace --migrate` — it prints
  a `key -> namespace -> status` table and, on full success, renames the file
  to `overrides.json.migrated` so it stops shadowing anything but stays
  around as a record. A partial failure leaves the file in place so a re-run
  retries only what did not land.
- **`~/.config/memini/config.json`**'s `tenantRoots`/`template` — an older
  cwd-to-tenant mapping a couple of integrations still read. There is no
  automatic translation for this one (it encodes a tenancy decision only a
  human can make): `/memini:namespace --migrate` detects it and prints the
  contents with instructions to recreate it either as a `namespace_prefix` on
  the relevant API keys (per-credential tenancy) or as explicit per-project
  pins.

Neither file is ever written by anything documented on this page except the
rename above. `overrides.json`'s **read** path stays in the client bundle on
purpose (three integrations — opencode, hermes, openwebui — still ship a
reader for it), so a staged rollout across a fleet does not lose anyone's
override mid-migration.

## Turning off session digests

`MEMINI_SESSION_DIGEST=0` (a client-side debug override — see
[Behavior](#4-behavior-settings-layered-server-data)) stops the lifecycle
hooks recording **activity**: "edited `auth.go` (3), ran `go test ./...`".
Those digests answer "what was I doing in this repo last week", which some
people want and some emphatically do not. If you only want memini to hold
durable facts, every session otherwise adds a memory that will never answer a
question and dilutes recall.

One switch covers all four write sites, because they are the same distilled
buffer: the `SessionEnd` digest, the `Stop` checkpoint, the `PreCompact`
rescue copy, and the `PostToolUse` buffering that feeds them.

It is easy to confuse with the knobs next to it, so:

| Knob                    | What it turns off                                                                                                          |
| ----------------------- | -------------------------------------------------------------------------------------------------------------------------- |
| `MEMINI_SESSION_DIGEST` | Activity records: what you edited and ran                                                                                  |
| `MEMINI_CAPTURE_TURNS`  | Each user/assistant turn, stored as episodic memory                                                                        |
| `MEMINI_INLINE_EXTRACT` | The directive asking the agent to save durable facts itself                                                                |
| `MEMINI_INJECT_DEDUPE`  | Suppression of duplicate per-file recall injections on `PreToolUse` (off means every tool call re-injects, even unchanged) |

They are independent. Turning digests off leaves the agent saving decisions
and conventions through `memory_remember` exactly as before. All four, like
every other behavior knob, can instead be set globally
(`PUT /v1/settings/defaults` / `MEMINI_CLIENT_DEFAULTS`) or per-key
(`PUT /v1/self/settings`) — the env var only ever overrides for the one
client it is exported in.

## Which side am I configuring?

Ask where the process runs.

- Editing a Compose file, a Helm values file, a systemd unit, or a Dockerfile?
  That is the **server**. Use the
  [configuration reference](configuration.md).
- Editing your shell profile, an MCP client config, or an agent's settings?
  That is the **client** — but past the bootstrap layer, you are more often
  editing server-stored state through `/memini:namespace`,
  `PUT /v1/self/settings`, or an admin action than exporting a variable at
  all. Use [`plugin/README.md`](../../plugin/README.md) for what remains
  client-side, and [api-keys.md](../api-keys.md) for per-key settings.

In the guides, every shell block says which side it is configuring, for exactly
this reason.

## A note on Windows

There is no `/proc` on Windows, so the MCP `headersHelper` cannot recover the
project directory from a live process the way it does on Linux/macOS on the
very first connect (before any hook has written its cache). On the first
connect only, this means auth-only headers go out — the bearer token, no
`X-Memini-Namespace` — and the server applies the calling key's
`default_namespace` (or the server default) until a hook has run once and
populated the per-session cache the helper reads on every connect after that.
This is a first-connect-only gap, not a persistent one.
