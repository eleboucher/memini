# memini

> A shared, persistent memory service for AI agents.

`memini` gives any [MCP](https://modelcontextprotocol.io)-capable agent (Claude Code,
opencode, Codex, Hermes, OpenClaw, Open WebUI) one place to `remember` and `recall`,
with retrieval quality that compounds over time. It runs as a single Go binary, boots
with zero configuration, and scales from an embedded SQLite file on a laptop to Postgres
in Kubernetes.

## Contents

- [How it works](#how-it-works)
- [Quick start](#quick-start)
- [Agent plugin](#agent-plugin)
- [Running in Docker](#running-in-docker)
- [Using it as an MCP server](#using-it-as-an-mcp-server)
- [Configuration](#configuration)
- [Web UI](#web-ui)
- [Answering](#answering)
- [Reranking](#reranking)
- [Importing existing memories](#importing-existing-memories)
- [Benchmarks](#benchmarks)
- [License](#license)

## How it works

memini draws on three earlier projects:

- A curated, deduplicated artifact rather than a pile of chunks (after Karpathy's
  "LLM wiki").
- Tiered memory (working → episodic → semantic → procedural) with decay and hybrid
  (vector + keyword) retrieval fused with Reciprocal Rank Fusion (after `agentmemory`).
  See [docs/tiers.md](docs/tiers.md) for what each tier means and how memories move
  between them.
- A stateless, K8s-native HTTP service with an opt-in LLM consolidation pipeline,
  per-memory TTLs, per-tenant isolation, Prometheus metrics, and an `fsck` consistency
  checker (after `mnemory`).

Hybrid results are re-ranked by a composite of relevance, access recency, and importance
(not similarity alone), and near-duplicates are collapsed at recall time.

When an LLM is configured, writes are stored immediately and then deduplicated and
contradiction-resolved in the background (a similarity gate skips the LLM when nothing
close exists), and each fresh episodic capture is distilled into durable semantic facts at
write time so a fact stated once is durable immediately (with a periodic promoter as a
backstop for older episodic memories). Without an LLM, marker heuristics run the same
lifecycle — write-time extraction, tier classification of untiered writes, the periodic
promoter, corroboration (a restated durable fact gains confidence instead of piling
up as chatter), and contradiction handling (a fresh durable write that changes a fact's
value or flips its polarity invalidates the stale one, which drops out of live recall but
stays reachable via `as_of`) — so durable knowledge still builds in the embedder-only and
embedder+reranker setups.

### Design

| Concern    | Choice                                                                                                                                                      |
| ---------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Language   | Go: single static binary, tiny image, low memory                                                                                                            |
| Storage    | Pluggable: **sqlite-vec** (embedded, default) or **Postgres + VectorChord** (scale)                                                                         |
| Embeddings | External OpenAI-compatible endpoint (you deploy the model)                                                                                                  |
| LLM        | **Opt-in**: runs headless without one; enables background dedup, consolidation, and answering when configured                                               |
| Ranking    | Hybrid (vector + keyword) RRF, re-ranked by relevance + recency + importance, deduplicated                                                                  |
| Interfaces | REST (server + UI types generated from [`api/openapi.yaml`](api/openapi.yaml)) + MCP (stdio & Streamable HTTP) + embedded web UI, sharing one service layer |

## Quick start

memini boots with zero configuration in its embedded (SQLite) mode. Vector search needs
an embeddings endpoint, so point it at any OpenAI-compatible embeddings API:

```sh
export MEMINI_EMBED_BASE_URL=http://localhost:8081/v1
export MEMINI_EMBED_MODEL=bge-m3
export MEMINI_EMBED_DIMS=1024
mise run run
curl -s localhost:8080/healthz
```

## Agent plugin

All plugins need a running memini (embeddings configured). To connect, set the
base URL and token (if your server requires auth). Default URL is always
`http://localhost:8080`.

Every integration reads the same canonical env vars, so one setup works
everywhere: **`MEMINI_BASE_URL`** for the server and **`MEMINI_API_KEY`** for the
token. The legacy names **`MEMINI_URL`** and **`MEMINI_TOKEN`** are still accepted
as aliases. Where a plugin has its own config (opencode options, Open WebUI
Valves, `openclaw.json`), that config wins over the env.

| Agent       | Base URL config                                       | Token (if auth)                |
| ----------- | ----------------------------------------------------- | ------------------------------ |
| Claude Code | `MEMINI_BASE_URL` (MCP endpoint: `MEMINI_MCP_URL`)    | `MEMINI_API_KEY`               |
| Codex CLI   | MCP config                                            | MCP config                     |
| opencode    | `MEMINI_BASE_URL` or inline `base_url`                | `MEMINI_API_KEY`               |
| Hermes      | `MEMINI_BASE_URL`                                     | `MEMINI_API_KEY`               |
| Open WebUI  | `base_url` Valve (defaults from `MEMINI_BASE_URL`)    | `MEMINI_API_KEY` (process env) |
| OpenClaw    | `base_url` in `openclaw.json`, else `MEMINI_BASE_URL` | `MEMINI_API_KEY` (gateway env) |

Full details and edge cases live in [`integrations/`](integrations/).

**Claude Code:**

```
/plugin marketplace add eleboucher/memini
/plugin install memini
```

**opencode:** add the plugin to `opencode.json` (or `~/.config/opencode/opencode.json`):

```json
{
  "plugin": ["@eleboucher/opencode-memini"]
}
```

**Hermes:**

```sh
hermes plugins install eleboucher/memini-hermes
```

**Open WebUI:** paste [`filter/memini_memory.py`](integrations/openwebui/filter/memini_memory.py) into Admin Panel → Functions → `+`, and optionally [`tools/memini_tools.py`](integrations/openwebui/tools/memini_tools.py) into Workspace → Tools for on-demand access.

**OpenClaw:**

```sh
openclaw plugins install clawhub:@eleboucher/memini
```

**Codex CLI:** MCP only — no plugin; wire the `memini mcp` server directly: see
[`integrations/codex/`](integrations/codex/).

Or wire any agent to the MCP server without a plugin: see [`integrations/`](integrations/).

## Running in Docker

### Full local stack with Compose

[`compose.yaml`](compose.yaml) brings up everything you need to try memini on a laptop:
Postgres + VectorChord, a CPU embeddings server (`text-embeddings-inference` serving
`bge-small-en-v1.5`, 384-d), and memini itself wired to both.

```sh
docker compose up --build      # builds the image, starts db + embeddings + memini
curl -s localhost:8080/healthz # -> ok, once the db healthcheck passes
open http://localhost:8080/    # embedded admin UI
```

memini is reachable at `http://localhost:8080` (REST + MCP + UI). To enable the opt-in
LLM pipeline (background dedup/consolidation, `/v1/answer`, `llm` rerank), uncomment
`MEMINI_LLM_BASE_URL` / `MEMINI_LLM_MODEL` in the `memini` service and point them at any
OpenAI-compatible chat endpoint. `docker compose down -v` tears it down and drops the
Postgres volume.

### Single container (SQLite mode)

For a self-contained server with no Postgres, run the image in its default embedded
(SQLite) mode. Just give it a volume for the database and an embeddings endpoint to talk
to:

```sh
docker build -t memini .       # or use a prebuilt image if you publish one
docker run --rm -p 8080:8080 \
  -v memini-data:/data \
  -e MEMINI_SQLITE_PATH=/data/memini.db \
  -e MEMINI_EMBED_BASE_URL=http://host.docker.internal:8081/v1 \
  -e MEMINI_EMBED_MODEL=bge-small-en-v1.5 \
  -e MEMINI_EMBED_DIMS=384 \
  memini
```

The image runs as a non-root user (`65532`); the named volume keeps memories across
restarts. On Linux, swap `host.docker.internal` for the host IP (or add
`--add-host=host.docker.internal:host-gateway`) to reach an embeddings server running on
the host.

## Using it as an MCP server

memini speaks the Model Context Protocol so agents can `remember` / `recall` / `answer`:

- **Remote (Streamable HTTP):** `http://<host>:8080/mcp`
- **Local (stdio):** `memini mcp`

For a **shared, always-on** server, run it over HTTP (the Compose or single-container
setups above already expose `/mcp` at `http://localhost:8080/mcp`) and point agents at
that URL.

For a **stdio** MCP server the agent spawns per session, run `memini mcp` in the container
with `-i` (keep stdin open) and no published port:

```sh
docker run -i --rm \
  -v memini-data:/data \
  -e MEMINI_SQLITE_PATH=/data/memini.db \
  -e MEMINI_EMBED_BASE_URL=http://host.docker.internal:8081/v1 \
  -e MEMINI_EMBED_MODEL=bge-small-en-v1.5 -e MEMINI_EMBED_DIMS=384 \
  memini mcp
```

Wire that into any MCP client as the launch command, e.g. for Claude Code / opencode:

```json
{
  "mcpServers": {
    "memini": {
      "command": "docker",
      "args": [
        "run",
        "-i",
        "--rm",
        "-v",
        "memini-data:/data",
        "-e",
        "MEMINI_SQLITE_PATH=/data/memini.db",
        "-e",
        "MEMINI_EMBED_BASE_URL=http://host.docker.internal:8081/v1",
        "-e",
        "MEMINI_EMBED_MODEL=bge-small-en-v1.5",
        "-e",
        "MEMINI_EMBED_DIMS=384",
        "memini",
        "mcp"
      ]
    }
  }
}
```

This works as-is: memory lands in the `default` namespace. A detached container can't
auto-detect the agent's repo the way the [plugin](plugin/) does, so for per-project
isolation set `MEMINI_DEFAULT_NAMESPACE` (or pass a `namespace` argument per tool call).

Ready-to-paste configs for Claude Code, opencode, Codex, Hermes, OpenClaw, and Open WebUI
(plus the shared cross-agent namespace trick) live in [`integrations/`](integrations/).
For Claude Code and Codex, prefer the [plugin/](plugin/), which auto-captures tool calls
and injects prior context at session start.

## Configuration

memini is configured entirely through environment variables (12-factor).

**Minimum to run:** set `MEMINI_EMBED_BASE_URL` (an OpenAI-compatible embeddings
endpoint) and make `MEMINI_EMBED_DIMS` match its model. Everything else has a
working default — SQLite storage, no LLM, recall and write-time dedup tuned out
of the box. Add `MEMINI_LLM_BASE_URL` to turn on the consolidation pipeline, and
`MEMINI_POSTGRES_DSN` (with `MEMINI_BACKEND=postgres`) for a server deployment.
The rest of the table is optional tuning — skip it until you need it.

| Env var                          | Default                  | Description                                                                                                                                                                                                                                                                                                                                                                                    |
| -------------------------------- | ------------------------ | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `MEMINI_HTTP_ADDR`               | `:8080`                  | HTTP listen address                                                                                                                                                                                                                                                                                                                                                                            |
| `MEMINI_SHUTDOWN_TIMEOUT`        | `15s`                    | graceful HTTP shutdown budget on SIGTERM                                                                                                                                                                                                                                                                                                                                                       |
| `MEMINI_REQUEST_TIMEOUT`         | `60s`                    | per-request budget for `/v1` REST routes only (never `/mcp`, `/healthz`, `/readyz`, `/metrics`); `0` disables                                                                                                                                                                                                                                                                                  |
| `MEMINI_BACKEND`                 | `sqlite`                 | `sqlite` or `postgres`                                                                                                                                                                                                                                                                                                                                                                         |
| `MEMINI_SQLITE_PATH`             | `memini.db`              | sqlite database path                                                                                                                                                                                                                                                                                                                                                                           |
| `MEMINI_POSTGRES_DSN`            | —                        | required when `MEMINI_BACKEND=postgres`                                                                                                                                                                                                                                                                                                                                                        |
| `MEMINI_EMBED_BASE_URL`          | —                        | OpenAI-compatible embeddings endpoint                                                                                                                                                                                                                                                                                                                                                          |
| `MEMINI_EMBED_MODEL`             | `text-embedding-3-small` | embedding model name                                                                                                                                                                                                                                                                                                                                                                           |
| `MEMINI_EMBED_API_KEY`           | —                        | bearer token for the embeddings endpoint (optional)                                                                                                                                                                                                                                                                                                                                            |
| `MEMINI_EMBED_DIMS`              | `1536`                   | embedding dimensions (must match model)                                                                                                                                                                                                                                                                                                                                                        |
| `MEMINI_EMBED_QUERY_PREFIX`      | —                        | instruction prepended to recall queries for asymmetric embedders (documents stay bare), e.g. Qwen3-Embedding's `Instruct: Given a user query, retrieve relevant memories that answer it\nQuery:`                                                                                                                                                                                               |
| `MEMINI_EMBED_MAX_BATCH`         | `20`                     | max items per `/embeddings` request (match your server's max client batch; TEI defaults to 32)                                                                                                                                                                                                                                                                                                 |
| `MEMINI_EMBED_MAX_BATCH_CHARS`   | `24000`                  | max total characters per `/embeddings` request (`0` disables)                                                                                                                                                                                                                                                                                                                                  |
| `MEMINI_WRITE_EMBED_TIMEOUT`     | `5s`                     | bounds the content embed on the remember path; on timeout or embed error the memory is stored without a vector (keyword-searchable, `pending_embed`) and backfilled in the background (`MEMINI_BACKFILL_INTERVAL`) instead of failing the write. `0` restores fail-fast writes                                                                                                                 |
| `MEMINI_REEMBED_ON_MODEL_CHANGE` | `false`                  | when `MEMINI_EMBED_MODEL` differs from the model the stored vectors were produced with, re-embed every memory at startup instead of refusing to start (blocks startup; one embeddings call per memory). Off by default — use the `memini reembed` command for an explicit, observable pass. Dims still can't change this way                                                                   |
| `MEMINI_WRITE_DEDUP_SCORE`       | `0.625`                  | vector similarity at/above which a write's nearest same-tier memory is treated as a near-duplicate, triggering `MEMINI_WRITE_DEDUP_ACTION` (`0` disables). `0.625` is the dedup-bench sweet spot for hints (~85% near-dup recall, ~1% false hit); use ~`0.9` for `coalesce`                                                                                                                    |
| `MEMINI_WRITE_DEDUP_ACTION`      | `hint`                   | what happens at/above the score: `hint` (default, non-destructive — store the write and return a merge hint so the caller can merge; durable tiers only), `coalesce` (reinforce the existing memory and drop the write; all tiers), `supersede` (**destructive** — store the write and tombstone the old memory), or `off`                                                                     |
| `MEMINI_CONTRADICT_DOWNRANK`     | `true`                   | when a fresh durable write contradicts an existing durable fact (changed value or flipped polarity, confirmed by an LLM-free lexical detector), invalidate the stale fact: stamp its `valid_to` so it leaves live recall while `as_of` time-travel can still reach it, and shrink its confidence. Reversible (`Restore` clears it), non-destructive. Set `false` to disable                    |
| `MEMINI_LLM_BASE_URL`            | —                        | opt-in LLM endpoint; empty disables it                                                                                                                                                                                                                                                                                                                                                         |
| `MEMINI_LLM_API_KEY`             | —                        | bearer token for the LLM endpoint (optional)                                                                                                                                                                                                                                                                                                                                                   |
| `MEMINI_LLM_API`                 | `openai`                 | chat backend: `openai` or `anthropic` (e.g. MiniMax)                                                                                                                                                                                                                                                                                                                                           |
| `MEMINI_LLM_MODEL`               | `gpt-4o-mini`            | consolidation model name                                                                                                                                                                                                                                                                                                                                                                       |
| `MEMINI_RECALL_REWRITE_TIMEOUT`  | `3s`                     | bounds the LLM query-rewrite call on `query_rewrite: true` recalls; past it, recall proceeds with the original query alone instead of riding the LLM client's own (much longer) timeout. `0` restores an unbounded rewrite call                                                                                                                                                                |
| `MEMINI_RERANK`                  | `off`                    | recall reranking: `off`, `llm`, or a cross-encoder `/rerank` URL (Infinity, vLLM, or `llama-server --rerank`); failures fall back to the composite order                                                                                                                                                                                                                                       |
| `MEMINI_RERANK_MODEL`            | —                        | cross-encoder model name (when `MEMINI_RERANK` is a URL)                                                                                                                                                                                                                                                                                                                                       |
| `MEMINI_RERANK_API_KEY`          | —                        | cross-encoder endpoint auth (when `MEMINI_RERANK` is a URL; optional)                                                                                                                                                                                                                                                                                                                          |
| `MEMINI_RERANK_TIMEOUT`          | `10s`                    | per-recall timeout on the reranker call; on timeout recall falls back to the composite order                                                                                                                                                                                                                                                                                                   |
| `MEMINI_RERANK_MAX_BATCH_CHARS`  | `6000`                   | cap the total query+documents characters per `/rerank` request; the pool is split across multiple requests when it would exceed this (`6000` keeps ~2 max-size docs per request). `0` disables                                                                                                                                                                                                 |
| `MEMINI_CONSOLIDATE_MODE`        | `async`                  | `async` (store now, dedup in background), `sync`, or `off`                                                                                                                                                                                                                                                                                                                                     |
| `MEMINI_CONSOLIDATE_MIN_SCORE`   | `0.6`                    | similarity gate: skip the LLM when the nearest candidate scores below it (`0` disables)                                                                                                                                                                                                                                                                                                        |
| `MEMINI_DISTILL_BATCH_TOKENS`    | `1024`                   | batch write-time distillation per session: captures with a `session_id` accumulate until roughly this many estimated tokens, then distill as one LLM call with cross-turn context (`0` restores per-capture distill; only applies with an LLM configured)                                                                                                                                      |
| `MEMINI_DISTILL_BATCH_MAX_AGE`   | `10m`                    | flush a session's buffered captures once the oldest has waited this long, so a quiet session still distills promptly                                                                                                                                                                                                                                                                           |
| `MEMINI_PROMOTE_INTERVAL`        | `24h`                    | how often frequently-used episodic memories are distilled into semantic facts (`0` disables; uses the LLM when configured, the marker extractor otherwise)                                                                                                                                                                                                                                     |
| `MEMINI_PROMOTE_MIN_ACCESS`      | `3`                      | minimum recall count before an episodic memory is eligible for promotion                                                                                                                                                                                                                                                                                                                       |
| `MEMINI_BACKFILL_INTERVAL`       | `1m`                     | how often the vector backfill loop re-embeds memories left vectorless by a degraded write (`0` disables)                                                                                                                                                                                                                                                                                       |
| `MEMINI_SWEEP_INTERVAL`          | `1h`                     | how often the decay sweeper purges expired memories                                                                                                                                                                                                                                                                                                                                            |
| `MEMINI_SHORT_TERM_CAP`          | `1000`                   | per-namespace cap on short-term (working+episodic) memories; the sweeper evicts the lowest-retention over it (`0` disables)                                                                                                                                                                                                                                                                    |
| `MEMINI_TOMBSTONE_TTL`           | `0`                      | sweeper hard-deletes tombstoned memories older than this TTL (`0` keeps them indefinitely); the one irreversible maintenance action                                                                                                                                                                                                                                                            |
| `MEMINI_DEMOTE_AFTER`            | `0`                      | sweeper demotes never-recalled, low-importance durable memories older than this back to episodic (`0` disables)                                                                                                                                                                                                                                                                                |
| `MEMINI_DEDUP_INTERVAL`          | `24h`                    | how often the store-wide dedup pass collapses near-duplicate clusters to one representative (rest tombstoned reversibly); `0` disables. Also on-demand via `POST /v1/dedup`                                                                                                                                                                                                                    |
| `MEMINI_DEDUP_SIMILARITY`        | `0.85`                   | cosine-like threshold for cluster membership; higher is stricter                                                                                                                                                                                                                                                                                                                               |
| `MEMINI_DEDUP_TIERS`             | —                        | comma-separated tiers to restrict the periodic pass to (`working,episodic,semantic,procedural`); empty means all                                                                                                                                                                                                                                                                               |
| `MEMINI_API_KEY`                 | —                        | if set, required as a bearer token (also gates `/metrics`)                                                                                                                                                                                                                                                                                                                                     |
| `MEMINI_UI_ENABLED`              | `true`                   | mount the embedded admin UI at `/` (`false` for a headless API/MCP-only service)                                                                                                                                                                                                                                                                                                               |
| `MEMINI_DEFAULT_NAMESPACE`       | auto                     | fallback namespace (see [Namespace resolution](#namespace-resolution))                                                                                                                                                                                                                                                                                                                         |
| `MEMINI_GLOBAL_NAMESPACE`        | —                        | a namespace whose durable (semantic/procedural) memories merge read-only into every other namespace's recall and briefing — a shared space for cross-project rules the agent should always remember ("no AI slop", commit conventions). Empty disables it (namespaces stay isolated); pin a memory there to keep it top-of-mind. See [Retrieval scope (read sets)](#retrieval-scope-read-sets) |
| `MEMINI_READ_NAMESPACES`         | —                        | extra namespaces whose durable (semantic/procedural) memories merge read-only into every recall and briefing, generalizing `MEMINI_GLOBAL_NAMESPACE` to a list. An entry ending in `/*` also includes namespaces nested under it. Empty disables it. See [Retrieval scope (read sets)](#retrieval-scope-read-sets)                                                                             |
| `MEMINI_LOG_LEVEL`               | `info`                   | `debug` / `info` / `warn` / `error`                                                                                                                                                                                                                                                                                                                                                            |
| `MEMINI_LOG_FORMAT`              | `json`                   | `json` or `text`                                                                                                                                                                                                                                                                                                                                                                               |

### Namespace resolution

A request's namespace is taken from the `X-Memini-Namespace` header. The
authoritative source of that header is the [plugin/](plugin/): each hook script
resolves the namespace from `MEMINI_NAMESPACE` env, if set; else the repo name from
`git remote get-url origin` (stable across worktrees and clones, unlike a toplevel
basename); else the basename of `git rev-parse --show-toplevel`; else `basename(cwd)`;
then sends it on every call. A self-healing cache remembers the derivation by remote
URL and toplevel path, so a later folder move or dropped remote resolves back to the
same namespace instead of silently orphaning its memories. See
[plugin/README.md](plugin/README.md#how-the-namespace-gets-resolved) for the full
order and `MEMINI_NAMESPACE_SCOPE`. That is what makes HTTP mode "just work" across
projects without per-project config.

Namespace values from env (`MEMINI_NAMESPACE`, `MEMINI_DEFAULT_NAMESPACE`) may contain
`/`, e.g. `project/agent`, and are preserved as-is; only git- and cwd-derived values are
reduced to a basename. Older memini versions flattened a slash-containing env value to
its basename too; if you are upgrading from one of those and `memini doctor` warns that
your old basename namespace still holds memories, merge it forward with `memini
namespace move --from <basename> --to <full-path>`.

When the header is absent (for example a stdio MCP launch without the plugin, or an HTTP
call that forgot to set it), the server falls back to a similar resolver at startup time,
in this order:

1. `MEMINI_DEFAULT_NAMESPACE` (or `MEMINI_NAMESPACE`) env var, if non-empty.
2. `git rev-parse --show-toplevel` in the server's cwd, using the repo basename, e.g.
   `memini` for `/home/dev/memini`.
3. `basename(cwd)` if the cwd is not inside a git worktree.
4. Literal `default` as a last resort.

Unlike the plugin, the server's fallback skips the git-remote step (existing stores
keyed by the worktree basename stay put); `memini doctor` compares both resolutions and
flags a mismatch.

The resolved value and its source (`env` / `git` / `cwd` / `fallback`) are logged at
startup, e.g.:

```json
{"level":"INFO","msg":"starting memini","default_namespace":"memini","namespace_source":"git",...}
```

In **HTTP mode**, the server-side auto-resolve is misleading: the server runs detached
from the agent's cwd, so the resolved basename reflects _the server's_ project, not the
agent's. Install the plugin (or send the header explicitly per request) to get the right
namespace. In **stdio mode** the server inherits the agent's cwd, so the fallback is
correct.

### Retrieval scope (read sets)

A write always lands in exactly one namespace, the storage partition described above.
A read (`memory_recall`, `memory_briefing`, `memory_answer`) can see more than that:
its **read set** is the namespaces a given call actually searches, composed from the
request namespace plus whichever of the mechanisms below apply. This keeps writes
simple and isolated while letting reads pull in related context on purpose.

**Default read set** (no per-call `namespaces` argument): the request namespace, plus

- its subtree, when the call passes `scope=subtree` (`project` also reads
  `project/agent-a`, `project/agent-b`, ...),
- every `MEMINI_READ_NAMESPACES` entry (durable tiers only),
- the request namespace's own persistent namespace links (tier mode per link), and
- `MEMINI_GLOBAL_NAMESPACE` (durable only), when set.

**Per-call `namespaces`** (recall, briefing, and `POST /v1/search`): passing a
`namespaces` list replaces the default read set entirely with exactly those
namespaces, in first-occurrence order, except the request namespace, if included, is
always moved to the front. An entry ending in `/*` also includes every namespace
nested under it. Max 16 entries; the request namespace is not force-added, so include
it explicitly if you still want it searched. Writes are never affected either way.

However it's composed, the fully resolved read set is capped at 64 entries after
subtree/pattern expansion; an oversized set is clamped to the front (primary and the
global namespace protected first) with a logged warning, and `memini doctor` reports
when a configured read set exceeds the cap.

**Namespace links** are a persistent, read-only attachment: `memini namespace link
--from A --to B` makes A's default read set also see B, one hop only (B's own links
are never followed, so linking A to B does not transitively expose whatever B links
to). `--tiers durable` (default) surfaces only B's semantic/procedural memories;
`--tiers all` also includes episodic/working. A namespace may declare at most 16
outgoing links. Manage them via the `memini namespace link` / `unlink` / `links` CLI
subcommands, the `memory_namespace_link` MCP tool (present only when the backend
supports links), or `GET` / `PUT` / `DELETE /v1/namespaces/{name}/links[/{target}]`.
Every read result carries a `namespace` field so callers can tell which partition each
hit came from.

| Mechanism                       | Contributes                   | Tier access                   |
| ------------------------------- | ----------------------------- | ----------------------------- |
| Request namespace               | the namespace itself          | the request's own tier filter |
| `scope=subtree`                 | nested child namespaces       | the request's own tier filter |
| `namespaces: [...]` (per-call)  | exactly the listed namespaces | the request's own tier filter |
| Namespace link, `tiers=durable` | the linked namespace          | semantic + procedural only    |
| Namespace link, `tiers=all`     | the linked namespace          | the request's own tier filter |
| `MEMINI_READ_NAMESPACES`        | each listed namespace         | semantic + procedural only    |
| `MEMINI_GLOBAL_NAMESPACE`       | one shared namespace          | semantic + procedural only    |

**Worked example:** two related repos, `api` and `web`, each with their own memory.
Link them one-way with `memini namespace link --from api --to web --tiers durable`.
A recall in `api` now also surfaces `web`'s durable facts and procedures (its episodic
chatter stays out), so an agent working in `api` can see a decision recorded while
working in `web`. Writes made from `api` still land only in `api`; `web`'s recall is
unaffected unless it links back. Run `memini doctor` to see the resolved read set for
any namespace, including which links and env entries are currently contributing to it.

## Web UI

memini ships an embedded admin UI (Preact + Vite, compiled into the binary) served at `/`.
It needs no separate process; open `http://localhost:8080/`.

- **Overview** — per-namespace stats and a tier "strata" bar (working → episodic →
  semantic → procedural).
- **Browser** — paginated, tier/expired/superseded-filterable list with a detail drawer
  and delete.
- **Search** — hybrid recall with relevance scores.
- **Graph** — D3 force-directed view; edges are supersession (directed) and shared-tag
  affinity.
- **Health** — runs `fsck` and surfaces duplicate clusters.

Use the namespace switcher (top bar) to change tenant, and **Settings** to set a bearer
token (sent as `Authorization: Bearer …`) or point the UI at a remote `memini`. The static
shell is unauthenticated so you can enter a token; the `/v1` API it calls still enforces
`MEMINI_API_KEY`. Disable the whole thing with `MEMINI_UI_ENABLED=false`.

> [!WARNING]
> When `MEMINI_API_KEY` is set, the server embeds the key in the UI shell so the
> same-origin UI authenticates without pasting it, which means anyone who can load `/` can
> read the key. Only expose the UI where reaching it already implies trust, or set
> `MEMINI_UI_ENABLED=false` on untrusted networks.

The UI is backed by three read-only endpoints alongside the core API: `GET /v1/memories`
(list with `tier`/`include_expired`/`include_superseded`/`limit` filters), `GET
/v1/stats`, and `GET /v1/namespaces`.

The UI sources live in [`ui/`](ui/); build the embedded bundle with `mise run ui` (or
iterate with HMR via `mise run ui-dev`, which proxies `/v1` to a local server on `:8080`).
The built bundle under `internal/api/ui/dist/` is a gitignored build artifact: the Docker
image builds it, while a plain `go build` without it still works and serves a placeholder
page.

## Answering

Beyond raw recall, `POST /v1/answer` `{query, limit}` retrieves memories and has the LLM
generate a grounded answer from them, returning the answer plus the supporting `sources`
(requires an LLM; also exposed as the `memory_answer` MCP tool).

## Reranking

`MEMINI_RERANK` adds an optional read-side rerank over the hybrid candidates (`off`, a
cross-encoder `/rerank` URL served by Infinity / vLLM / `llama-server --rerank`, or
`llm`). See the [benchmark table](#benchmarks) for measured numbers across every config
and dataset. Two things worth knowing:

- Reranking only helps where base recall has headroom. On session-level sets hybrid is
  already at ~98–99%, so reranking is a no-op. On turn-level LoCoMo (gold = exact turns) it
  pays off: +11pp R@5 / +17pp MRR (cross-encoder) or +15pp / +25pp (LLM).
- The cross-encoder is the better default when you need it: most of the LLM's lift at a
  fraction of the latency, a tiny 0.6B model, and no chat dependency. Use `llm` only if you
  already run a chat model and want the last few points.

## Importing existing memories

`memini import` loads an export from `agentmemory`, `mem0`, `mnemory`, memini's own
format, or your **Claude Code session history**, into the local store or a running server.

```sh
# Local store (embeds + preserves source IDs, timestamps, tiers):
memini import --source agentmemory ./agentmemory-export.json

# Remote server over REST:
memini import --source mem0 --remote https://memini.example.com \
  --token "$MEMINI_API_KEY" --namespace my-project ./mem0-export.json

# Backfill Claude Code history: each user→assistant exchange becomes one
# episodic memory, scoped to the project namespace (the transcript's cwd
# basename). Accepts a single transcript, a project dir, or all projects:
memini import --source claude-code ~/.claude/projects
```

The `claude-code` source reconstructs verbatim exchanges from session transcripts
(`~/.claude/projects/<project>/<session>.jsonl`), skipping tool-result noise, sidechains,
and slash-command wrappers. IDs are deterministic, so re-importing is idempotent.
Backfilled memories get a fresh 30-day episodic TTL (so old history isn't swept on
arrival) while keeping the original timestamp for recency ranking. This pairs with the
[plugin](plugin/)'s auto-capture: backfill once, then the hooks keep it current.

Each source's fields map onto memini's tiers (e.g. agentmemory `workflow`→procedural, mem0
facts→semantic) and namespace (`project`/`user_id`). Records whose source carries no
recognized tier default to **episodic** (30-day TTL), so a bulk import of unknown quality
ages out unless recall reinforces it rather than living forever as durable facts. Empty
records are skipped; per-record failures don't abort the run. Over `--remote` the server
sets its own timestamps, so the source's created-at is kept in
`metadata.imported_created_at`. Reads stdin when the path is `-`.

For low-quality bulk exports, two optional gates drop weak records before they're written
(both off by default):

```sh
# Skip stubs shorter than 40 bytes and anything below importance 0.3:
memini import --source mem0 --min-length 40 --min-importance 0.3 ./export.json
```

Note `--min-importance` skips records whose source reported no importance (they arrive as
`0`); leave it off unless your export carries real importance scores.

## Switching embedding models

Vectors from different embedding models aren't comparable, so memini records which model
produced a store's vectors and **refuses to start** when `MEMINI_EMBED_MODEL` later differs
— otherwise a same-dimension model swap would silently degrade recall with no error. To
migrate a store to a new model in place:

```sh
# dry-run: report how many memories would be re-embedded
MEMINI_EMBED_MODEL=new-model memini reembed

# apply (re-embeds every memory, then records the new model)
MEMINI_EMBED_MODEL=new-model memini reembed --yes
```

Re-embedding keeps the store's dimensionality — switching dims (e.g. `1536` → `1024`) still
requires a fresh store (`memini export`, then `memini import` into a new one). Set
`MEMINI_REEMBED_ON_MODEL_CHANGE=true` to re-embed automatically at startup instead of
refusing; it's off by default because re-embedding blocks startup and calls the embeddings
endpoint once per memory.

## Benchmarks

```sh
mise run bench   # offline retrieval benchmark (hybrid vs vector vs keyword)
```

Full results from a `bench/results/` run (written locally; gitignored), all on the same
all-MiniLM-L6-v2 (384-d) endpoint, the model agentmemory benchmarks with. Cells are
`recall_any@5 / @10 / MRR` (%); `p50` is in-process recall latency (rerank rows show the
cost they add on top):

| Strategy                                | LongMemEval · session  | LoCoMo · turn-level    | LoCoMo · session-level | p50         |
| --------------------------------------- | ---------------------- | ---------------------- | ---------------------- | ----------- |
| vector                                  | 92.6 / 95.4 / 80.7     | 41.3 / 51.8 / 28.1     | 64.1 / 79.8 / 45.2     | <1 ms       |
| keyword (Porter BM25)                   | 97.6 / 99.0 / 92.2     | 58.7 / 67.1 / 44.8     | 92.6 / 96.8 / 79.4     | ~3 ms       |
| **hybrid** (default)                    | **98.4 / 99.2 / 93.0** | **59.7 / 69.9 / 42.4** | **90.9 / 96.6 / 74.3** | ~5 ms       |
| + cross-encoder (`MEMINI_RERANK=<url>`) | 98.4 / 99.2 / 93.1     | **70.9 / 75.0 / 59.8** | 90.9 / 96.6 / 74.3     | +20–230 ms  |
| + LLM rerank (`MEMINI_RERANK=llm`)      | 98.4 / 99.2 / 93.0     | **74.4 / 76.5 / 67.4** | —                      | +350–420 ms |

Questions: LongMemEval 500, LoCoMo turn 1,982, LoCoMo session 1,981 (rerank =
Qwen3-Reranker-0.6B cross-encoder, Qwen3.5-9B LLM). Hybrid never trails either single leg
on the saturated session sets; on turn-level LoCoMo (gold = exact evidence turns) base
recall has headroom, so reranking pays off (cross-encoder +11pp R@5 / +17pp MRR, LLM +15pp
/ +25pp) while being a no-op once recall is already at ceiling.

On the same model, dataset, and metric, memini hybrid beats agentmemory's published
LongMemEval-S numbers, and goes higher with a premium embedder:

| System                    | Embedding          |       R@5 |      R@10 |
| ------------------------- | ------------------ | --------: | --------: |
| memini — hybrid           | all-MiniLM-L6-v2   | **98.4%** | **99.2%** |
| memini — hybrid           | Qwen3-Embedding-8B | **98.8%** | **99.6%** |
| agentmemory — BM25+Vector | all-MiniLM-L6-v2   |     95.2% |     98.6% |
| agentmemory — BM25-only   | —                  |     86.2% |     94.6% |

memini's Porter-stemming keyword leg is +11pp over their BM25-only.

These numbers are on the full 500-question set, which is also where parameters were swept,
so to check they aren't tuned-to-test the harness splits LongMemEval deterministically
into a 450-question tune set and a never-swept 50-question held set (`-holdout`). Hybrid
scores 98.2% R@5 on tune and does not regress on held (100% R@5, 50q), so the tuning
choices generalize. The per-category headroom is concentrated in `single-session-preference`
(88.9% R@5 on tune).

Full per-leg/per-category tables, the split breakdown, parameter sweeps, methodology,
caveats, and the LoCoMo QA comparison (vs mem0/Letta) are in [`bench/`](bench/README.md).

## License

[AGPL-3.0](LICENSE).
