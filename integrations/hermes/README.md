# memini + Hermes (NousResearch)

Hermes Agent has a native, single-select **memory provider** interface: you drop
a `MemoryProvider` implementation into `plugins/<name>/` and select it via
`memory.provider` in `~/.hermes/config.yaml`. memini ships one in
[`plugin/memini/`](plugin/memini/): Hermes drives it directly, so recall and
capture happen automatically with no MCP server.

## Recommended: native memory provider plugin

What it wires:

- **`prefetch`** — recalls relevant memories from memini before each turn and
  injects them into context. It excludes this session's own captured turns
  (already in the live transcript), so they aren't echoed back as memory a turn
  behind; past sessions still recall.
- **`sync_turn`** — captures each user/assistant exchange into memini (episodic,
  tagged with the session id).
- **`on_pre_compress`** — re-injects recalled context before history compaction
  (also excluding this session's own captures).
- **`on_memory_write`** — mirrors Hermes `MEMORY.md` / `USER.md` edits into
  memini as durable (semantic) facts.
- **tools** — `memory_recall` (with optional `tags` / `metadata` filters),
  `memory_list` (query-less browse by tier / tags / metadata category),
  `memory_remember` (with optional `tags` and a `category`), `memory_forget`
  (delete a wrong/outdated memory by `id` from recall/list), and `memory_status`
  (read-only: which namespace is in force and why) for when the agent wants to
  read, browse, write, prune, or explain memory explicitly. See
  `docs/categories.md` for the category convention.

### Install

Via Hermes (recommended) — from the standalone repo
[`eleboucher/memini-hermes`](https://github.com/eleboucher/memini-hermes), a
mirror of [`plugin/memini/`](plugin/memini/) synced on each release:

```bash
hermes plugins install eleboucher/memini-hermes
```

Or from a checkout:

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

| Variable                         | Default                        | Purpose                                                                          |
| -------------------------------- | ------------------------------ | -------------------------------------------------------------------------------- |
| `MEMINI_BASE_URL`                | `http://localhost:8080`        | memini service endpoint (alias: `MEMINI_URL`)                                    |
| `MEMINI_NAMESPACE`               | basename of cwd, else `hermes` | project the memory is scoped to (see the config-file note below)                 |
| `MEMINI_AGENT`                   | (none)                         | `{agent}` segment for the config-file namespace template (see below)             |
| `MEMINI_HOME`                    | (none)                         | caller's personal namespace, sent as `X-Memini-Home`; unset = no home leg        |
| `MEMINI_API_KEY`                 | (none)                         | bearer token, if memini requires auth (alias: `MEMINI_TOKEN`)                    |
| `MEMINI_REQUIRE_HTTPS`           | (off)                          | set `1` to refuse sending a token over plaintext HTTP                            |
| `MEMINI_RECALL_LIMIT`            | `3`                            | max memories recalled per turn                                                   |
| `MEMINI_INJECT_RECALL_MIN_SCORE` | `0`                            | fused-score floor (>=) for auto-recall, sent as `min_score`                      |
| `MEMINI_INJECT_RECALL_MAX_TOK`   | `0`                            | hard token ceiling on the recall block (`0` = unbounded; tail dropped w/ footer) |
| `MEMINI_INJECT_LABELS`           | (none)                         | per-bullet tag prefix toggles: `tier`, `confidence`, `age`                       |

### Namespace resolution

In order: a **per-project override** in `$XDG_CONFIG_HOME/memini/overrides.json`
(default `~/.config/memini/overrides.json`) > `MEMINI_NAMESPACE` > the config
template below > the cwd basename.

The override wins over `MEMINI_NAMESPACE` deliberately. A globally exported
`MEMINI_NAMESPACE` — a shell rc, or a fish universal variable — pins every repo
on the machine to one namespace, and if the environment won, setting an override
would silently do nothing on exactly the machines that need one. The file is
keyed by git toplevel (so an override set at the top of a repo applies from any
subdirectory), it is the same file the Claude Code plugin writes and `memini
doctor` reads, and a malformed one degrades to automatic resolution rather than
failing a turn. `memory_status` reports which of these is in force, what the
namespace would be without each layer, and any misconfiguration worth flagging —
with secrets redacted.

When `MEMINI_NAMESPACE` is unset and `$XDG_CONFIG_HOME/memini/config.json`
(default `~/.config/memini/config.json`) exists, the namespace is rendered from
its `template` (default `{tenant}/{project}/{agent}`): `{tenant}` comes from
the first `tenantRoots` entry whose `path` contains the cwd, `{project}` is
git-derived (remote repo name > toplevel basename > cwd basename), and
`{agent}` from `MEMINI_AGENT`; unresolved segments are dropped. This mirrors
the shared resolver the JS integrations use, so the same repo lands in the
same namespace everywhere. Without the file, the namespace is just the cwd
basename.

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
