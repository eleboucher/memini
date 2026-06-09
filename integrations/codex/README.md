# memini + Codex CLI

Codex reads MCP servers from `~/.codex/config.toml` (or a project-scoped
`.codex/config.toml`) under `[mcp_servers.<name>]`.

**Local (stdio)** — widely supported; see [`config.stdio.toml`](config.stdio.toml):

```sh
codex mcp add memini -- memini mcp
```

**Remote (Streamable HTTP)** — recent Codex; see [`config.remote.toml`](config.remote.toml).

Verify:

```sh
codex mcp list
```

Use the same namespace as your other agents (the `MEMINI_DEFAULT_NAMESPACE` env
for stdio, or the `X-Memini-Namespace` header for remote) to share memory.
The `my-project` placeholder can be replaced with your real project name,
or removed entirely — memini auto-resolves the namespace from the git repo
basename of its own working directory when unset.
