# Web UI

memini ships an embedded admin UI (Preact + Vite, compiled into the binary) and
serves it at `/`. There is no separate process to run. With the default config,
open `http://localhost:8080/`.

It is an **admin** UI, not a read-only viewer. It deletes memories, moves and
splits namespaces, creates and rotates API keys, and runs fsck and dedup. Treat
access to it as administrative access to the store.

## Authentication: you sign in, the shell carries no key

The UI is a real login now. The served shell is **credential-free**: it never
contains `MEMINI_API_KEY` or any bearer token, so serving `/` to an anonymous
request leaks nothing. Instead the SPA authenticates in the browser. You paste an
API key once at the sign-in gate, it is verified against `GET /v1/self` before it
is ever stored, and it then persists in that browser's `localStorage` and rides
every `/v1` call as `Authorization: Bearer ...`.

### First run: bootstrap in dev mode

A fresh server with no `MEMINI_API_KEY`, no `api_keys` rows, and no
`MEMINI_API_KEYS_FILE` runs in **dev/bootstrap mode**: every request is allowed
unauthenticated, so the UI opens straight to the dashboard with no key. The top
bar shows a persistent amber **no auth** chip so nobody mistakes it for a
locked-down server.

Bootstrap by creating your first key:

1. Open **Keys**. The create form's **admin** checkbox is **checked by default**
   in dev mode, because your first key has to be an admin (there is no other
   admin around to promote it later). Unchecking it shows an inline warning: a
   non-admin first key locks this browser out of the admin views the moment auth
   turns on.
2. Create the key. Its secret is shown exactly once, so save it.
3. Auth turns on immediately. The UI **auto-adopts** the new secret into the
   session so you stay signed in seamlessly (without this the very next request
   would 401 and drop you to the sign-in screen). From here the **no auth** chip
   is gone and you are signed in as your new admin key.

### Signing in, and out

On a server that already has auth configured, the UI opens on the **sign-in**
screen. Paste an API key and it is verified before being saved (a bad paste
leaves any previously stored credential untouched). The collapsed **Advanced**
section carries the API base URL, for pointing the UI at a remote memini (the
Settings view is unreachable until you are signed in, so a remote target is set
here).

Log out under **UI settings** > **Session**, which shows the credential you are
signed in as (with an **admin** badge when it is one) and a **Log out** button
that clears the stored token and returns you to the sign-in screen.

If the key your session runs on is revoked or rotated out from under you
mid-session, the next request 401s and the UI drops back to sign-in with a "your
session ended" notice, rather than leaving every view stuck on an error.

### What a non-admin sees

The nav stays fully visible for everyone; the two admin-gated views render locked
states rather than disappearing. Signed in with a **non-admin** key, **Keys** and
**Config**'s server-defaults tab show a purpose-built locked panel that names the
signed-in key and points at how to get admin (sign in with an admin key, or mint
one with `memini key add --admin`). Everything else, the reads, search, activity,
scopes, and the health tools, works normally. This is driven by
`identity.admin` from `GET /v1/self`, not by probing for a 403.

### Rotating the key you are signed in with

