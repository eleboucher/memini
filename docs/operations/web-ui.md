# Web UI

memini ships an embedded admin UI (Preact + Vite, compiled into the binary) and
serves it at `/`. There is no separate process to run. With the default config,
open `http://localhost:8080/`.

It is an **admin** UI, not a read-only viewer. It deletes memories, moves and
splits namespaces, creates and rotates API keys, and runs fsck and dedup. Treat
access to it as administrative access to the store.

## Security: the UI shell embeds your API key

> [!WARNING]
> When `MEMINI_API_KEY` is set, the server injects it into the UI shell (a
> `<meta name="memini-token">` tag) so the same-origin UI can authenticate
> without you pasting it. **Anyone who can load `/` can read that key**, and the
> key is the admin credential: it is the only one allowed to manage other keys.

Three ways to live with that, in increasing order of paranoia:

1. **Expose the UI only where reaching it already implies trust.** A laptop, a
   LAN, a `kubectl port-forward`.
2. **Give the UI its own listener** with `MEMINI_UI_ADDR` (for example `:8081`).
   The main port then carries only the API and MCP, with no embedded key, and you
   route the UI port to an internal gateway. The UI listener also serves `/v1`,
   so the same-origin SPA still works. This is the clean answer for anything
   shared, and it is what the Helm chart does by default.
3. **Turn it off** with `MEMINI_UI_ENABLED=false` for a headless API/MCP service.

The static shell itself is unauthenticated so you _can_ reach Settings and type a
token by hand; the `/v1` API it calls still enforces `MEMINI_API_KEY` either way.

## Views

Ten views, in nav order.

| View         | Path        | What it is for                                                                                                                                                                                                                                                                                                        |
| ------------ | ----------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| **Projects** | `/projects` | The landing page. Every namespace as a nested box tree of "pods" (memory count, tier bar, last write). Click one to make it the active namespace. Per-pod: delete, or a drawer for namespace **move** and **split** (dry-run first).                                                                                  |
| **Overview** | `/`         | Read-only stats for the active namespace, or aggregated across all: totals, recalls, average importance, last write, expired/superseded/low-confidence counts, and the tier strata bar.                                                                                                                               |
| **Browse**   | `/browse`   | Filterable list of up to 500 memories. Filters run server-side: tier, memory type, level, tags, metadata, created-after, accessed-after, sort, include-expired, include-superseded. The detail drawer is where **delete** and **reassign** live.                                                                      |
| **Search**   | `/search`   | Hybrid recall against `/v1/search` with tier/tag/metadata filters, showing relevance scores and read set provenance ("in scope via ..."). It does not call `/v1/answer`.                                                                                                                                              |
| **Activity** | `/activity` | Paged event feed: recall, briefing, get, remember, update, forget, supersede. Each row shows the query, the memories served with rank and score, and a "degraded" chip when a recall fell back to keyword-only. This is the view for "why did the agent not see that memory".                                         |
| **Graph**    | `/graph`    | Force-directed view, two modes. _Memories_: nodes are memories, edges are supersession (directed) and shared-tag affinity. _Namespaces_: nodes are namespaces, edges are the parent/child ancestor cascade plus stored links.                                                                                         |
| **Scopes**   | `/scopes`   | The effective **read set** for the active namespace: every namespace a recall draws from, each tagged with why (primary, ancestor, home, link, call) and which tiers that leg contributes. Below it, the outgoing **links**, with add and delete. This is the view that answers "what can this project actually see". |
| **Keys**     | `/keys`     | Named API keys: list, create, rotate, enable/disable, delete. New and rotated secrets are shown exactly once. Keys from `MEMINI_API_KEYS_FILE` are listed as "declarative" and are read-only here (the file is the source of truth). Needs the admin key.                                                             |
| **Health**   | `/health`   | Two manual maintenance tools. **fsck** purges expired memories, evicts short-term ones, and reports duplicate clusters. **dedup** collapses near-duplicates at a similarity threshold, and defaults to dry-run. Neither runs on its own.                                                                              |
| **Settings** | `/settings` | Client-side only, persisted to `localStorage`. API base URL (empty means same origin), the namespace header name, and a bearer token. No server config is exposed or edited here. A token typed here overrides the one the server injected.                                                                           |

Change the active namespace with the switcher in the top bar. Several views also
have an "All projects" mode that fans the query out store-wide.

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
