# memini

A memory service for AI agents. `memini` gives any
[MCP](https://modelcontextprotocol.io)-capable agent — Claude Code, opencode, Codex, Hermes,
OpenClaw — a shared, persistent place to `remember` and `recall`, with retrieval quality that
compounds over time.

It synthesizes three ideas:

- **A curated, deduplicated artifact** rather than a pile of chunks (after Karpathy's "LLM wiki").
- **Tiered memory** — working → episodic → semantic → procedural — with decay and hybrid
  (vector + keyword) retrieval fused with Reciprocal Rank Fusion (after `agentmemory`).
- **A stateless, K8s-native HTTP service** with an opt-in LLM consolidation pipeline, per-memory
  TTLs, per-tenant isolation, Prometheus metrics, and an `fsck` consistency checker (after `mnemory`).

Retrieval is tuned for quality-per-byte: hybrid results are re-ranked by a composite of
relevance, access **recency**, and **importance** (not similarity alone), and near-duplicates
are collapsed at recall time. When an LLM is configured, writes are stored immediately and
**deduplicated/contradiction-resolved in the background** (a similarity gate skips the LLM when
nothing close exists), and frequently-recalled episodic memories are periodically **distilled into
durable semantic facts** so retrieval quality compounds over time.

## Design at a glance

| Concern    | Choice                                                                                                                                                                 |
| ---------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Language   | Go — single static binary, tiny image, low memory                                                                                                                      |
| Storage    | Pluggable: **sqlite-vec** (embedded, default) or **Postgres + VectorChord** (scale)                                                                                    |
| Embeddings | External OpenAI-compatible endpoint (you deploy the model)                                                                                                             |
| LLM        | **Opt-in** — runs headless without one; enables background dedup, consolidation, and episodic→semantic promotion when configured                                       |
| Ranking    | Hybrid (vector + keyword) RRF, re-ranked by relevance + recency + importance, deduplicated                                                                             |
| Interfaces | REST (API-first: server + UI types generated from [`api/openapi.yaml`](api/openapi.yaml)) + MCP (stdio & Streamable HTTP) + embedded web UI, sharing one service layer |

## Running

`memini` boots with zero configuration in its embedded (sqlite) mode — but vector search needs an
embeddings endpoint:

```sh
export MEMINI_EMBED_BASE_URL=http://localhost:8081/v1   # any OpenAI-compatible embeddings API
export MEMINI_EMBED_MODEL=bge-m3
export MEMINI_EMBED_DIMS=1024
mise run run
curl -s localhost:8080/healthz
```

### Docker Compose (full local stack)

[`compose.yaml`](compose.yaml) brings up everything you need to try memini on a
laptop — Postgres + VectorChord, a CPU embeddings server
(`text-embeddings-inference` serving `bge-small-en-v1.5`, 384-d), and memini
itself wired to both:

```sh
docker compose up --build      # builds the image, starts db + embeddings + memini
curl -s localhost:8080/healthz # -> ok, once the db healthcheck passes
open http://localhost:8080/    # embedded admin UI
```

memini is reachable at `http://localhost:8080` (REST + MCP + UI). To enable the
opt-in LLM pipeline (background dedup/consolidation, `/v1/answer`, `llm` rerank),
uncomment `MEMINI_LLM_BASE_URL`/`MEMINI_LLM_MODEL` in the `memini` service and
point them at any OpenAI-compatible chat endpoint. `docker compose down -v` tears
it down and drops the Postgres volume.

### A single container (sqlite mode)

For a self-contained server with no Postgres, run the image in its default
embedded (sqlite) mode — just give it a volume for the database and an
embeddings endpoint to talk to:

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

The image runs as a non-root user (`65532`); the named volume keeps memories
across restarts. On Linux, swap `host.docker.internal` for the host IP (or add
`--add-host=host.docker.internal:host-gateway`) to reach an embeddings server
running on the host.

### As an MCP server in Docker

The same image serves MCP. For a **shared, always-on** server, run it over HTTP
(the Compose or single-container setups above already expose `/mcp` at
`http://localhost:8080/mcp`) and point agents at that URL.

For a **stdio** MCP server the agent spawns per session, run `memini mcp` in the
container with `-i` (keep stdin open) and no published port:

```sh
docker run -i --rm \
  -v memini-data:/data \
  -e MEMINI_SQLITE_PATH=/data/memini.db \
  -e MEMINI_EMBED_BASE_URL=http://host.docker.internal:8081/v1 \
  -e MEMINI_EMBED_MODEL=bge-small-en-v1.5 -e MEMINI_EMBED_DIMS=384 \
  memini mcp
```

Wire that into any MCP client as the launch command — e.g. for Claude Code /
opencode:

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

This works as-is — memory lands in the `default` namespace. A detached container
can't auto-detect the agent's repo the way the [plugin](plugin/) does, so if you
want **per-project isolation** set `MEMINI_DEFAULT_NAMESPACE` (or pass a
`namespace` argument per tool call). See [`integrations/`](integrations/) for
per-agent recipes and the shared-namespace trick.

### Configuration (12-factor)

| Env var                         | Default                  | Description                                                                                                                                                                                                                                                                                                                        |
| ------------------------------- | ------------------------ | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `MEMINI_HTTP_ADDR`              | `:8080`                  | HTTP listen address                                                                                                                                                                                                                                                                                                                |
| `MEMINI_SHUTDOWN_TIMEOUT`       | `15s`                    | graceful HTTP shutdown budget on SIGTERM                                                                                                                                                                                                                                                                                           |
| `MEMINI_BACKEND`                | `sqlite`                 | `sqlite` or `postgres`                                                                                                                                                                                                                                                                                                             |
| `MEMINI_SQLITE_PATH`            | `memini.db`              | sqlite database path                                                                                                                                                                                                                                                                                                               |
| `MEMINI_POSTGRES_DSN`           | —                        | required when `MEMINI_BACKEND=postgres`                                                                                                                                                                                                                                                                                            |
| `MEMINI_EMBED_BASE_URL`         | —                        | OpenAI-compatible embeddings endpoint                                                                                                                                                                                                                                                                                              |
| `MEMINI_EMBED_MODEL`            | `text-embedding-3-small` | embedding model name                                                                                                                                                                                                                                                                                                               |
| `MEMINI_EMBED_API_KEY`          | —                        | bearer token for the embeddings endpoint (optional)                                                                                                                                                                                                                                                                                |
| `MEMINI_EMBED_DIMS`             | `1536`                   | embedding dimensions (must match model)                                                                                                                                                                                                                                                                                            |
| `MEMINI_EMBED_QUERY_PREFIX`     | —                        | instruction prepended to recall queries before embedding, for instruction-tuned asymmetric embedders (documents stay bare). For Qwen3-Embedding: `Instruct: Given a user query, retrieve relevant memories that answer it\nQuery:`                                                                                                 |
| `MEMINI_FUSION_ALPHA`           | `0.5`                    | hybrid fusion: convex score-fusion weight on the vector leg (`0.5` balanced; higher favors vector, lower favors keyword). A negative value falls back to rank fusion (RRF).                                                                                                                                                        |
| `MEMINI_WRITE_DEDUP_MIN_SCORE`  | `0`                      | non-LLM corpus hygiene: coalesce a fresh write into an existing same-tier memory at or above this vector similarity instead of storing a near-duplicate (only when LLM consolidation isn't handling the write). `0` disables; ~`0.9` collapses near-identical restatements only (embedder-dependent).                              |
| `MEMINI_TEMPORAL_BOOST`         | `0.40`                   | query-conditioned temporal targeting: when a query names a relative time ("3 weeks ago"), candidates dated near the referenced point are boosted by up to this much on the composite score. **On by default**; `0` disables.                                                                                                       |
| `MEMINI_LLM_BASE_URL`           | —                        | opt-in LLM endpoint; empty disables it                                                                                                                                                                                                                                                                                             |
| `MEMINI_LLM_API_KEY`            | —                        | bearer token for the LLM endpoint (optional)                                                                                                                                                                                                                                                                                       |
| `MEMINI_LLM_API`                | `openai`                 | chat backend: `openai` or `anthropic` (e.g. MiniMax)                                                                                                                                                                                                                                                                               |
| `MEMINI_LLM_MODEL`              | `gpt-4o-mini`            | consolidation model name                                                                                                                                                                                                                                                                                                           |
| `MEMINI_RERANK`                 | `off`                    | recall reranking: `off`, `llm` (reorder with the chat LLM), or a cross-encoder `/rerank` base URL (e.g. `http://host:8002/v1`, served by Infinity, vLLM, or `llama-server --rerank`). Reorders the top candidates; big gain where recall has headroom (see matrix), a no-op at ceiling. Failures fall back to the composite order. |
| `MEMINI_RERANK_MODEL`           | —                        | cross-encoder model name (when `MEMINI_RERANK` is a URL)                                                                                                                                                                                                                                                                           |
| `MEMINI_RERANK_API_KEY`         | —                        | cross-encoder endpoint auth (when `MEMINI_RERANK` is a URL; optional)                                                                                                                                                                                                                                                              |
| `MEMINI_RERANK_TOP_N`           | `20`                     | how many composite-ranked candidates the reranker sees                                                                                                                                                                                                                                                                             |
| `MEMINI_RERANK_MAX_DOC_CHARS`   | `1200`                   | truncate each document to this many characters before sending to the cross-encoder, so one oversized memory can't exceed the server's physical batch (`llama-server --rerank` `n_ubatch`, default 512 tokens) and fail the whole recall. `0` disables truncation; raise it if you increase the server's batch size.                |
| `MEMINI_CONSOLIDATE_MODE`       | `async`                  | `async` (store now, dedup in background), `sync`, or `off`                                                                                                                                                                                                                                                                         |
| `MEMINI_CONSOLIDATE_MIN_SCORE`  | `0.6`                    | similarity gate: skip the LLM when the nearest candidate scores below it (`0` disables)                                                                                                                                                                                                                                            |
| `MEMINI_PROMOTE_INTERVAL`       | `24h`                    | how often frequently-used episodic memories are distilled into semantic facts (`0` disables; needs LLM)                                                                                                                                                                                                                            |
| `MEMINI_PROMOTE_MIN_ACCESS`     | `3`                      | minimum recall count before an episodic memory is eligible for promotion                                                                                                                                                                                                                                                           |
| `MEMINI_SWEEP_INTERVAL`         | `1h`                     | how often the decay sweeper purges expired memories                                                                                                                                                                                                                                                                                |
| `MEMINI_SHORT_TERM_CAP`         | `1000`                   | per-namespace cap on short-term (working+episodic) memories; the sweeper evicts the lowest-retention ones over it. `0` disables.                                                                                                                                                                                                   |
| `MEMINI_DEDUP_INTERVAL`         | `0`                      | how often the periodic store-wide dedup pass collapses near-duplicate memories into one representative per cluster (the rest are tombstoned, reversibly). `0` disables the job; the pass is primarily an on-demand post-import cleanup tool via `POST /v1/dedup`.                                                                  |
| `MEMINI_DEDUP_SIMILARITY`       | `0.85`                   | cosine-like threshold for cluster membership; higher is stricter (fewer, tighter clusters)                                                                                                                                                                                                                                         |
| `MEMINI_DEDUP_MIN_CLUSTER_SIZE` | `2`                      | smallest cluster acted on                                                                                                                                                                                                                                                                                                          |
| `MEMINI_DEDUP_NEIGHBOURS`       | `20`                     | per-anchor vector-search fan-out bounding the cluster width                                                                                                                                                                                                                                                                        |
| `MEMINI_DEDUP_TIERS`            | —                        | comma-separated tiers to restrict the periodic pass to (`working,episodic,semantic,procedural`); empty means all                                                                                                                                                                                                                   |
| `MEMINI_API_KEY`                | —                        | if set, required as a bearer token (also gates `/metrics`)                                                                                                                                                                                                                                                                         |
| `MEMINI_UI_ENABLED`             | `true`                   | mount the embedded admin UI at `/` (`false` for a headless API/MCP-only service)                                                                                                                                                                                                                                                   |
| `MEMINI_NAMESPACE_HEADER`       | `X-Memini-Namespace`     | header used to scope tenants                                                                                                                                                                                                                                                                                                       |
| `MEMINI_DEFAULT_NAMESPACE`      | auto                     | fallback namespace (see [Namespace resolution](#namespace-resolution))                                                                                                                                                                                                                                                             |
| `MEMINI_LOG_LEVEL`              | `info`                   | `debug`/`info`/`warn`/`error`                                                                                                                                                                                                                                                                                                      |
| `MEMINI_LOG_FORMAT`             | `json`                   | `json` or `text`                                                                                                                                                                                                                                                                                                                   |

### Namespace resolution

A request's namespace is taken from `X-Memini-Namespace` (configurable via
`MEMINI_NAMESPACE_HEADER`). The **authoritative** source of that header is
the [plugin/](plugin/) — each hook script resolves the namespace from the
agent's working directory via `git rev-parse --show-toplevel` and sends
it on every call. That is what makes HTTP mode "just work" across
projects without per-project config.

When the header is absent — for example on a stdio MCP launch without
the plugin, or an HTTP call that forgot to set it — the server falls
back to the same resolver at startup time, in this order:

1. `MEMINI_DEFAULT_NAMESPACE` (or `MEMINI_NAMESPACE`) env var, if non-empty.
2. `git rev-parse --show-toplevel` in the server's cwd — uses the **repo
   basename**, e.g. `memini` for `/home/dev/memini`.
3. `basename(cwd)` if the cwd is not inside a git worktree.
4. Literal `default` as a last resort.

The resolved value and its source (`env` / `git` / `cwd` / `fallback`) are
logged at startup, e.g.:

```json
{"level":"INFO","msg":"starting memini","default_namespace":"memini","namespace_source":"git",...}
```

In **HTTP mode**, the server-side auto-resolve is misleading: the server
runs detached from the agent's cwd, so the resolved basename reflects
_the server's_ project, not the agent's. Install the plugin (or send the
header explicitly per request) to get the right namespace. In **stdio
mode** the server inherits the agent's cwd, so the fallback is correct.

## Web UI

`memini` ships an embedded admin UI (Preact + Vite, compiled into the binary)
served at `/`. It needs no separate process — open `http://localhost:8080/`.

- **Overview** — per-namespace stats and a tier "strata" bar (working →
  episodic → semantic → procedural).
- **Browser** — paginated, tier/expired/superseded-filterable list with a
  detail drawer and delete.
- **Search** — hybrid recall with relevance scores.
- **Graph** — D3 force-directed view; edges are supersession (directed) and
  shared-tag affinity.
- **Health** — runs `fsck` and surfaces duplicate clusters.

Use the namespace switcher (top bar) to change tenant, and **Settings** to set
a bearer token (sent as `Authorization: Bearer …`) or point the UI at a remote
`memini`. The static shell is unauthenticated so you can enter a token; the
`/v1` API it calls still enforces `MEMINI_API_KEY`. Disable the whole thing with
`MEMINI_UI_ENABLED=false`.

> [!WARNING]
> When `MEMINI_API_KEY` is set, the server embeds the key in the UI shell so the
> same-origin UI authenticates without pasting it — which means **anyone who can
> load `/` can read the key**. Only expose the UI where reaching it already
> implies trust, or set `MEMINI_UI_ENABLED=false` on untrusted networks.

It is backed by three read-only endpoints alongside the core API: `GET
/v1/memories` (list with `tier`/`include_expired`/`include_superseded`/`limit`
filters), `GET /v1/stats`, and `GET /v1/namespaces`.

The UI sources live in [`ui/`](ui/); build the embedded bundle with `mise run
ui` (or iterate with HMR via `mise run ui-dev`, which proxies `/v1` to a local
server on `:8080`). The built bundle under `internal/api/ui/dist/` is a
gitignored build artifact: the Docker image builds it, while a plain `go build`
without it still works and serves a placeholder page.

## Answering

Beyond raw recall, `POST /v1/answer` `{query, limit}` retrieves memories and has
the LLM generate a grounded answer from them, returning the answer plus the
supporting `sources` (requires an LLM; also exposed as the `memory_answer` MCP
tool).

## Reranking — which recall config to use

`MEMINI_RERANK` adds an optional read-side rerank over the hybrid candidates
(`off`, a cross-encoder `/rerank` URL served by Infinity / vLLM / `llama-server
--rerank`, or `llm`). See the [full benchmark table](#benchmark) for measured
numbers across every config and dataset. Two rules of thumb:

- **Reranking only helps where base recall has headroom.** On session-level sets
  hybrid is already at ~98–99% — reranking is a no-op. On turn-level LoCoMo
  (gold = exact turns) it pays off big: **+11pp R@5 / +17pp MRR** (cross-encoder)
  or **+15pp / +25pp** (LLM).
- **The cross-encoder is the better default when you need it:** most of the LLM's
  lift at a fraction of the latency, a tiny 0.6B model, and no chat dependency.
  Use `llm` only if you already run a chat model and want the last few points.

## MCP

memini speaks the Model Context Protocol so agents can `remember`/`recall`/`answer`:

- **Remote (Streamable HTTP):** `http://<host>:8080/mcp`
- **Local (stdio):** `memini mcp`

Ready-to-paste configs for Claude Code, opencode, Codex, Hermes, and OpenClaw —
plus the shared cross-agent namespace trick — live in [`integrations/`](integrations/).
For Claude Code and Codex, prefer the [plugin/](plugin/) which auto-captures
tool calls and injects prior context at session start.

## Importing

`memini import` loads an export from `agentmemory`, `mem0`, `mnemory`, memini's
own format, or your **Claude Code session history**, into the local store or a
running server.

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
(`~/.claude/projects/<project>/<session>.jsonl`), skipping tool-result noise,
sidechains, and slash-command wrappers. IDs are deterministic, so re-importing
is idempotent. Backfilled memories get a fresh 90-day episodic TTL (so old
history isn't swept on arrival) while keeping the original timestamp for
recency ranking. This pairs with the [plugin](plugin/)'s auto-capture: backfill
once, then the hooks keep it current.

Each source's fields map onto memini's tiers (e.g. agentmemory `workflow`→procedural,
mem0 facts→semantic) and namespace (`project`/`user_id`). Records whose source
carries no recognized tier default to **episodic** (90-day TTL), so a bulk import
of unknown quality ages out unless recall reinforces it rather than living forever
as durable facts. Empty records are skipped; per-record failures don't abort the run.
Over `--remote` the server sets its own timestamps, so the source's created-at is
kept in `metadata.imported_created_at`. Reads stdin when the path is `-`.

For low-quality bulk exports, two optional gates drop weak records before they're
written (both off by default):

```sh
# Skip stubs shorter than 40 bytes and anything below importance 0.3:
memini import --source mem0 --min-length 40 --min-importance 0.3 ./export.json
```

Note `--min-importance` skips records whose source reported no importance (they
arrive as `0`); leave it off unless your export carries real importance scores.

## Benchmark

```sh
mise run bench   # offline retrieval benchmark (hybrid vs vector vs keyword)
```

Full results from a `bench/results/` run (written locally; gitignored), all on
the **same all-MiniLM-L6-v2** (384-d) endpoint — the model agentmemory benchmarks
with. Cells are `recall_any@5 / @10 / MRR` (%); `p50` is in-process recall latency
(rerank rows show the cost they add on top):

| Strategy                                | LongMemEval · session  | LoCoMo · turn-level    | LoCoMo · session-level | p50         |
| --------------------------------------- | ---------------------- | ---------------------- | ---------------------- | ----------- |
| vector                                  | 92.6 / 95.4 / 80.7     | 41.3 / 51.8 / 28.1     | 64.1 / 79.8 / 45.2     | <1 ms       |
| keyword (Porter BM25)                   | 97.6 / 99.0 / 92.2     | 58.7 / 67.1 / 44.8     | 92.6 / 96.8 / 79.4     | ~3 ms       |
| **hybrid** (default)                    | **98.4 / 99.2 / 93.0** | **59.7 / 69.9 / 42.4** | **90.9 / 96.6 / 74.3** | ~5 ms       |
| + cross-encoder (`MEMINI_RERANK=<url>`) | 98.4 / 99.2 / 93.1     | **70.9 / 75.0 / 59.8** | 90.9 / 96.6 / 74.3     | +20–230 ms  |
| + LLM rerank (`MEMINI_RERANK=llm`)      | 98.4 / 99.2 / 93.0     | **74.4 / 76.5 / 67.4** | —                      | +350–420 ms |

Questions: LongMemEval 500, LoCoMo turn 1,982, LoCoMo session 1,981 (rerank =
Qwen3-Reranker-0.6B cross-encoder, Qwen3.5-9B LLM). Hybrid never trails either
single leg on the saturated session sets; on turn-level LoCoMo (gold = exact
evidence turns) base recall has headroom, so reranking pays off big —
cross-encoder **+11pp R@5 / +17pp MRR**, LLM **+15pp / +25pp** — while being a
no-op once recall is already at ceiling.

On the **same model, dataset, and metric**, memini hybrid beats agentmemory's
published LongMemEval-S numbers, and goes higher with a premium embedder:

| System                    | Embedding          |       R@5 |      R@10 |
| ------------------------- | ------------------ | --------: | --------: |
| memini — hybrid           | all-MiniLM-L6-v2   | **98.4%** | **99.2%** |
| memini — hybrid           | Qwen3-Embedding-8B | **98.8%** | **99.6%** |
| agentmemory — BM25+Vector | all-MiniLM-L6-v2   |     95.2% |     98.6% |
| agentmemory — BM25-only   | —                  |     86.2% |     94.6% |

memini's Porter-stemming keyword leg is +11pp over their BM25-only. Full
per-leg/per-category tables, parameter sweeps, methodology, caveats, and the
LoCoMo QA comparison (vs mem0/Letta) are in [`bench/`](bench/README.md).

## License

[AGPL-3.0](LICENSE).
