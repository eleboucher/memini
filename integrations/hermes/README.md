# memini + Hermes (NousResearch)

Hermes Agent has a native, single-select **memory provider** interface: you drop
a `MemoryProvider` implementation into `plugins/<name>/` and select it via
`memory.provider` in `~/.hermes/config.yaml`. memini ships one in
[`plugin/memini/`](plugin/memini/): Hermes drives it directly, so recall and
capture happen automatically with no MCP server.

## Recommended: native memory provider plugin

What it wires:

- **`prefetch`** — recalls relevant memories from memini before each turn and
  injects them into context.
- **`sync_turn`** — captures each user/assistant exchange into memini (episodic).
- **`on_pre_compress`** — re-injects recalled context before history compaction.
- **`on_memory_write`** — mirrors Hermes `MEMORY.md` / `USER.md` edits into
  memini as durable (semantic) facts.
- **tools** — `memory_recall` / `memory_remember` for when the agent wants to
  read or write memory explicitly.

### Install

Via Hermes (recommended) — from the standalone repo
[`eleboucher/memini-hermes`](https://github.com/eleboucher/memini-hermes), a
mirror of [`plugin/memini/`](plugin/memini/) synced on each release. Plain
`owner/repo`, so it works on any Hermes version and `hermes plugins update
memini` tracks it:

```bash
hermes plugins install eleboucher/memini-hermes
```

This only installs the files; activate it via `memory.provider` (below), not
the `Enable? [y/N]` prompt.

Or fetch just the plugin off `main` (no clone):

```bash
curl -fsSL https://raw.githubusercontent.com/eleboucher/memini/main/integrations/hermes/install.sh | sh
```

Or from a checkout of this repo:

```bash
cp -r integrations/hermes/plugin/memini ~/.hermes/plugins/memini
```

Activate it in `~/.hermes/config.yaml` (memory providers are single-select, set
via `memory.provider`, not `plugins.enabled`), or run `hermes memory setup`:

```yaml
memory:
  provider: memini
```

> Plugins live in `$HERMES_HOME/plugins/<name>/`. Deploying on Kubernetes
> (bjw-s `app-template`)? Use [`kubernetes.md`](kubernetes.md) — an
> initContainer installs the plugin into the data volume at rollout.

Point it at your memini (environment, or the Hermes onboarding prompts):

| Variable               | Default                        | Purpose                                               |
| ---------------------- | ------------------------------ | ----------------------------------------------------- |
| `MEMINI_URL`           | `http://localhost:8080`        | memini service endpoint                               |
| `MEMINI_NAMESPACE`     | basename of cwd, else `hermes` | tenant the memory is scoped to                        |
| `MEMINI_API_KEY`       | (none)                         | bearer token, if memini requires auth                 |
| `MEMINI_REQUIRE_HTTPS` | (off)                          | set `1` to refuse sending a token over plaintext HTTP |

Restart Hermes. On the next turn, recalled memories appear in context and new
exchanges are written back. Use the **same `MEMINI_NAMESPACE`** as your other
agents to share one memory across all of them.

The plugin is dependency-free (Python stdlib only) and fails silently on
network errors.

## Fallback: MCP server

If you'd rather not install the plugin, wire memini as a plain MCP server — see
[`mcp-config.yaml`](mcp-config.yaml) for the exact `mcp_servers` block (HTTP or
stdio). You lose automatic prefetch/capture; the agent must call the memory
tools itself. Note Hermes filters tools with `tools.include` / `tools.exclude`
(not an `allowedTools` array).
