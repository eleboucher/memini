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

| Option              | Env var                | Default                 | Purpose                                                |
| ------------------- | ---------------------- | ----------------------- | ------------------------------------------------------ |
| `base_url`          | `MEMINI_BASE_URL`      | `http://localhost:8080` | memini REST base URL                                   |
| `namespace`         | `MEMINI_NAMESPACE`     | git repo basename       | tenant the memory is scoped to (`X-Memini-Namespace`)  |
| `recall`            | `MEMINI_RECALL`        | on                      | `false` disables recall-before-turn                    |
| `capture`           | `MEMINI_CAPTURE`       | on                      | `false` disables capture-after-turn                    |
| `recall_limit`      | `MEMINI_RECALL_LIMIT`  | `5`                     | max memories injected per turn                         |
| `timeout_ms`        | `MEMINI_TIMEOUT_MS`    | `30000`                 | per-request timeout                                    |
| `fallback_on_error` | `MEMINI_FALLBACK`      | on                      | `false` surfaces errors instead of degrading silently  |
| —                   | `MEMINI_API_KEY`       | —                       | bearer token, if memini needs auth (env only — secret) |
| —                   | `MEMINI_REQUIRE_HTTPS` | —                       | `1` refuses to send the token over plaintext HTTP      |

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
