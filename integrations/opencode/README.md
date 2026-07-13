# memini + opencode

[opencode](https://opencode.ai) supports [plugins](https://opencode.ai/docs/plugins/)
— JS/TS modules that hook into its lifecycle. memini ships a native plugin in
[`plugin/`](plugin/) that makes memory automatic: it recalls relevant memories
before each turn and captures completed turns afterward, with no tool calls
required from the model.

## Recommended: the memory plugin

What it wires (two hooks):

- **`chat.message`** — searches memini for the incoming user message and
  prepends the matches as a synthetic context part before the turn runs. It
  excludes this session's own captured turns (already in the live context), so
  they aren't echoed back as memory a turn behind; past sessions still recall.
- **`event` (`session.idle`)** — once the session goes idle, stores the
  completed user/assistant turn back into memini (episodic, tagged with the
  session id) so it can be recalled later.

### Install

Add the package to the `plugin` array in your `opencode.json` — a project
`opencode.json` to scope it to one repo, or `~/.config/opencode/opencode.json`
for every project:

```json
{
  "$schema": "https://opencode.ai/config.json",
  "plugin": ["@eleboucher/opencode-memini"]
}
```

opencode installs it from npm with Bun at startup.

### opencode v2 (beta)

opencode's v2 preview (`opencode2`) uses a different plugin system: a `plugins`
(plural) config array and a `Plugin.define({ id, setup })` module — the v1
`{ id, server }` plugin above does not load under it. memini ships a v2 sibling
at the `/v2` subpath that wires the same recall / capture / `memini_status`
behaviour to the v2 `ctx.session.hook("request")`, `ctx.event.subscribe`, and
`ctx.tool.transform` API:

```jsonc
{
  "$schema": "https://opencode.ai/config.json",
  "plugins": [["@eleboucher/opencode-memini/v2", { "namespace": "my-project" }]],
}
```

The same options and env vars below apply. Recall injects into the request's
`system` prompt (rather than a synthetic message part).

> **Beta status.** The v2 plugin `ctx` is still gaining these hooks upstream.
> On a build where `ctx.session.hook` / `ctx.event.subscribe` / `ctx.tool.transform`
> are absent, the plugin logs which capability is unavailable and no-ops that
> part rather than crashing — it activates cleanly and lights up automatically
> once opencode exposes the hook. Track the plugin `ctx` in opencode's
> `core/src/plugin/host.ts`. Until then, stay on the v1 entry above with the
> stable `opencode` binary.

### Configure

Pass options inline via the `[name, options]` form:

```json
{
  "plugin": [["@eleboucher/opencode-memini", { "namespace": "my-project", "recall_limit": 8 }]]
}
```

| Option              | Env var                          | Default                 | Purpose                                                                                                                                |
| ------------------- | -------------------------------- | ----------------------- | -------------------------------------------------------------------------------------------------------------------------------------- |
| `base_url`          | `MEMINI_BASE_URL`                | `http://localhost:8080` | memini REST base URL                                                                                                                   |
| `namespace`         | `MEMINI_NAMESPACE`               | server handshake        | project the memory is scoped to (`X-Memini-Namespace`)                                                                                 |
| `home`              | `MEMINI_HOME`                    | unset                   | caller's personal namespace, sent as `X-Memini-Home`; unset = no home leg                                                              |
| `recall`            | `MEMINI_RECALL`                  | on                      | `false` disables recall-before-turn                                                                                                    |
| `capture`           | `MEMINI_CAPTURE`                 | on                      | `false` disables capture-after-turn                                                                                                    |
| `recall_limit`      | `MEMINI_RECALL_LIMIT`            | `3`                     | max memories injected per turn                                                                                                         |
| `recall_max_tokens` | `MEMINI_INJECT_RECALL_MAX_TOK`   | `0`                     | hard ceiling on the recall-block tokens (`0` = unbounded); the tail is dropped with a `[… N item(s) truncated by token budget]` footer |
| `recall_min_score`  | `MEMINI_INJECT_RECALL_MIN_SCORE` | `0`                     | fused-score floor (>=) sent as `min_score` to `/v1/search`                                                                             |
| `recall_budget_ms`  | `MEMINI_RECALL_BUDGET_MS`        | `2000`                  | how long a turn waits for recall before proceeding without it (`0` = wait for the full `timeout_ms`)                                   |
| `timeout_ms`        | `MEMINI_TIMEOUT_MS`              | `30000`                 | per-request timeout (recall past its budget keeps running in the background under this bound)                                          |
| `fallback_on_error` | `MEMINI_FALLBACK`                | on                      | `false` surfaces errors instead of degrading silently                                                                                  |
| —                   | `MEMINI_INJECT_LABELS`           | —                       | comma-separated label toggles for each bullet: `tier`, `confidence`, `age`, `reason`                                                   |
| —                   | `MEMINI_API_KEY`                 | —                       | bearer token, if memini needs auth (env only — secret)                                                                                 |
| —                   | `MEMINI_REQUIRE_HTTPS`           | —                       | `1` refuses to send the token over plaintext HTTP                                                                                      |

opencode awaits `chat.message` before the model sees the message, so a slow or
unreachable memini would otherwise freeze the turn for the full `timeout_ms`.
Instead, recall races `recall_budget_ms`: if the search hasn't answered in time,
the turn proceeds without memories and the search keeps running in the
background — results that arrive late are injected on the session's next
message instead of being dropped. The plugin also pings `/healthz` once at
startup to warm the connection, so the first recall doesn't pay the
DNS/TLS cold-start. Set `recall_budget_ms: 0` to restore fully blocking
same-turn injection.

Inline options win over the env vars. Secrets stay in the environment: set
`MEMINI_API_KEY` (sent as `Authorization: Bearer …`), and optionally
`MEMINI_REQUIRE_HTTPS=1` to refuse plaintext HTTP, in the shell that launches
opencode — not in `opencode.json`.

Every option is optional, `namespace` included: `["@eleboucher/opencode-memini"]`
runs with no config. On plugin load (and again every 10 minutes thereafter),
the plugin calls `POST /v1/handshake` with what it cheaply knows about the
project (the git remote/toplevel, when the worktree is a repo, plus the
worktree basename) and lets the server resolve the namespace and behavioral
settings (`recall`, `capture`, `recall_limit`, the recall-injection budget)
the same way every other memini client does. The call is fail-soft: any
error or a ~2.5s timeout falls back to purely local resolution below, so an
unreachable or older memini never breaks a turn.

### Namespace resolution

In full, in order: the `namespace` option / `MEMINI_NAMESPACE` > the
server's handshake-resolved namespace > the git worktree basename > the
built-in default (`opencode`).

The option/env tier wins over the handshake outright and deliberately: a
`namespace` option in a global `~/.config/opencode/opencode.json`, or a
globally exported `MEMINI_NAMESPACE`, is this integration's own explicit pin
and is honored as such rather than second-guessed by the server. Absent
either, the server's resolution (which can draw on a pin, the git remote, or
an operator's per-key default) wins over this plugin's own git
worktree/default fallback, which only applies when the handshake itself is
unavailable.

Each recall/capture setting follows the same shape: the plugin option beats
`MEMINI_NAMESPACE`'s sibling env vars above beats the server's resolved
`ClientSettings` beats the built-in default baked into this plugin.

### The `memini_status` tool

The plugin registers one tool, `memini_status`: read-only, no arguments. It
reports the namespace in force and where it came from (the namespace option,
`MEMINI_NAMESPACE`, the server's handshake, or the git worktree fallback),
what it would be without the env/option pin, the connection settings (the
API key fingerprinted, never printed), and warnings — a global
`MEMINI_NAMESPACE` pin, a bearer token crossing plaintext HTTP.

There is no `/memini:status` slash command: opencode's plugin contract registers
tools, not commands, and this plugin does not invent an API it does not have.

### Tests

```bash
cd integrations/opencode/plugin && node --test
```

## Alternative: manual MCP wire (tools, not automatic)

Instead of the plugin you can expose memini's `memory_*` tools to the model and
let it call them on demand. opencode reads MCP servers from `opencode.json` under
`mcp`. This wire gives the model the full tool set, including `memory_recall`
(with `tags` / `metadata` filters), `memory_list` (query-less browse by tier /
tags / metadata category), and `memory_remember`. Set `metadata.category` on
writes to browse by subject later — see `docs/categories.md`.

**Remote:** merge [`opencode.json`](opencode.json) into your config.

**Local (stdio):** see [`opencode.local.json`](opencode.local.json) — opencode
spawns `memini mcp`.

Mind the context budget: memini adds a handful of tools, which is light, but
disable other large MCP servers you aren't using. Use the same
`X-Memini-Namespace` as your other agents to share memory. `my-project` is a
placeholder — replace it with your real project name, or drop the header and let
memini auto-resolve from its own working directory.
