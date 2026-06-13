# memini + OpenClaw

OpenClaw is a self-hosted gateway that drives coding agents and exposes a
pluggable **memory slot** (`plugins.slots.memory`). memini ships a native
extension in [`plugin/`](plugin/) that claims that slot: it recalls relevant
memories before the agent starts and captures completed turns afterwards, with
no tool calls required from the agent.

## Recommended: native memory-slot extension

What it wires (via `api.registerMemoryCapability` + two hooks):

- **`before_agent_start`** — searches memini for the incoming prompt and
  prepends the matches as context.
- **`agent_end`** — stores the completed user/assistant turn back into memini
  (episodic) so it can be recalled later.

### Install

From ClawHub (recommended) — a tracked install OpenClaw's plugin updater keeps
current (`openclaw plugins update @eleboucher/memini`):

```bash
openclaw plugins install clawhub:@eleboucher/memini
```

Or fetch the extension files directly off `main` (no clone, not tracked for
updates):

```bash
curl -fsSL https://raw.githubusercontent.com/eleboucher/memini/main/integrations/openclaw/install.sh | sh
```

Or from a checkout of this repo:

```bash
mkdir -p ~/.openclaw/extensions
cp -r integrations/openclaw/plugin ~/.openclaw/extensions/memini
```

Claim the memory slot in `~/.openclaw/openclaw.json`:

```json
{
  "plugins": {
    "slots": { "memory": "memini" },
    "entries": {
      "memini": {
        "enabled": true,
        "config": {
          "base_url": "http://localhost:8080",
          "namespace": "openclaw",
          "namespace_per_agent": true,
          "namespace_template": "{namespace}-{agent}",
          "fallback_on_error": true,
          "timeout_ms": 5000
        }
      }
    }
  }
}
```

If memini requires auth, set `MEMINI_API_KEY` in the gateway environment (the
plugin sends it as `Authorization: Bearer …`; set `MEMINI_REQUIRE_HTTPS=1` to
refuse sending it over plaintext HTTP). Restart OpenClaw.

Memory is isolated **per agent by default** (`namespace_per_agent: true`): each
agent reads and writes its own scope, resolved from the agent id on each hook
event and formatted by `namespace_template`. This prevents subagents sharing one
gateway from poisoning each other's memory:

- `"{namespace}-{agent}"` (default) → `openclaw-miso`, `openclaw-saffron`, …
- `"{agent}"` → `miso`, `saffron`, `matcha`

When an event carries no agent identity, it falls back to the base `namespace`
(`openclaw`), so unattributable sessions (cron, heartbeat) still get shared
memory rather than being dropped. Set `"skip_without_agent": true` to skip those
sessions entirely instead.

To share **one** memory across all agents (the previous default), set
`"namespace_per_agent": false`. If you previously ran with shared memory and want
to separate already-pooled agents, see `memini namespace split` below.

### Explicit tools (`expose_tools`)

By default the plugin only fills the memory slot — recall and capture are
automatic, with no tool calls. Set `"expose_tools": true` in the plugin config to
_also_ register explicit tools the agent can call on demand, alongside the slot:

- **`memory_recall`** — search, with optional `tags` / `metadata` filters.
- **`memory_list`** — query-less browse by tier / tags / metadata category
  (e.g. "all procedural memories" or "everything categorized `bug_fixes`"; see
  `docs/categories.md`).
- **`memory_remember`** — store a fact, with optional `tags` and a `category`.

Each tool resolves the same per-agent namespace as the hooks, and is registered
`optional`. Parameter schemas use [typebox](https://github.com/sinclairzx81/typebox),
a plugin dependency loaded lazily — if it can't be loaded the tools are skipped
and the memory slot keeps working.

### On Kubernetes

For a containerized gateway, install the extension from ClawHub into the data
volume — either once by hand (`kubectl exec … -- openclaw plugins install
clawhub:@eleboucher/memini`) or at rollout with an initContainer guarded so it
only runs on a fresh volume:

```sh
openclaw plugins install clawhub:@eleboucher/memini
```

This is a tracked install, so the gateway updater (and `openclaw plugins update`)
keeps it current — unlike a raw `git clone`, which is never tracked and never
updates. Enablement is handled by `openclaw.json`, not the install command:
claim `slots.memory: memini` and set `entries.memini.enabled`. Put
`base_url`/`namespace` there (templated from your ConfigMap) and inject
`MEMINI_API_KEY` as a container env var from a Secret.

## Alternatives

1. **Through the managed agent.** OpenClaw runs Claude Code / Codex — configure
   memini there using the [`plugin/`](../../plugin/) or [`codex/`](../codex/)
   recipes, and the managed sessions inherit memory.

2. **As a skill.** [`skills/memory/SKILL.md`](skills/memory/SKILL.md) teaches
   OpenClaw to remember/recall via memini's REST API on demand. Lighter than the
   slot extension (no automatic recall/capture); useful if you don't want memini
   to own the memory slot.
