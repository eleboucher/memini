# memini plugin

A Claude Code / Codex / opencode plugin that wires the [memini](../)
memory service into the agent's lifecycle. It captures what the agent
does, surfaces prior context at session start, and ships skills that teach
the agent _when_ to use the memory tools.

## What it does

| Hook event     | What memini does                                                                                                                                                                                   |
| -------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `SessionStart` | Searches prior context, writes a short block to the agent's input                                                                                                                                  |
| `PreToolUse`   | Before Edit/Write/Read/Glob/Grep, surfaces related memories                                                                                                                                        |
| `PostToolUse`  | Buffers state-changing tool calls locally (no network, no per-call memory)                                                                                                                         |
| `Stop`         | Distills the buffer into a working-tier checkpoint, captures the last turn as episodic memory, scrapes legacy inline `<memory>` blocks (back-compat), and periodically nudges an auto-save (below) |
| `PreCompact`   | Before context compaction, distills the buffer into an episodic emergency checkpoint (Claude Code only)                                                                                            |
| `SessionEnd`   | Distills the buffer into one durable episodic **session digest**                                                                                                                                   |

### Auto-save (Stop)

Agents forget to save. So the `Stop` hook counts the conversation's user
messages (from the transcript) and, every `MEMINI_AUTO_SAVE_INTERVAL` (default
10), **blocks the stop once** with a short instruction: review the conversation
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

### Automatic memory capture (Stop)

Two layers run at every `Stop`, both **on by default** so the plugin produces
real memories out of the box — not just session digests:

- **Turn capture** (`MEMINI_CAPTURE_TURNS`): the last user→assistant turn is
  stored as an **episodic** memory (deduped on the assistant message id), the
  same automatic per-turn recall layer the opencode plugin gets from
  `session.idle`. Set to `0` to disable.
- **Memory directive** (`MEMINI_INLINE_EXTRACT`): `SessionStart` injects a
  short directive asking the agent to persist durable facts via the
  `memory_remember` MCP tool (tier `semantic`) instead of printing them into
  its reply. `Stop` still scans transcripts for legacy `<memory>` blocks and
  persists those too, as a back-compat fallback for sessions started under the
  old directive. Model-curated, so it stays low-noise. Set to `0` to disable
  both.

Plus 3 skills (`remember`, `recall`, `recap`) the agent invokes directly.

### Turning session digests off (`MEMINI_SESSION_DIGEST=0`)

Session digests are **activity records**, not knowledge: "edited `auth.go` (3), ran
`go test ./...`". They answer "what was I doing in this repo last week", which is
genuinely useful to some people and pure noise to others. If you want memini to
hold only durable facts, every session otherwise adds a memory that will never
answer a question and quietly dilutes recall.

```sh
export MEMINI_SESSION_DIGEST=0
```

That switches off all four write sites at once, since they are the same distilled
buffer: the `SessionEnd` episodic digest, the `Stop` working-tier checkpoint, the
`PreCompact` rescue copy, and the `PostToolUse` buffering that feeds them (with
digests off, nothing would ever read the buffer, so the hot-path write is skipped
too).

It is deliberately separate from the two knobs it is easy to confuse it with:

| Knob                    | What it controls                                            |
| ----------------------- | ----------------------------------------------------------- |
| `MEMINI_SESSION_DIGEST` | Activity records: what you edited and ran                   |
| `MEMINI_CAPTURE_TURNS`  | Each user→assistant turn, stored as episodic memory         |
| `MEMINI_INLINE_EXTRACT` | The directive asking the agent to save durable facts itself |

Turning digests off leaves the other two alone, so the agent keeps saving decisions
and conventions through `memory_remember`. `/memini:status` shows all three with
their current values.

## Slash commands

| Command             | What it does                                                                        |
| ------------------- | ----------------------------------------------------------------------------------- |
| `/memini:status`    | Effective settings, resolved namespace **with provenance**, read set, server health |
| `/memini:namespace` | Show, set, or clear the server-side namespace **pin** for this project              |
| `/memini:remember`  | Save a fact (drives `memory_remember`)                                              |
| `/memini:recall`    | Search memory (drives `memory_recall`)                                              |
| `/memini:pin`       | Pin a memory so it surfaces in every briefing                                       |
| `/memini:forget`    | Delete a memory (confirms first)                                                    |
| `/memini:doctor`    | Store-level diagnostics — **needs the `memini` binary**                             |
| `/memini:backfill`  | Import past sessions — **needs the `memini` binary**                                |

`status` and `namespace` are implemented in the plugin itself, so they work for
plugin-only installs pointed at a remote server — which do not have the `memini`
binary at all.