Rotating your own key from the **Keys** view is safe: the UI swaps the session
token to the freshly minted secret before anything refetches, so you stay signed
in and just see the new secret once. (A raw `POST /v1/keys/{name}/rotate` from a
script has to adopt the new secret itself, or its next call 401s.) Demoting,
disabling, or deleting your own key is blocked by the server's self-guard and
surfaces here as the `409` error; see
[api-keys.md](../api-keys.md#the-self-guard).

### The `localStorage` caveat, stated plainly

The token lives in `localStorage`, which means **any script running on the same
origin can read it**. That is a real consideration if you serve untrusted content
from the same host. It is still strictly better than the old behavior (serving
the admin key inside the public HTML shell to anyone who could so much as
`GET /`), and the token you store can now be a per-person, non-break-glass
credential rather than the shared admin key. Two ways to tighten it further:

- **Give the UI its own listener** with `MEMINI_UI_ADDR` (for example `:8081`)
  and route that port only to a trusted (LAN) gateway. The main port then carries
  only the API and MCP. The UI listener also serves `/v1`, so the same-origin SPA
  still works. This is what the Helm chart does by default.
- **Turn the UI off** with `MEMINI_UI_ENABLED=false` for a headless API/MCP
  service.

The [access control guide](../guides/access-control.md) walks the whole
per-person-key setup end to end.

## Views

Eleven views, in nav order.

| View           | Path          | What it is for                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                  |
| -------------- | ------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| **Namespaces** | `/namespaces` | The landing page. Every namespace as a nested box tree of "pods" (memory count, tier bar, last write). Click one to make it the active namespace. Per-pod: delete, or a drawer for namespace **move** and **split** (dry-run first).                                                                                                                                                                                                                                                                                                                                                                                                                                            |
| **Overview**   | `/`           | Read-only stats for the active namespace, or aggregated across all: totals, recalls, average importance, last write, expired/superseded/low-confidence counts, and the tier strata bar.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                         |
| **Browse**     | `/browse`     | Filterable list of up to 500 memories. Filters run server-side: tier, memory type, level, tags, metadata, created-after, accessed-after, sort, include-expired, include-superseded. The detail drawer is where **delete** and **reassign** live.                                                                                                                                                                                                                                                                                                                                                                                                                                |
| **Search**     | `/search`     | Hybrid recall against `/v1/search` with tier/tag/metadata filters, showing relevance scores and read set provenance ("in scope via ..."). It does not call `/v1/answer`.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                        |
| **Activity**   | `/activity`   | Paged event feed: recall, briefing, get, remember, update, forget, supersede, and the config kinds pin, unpin, settings. Each row shows the query, the memories served with rank and score, a "degraded" chip when a recall fell back to keyword-only, an **actor** chip naming who performed it (the API key name, or "admin key"/"open access" for the env key and dev mode), and the "why": a recall's **source** (`pretool`, `ui`, `mcp`, `answer`, ...) and a write's outcome (tier, auto-superseded, merge hint). Filter by event kind, tier, free text, or **actor** (a key-name box, with a datalist of keys for admins). This is the view for "who did what, and why". |
| **Graph**      | `/graph`      | Force-directed view, two modes. _Memories_: nodes are memories, edges are supersession (directed) and shared-tag affinity. _Namespaces_: nodes are namespaces, edges are the parent/child ancestor cascade plus stored links.                                                                                                                                                                                                                                                                                                                                                                                                                                                   |
| **Read set**   | `/scopes`     | The effective **read set** for the active namespace: every namespace a recall draws from, each tagged with why (primary, ancestor, home, link, call) and which tiers that leg contributes. Below it, the outgoing **links**, with add and delete. This is the view that answers "what can this namespace actually see".                                                                                                                                                                                                                                                                                                                                                         |
| **Keys**       | `/keys`       | Named API keys: list, create, rotate, enable/disable, grant/revoke admin, delete. New and rotated secrets are shown exactly once. Keys from `MEMINI_API_KEYS_FILE` are listed as "declarative" and are read-only here (the file is the source of truth). Needs an admin credential (the env key or an `admin: true` key); a non-admin sees a locked state naming its key.                                                                                                                                                                                                                                                                                                       |
| **Config**     | `/config`     | Server configuration and the global behavior-defaults layer. The defaults tab (`GET`/`PUT /v1/settings/defaults`) is admin-gated the same way **Keys** is, so a non-admin sees a locked state there too.                                                                                                                                                                                                                                                                                                                                                                                                                                                                        |
| **Health**     | `/health`     | Two manual maintenance tools. **fsck** purges expired memories, evicts short-term ones, and reports duplicate clusters. **dedup** collapses near-duplicates at a similarity threshold, and defaults to dry-run. Neither runs on its own.                                                                                                                                                                                                                                                                                                                                                                                                                                        |
| **Settings**   | `/settings`   | Client-side only, persisted to `localStorage`. A **Session** panel shows the credential this browser is signed in as (with an **admin** badge when it is one) and a **Log out** button; a **Connection** panel edits the API base URL (empty means same origin) and the namespace header name. There is no token field: the key is set once at the sign-in gate, not typed here. No server config is exposed or edited here.                                                                                                                                                                                                                                                    |

Change the active namespace with the switcher in the top bar. Several views also
have an "All namespaces" mode that fans the query out store-wide.

## The endpoints behind it

The UI is not backed by three read-only endpoints. It uses most of the REST
surface, including writes:

- `GET /v1/stats`, `GET /v1/namespaces`, `DELETE /v1/namespaces`
- `POST /v1/namespaces/move`, `POST /v1/namespaces/split` (both take `dry_run`)
- `GET /v1/namespaces/readset`
- `GET /v1/memories`, `GET /v1/memories/{id}`, `DELETE /v1/memories/{id}`,
  `POST /v1/memories/{id}/reassign`
- `POST /v1/search`
- `GET /v1/activity`
- `GET /v1/links`, `POST /v1/links`, `DELETE /v1/links?dst=...`
- `GET /v1/keys`, `POST /v1/keys`, `PATCH /v1/keys/{name}`,
  `DELETE /v1/keys/{name}`, `POST /v1/keys/{name}/rotate`
- `POST /v1/fsck`, `POST /v1/dedup`

The full contract is [`api/openapi.yaml`](../../api/openapi.yaml). Note what is
_not_ there: the UI cannot author a memory or edit a memory's content. Writing is
the agent's job.

## Building it

Sources live in [`ui/`](../../ui).

```sh
mise run ui       # build the embedded bundle into internal/api/ui/dist
mise run ui-dev   # dev server with HMR, proxies /v1 to a local memini on :8080
```

`mise run ui` also regenerates `ui/src/api-schema.gen.ts` from
`api/openapi.yaml`, so the UI's types cannot drift from the spec. That generated
file stays committed, because the Docker build only copies `ui/` and cannot
regenerate it.

The built bundle under `internal/api/ui/dist/` is a gitignored build artifact.
The Docker image builds it; a plain `go build` without it still works and serves
a placeholder page.

## Configuration

| Variable            | Default | Effect                                                                                                                      |
| ------------------- | ------- | --------------------------------------------------------------------------------------------------------------------------- |
| `MEMINI_UI_ENABLED` | `true`  | Mount the UI at `/`. Set `false` for a headless API/MCP service.                                                            |
| `MEMINI_UI_ADDR`    | (empty) | Serve the UI on its own listener (for example `:8081`) instead of the main HTTP port. Empty keeps it on `MEMINI_HTTP_ADDR`. |

See [configuration.md](../reference/configuration.md) for the rest.
