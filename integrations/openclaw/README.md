# memini + OpenClaw

OpenClaw is a self-hosted gateway that drives coding agents and exposes a
pluggable **memory slot** (`plugins.slots.memory`). memini ships a native
extension in [`plugin/`](plugin/) that claims that slot: it recalls relevant
memories before the agent starts and captures completed turns afterwards, with
no tool calls required from the agent.

## Recommended: native memory-slot extension

What it wires (via `api.registerMemoryCapability` + two hooks):

- **`before_prompt_build`** — searches memini for the incoming prompt and
  prepends the matches as context. It excludes the current session's own
  captured turns (via `exclude_metadata`), so a turn still in the live transcript
  isn't echoed back as "long-term memory" the next turn; captures from earlier
  sessions are still recalled.
- **`agent_end`** — stores the completed user/assistant turn back into memini
  (episodic, tagged with the session id) so it can be recalled later. This is a
  raw-conversation hook, so it requires `hooks.allowConversationAccess: true`
  (see config below); without it OpenClaw withholds `event.messages` and capture
  silently no-ops.

### Install

From ClawHub (recommended) — a tracked install OpenClaw's plugin updater keeps
current (`openclaw plugins update @eleboucher/memini`):

```bash
openclaw plugins install clawhub:@eleboucher/memini
```

Or from a checkout of this repo — the plugin is a TypeScript source tree that
needs a build before it can be loaded:

```bash
cd integrations/openclaw/plugin
npm install
npm run build
mkdir -p ~/.openclaw/extensions/memini
cp -r dist openclaw.plugin.json package.json plugin.yaml pnpm-workspace.yaml ~/.openclaw/extensions/memini/
```

Claim the memory slot in `~/.openclaw/openclaw.json`:

```json
{
  "plugins": {
    "slots": { "memory": "memini" },
    "entries": {
      "memini": {
        "enabled": true,
        "hooks": { "allowConversationAccess": true },
        "config": {
          "base_url": "http://localhost:8080",
          "namespace": "openclaw",
          "namespace_per_agent": true,
          "namespace_template": "{namespace}-{agent}",
          "fallback_on_error": true,
          "timeout_ms": 5000,
          "expose_tools": true
        },
        "tools": {
          "allow": ["memory_recall", "memory_list", "memory_remember", "memory_forget"]
        }
      }
    }
  }
}
```

The `tools.allow` allowlist is required for the three explicit tools to be sent
to the model — they're declared `optional: true` in the manifest so OpenClaw
won't auto-expose them.

If memini requires auth, set `MEMINI_API_KEY` (alias: `MEMINI_TOKEN`) in the
gateway environment (the plugin sends it as `Authorization: Bearer …`; set
`MEMINI_REQUIRE_HTTPS=1` to refuse sending it over plaintext HTTP). Restart
OpenClaw.

`base_url` can also be omitted from `config` and supplied via the
`MEMINI_BASE_URL` env var (alias: `MEMINI_URL`); the `config` value wins when
both are present.

Recall shaping (both optional, matching the opencode/Claude Code plugins):
`recall_limit` (max memories per turn, default **3**) and `recall_max_tokens`
(hard token ceiling on the recall block, default **0** = uncapped, matching the
other integrations; set it `> 0` to cap a raised `recall_limit`, the tail is
dropped with a truncation footer). `recall_max_tokens` also reads
`MEMINI_INJECT_RECALL_MAX_TOK`, and `MEMINI_INJECT_LABELS` (`tier`, `confidence`,
`age`) toggles the per-bullet tag prefix.

**There is no relevance-score floor knob — by design.** Bounding per-turn volume
is `recall_limit`'s job, not a score gate, because benchmarking
(`cmd/bench -vec-gate`) showed neither score can decide "inject nothing when
nothing is relevant" with the default MiniLM embedder:

- The **fused score** is min-max-normalised within the candidate pool
  (`internal/search/fusion.go`), so the pool's best always lands near ~1.0
  regardless of absolute relevance — a nonsense query produces the same
  high-scoring shape as a perfect match.
- The **raw vector score** doesn't separate either: across 240 LongMemEval
  queries the top score for a relevant namespace (median 0.469) overlaps the
  top score for a foreign one (median 0.448). Any floor that suppresses the
  irrelevant case also guts real recall (at 0.46, recall drops 98%→61%).

A genuine relevance gate would need a model whose absolute scores separate (a
stronger embedder, or a cross-encoder reranker, which emits calibrated scores) —
not a threshold on the current pipeline. Until then, `recall_limit` is the lever.

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

`skip_without_agent` only drops turns with **no** agent identity. A heartbeat or
cron run _is_ agent-attributed (it reuses the agent's main session), so without
further gating it would pull long-term memory into the poll and get captured as
episodic noise. To prevent that, `"skip_system_turns"` is **on by default**:
recall **and** capture are skipped for system-initiated runs. Set
`"skip_system_turns": false` to recall/capture on them as before. A run is
system-initiated when OpenClaw's `ctx.trigger` (set per run to `user`,
`heartbeat`, or `cron`) matches one of `system_kinds` (default
`["heartbeat", "cron"]`, matched case-insensitively; override for custom
triggers).

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
- **`memory_forget`** — delete a memory by `id` (from recall/list) when it's
  wrong, outdated, or poisoned. Soft delete (tombstone).

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
claim `slots.memory: memini`, set `entries.memini.enabled`, and set
`entries.memini.hooks.allowConversationAccess: true` (capture needs it). Put
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