### `/memini:status`

Answers "what is this plugin actually doing right now?", and more usefully _why_.
Every setting carries its provenance (`<- env` vs `(default)`), because a list of
values alone is nearly useless: it would show `namespace: default` and look fine.
What catches a real problem is the line underneath saying git would have given
`memini` — which is how a forgotten `MEMINI_NAMESPACE` export (a shell rc, or a
fish _universal_ variable) gets found after quietly collapsing every repo on the
machine into one shared memory pool.

It cross-checks the three things that must agree — the namespace the **hooks**
write to, the namespace the **MCP tools** write to, and the **read set** the
server assembles — and warns when they diverge. Secrets are always redacted, so
the output is safe to paste into an issue.

### `/memini:namespace`

```
/memini:namespace                # show the namespace and where it came from
/memini:namespace acme/api       # pin it for this project
/memini:namespace --clear        # back to automatic resolution
```

A **pin** lives on the memini server (keyed by the project's git remote /
toplevel), so it **follows you across machines** and every client — the hooks,
the MCP tools, and the `memini` CLI — resolves the same namespace from it. A pin
**beats `MEMINI_NAMESPACE`**: a globally exported `MEMINI_NAMESPACE` pins every
repo on the machine to one namespace, and if the environment won, this command
would silently do nothing on exactly the machines that most need it.

Two caveats, both stated by the command itself:

- **The hooks pick it up on their next invocation**; the MCP tools need
  **`/reload-plugins`**. Claude Code runs the MCP `headersHelper` only when the
  server _connects_, so `memory_remember` / `memory_recall` keep targeting the
  old namespace until the plugin reconnects.
- **Scope is the project** (its git remote / toplevel), not the session. Two
  sessions in the same repo share the pin. The `headersHelper` is given no
  session id, so it cannot tell them apart — per-project is the honest
  granularity here.

Because pins are server-side, setting or clearing one needs the server reachable.
When it is not, the command says so and points you at `MEMINI_NAMESPACE=<ns>` as a
machine-local offline override.

## Install

### Claude Code (one block)

