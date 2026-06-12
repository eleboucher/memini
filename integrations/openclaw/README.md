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

One-liner (downloads just the extension, no clone):

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

### On Kubernetes

For a containerized gateway, install the extension at rollout with an
initContainer that clones memini into the data volume:

```sh
git clone --depth 1 --branch main https://github.com/eleboucher/memini /tmp/memini-src
mv /tmp/memini-src/integrations/openclaw/plugin ~/.openclaw/extensions/memini
```

Put `base_url`/`namespace` in `openclaw.json` (templated from your ConfigMap)
and inject `MEMINI_API_KEY` as a container env var from a Secret.

## Alternatives

1. **Through the managed agent.** OpenClaw runs Claude Code / Codex — configure
   memini there using the [`plugin/`](../../plugin/) or [`codex/`](../codex/)
   recipes, and the managed sessions inherit memory.

2. **As a skill.** [`skills/memory/SKILL.md`](skills/memory/SKILL.md) teaches
   OpenClaw to remember/recall via memini's REST API on demand. Lighter than the
   slot extension (no automatic recall/capture); useful if you don't want memini
   to own the memory slot.
