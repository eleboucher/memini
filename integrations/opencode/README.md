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

### Configure

Pass options inline via the `[name, options]` form:

```json
{
  "plugin": [["@eleboucher/opencode-memini", { "namespace": "my-project", "recall_limit": 8 }]]
}
```

| Option              | Env var                          | Default                 | Purpose                                                                                                                                |
| ------------------- | -------------------------------- | ----------------------- | -------------------------------------------------------------------------------------------------------------------------------------- |
| `base_url`          | `MEMINI_BASE_URL`                | `http://localhost:8080` | memini REST base URL (alias: `MEMINI_URL`)                                                                                             |
| `namespace`         | `MEMINI_NAMESPACE`               | git repo basename       | project the memory is scoped to (`X-Memini-Namespace`)                                                                                 |
| `home`              | `MEMINI_HOME`                    | unset                   | caller's personal namespace, sent as `X-Memini-Home`; unset = no home leg                                                              |
| `recall`            | `MEMINI_RECALL`                  | on                      | `false` disables recall-before-turn                                                                                                    |
| `capture`           | `MEMINI_CAPTURE`                 | on                      | `false` disables capture-after-turn                                                                                                    |
| `recall_limit`      | `MEMINI_RECALL_LIMIT`            | `3`                     | max memories injected per turn                                                                                                         |
| `recall_max_tokens` | `MEMINI_INJECT_RECALL_MAX_TOK`   | `0`                     | hard ceiling on the recall-block tokens (`0` = unbounded); the tail is dropped with a `[… N item(s) truncated by token budget]` footer |
| `recall_min_score`  | `MEMINI_INJECT_RECALL_MIN_SCORE` | `0`                     | fused-score floor (>=) sent as `min_score` to `/v1/search`                                                                             |
| `timeout_ms`        | `MEMINI_TIMEOUT_MS`              | `30000`                 | per-request timeout                                                                                                                    |
| `fallback_on_error` | `MEMINI_FALLBACK`                | on                      | `false` surfaces errors instead of degrading silently                                                                                  |
| —                   | `MEMINI_INJECT_LABELS`           | —                       | comma-separated label toggles for each bullet: `tier`, `confidence`, `age`, `reason`                                                   |
| —                   | `MEMINI_API_KEY`                 | —                       | bearer token, if memini needs auth (env only — secret; alias: `MEMINI_TOKEN`)                                                          |
| —                   | `MEMINI_REQUIRE_HTTPS`           | —                       | `1` refuses to send the token over plaintext HTTP                                                                                      |

Inline options win over the env vars. Secrets stay in the environment: set
`MEMINI_API_KEY` (sent as `Authorization: Bearer …`), and optionally
`MEMINI_REQUIRE_HTTPS=1` to refuse plaintext HTTP, in the shell that launches
opencode — not in `opencode.json`.

Every option is optional, `namespace` included: `["@eleboucher/opencode-memini"]`
runs with no config. Unset, the plugin derives the namespace from the git
worktree basename and sends it as the `X-Memini-Namespace` header, so scoping
stays correct even against a remote memini (the HTTP MCP wire below can't — a
remote server has no access to your cwd). Set it to share one memory pool with
your other agents.

If `$XDG_CONFIG_HOME/memini/config.json` (default `~/.config/memini/config.json`)
exists, the unset namespace is instead rendered from its `template` (default
`{tenant}/{project}/{agent}`): `{tenant}` from the `tenantRoots` entry whose
`path` contains the cwd, `{project}` from the git repo, `{agent}` from
`MEMINI_AGENT`; unresolved segments are dropped. The Hermes and Pi integrations
share this resolver, so one config file scopes them all identically.

### Namespace resolution

In full, in order: a **per-project override** in
`$XDG_CONFIG_HOME/memini/overrides.json` > the `namespace` option /
`MEMINI_NAMESPACE` > the config template above > the git worktree basename.

The override wins over both deliberately. A globally exported `MEMINI_NAMESPACE`
— a shell rc, or a fish universal variable — pins every repo on the machine to
one namespace (as does a `namespace` option in a global
`~/.config/opencode/opencode.json`), and if either won, setting an override would
silently do nothing on exactly the machines that need one. The file is keyed by
git toplevel, so an override set at the top of a repo applies from any
subdirectory; it is the same file the Claude Code plugin writes and `memini
doctor` reads; and a malformed one degrades to automatic resolution rather than
breaking a turn.

### The `memini_status` tool

The plugin registers one tool, `memini_status`: read-only, no arguments. It
reports the namespace in force and where it came from, what it would be _without_
the override and without the env pin, the connection settings (the API key
fingerprinted, never printed), and warnings — a global `MEMINI_NAMESPACE` pin, a
bearer token crossing plaintext HTTP, an override you forgot you set.

There is no `/memini:status` slash command: opencode's plugin contract registers
tools, not commands, and this plugin does not invent an API it does not have.
Setting or clearing an override is likewise not exposed here — declaring a tool
argument requires a zod schema, and this plugin ships dependency-free — so use
`/memini:namespace` from the Claude Code plugin, or edit `overrides.json`
directly; all harnesses read the same file.

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