```
Install the memini plugin for persistent memory: run `/plugin marketplace add eleboucher/memini`
then `/plugin install memini`. The plugin registers 6 hooks + 3 skills + the memini MCP server
so the agent has memory_remember / memory_recall / memory_get / memory_update / memory_forget /
memory_list / memory_briefing (plus memory_answer, when an LLM is configured) without extra
config. Verify with `curl http://localhost:8080/healthz`.
```

The memory directive makes the agent call `memory_remember` on its own, which
triggers a permission prompt on first use. To pre-approve, add the tools to
`permissions.allow` in your Claude Code settings, e.g.
`"mcp__plugin_memini_memini__memory_remember"` (plugin-shipped MCP tools are
namespaced `mcp__plugin_memini_memini__*`).

### Codex CLI

Codex implements a Claude-Code-compatible plugin model: it auto-discovers
`hooks/hooks.json` and expands `${CLAUDE_PLUGIN_ROOT}` (which Codex provides for
compatibility), so the same hook wiring drives both. Mount this directory as a
Codex plugin via `.codex-plugin/plugin.json` — no Codex-specific hooks file is
needed. (Matchers naming Claude-only tools like `Read`/`Glob`/`Grep` just don't
fire under Codex, which exposes `Bash`/`apply_patch`/`mcp__*`.) The
`PostToolUse` hook captures Codex `apply_patch` calls by parsing the patch
header lines, so session digests still list edited files.

### opencode

opencode doesn't use the Claude Code hook protocol; it has its own plugin
system, shipped separately at [integrations/opencode/](../integrations/opencode/).
That plugin already does automatic memory — recall on `chat.message` and
per-turn episodic capture on `session.idle`, both on by default — plus the same
`memory_*` MCP tools and skills. Defaults match this plugin (`recall_limit=3`,
uncapped); see the opencode recipe for its options.

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

The **server** is the authoritative namespace resolver. On `SessionStart` the
plugin gathers what it knows about the project — the git remote URL, the git
toplevel, the cwd basename, an optional `MEMINI_AGENT` suffix, and
`MEMINI_NAMESPACE` if set — and POSTs those facts to `/v1/handshake`. The server
resolves the effective namespace and returns it (plus the caller's identity and
the fully-merged behavioral settings). Resolution order, server-side:

1. A **pin** (`/memini:namespace <ns>`), keyed by the project's git remote /
   toplevel. It wins outright, including over `MEMINI_NAMESPACE`.
2. `MEMINI_NAMESPACE`, sent as a fact so a pin can still beat it.
3. A declared namespace (gateway/integration callers only).
4. Derivation from the git remote, then the toplevel, then the cwd basename.
5. The API key's bound default namespace, then the server default.

The result is written to a **per-session handshake cache**
(`$XDG_CACHE_HOME/memini/sessions/pid-<ppid>.handshake.json`, 10-minute TTL). Every
other hook reads that cache: `Stop` / `PreCompact` / `SessionEnd` refresh it on a
miss, while `PreToolUse` / `PostToolUse` are **network-free** — they use the cache
only, so a live handshake can never race or add latency on the hot path. When the
server is unreachable the plugin degrades to local derivation (the same order,
minus the pin/key-default legs) and writes no cache; the absence of a cache entry
is itself the signal the other hooks read.

### How the MCP tools find the same namespace

The hooks are handed `data.cwd` on stdin, so they always know their project. The
MCP `headersHelper` — the only thing that sets `X-Memini-Namespace` for
`memory_remember` / `memory_recall` — is not. Measured against a live session:

```
cwd                : <plugin install root>
PWD                : <plugin install root>   <- rewritten; NOT the project
CLAUDE_PROJECT_DIR : unset
CLAUDE_PLUGIN_ROOT : <plugin install root>
process.ppid       : the session's `claude` process, cwd = the project dir
```

So `process.cwd()` and `PWD` are both traps: resolving from either would derive
the namespace from the plugin's own version-named directory and scatter memories
into namespaces like `0.6.7`.

The helper walks the process tree to recover the **project directory**:

1. `CLAUDE_PROJECT_DIR`, if Claude Code ever provides it.
2. The **parent process's cwd** (Linux `/proc`, macOS `lsof`) — always fresh, and
   works on the first connect before any hook has run.
3. `$XDG_CACHE_HOME/memini/sessions/pid-<ppid>.cwd`, written by the hooks under
   the same ppid both sides observe. Portable (Windows), but only exists once a
   hook has fired.

From that directory it runs the **same handshake flow** the hooks use — reusing
the per-session cache `SessionStart` populated, else one bounded live handshake,
else env/local derivation. There is deliberately **no global-namespace file**: the
old shared file was last-writer-wins across concurrent sessions (two repos, one
namespace — the "writes land where recall doesn't look" split), which the
per-session cache exists to end. With no project signal at all the helper emits
auth-only headers and lets the server apply the key's default namespace.

A pin follows you across machines and every client resolves the same one, so the
hooks, the MCP tools, and `memini doctor` can never disagree about which namespace
is in force.

## Environment

| Env var                     | Default                  | Used by               | Description                                                                                              |
| --------------------------- | ------------------------ | --------------------- | -------------------------------------------------------------------------------------------------------- |
| `MEMINI_BASE_URL`           | `http://localhost:8080`  | hooks (REST)          | memini base URL for the lifecycle hooks (alias: `MEMINI_URL`)                                            |
| `MEMINI_MCP_URL`            | `${MEMINI_BASE_URL}/mcp` | MCP tools             | memini `/mcp` URL for the model-invoked memory tools; derived from `MEMINI_BASE_URL` unless set          |
| `MEMINI_API_KEY`            | —                        | hooks + MCP           | bearer token; required when the server sets `MEMINI_API_KEY` (alias: `MEMINI_TOKEN`)                     |
| `MEMINI_NAMESPACE`          | auto (cwd/git basename)  | hooks + MCP           | explicit namespace override; otherwise auto-resolved                                                     |
| `MEMINI_NAMESPACE_SCOPE`    | `repo`                   | hooks                 | `owner-repo` derives `owner-repo` slugs from the git remote                                              |
| `MEMINI_AUTO_SAVE`          | on                       | `Stop` hook           | set to `0` to disable the periodic auto-save nudge                                                       |
| `MEMINI_AUTO_SAVE_INTERVAL` | `10`                     | `Stop` hook           | user messages between auto-save nudges                                                                   |
| `MEMINI_CAPTURE_TURNS`      | on                       | `Stop` hook           | auto-capture each user→assistant turn as episodic memory; set to `0` to disable                          |
| `MEMINI_SESSION_DIGEST`     | on                       | capture hooks         | record session digests (files edited, commands run); set to `0` to keep memory to durable facts only     |
| `MEMINI_INLINE_EXTRACT`     | on                       | SessionStart + `Stop` | inject the memory-save directive (`memory_remember`) and scrape legacy `<memory>` blocks; `0` to disable |
| `MEMINI_DEBUG`              | —                        | hooks                 | set to `1` for verbose hook logging                                                                      |

