# memini plugin

A Claude Code / Codex / opencode plugin that wires the [memini](../)
memory service into the agent's lifecycle. It captures what the agent
does, surfaces prior context at session start, and ships skills that teach
the agent _when_ to use the memory tools.

## What it does

| Hook event     | What memini does                                                  |
| -------------- | ----------------------------------------------------------------- |
| `SessionStart` | Searches prior context, writes a short block to the agent's input |
| `PreToolUse`   | Before Edit/Write/Read/Glob/Grep, surfaces related memories       |
| `PostToolUse`  | Records a one-line episodic note for state-changing tool calls    |
| `Stop`         | Drops a working-tier checkpoint                                   |
| `SessionEnd`   | Writes a durable session-end marker                               |

Plus 3 skills (`remember`, `recall`, `recap`) the agent invokes directly.

## Install

### Claude Code (one block)

```
Install the memini plugin for persistent memory: run `/plugin marketplace add eleboucher/memini`
then `/plugin install memini`. The plugin registers 5 hooks + 3 skills + the memini MCP server
so the agent has memory_remember / memory_recall / memory_get / memory_forget without extra
config. Verify with `curl http://localhost:8080/healthz`.
```

### Codex CLI

The hooks reuse Claude Code's protocol and resolve paths from
`$CODEX_PLUGIN_ROOT`. Mount this directory as a Codex plugin.

### opencode

opencode doesn't use the Claude Code hook protocol; it has its own plugin
system. See the [opencode recipe](../integrations/opencode/) for the MCP
side. The opencode-side auto-capture is a separate concern that can land
later — for now, opencode gets the same `memory_*` MCP tools and skills
as the others.

## Layout

```
plugin/
├── .claude-plugin/plugin.json   # Claude Code manifest
├── .codex-plugin/plugin.json    # Codex manifest
├── hooks/
│   ├── hooks.json               # Claude Code hook wiring
│   └── hooks.codex.json         # Codex hook wiring
├── scripts/
│   ├── _shared.mjs              # resolveProject, postJSON, postSearch, postRemember
│   ├── session-start.mjs
│   ├── session-end.mjs
│   ├── stop.mjs
│   ├── pre-tool-use.mjs
│   └── post-tool-use.mjs
└── skills/
    ├── remember/SKILL.md
    ├── recall/SKILL.md
    └── recap/SKILL.md
```

## How the namespace gets resolved

The plugin is the **authoritative** namespace resolver. Each hook script
runs `resolveProject(data.cwd)` and sends the result as the
`X-Memini-Namespace` header. Resolution order:

1. `MEMINI_NAMESPACE` env var, if set.
2. `git rev-parse --show-toplevel` in `data.cwd`, then take the basename.
3. `basename(data.cwd)`.

This matches agentmemory's resolver. The server-side auto-resolve (when
no namespace header is sent) is only a fallback for clients that don't
send one — it's wrong in HTTP mode because the server is detached from
the agent's cwd.

## Environment

| Env var            | Default                     | Used by      | Description                                                  |
| ------------------ | --------------------------- | ------------ | ------------------------------------------------------------ |
| `MEMINI_URL`       | `http://localhost:8080`     | hooks (REST) | memini base URL for the lifecycle hooks                      |
| `MEMINI_MCP_URL`   | `http://localhost:8080/mcp` | MCP tools    | memini `/mcp` URL for the model-invoked memory tools         |
| `MEMINI_TOKEN`     | —                           | hooks + MCP  | bearer token; required when the server sets `MEMINI_API_KEY` |
| `MEMINI_NAMESPACE` | auto (cwd/git basename)     | hooks + MCP  | explicit namespace override; otherwise auto-resolved         |
| `MEMINI_DEBUG`     | —                           | hooks        | set to `1` for verbose hook logging                          |

## Remote memini

The plugin works against a remote memini with **no code changes** — point it at
the server and give it a token:

```sh
export MEMINI_URL=https://memini.example.com
export MEMINI_MCP_URL=https://memini.example.com/mcp
export MEMINI_TOKEN=<the server's MEMINI_API_KEY>
```

Both the hooks (REST) and the MCP tools then send `Authorization: Bearer
$MEMINI_TOKEN`. The **namespace stays per-project** even against one shared
remote: the hooks resolve it from `data.cwd`, and the MCP server's
`headersHelper` (`scripts/mcp-headers.mjs`) resolves the _same_ value from
`CLAUDE_PROJECT_DIR` per connection — so capture and recall always target the
same namespace. Run the server with `MEMINI_API_KEY` set so `/mcp` (and `/v1`)
require the token.
