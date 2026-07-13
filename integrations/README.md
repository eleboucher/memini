# Agent integrations

memini exposes its tools over the **Model Context Protocol (MCP)**, so any
MCP-capable agent connects with config only — no per-agent code. Two
transports:

- **Remote (Streamable HTTP):** `http://<memini-host>:8080/mcp` — best for a
  shared, always-on deployment.
- **Local (stdio):** `memini mcp` — the agent spawns memini as a subprocess.

Tools exposed: `memory_briefing`, `memory_recall`, `memory_remember`,
`memory_answer`, `memory_list`, `memory_get`, `memory_history`,
`memory_update`, `memory_forget`.

`memory_answer` is only registered when the server has an LLM configured;
without one it does not exist rather than existing and failing. Full
parameters and the standing policy the server sends on connect are in
[docs/reference/mcp-tools.md](../docs/reference/mcp-tools.md), which is
generated from the server itself.

## Recommended: install the plugin

memini ships a [plugin](../plugin/) that wires the MCP server, lifecycle
hooks (context injection at session start, recall before file ops,
auto-capture of tool calls), and skills in one shot. This is the path
that matches how [agentmemory](https://github.com/rohitg00/agentmemory)
does it, and gives you the auto-capture + context-injection loop that
makes memory actually useful.

**Claude Code / Codex:** install via the host's plugin manager
(`/plugin marketplace add eleboucher/memini` then
`/plugin install memini` for Claude Code; mount the directory as a Codex
plugin for Codex CLI).

**opencode:** install the native [opencode plugin](opencode/) for the
same auto-capture + recall loop (it hooks `chat.message` and
`session.idle`), or wire the MCP server from the recipe below to expose
the tools on demand.

**Others:** wire the MCP server from the recipes below; the Claude
Code/Codex hooks layer is host-specific, so the agent has to be told to
use the tools via a `CLAUDE.md`-style file.

## Shared namespace across agents

Every request is scoped to a **namespace** (tenant/agent). Point multiple
agents at the same memini with the **same namespace** and they share one
memory: remember a fact in Claude Code, recall it in Codex.

- Remote transport: set the `X-Memini-Namespace` header (also set `X-Memini-Home`
  to give the agent a personal namespace — see below).
- stdio transport: set `MEMINI_DEFAULT_NAMESPACE` (and `MEMINI_HOME`).
- REST callers may additionally override the namespace(s) searched with a
  per-call `namespaces` argument; the MCP tools don't expose this — namespaces
  are managed for you there, resolved from the request/env, never typed by
  the model.

As an alternative to one shared namespace, agents can keep private namespaces
and still see relevant memory read-only, via memini's ancestor/home/link
cascade — no server-wide opt-in flag required:

- **Ancestors**: a namespace automatically inherits its durable (semantic/
  procedural) memories from every ancestor in its path — `acme/phoenix/api`
  reads from `acme/phoenix` and `acme` too. Writing a fact with
  `visibility: "personal"` writes it to the caller's home namespace instead;
  naming an ancestor writes it there. `memory_remember`'s `visibility` field
  (default `project`) controls this on write; recall/briefing always inherit
  the chain automatically.
- **Home**: set `X-Memini-Home` / `MEMINI_HOME` to a personal namespace (e.g.
  `personal/kit`) and it merges into every recall/briefing as a read-only leg,
  independent of which project namespace the agent is scoped to.
- **Links**: `POST /v1/links` (`{"dst": "<namespace>"}`, X-Memini-Namespace as
  the src) stores an explicit one-way durable-tier read link between two
  namespaces (e.g. sharing conventions across unrelated repos) without an
  ancestor relationship.
- `scope` on `memory_recall`/`memory_briefing`/`memory_answer` picks how wide
  to read: `project` (just this namespace), `full` (default: project plus the
  ancestor/home/link cascade), or `everywhere` (`full` plus nested
  sub-projects — the old subtree pattern, now built in rather than requiring
  `MEMINI_AGENT` nesting).

Every result carries `from` provenance (which ancestor/home/link it came
from, or absent for the primary namespace) so an agent can see where
knowledge lives without guessing. See
[docs/scopes.md](../docs/scopes.md#data-flow-read) for the full model.

### How the namespace is resolved

**Authoritative source: the plugin hooks.** Each hook script calls
`resolveProject(data.cwd)` and sends the result as the `X-Memini-Namespace`
header. Resolution order: `MEMINI_NAMESPACE` env > repo name from `git remote
get-url origin` (stable across worktrees and clones) > `git rev-parse
--show-toplevel` basename > `basename(cwd)`, cached in a self-healing project
map so a later folder move or dropped remote resolves back to the same
namespace. See [`../plugin/README.md`](../plugin/README.md#how-the-namespace-gets-resolved)
for the full order.

**Server-side fallback:** if no `X-Memini-Namespace` header is sent
(typical for stdio without a plugin, or HTTP clients that forget it),
the server falls back to a similar resolver at startup time (it skips the
git-remote step, so existing stores keyed by the worktree basename stay
put). The result and its source (`env` / `git` / `cwd` /
`fallback`) are logged at startup. This is the only correct behavior
for the stdio case, but **wrong** for HTTP — in HTTP mode the server
runs detached from the agent's cwd, so install the plugin (or send the
header explicitly) instead of relying on the server-side resolve.

## Recipes

| Agent       | Folder                     | Transport                               |
| ----------- | -------------------------- | --------------------------------------- |
| Claude Code | [`../plugin/`](../plugin/) | HTTP (plugin)                           |
| Codex CLI   | [`codex/`](codex/)         | stdio (plugin) or HTTP                  |
| opencode    | [`opencode/`](opencode/)   | native plugin (or HTTP / stdio MCP)     |
| Pi          | [`pi/`](pi/)               | native extension (or MCP via extension) |
| Hermes      | [`hermes/`](hermes/)       | native MemoryProvider plugin (or MCP)   |
| OpenClaw    | [`openclaw/`](openclaw/)   | native memory-slot extension (or skill) |
| Open WebUI  | [`openwebui/`](openwebui/) | native Filter plugin (or Tools / MCP)   |

All recipes assume memini is reachable and that its embeddings endpoint
is configured (`MEMINI_EMBED_BASE_URL`). If memini requires a bearer
token (`MEMINI_API_KEY`), add `Authorization: Bearer <token>` to HTTP
headers.

## Defaults & cross-host notes

The native plugins (opencode, Pi, Hermes, OpenClaw, Open WebUI, and the
Claude Code plugin) share the same recall defaults: **recall and capture
on, `recall_limit=3`, recall token budget uncapped**, plain bullet
formatting (set `MEMINI_INJECT_LABELS=tier` to add a tier prefix — supported
by opencode, Pi, Hermes, and OpenClaw; the Open WebUI filter always emits
plain bullets). Each
auto-captures the completed user→assistant turn as episodic memory and
excludes its **own** session's captures from recall so it never echoes
the live conversation back.

Where a host exposes explicit memory tools, the set is the same:
`memory_recall`, `memory_list`, `memory_remember`, and **`memory_forget`**
(permanently delete a wrong/outdated/poisoned memory by its id). Hermes
and Open WebUI (Tools module) always expose them; Pi registers them natively via
`pi.registerTool`; OpenClaw registers them by default (set `expose_tools: false`
to opt out); Claude Code / Codex get them from the MCP server. **opencode**
is the exception: its native plugin is deliberately tool-free (automatic recall +
capture only), so to give it `memory_forget` (or any tool) wire the memini MCP
server alongside the plugin.

**`memory_update` (partial update by id) is MCP-only** — REST has no PATCH
endpoint, but `POST /v1/memories` **upserts when given an `id`**, replacing the
memory in place while preserving its identity and history. The REST-backed
plugins (Hermes, Pi, OpenClaw, Open WebUI) therefore accept an optional `id` on
`memory_remember`, and their tool descriptions point to re-remember-with-id as
the way to correct a stored fact; `memory_forget` is only for memories that
should not exist at all. Wire the MCP server alongside a native plugin if a
host needs true partial updates (`memory_update` merges field-by-field; the
REST upsert replaces the record with what you send).

Two host-specific differences remain by design:

- **Relevance floor (`MEMINI_INJECT_RECALL_MIN_SCORE`)** is honored by
  opencode, Pi, and Hermes but not OpenClaw/Open WebUI. It defaults to `0`
  (off) everywhere, so default behavior is identical; benchmarking found
  no score floor reliably separates signal from noise with the default
  embedder (use `recall_limit` to bound volume instead), which is why
  OpenClaw omits the knob entirely.
- **Shared-namespace echo:** the echo-exclusion is keyed on each host's
  native conversation id (`session_id`, or `chat_id` on Open WebUI). If
  you point two _different_ integrations at the **same** namespace, one
  can't exclude the other's just-captured turns — give chatty agents
  their own namespace (or an agent segment) if that matters.