### Tuning injection budgets

The SessionStart and PreToolUse hooks inject context into the agent's
prompt. The volume is configurable per-knob — shrink it for small / fast
models, grow it where more recall helps. All knobs are env-only; defaults
match the prior hardcoded behavior, so existing installs see no change.

**SessionStart** (one briefing call → pinned / facts / procedures / recent):

| Env var                             | Default  | Description                                                                                          |
| ----------------------------------- | -------- | ---------------------------------------------------------------------------------------------------- |
| `MEMINI_INJECT_BRIEFING_PINNED`     | `5`      | Max pinned memories. `0` disables the section.                                                       |
| `MEMINI_INJECT_BRIEFING_FACTS`      | `5`      | Max durable semantic facts. `0` disables.                                                            |
| `MEMINI_INJECT_BRIEFING_PROCEDURES` | `5`      | Max procedural how-tos. `0` disables.                                                                |
| `MEMINI_INJECT_BRIEFING_RECENT`     | `3`      | Max recent episodic entries. `0` disables.                                                           |
| `MEMINI_INJECT_BRIEFING_MAX_TOK`    | uncapped | Hard ceiling on rendered tokens; drops tail blocks/bullets first. Pinned keeps priority over recent. |

**PreToolUse** (one search per file in `Edit|Write|Read|Glob|Grep`):

| Env var                           | Default                         | Description                                                   |
| --------------------------------- | ------------------------------- | ------------------------------------------------------------- |
| `MEMINI_INJECT_PRETOOL_ITEMS`     | `3`                             | Max hits surfaced per file.                                   |
| `MEMINI_INJECT_PRETOOL_MAX_TOK`   | uncapped                        | Hard ceiling on rendered tokens (per file).                   |
| `MEMINI_INJECT_PRETOOL_MIN_SCORE` | `0`                             | Floor on the fused score; hits below are dropped server-side. |
| `MEMINI_INJECT_PRETOOL_TOOLS`     | `Read\|Write\|Edit\|Glob\|Grep` | Pipe- or comma-separated tool allowlist override.             |

**Output labels** (both hooks):

| Env var                | Default | Description                                                     |
| ---------------------- | ------- | --------------------------------------------------------------- |
| `MEMINI_INJECT_LABELS` | —       | Comma-separated toggles: `tier`, `confidence`, `age`, `reason`. |

Labels annotate each injected bullet with metadata, e.g.
`[semantic · conf=0.85 · 14d · durable fact] use tabs in this project`.
Off by default — the unannotated format matches prior installs.

**Tight preset for a small / fast model:**

```sh
export MEMINI_INJECT_BRIEFING_PINNED=2
export MEMINI_INJECT_BRIEFING_FACTS=2
export MEMINI_INJECT_BRIEFING_PROCEDURES=1
export MEMINI_INJECT_BRIEFING_RECENT=0
export MEMINI_INJECT_BRIEFING_MAX_TOK=300
export MEMINI_INJECT_PRETOOL_ITEMS=1
export MEMINI_INJECT_PRETOOL_MIN_SCORE=0.65
export MEMINI_INJECT_PRETOOL_TOOLS="Read|Edit|Write"
export MEMINI_INJECT_LABELS=tier,reason
```

Top-of-mind: tag a memory `pinned` (via `memory_remember` `tags: ["pinned"]`
or `memini remember ... --tag pinned`) to make it auto-inject as part of the
curated briefing, exempt from demotion, and never excluded by the budget
(within `MEMINI_INJECT_BRIEFING_PINNED`). The default cap is small enough
that only durable identity / preferences should earn the pin.

## Remote memini

The plugin works against a remote memini with **no code changes** — point it at
the server and give it a token:

```sh
export MEMINI_BASE_URL=https://memini.example.com   # MCP /mcp endpoint is derived
export MEMINI_API_KEY=<the server's MEMINI_API_KEY>
```

Both the hooks (REST) and the MCP tools then send `Authorization: Bearer
$MEMINI_API_KEY`. The **namespace stays per-project** even against one shared
remote: the hooks resolve it from `data.cwd`, and the MCP `headersHelper`
(`scripts/mcp-headers.mjs`) resolves the _same_ value per connection by walking
the process tree (see [above](#how-the-mcp-tools-find-the-same-namespace)) — so
capture and recall always target the same namespace, even with several sessions
open in different repos. Run the server with `MEMINI_API_KEY` set so `/mcp` (and
`/v1`) require the token.

`/memini:status` verifies all of this against the live server and is the fastest
way to confirm a remote setup is actually wired the way you think it is.
