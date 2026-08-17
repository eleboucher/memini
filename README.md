# memini

> A shared, persistent memory service for AI agents.

`memini` gives any [MCP](https://modelcontextprotocol.io)-capable agent (Claude Code,
opencode, Codex, Hermes, OpenClaw, Open WebUI) one place to `remember` and `recall`,
with retrieval quality that compounds over time. It runs as a single Go binary, boots
with zero configuration, and scales from an embedded SQLite file on a laptop to Postgres
in Kubernetes.

## Documentation

| I want to...                                 | Go to                                                                                                      |
| -------------------------------------------- | ---------------------------------------------------------------------------------------------------------- |
| Get it running in five minutes               | [Quick start](#quick-start), then [Solo laptop](docs/guides/solo-laptop.md)                                |
| Self-host it for my team                     | [Homelab and team](docs/guides/homelab-team.md)                                                            |
| **Fix bad recall**                           | [Tuning recall](docs/guides/tuning-recall.md)                                                              |
| Lay out namespaces for several agents        | [Multi-agent namespaces](docs/guides/multi-agent-namespaces.md)                                            |
| **Upgrade, and my server will not start**    | [Upgrading](docs/operations/upgrading.md)                                                                  |
| Look up a setting                            | [Configuration](docs/reference/configuration.md)                                                           |
| Look up an MCP tool, CLI command or endpoint | [MCP tools](docs/reference/mcp-tools.md), [CLI](docs/reference/cli.md), [REST](docs/reference/rest-api.md) |
| Understand how it works under the hood       | [How it works](docs/how-it-works/README.md)                                                                |
| See a full worked example                    | [Examples](docs/examples/README.md)                                                                        |
| Understand tiers, scopes, categories, keys   | [Concepts](docs/README.md#concepts)                                                                        |
| See the retrieval numbers                    | [Benchmarks](bench/README.md)                                                                              |

Everything is indexed in [`docs/`](docs/README.md).

## How it works

memini draws on three earlier projects:

- A curated, deduplicated artifact rather than a pile of chunks (after Karpathy's
  "LLM wiki").
- Tiered memory (working, episodic, semantic, procedural) with decay and hybrid
  (vector + keyword) retrieval fused with Reciprocal Rank Fusion (after `agentmemory`).
  See [docs/tiers.md](docs/tiers.md) for what each tier means and how memories move
  between them.
- A stateless, K8s-native HTTP service with an opt-in LLM consolidation pipeline,
  per-memory TTLs, per-namespace isolation, Prometheus metrics, and an `fsck` consistency
  checker (after `mnemory`).

Hybrid results are re-ranked by a composite of relevance, access recency, and importance
rather than similarity alone, and near-duplicates are collapsed at recall time.

An LLM is optional. With one configured, writes are stored immediately and then
deduplicated and contradiction-resolved in the background, and each fresh episodic
capture is distilled into durable semantic facts at write time, so a fact stated once is
durable immediately. Without one, marker heuristics run the same lifecycle, so durable
knowledge still accumulates in an embedder-only deployment.

| Concern    | Choice                                                                                     |
| ---------- | ------------------------------------------------------------------------------------------ |
| Language   | Go: single static binary, tiny image, low memory                                           |
| Storage    | Pluggable: **sqlite-vec** (embedded, default) or **Postgres + VectorChord** (scale)        |
| Embeddings | External OpenAI-compatible endpoint (you deploy the model)                                 |
| LLM        | **Opt-in**: runs headless without one                                                      |
| Ranking    | Hybrid (vector + keyword) RRF, re-ranked by relevance + recency + importance, deduplicated |
| Interfaces | REST + MCP (stdio and Streamable HTTP) + embedded web UI, sharing one service layer        |

## Quick start

memini boots with zero configuration in its embedded SQLite mode. Vector search is the
one thing it cannot invent, so point it at any OpenAI-compatible embeddings endpoint:

```sh
export MEMINI_EMBED_BASE_URL=http://localhost:8081/v1
export MEMINI_EMBED_MODEL=bge-m3
export MEMINI_EMBED_DIMS=1024   # must match the model
mise run run
curl -s localhost:8080/healthz
```

`MEMINI_EMBED_DIMS` has to match the model you point at. That is the most common setup
mistake, and it corrupts the store rather than failing cleanly.

For the full walk-through, including wiring it into an agent, see
[Solo laptop](docs/guides/solo-laptop.md). For Docker, Compose and Kubernetes, see
[Deployment](docs/operations/deployment.md).

## Agent plugin

Most integrations read **`MEMINI_BASE_URL`** for the server and
**`MEMINI_API_KEY`** for the token. `MEMINI_URL` and `MEMINI_TOKEN` are removed:
clients warn once at session start and otherwise ignore them. The native Codex plugin is the exception: its
bundled server uses a fixed local URL and accepts only `MEMINI_API_KEY`; see the
remote override recipe below. Where an integration has its own config (opencode
options, Open WebUI Valves, `openclaw.json`), that config wins over the
environment.

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

**Hermes:** `hermes plugins install eleboucher/memini-hermes`

**OpenClaw:** `openclaw plugins install clawhub:@eleboucher/memini`

**Open WebUI:** paste [`filter/memini_memory.py`](integrations/openwebui/filter/memini_memory.py)
into Admin Panel, Functions, and optionally
[`tools/memini_tools.py`](integrations/openwebui/tools/memini_tools.py) into Workspace,
Tools.

**Codex:**

```sh
codex plugin marketplace add eleboucher/memini
codex plugin add memini@memini
```

Start the local server first (`memini`, default
`http://localhost:8080`), set `MEMINI_API_KEY` when authentication is enabled,
review and trust the bundled hooks with `/hooks`, then start a new thread.
Remote and custom-server setup is covered in
[`integrations/codex/`](integrations/codex/).

Full details, edge cases and every client-side setting live in
[`integrations/`](integrations/) and [`plugin/README.md`](plugin/README.md). If a variable
seems to have no effect, check [server vs client variables](docs/reference/env-vars.md):
four names mean different things on each side.

## Using it as an MCP server

memini speaks the Model Context Protocol, over two transports:

- **Remote (Streamable HTTP):** `http://<host>:8080/mcp`
- **Local (stdio):** `memini mcp`

The nine tools an agent sees, their parameters, and the standing policy the server sends
every client on connect are in [MCP tools](docs/reference/mcp-tools.md). Ready-to-paste
client configs are in [`integrations/`](integrations/).

## Benchmarks

memini's hybrid retrieval beats `agentmemory`'s published LongMemEval-S numbers on the
same model, dataset and metric (98.4% recall@5 against 95.2%), and reranking adds
double-digit gains on the turn-level sets where base recall still has headroom.

```sh
mise run bench
```

Full tables, per-leg and per-category breakdowns, the tune and held-out split, parameter
sweeps, methodology, caveats, and the LoCoMo comparison against mem0 and Letta are in
[`bench/README.md`](bench/README.md).

## Contributing

See [`CONTRIBUTING.md`](CONTRIBUTING.md) for the dev loop, the generated-code
drift gates, and the docs conventions. The short version: the reference under
[`docs/reference/`](docs/reference/) is generated from the code
(`mise run docs`), and CI fails if it has drifted. If you add a setting, a CLI command or
an MCP tool, run `mise run docs` and commit the result. The generator refuses to run when
a new setting has no doc comment or belongs to no section, which is deliberate: that is
how a knob ships undocumented.

## License

[AGPL-3.0](LICENSE).
