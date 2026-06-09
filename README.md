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

| Concern    | Choice                                                                              |
| ---------- | ----------------------------------------------------------------------------------- |
| Language   | Go — single static binary, tiny image, low memory                                   |
| Storage    | Pluggable: **sqlite-vec** (embedded, default) or **Postgres + VectorChord** (scale) |
| Embeddings | External OpenAI-compatible endpoint (you deploy the model)                          |
| LLM        | **Opt-in** — runs headless without one; enables background dedup, consolidation, and episodic→semantic promotion when configured |
| Ranking    | Hybrid (vector + keyword) RRF, re-ranked by relevance + recency + importance, deduplicated |
| Interfaces | REST (OpenAPI) + MCP (stdio & Streamable HTTP), sharing one service layer           |

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

### Configuration (12-factor)

| Env var                    | Default                  | Description                                                            |
| -------------------------- | ------------------------ | ---------------------------------------------------------------------- |
| `MEMINI_HTTP_ADDR`         | `:8080`                  | HTTP listen address                                                    |
| `MEMINI_BACKEND`           | `sqlite`                 | `sqlite` or `postgres`                                                 |
| `MEMINI_SQLITE_PATH`       | `memini.db`              | sqlite database path                                                   |
| `MEMINI_POSTGRES_DSN`      | —                        | required when `MEMINI_BACKEND=postgres`                                |
| `MEMINI_EMBED_BASE_URL`    | —                        | OpenAI-compatible embeddings endpoint                                  |
| `MEMINI_EMBED_MODEL`       | `text-embedding-3-small` | embedding model name                                                   |
| `MEMINI_EMBED_DIMS`        | `1536`                   | embedding dimensions (must match model)                                |
| `MEMINI_LLM_BASE_URL`      | —                        | opt-in LLM endpoint; empty disables it                                 |
| `MEMINI_LLM_API`           | `openai`                 | chat backend: `openai` or `anthropic` (e.g. MiniMax)                   |
| `MEMINI_LLM_MODEL`         | `gpt-4o-mini`            | consolidation model name                                               |
| `MEMINI_CONSOLIDATE_MODE`  | `async`                  | `async` (store now, dedup in background), `sync`, or `off`             |
| `MEMINI_CONSOLIDATE_MIN_SCORE` | `0.6`                | similarity gate: skip the LLM when the nearest candidate scores below it (`0` disables) |
| `MEMINI_PROMOTE_INTERVAL`  | `24h`                    | how often frequently-used episodic memories are distilled into semantic facts (`0` disables; needs LLM) |
| `MEMINI_PROMOTE_MIN_ACCESS` | `3`                     | minimum recall count before an episodic memory is eligible for promotion |
| `MEMINI_API_KEY`           | —                        | if set, required as a bearer token (also gates `/metrics`)             |
| `MEMINI_NAMESPACE_HEADER`  | `X-Memini-Namespace`     | header used to scope tenants                                           |
| `MEMINI_DEFAULT_NAMESPACE` | auto                     | fallback namespace (see [Namespace resolution](#namespace-resolution)) |
| `MEMINI_LOG_LEVEL`         | `info`                   | `debug`/`info`/`warn`/`error`                                          |
| `MEMINI_LOG_FORMAT`        | `json`                   | `json` or `text`                                                       |

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

## MCP

memini speaks the Model Context Protocol so agents can `remember`/`recall`:

- **Remote (Streamable HTTP):** `http://<host>:8080/mcp`
- **Local (stdio):** `memini mcp`

Ready-to-paste configs for Claude Code, opencode, Codex, Hermes, and OpenClaw —
plus the shared cross-agent namespace trick — live in [`integrations/`](integrations/).
For Claude Code and Codex, prefer the [plugin/](plugin/) which auto-captures
tool calls and injects prior context at session start.

## Importing

`memini import` loads an export from `agentmemory`, `mem0`, `mnemory`, or memini's
own format, into the local store or a running server.

```sh
# Local store (embeds + preserves source IDs, timestamps, tiers):
memini import --source agentmemory ./agentmemory-export.json

# Remote server over REST:
memini import --source mem0 --remote https://memini.example.com \
  --token "$MEMINI_API_KEY" --namespace my-project ./mem0-export.json
```

Each source's fields map onto memini's tiers (e.g. agentmemory `workflow`→procedural,
mem0 facts→semantic) and namespace (`project`/`user_id`). Empty records are skipped;
per-record failures don't abort the run. Over `--remote` the server sets its own
timestamps, so the source's created-at is kept in `metadata.imported_created_at`.
Reads stdin when the path is `-`.

## Benchmark

```sh
mise run bench   # offline retrieval benchmark (hybrid vs vector vs keyword)
```

On the full **500-question LongMemEval-S** (`recall_any@K`), memini hybrid beats
agentmemory on the **same embedding model** (all-MiniLM-L6-v2, 384-d) — a true
apples-to-apples head-to-head — and goes higher with a premium model:

| System                    | Embedding          |       R@5 |      R@10 |
| ------------------------- | ------------------ | --------: | --------: |
| memini — hybrid (RRF)     | all-MiniLM-L6-v2   |     96.8% |     98.6% |
| memini — hybrid (RRF)     | Qwen3-Embedding-8B | **97.6%** | **98.4%** |
| agentmemory — BM25+Vector | all-MiniLM-L6-v2   |     95.2% |     98.6% |
| agentmemory — BM25-only   | —                  |     86.2% |     94.6% |

Same model, dataset, and metric; memini's Porter-stemming keyword leg is +11pp
over their BM25-only. Full per-leg/per-category tables, methodology, caveats, and
the LoCoMo QA comparison (vs mem0/Letta) are in [`bench/`](bench/README.md).

## License

[AGPL-3.0](LICENSE).
