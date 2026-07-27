# Homelab team

Three to five developers, one self-hosted memini, everything on your own
hardware. Postgres with VectorChord for the store, an embeddings endpoint, an
optional chat model, and the MCP server reached over HTTP instead of spawned per
session.

The shape of the problem changes once memini is shared. On a laptop the only
question is "does recall work". Here you also have to answer: who is this caller,
whose memories are these, and who can read the admin token.

## The store

SQLite is single-writer and lives on one machine's disk. A shared server wants
Postgres:

```sh
# server (the memini process)
export MEMINI_BACKEND=postgres
export MEMINI_POSTGRES_DSN='postgres://memini:secret@db:5432/memini?sslmode=disable'
export MEMINI_EMBED_BASE_URL=http://embeddings:80/v1
export MEMINI_EMBED_MODEL=bge-m3
export MEMINI_EMBED_DIMS=1024          # must match the model, see the note below
export MEMINI_METRICS_ADDR=:9090
```

memini refuses to start with `MEMINI_BACKEND=postgres` and no DSN, so that pair
is hard to get half-right. The dimensionality is not: `MEMINI_EMBED_DIMS` must
equal what the model at `MEMINI_EMBED_BASE_URL` actually serves, and a mismatch
corrupts the store instead of erroring (see
[`MEMINI_EMBED_DIMS`](../reference/configuration.md#memini_embed_dims)). It is
worth double-checking before the first write, because a fix afterwards means a
fresh store.

[`compose.yaml`](../../compose.yaml) in the repo root brings the whole stack up
locally: `vchord-postgres`, a CPU `text-embeddings-inference` server, and memini
wired to both. It is the fastest way to see the moving parts before you commit to
a deployment. For Kubernetes, [`charts/memini`](../../charts/memini) has the
production shape already: a StatefulSet for SQLite by default, with comments on
every value explaining how to switch it to a Postgres-backed Deployment.

`MEMINI_METRICS_ADDR` moves `/metrics` onto its own listener. Keep that port
in-cluster and it needs no bearer token; leave the variable unset and `/metrics`
stays on the main port, where `MEMINI_API_KEY` gates it.

## The LLM is optional, and one thing hinges on it

memini runs the full memory lifecycle without a chat model (see the [solo
guide](solo-laptop.md#no-llm-is-a-real-mode-not-a-degraded-one)). Adding one buys
background consolidation, `POST /v1/answer`, `MEMINI_RERANK=llm`, and the
`memory_answer` MCP tool:

```sh
# server (the memini process)
export MEMINI_LLM_BASE_URL=http://llm:8080/v1
export MEMINI_LLM_MODEL=qwen3-30b
export MEMINI_LLM_API=openai          # or "anthropic"
```

The detail worth knowing before someone files a bug: **`memory_answer` is only
registered as an MCP tool when an LLM is configured.** Without one, agents never
see the tool at all, so "why can't Claude answer from memory" has a boring
answer. It is deliberate (a tool that would error on every call is worse than an
absent one), but it is invisible from the agent's side.

## One key per person

A single shared bearer token tells you nothing about who wrote what. Named keys
do, and each one can carry a home namespace, which is how a developer's personal
memories follow them without every client having to export the right env var.

The declarative file is the right form for a homelab, because it lives in the
same repo as the rest of your config. Give each human an **admin** key
(`admin: true`) so they can manage keys and edit server defaults without reaching
for the break-glass env key, and leave agents and CI **non-admin** (the default):

```yaml
# api-keys.yaml, referenced by MEMINI_API_KEYS_FILE
keys:
  - name: kit
    secret: "correct-horse-battery-staple" # SOPS-encrypt this file at rest
    home: personal/kit
    default_namespace: acme
    admin: true
  - name: robin
    hash: "b9f195c5cc7ef6afadbfbc42892ad47d3b24c6bc94bb510c4564a90a14e8b799"
    home: personal/robin
    default_namespace: acme
    admin: true
  - name: ci
    secret: "another-secret" # non-admin: reads and writes, cannot manage keys
```

Admin is a per-key attribute, not the env key's exclusive property anymore. With
one admin key per human, `MEMINI_API_KEY` becomes **break-glass**: set it and
keep it in a secret, reach for it only when named-key administration has locked
itself out. The [access control guide](access-control.md) walks the whole team
setup (break-glass key, admin per human, non-admin per agent, rotation, and what
each role sees) end to end.

```sh
# server (the memini process)
export MEMINI_API_KEYS_FILE=/etc/memini/api-keys.yaml
```

The file is read once at boot, with fail-loud validation: a duplicate name, a
bad hash, an invalid namespace, and the server refuses to start naming the
offending entry. There is no live reload, which is fine for GitOps (a change to
the file restarts the pod anyway).

For an imperatively managed instance, the CLI does the same job against the
store's own key table:

```sh
memini key add kit   --home personal/kit   --default-namespace acme --admin
memini key add robin --home personal/robin --default-namespace acme --admin
memini key add ci    --default-namespace acme          # non-admin: no --admin
memini key ls                                          # NAME/HOME/DEFAULT NS/CREATED/DISABLED/ADMIN
```

`key add` prints the secret exactly once. Re-running it against an existing name
rotates the secret and preserves everything else, including the admin flag (a
bare `memini key add kit` never silently demotes or promotes; pass `--admin` /
`--admin=false` to change it).

Two rules that bite people, both covered in full in
[api-keys.md](../api-keys.md): a key is **identity, not isolation** (any valid key
can read any namespace, so key bindings are convenience, not a fence — and while
`--read-only` stops a key writing, nothing stops it reading), and a key's bound
`home` **beats** the `X-Memini-Home` header while its
`default_namespace` **loses** to the `X-Memini-Namespace` header. Home is who you
are; namespace is where you are working.

## Team-wide behavior defaults

Identity (keys, home namespaces) is one axis; **behavior** — whether turns get
captured, how much a briefing injects, and so on — is a separate one, and it is
now server data everyone's client resolves fresh instead of local config each
person sets up themselves. For a homelab managed the same way as everything
else here (GitOps, values checked into a repo), set it once as a **server** env
var and it applies to the whole team without anyone touching a client:

```sh
# server (the memini process)
export MEMINI_CLIENT_DEFAULTS='{"capture_turns":false,"recall_limit":5}'
```

This locks the global-defaults layer read-only — `PUT /v1/settings/defaults`
is refused with 409 while it is set, so a stray API call can't drift the
team's defaults out from under the values file. It is a complete no-op if
left unset (the KV-backed defaults apply as before), and a per-key
`settings` override (in `api-keys.yaml` above, or `PUT /v1/self/settings`)
still wins over it for anyone who needs to differ from the team default. The
Helm chart has a commented example in `values.yaml`; see
[`MEMINI_CLIENT_DEFAULTS`](../reference/configuration.md#memini_client_defaults)
for the full validation rules.

## Signing in to the UI

The admin UI is a real login now, and the served shell carries **no** credential:
it never contains `MEMINI_API_KEY`. Each person signs in once per browser by
pasting an API key (their own named admin key, ideally, not the break-glass env
key); it is verified against `GET /v1/self` and then kept in that browser's
`localStorage` and sent as a bearer on every `/v1` call. Serving `/` to an
anonymous request leaks nothing.

The security point to internalize is narrower than it used to be, but real: a
stored token lives in `localStorage`, which **any same-origin script can read**.
That is a much better position than the old behavior (the server used to embed
the admin key in the public HTML shell, so anyone who could `GET /` had it), and
the stored token can now be a per-person, non-admin-of-last-resort credential.
Still, isolate the UI if the origin is at all shared:

```sh
# server (the memini process)
export MEMINI_UI_ADDR=:8081        # UI on its own listener; expose only on a trusted LAN gateway
```

`MEMINI_UI_ADDR` moves the UI to a dedicated port. That port serves both the UI
and the API (the SPA needs same-origin `/v1`), so route it only where reaching it
already implies trust. The Helm chart does exactly this and gives the UI its own
service port.

The blunter option is `MEMINI_UI_ENABLED=false`, which runs memini headless as an
API and MCP service. Or leave the UI on the main port and simply never route that
port anywhere untrusted, which on a homelab is often the honest answer.

See [web-ui.md](../operations/web-ui.md) for the full login flow (first-run
bootstrap, logout, the dev-mode banner, rotate-self) and
[access-control.md](access-control.md) for what each role sees signed in.

## Namespace topology

Namespaces are a slash-separated tree, and reads cascade upward for free. A team
setup is three levels:

```
acme                 <- org root: org-wide durable facts
acme/phoenix         <- the project everyone works on
personal/kit         <- kit's home namespace, bound to kit's key
personal/robin       <- robin's home namespace
```

A recall running in `acme/phoenix` reads `acme/phoenix` at all tiers, then
`acme`'s durable memories, then the caller's home namespace's durable memories.
Nobody has to duplicate an org-wide convention into every project, and nobody
sees anyone else's home namespace. That is the ancestor cascade, and it is on by
default. [scopes.md](../scopes.md) is the full model, including how a write can be
routed up the chain with `visibility` and why episodic writes never leave the
project they were made in.

Each developer's client points at the shared server with their own key:

```sh
# client (your agent host)
export MEMINI_BASE_URL=https://memini.internal.example
export MEMINI_API_KEY=<kit's own named key, not the shared break-glass env key>
```

The plugin resolves the project namespace from the git remote and sends it as
`X-Memini-Namespace` on every call, so `acme/phoenix` need not be exported
anywhere. `MEMINI_HOME` need not be either, because kit's key is bound to
`personal/kit` server-side, and a bound key's home wins over whatever header a
client sends.

## Before you upgrade an older instance

Two environment variables from the pre-cascade scope model are now **fatal at
boot**: `MEMINI_GLOBAL_NAMESPACE` and `MEMINI_TENANT_SHARED`. The server refuses
to start rather than boot as though they were never set, because both encoded an
expectation of shared visibility that the ancestor cascade replaced outright, and
silently dropping that expectation would quietly change what every recall
returns.

If your rollout dies on boot with a message naming one of them, that is why. Run
`memini migrate scopes` and read [upgrading](../operations/upgrading.md). A
handful of other removed settings are merely ignored with a startup warning, so
check the boot log after any upgrade: a tuning value that quietly stopped
applying is exactly the kind of thing that shows up two weeks later as "recall
got worse".
