# memini + Claude Code

memini ships a Claude Code plugin under [`../../plugin/`](../../plugin/)
that wires the MCP server, 5 lifecycle hooks, and 3 skills in one install.

## Install

```
Install the memini plugin for persistent memory: run `/plugin marketplace add eleboucher/memini`
then `/plugin install memini`. Verify with `curl http://localhost:8080/healthz`.
```

The plugin registers:

- The `memini` MCP server (HTTP transport, no manual config needed).
- 5 hooks: `SessionStart` (with context injection), `PreToolUse` (read/edit
  tools), `PostToolUse`, `Stop`, `SessionEnd`.
- 3 skills: `remember`, `recall`, `recap`.

After the plugin loads, restart Claude Code (or run `/mcp`) and the
`memory_*` tools are available.

## Namespace

The plugin's hook scripts resolve the namespace from the agent's cwd via
`git rev-parse --show-toplevel`. No per-project config is needed.

To force a specific namespace (e.g. for a shared deployment), export
`MEMINI_NAMESPACE=acme-web` in your shell before starting Claude Code.

## Manual fallback (no plugin)

If you'd rather wire the MCP server by hand, drop [`.mcp.json`](.mcp.json)
in your project root, or:

```sh
claude mcp add --transport http memini http://localhost:8080/mcp
```

For stdio (memini runs as a subprocess), use [`mcp.local.json`](mcp.local.json).
You lose auto-capture and context injection — the agent has the tools but
no lifecycle hooks, and has to be reminded to use them via
[`CLAUDE.snippet.md`](CLAUDE.snippet.md) appended to your project `CLAUDE.md`.

## Tell Claude to use it

Either rely on the `remember`/`recall`/`recap` skills (preferred — they
are auto-loaded and teach the agent _when_ to call the tools), or paste
[`CLAUDE.snippet.md`](CLAUDE.snippet.md) into your project `CLAUDE.md`.
