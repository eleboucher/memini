# memini + Codex

## Install the native plugin

```sh
codex plugin marketplace add eleboucher/memini
codex plugin add memini@memini
```

Run the local server first; the bundled MCP server targets
`http://localhost:8080/mcp`.

```sh
memini serve
curl http://localhost:8080/healthz
```

Set authentication and optional static identity before starting Codex:

```sh
export MEMINI_API_KEY="..."
export MEMINI_NAMESPACE="my-project" # optional
export MEMINI_HOME="personal/me"      # optional
```

`MEMINI_API_KEY` is Codex's bearer variable. The legacy `MEMINI_TOKEN` alias is
Claude-only because Codex accepts one `bearer_token_env_var`.

Installing does not trust plugin hooks. Open `/hooks`, review the Memini
commands, and trust the current definition. Start a new thread after install so
Codex discovers all nine skills, the `memory_*` tools, and lifecycle hooks.
Verify with `codex plugin list`, `codex mcp list`, `/hooks`, and a
`memory_recall` call.

The plugin provides session briefing and compaction recovery, prompt/file/tool
recall, local tool-event buffering, rolling `Stop` checkpoints, PreCompact
episodic checkpoints, and skills for remember, recall, recap, forget, pin,
status, namespace, doctor, and backfill. Doctor and backfill require the local
`memini` binary.

## Remote or custom server URL

Codex does not expand Claude-style `${VAR:-default}` expressions inside plugin
MCP URLs. The bundled endpoint is therefore static. Disable the bundled server
and register a project or user MCP server in `config.toml` using
[`config.remote.toml`](config.remote.toml). This limitation is tracked in
[openai/codex#2680](https://github.com/openai/codex/issues/2680).

```toml
[plugins.memini.mcp_servers.memini]
enabled = false
```

Then configure `[mcp_servers.memini]` with the desired URL and headers.
[`config.stdio.toml`](config.stdio.toml) remains available when you prefer the
local `memini mcp` process.

## Update

Refresh the marketplace, reinstall `memini@memini`, and start a new thread.
Codex loads an installed plugin copy and thread-scoped skills/tools, so an
existing thread is not a reliable activation check.

## Stable parity limits

Codex has no reliable final-session event equivalent to Claude's `SessionEnd`,
so it keeps rolling checkpoints but cannot guarantee the same final digest.
Codex also does not expose a stable main-session transcript contract: Memini
does not parse Codex transcripts for automatic turn capture, legacy inline
extraction, or auto-save nudges. Explicit memory writes, recall, tool buffering,
Stop checkpoints, and PreCompact recovery remain active. Hooks stay fail-soft
when Memini is unreachable.
