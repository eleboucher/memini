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

Use the same `namespace` as your other agents to share one memory across the
gateway and its managed coding sessions.

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
   memini there using the [`claude-code/`](../claude-code/) or [`codex/`](../codex/)
   recipes, and the managed sessions inherit memory.

2. **As a skill.** [`skills/memory/SKILL.md`](skills/memory/SKILL.md) teaches
   OpenClaw to remember/recall via memini's REST API on demand. Lighter than the
   slot extension (no automatic recall/capture); useful if you don't want memini
   to own the memory slot.
