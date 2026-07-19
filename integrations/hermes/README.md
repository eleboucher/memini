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

| Variable                         | Default                 | Purpose                                                                                                                                                                |
| -------------------------------- | ----------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `MEMINI_BASE_URL`                | `http://localhost:8080` | memini service endpoint                                                                                                                                                |
| `MEMINI_NAMESPACE`               | server handshake        | project the memory is scoped to (see Namespace resolution below)                                                                                                       |
| `MEMINI_HOME`                    | (none)                  | caller's personal namespace, sent as `X-Memini-Home`; unset = no home leg                                                                                              |
| `MEMINI_API_KEY`                 | (none)                  | bearer token, if memini requires auth                                                                                                                                  |
| `MEMINI_REQUIRE_HTTPS`           | (off)                   | set `1` to refuse sending a token over plaintext HTTP                                                                                                                  |
| `MEMINI_RECALL_LIMIT`            | `3`                     | max memories recalled per turn (beneath the server's `recall_limit` setting)                                                                                           |
| `MEMINI_INJECT_RECALL_MIN_SCORE` | `0`                     | fused-score floor (>=) for auto-recall, sent as `min_score` (beneath the server's setting)                                                                             |
| `MEMINI_INJECT_RECALL_MAX_TOK`   | `0`                     | hard token ceiling on the recall block (`0` = unbounded; tail dropped w/ footer; beneath the server's setting)                                                         |
| `MEMINI_INJECT_COOLDOWN_MS`      | `1800000`               | repeat-injection cooldown, **time** window (ms): an already-injected memory is held back this long before it may re-serve; `0` disables the time dimension             |
| `MEMINI_INJECT_COOLDOWN_PROMPTS` | `3`                     | repeat-injection cooldown, **prompt** window (counted per prefetch / turn); `0` disables the prompt dimension; both cooldown vars `0` = suppress for the whole session |
| `MEMINI_INJECT_LABELS`           | (none)                  | per-bullet tag prefix toggles: `tier`, `confidence`, `age`                                                                                                             |

The two `MEMINI_INJECT_COOLDOWN_*` vars are the windowed **repeat-injection
cooldown**: an already-injected memory is excluded from prefetch (server-side via
`exclude_ids`, with a client-side backstop) while it is inside _either_ window,
and re-served only once _both_ have lapsed. Entries carry a **content-identity
hash** (the server-minted `content_hash` when present, else
`sha256(content‖summary)[:16]` — the same recipe as the Claude Code plugin), so
a memory that was **updated in place re-injects immediately** instead of staying
withheld for the window; unlike that plugin there is still no forever-suppressed
tool-read entry — hermes tool results carry full content, so every hermes entry
lapses. Both vars `0` restores the prior suppress-for-the-session behavior. The
predicate and hash are verified against the shared golden vectors
(`packages/memini-client/vectors/enforcement.json`) by
`plugin/test_enforcement_vectors.py`.

### Namespace resolution

In order: `MEMINI_NAMESPACE` (raw-trimmed) > the namespace a `POST /v1/handshake`
resolves server-side (api/openapi.yaml) > the local git remote/toplevel/cwd
derivation chain.

On `initialize`, the plugin calls the memini server's handshake endpoint with
what it cheaply knows about the project (the git remote/toplevel, when the
working directory is a repo, plus the cwd basename) and lets the server
resolve the namespace and behavioral settings (`recall`, `capture`,
`recall_limit`, the recall-injection budget) the same way every other memini
client does. The call is fail-soft: any error or a ~2.5s timeout falls back to
the local git remote/toplevel/cwd derivation chain, so an unreachable or older
memini never breaks a turn. It is memoized for 10 minutes, so a long-lived
Hermes process re-handshakes at most once per ~10 minutes rather than on every
call.

`MEMINI_NAMESPACE` wins over the handshake outright and deliberately: it is
this integration's own explicit pin, honored as such rather than
second-guessed by the server (the server still sees it, sent as
`project.env_namespace`, so a pin can still beat it server-side for other
clients that check in without setting the env var locally). Each recall
setting above follows the same shape: the env var beats the server's
resolved setting beats the built-in default. `recall`/`capture` (on by
default) have no local env toggle here — only the server's settings can turn
them off. `memory_status` reports which layer is in force, what the
namespace would be without the env pin, and any misconfiguration worth
flagging — with secrets redacted.

Restart Hermes to pick up a `MEMINI_NAMESPACE` change (a server-side change —
a new pin, edited settings — is picked up on the next handshake, at most 10
minutes later). On the next turn, recalled memories appear in context and new
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
