# Agent integrations

memini exposes its tools over the **Model Context Protocol (MCP)**, so any
MCP-capable agent connects with config only — no per-agent code. Two
transports:

- **Remote (Streamable HTTP):** `http://<memini-host>:8080/mcp` — best for a
  shared, always-on deployment.
- **Local (stdio):** `memini mcp` — the agent spawns memini as a subprocess.

Tools exposed:

- `memory_remember`, `memory_recall`, `memory_briefing`, `memory_answer`
- `memory_list`, `memory_get`, `memory_forget`

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

- Remote transport: set the `X-Memini-Namespace` header.
- stdio transport: set `MEMINI_DEFAULT_NAMESPACE`.
- Either way, individual tool calls may override it with a `namespace` argument.

### How the namespace is resolved

**Authoritative source: the plugin hooks.** Each hook script calls
`resolveProject(data.cwd)` and sends the result as the
`X-Memini-Namespace` header. Resolution order: `MEMINI_NAMESPACE` env >
`git rev-parse --show-toplevel` basename > `basename(cwd)`. This
matches how agentmemory does it.

**Server-side fallback:** if no `X-Memini-Namespace` header is sent
(typical for stdio without a plugin, or HTTP clients that forget it),
the server falls back to the same resolver at startup time, with the
same priority. The result and its source (`env` / `git` / `cwd` /
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
| Hermes      | [`hermes/`](hermes/)       | native MemoryProvider plugin (or MCP)   |
| OpenClaw    | [`openclaw/`](openclaw/)   | native memory-slot extension (or skill) |
| Open WebUI  | [`openwebui/`](openwebui/) | native Filter plugin (or Tools / MCP)   |

All recipes assume memini is reachable and that its embeddings endpoint
is configured (`MEMINI_EMBED_BASE_URL`). If memini requires a bearer
token (`MEMINI_API_KEY`), add `Authorization: Bearer <token>` to HTTP
headers.
