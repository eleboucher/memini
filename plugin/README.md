# memini plugin

A Claude Code / Codex / opencode plugin that wires the [memini](../)
memory service into the agent's lifecycle. It captures what the agent
does, surfaces prior context at session start, and ships skills that teach
the agent _when_ to use the memory tools.

## What it does

| Hook event     | What memini does                                                                                        |
| -------------- | ------------------------------------------------------------------------------------------------------- |
| `SessionStart` | Searches prior context, writes a short block to the agent's input                                       |
| `PreToolUse`   | Before Edit/Write/Read/Glob/Grep, surfaces related memories                                             |
| `PostToolUse`  | Buffers state-changing tool calls locally (no network, no per-call memory)                              |
| `Stop`         | Distills the buffer into a working-tier checkpoint, and periodically nudges an auto-save (below)        |
| `PreCompact`   | Before context compaction, distills the buffer into an episodic emergency checkpoint (Claude Code only) |
| `SessionEnd`   | Distills the buffer into one durable episodic **session digest**                                        |

### Auto-save (Stop)

Agents forget to save. So the `Stop` hook counts the conversation's user
messages (from the transcript) and, every `MEMINI_AUTO_SAVE_INTERVAL` (default
15), **blocks the stop once** with a short instruction: review the conversation
for durable decisions/facts/preferences and persist each via the `memory_remember`
MCP tool. The agent saves, then stops normally (the next `Stop` carries
`stop_hook_active` and passes through — no loop). It nudges at most once per
interval even if the agent saves nothing, and never blocks when the transcript
is unreadable. On by default; set `MEMINI_AUTO_SAVE=0` to disable. Codex sends no
transcript path, so the nudge is inert there.

### Session capture: buffer → digest

Rather than POSTing a memory per tool call (noisy — recall ends up full of thin
fragments), `PostToolUse` appends one JSON line per state-changing call to a
local buffer at `${XDG_CACHE_HOME:-~/.cache}/memini/sessions/<session_id>.jsonl`.
At `SessionEnd` the buffer is distilled into a **single** dense, searchable
episodic memory — files edited (with counts), commands run, event count — then
deleted. `Stop` writes the same digest as a 24h working-tier checkpoint without
deleting the buffer. `SessionStart` also sweeps away buffers older than 7 days
left behind by crashed sessions. Net effect: zero network traffic on the hot
path and one dense memory per session instead of dozens.

Plus 3 skills (`remember`, `recall`, `recap`) the agent invokes directly.

## Install

### Claude Code (one block)

```
Install the memini plugin for persistent memory: run `/plugin marketplace add eleboucher/memini`
then `/plugin install memini`. The plugin registers 6 hooks + 3 skills + the memini MCP server
so the agent has memory_remember / memory_recall / memory_get / memory_forget without extra
config. Verify with `curl http://localhost:8080/healthz`.
```

### Codex CLI

Codex implements a Claude-Code-compatible plugin model: it auto-discovers
`hooks/hooks.json` and expands `${CLAUDE_PLUGIN_ROOT}` (which Codex provides for
compatibility), so the same hook wiring drives both. Mount this directory as a
Codex plugin via `.codex-plugin/plugin.json` — no Codex-specific hooks file is
needed. (Matchers naming Claude-only tools like `Read`/`Glob`/`Grep` just don't
fire under Codex, which exposes `Bash`/`apply_patch`/`mcp__*`.)

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
│   └── hooks.json               # hook wiring (Claude Code + Codex, via ${CLAUDE_PLUGIN_ROOT})
├── scripts/
│   ├── _shared.mjs              # resolveProject, postJSON/Search/Remember, session buffer + digest
│   ├── session-start.mjs
│   ├── session-end.mjs
│   ├── stop.mjs
│   ├── pre-compact.mjs
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
2. `git remote get-url origin` in `data.cwd`, then take the repo basename
   (or the `owner-repo` slug when `MEMINI_NAMESPACE_SCOPE=owner-repo`).
3. `git rev-parse --show-toplevel` in `data.cwd`, then take the basename.
4. `basename(data.cwd)`.

The first resolution is cached in a self-healing project map
(`$XDG_CACHE_HOME/memini/project-map.json`) keyed by both the remote URL and
the repo's path. So if the folder moves (path changes, remote same) or the
`origin` remote is later removed or renamed (path same, remote gone), the
project still resolves to the **same** namespace instead of silently orphaning
its memory. `MEMINI_NAMESPACE` always overrides the cache; delete the map to
re-derive (e.g. after switching `MEMINI_NAMESPACE_SCOPE`).

By default the bare repo name is used (backward compatible). Set
`MEMINI_NAMESPACE_SCOPE=owner-repo` to disambiguate same-named repos under
different owners (`alice/app` → `alice-app`, `bob/app` → `bob-app`); note this
changes the namespace, so existing memory under the bare name needs
`memini namespace move` to migrate.

The server-side auto-resolve (when no namespace header is sent) is only a
fallback for clients that don't send one — it's wrong in HTTP mode because the
server is detached from the agent's cwd.

## Environment

| Env var                     | Default                     | Used by      | Description                                                  |
| --------------------------- | --------------------------- | ------------ | ------------------------------------------------------------ |
| `MEMINI_URL`                | `http://localhost:8080`     | hooks (REST) | memini base URL for the lifecycle hooks                      |
| `MEMINI_MCP_URL`            | `http://localhost:8080/mcp` | MCP tools    | memini `/mcp` URL for the model-invoked memory tools         |
| `MEMINI_TOKEN`              | —                           | hooks + MCP  | bearer token; required when the server sets `MEMINI_API_KEY` |
| `MEMINI_NAMESPACE`          | auto (cwd/git basename)     | hooks + MCP  | explicit namespace override; otherwise auto-resolved         |
| `MEMINI_NAMESPACE_SCOPE`    | `repo`                      | hooks        | `owner-repo` derives `owner-repo` slugs from the git remote  |
| `MEMINI_AUTO_SAVE`          | on                          | `Stop` hook  | set to `0` to disable the periodic auto-save nudge           |
| `MEMINI_AUTO_SAVE_INTERVAL` | `15`                        | `Stop` hook  | user messages between auto-save nudges                       |
| `MEMINI_DEBUG`              | —                           | hooks        | set to `1` for verbose hook logging                          |

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
