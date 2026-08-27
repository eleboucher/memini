# memini + OpenClaw

OpenClaw is a self-hosted gateway that drives coding agents and exposes a
pluggable **memory slot** (`plugins.slots.memory`). memini ships a native
extension in [`plugin/`](plugin/) that claims that slot: it recalls relevant
memories before the agent starts and captures completed turns afterwards, with
no tool calls required from the agent.

## Recommended: native memory-slot extension

What it wires (via `api.registerMemoryCapability` + two hooks):

- **`before_prompt_build`** — searches memini for the incoming prompt and
  injects the matches as context. It excludes the current session's own
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
cp -r dist openclaw.plugin.json package.json plugin.yaml ~/.openclaw/extensions/memini/
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
          "timeout_ms": 30000,
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

The `tools.allow` allowlist is required for the four explicit tools to be sent
to the model — they're declared `optional: true` in the manifest so OpenClaw
won't auto-expose them.

If memini requires auth, set `MEMINI_API_KEY` in the gateway environment (the
plugin sends it as `Authorization: Bearer …`; set `MEMINI_REQUIRE_HTTPS=1` to
refuse sending it over plaintext HTTP). Restart OpenClaw.

`base_url` can also be omitted from `config` and supplied via the
`MEMINI_BASE_URL` env var; the `config` value wins when both are present.

Set `home` (or the `MEMINI_HOME` env var, `config` wins) to the caller's
personal namespace — every request then carries `X-Memini-Home`, which server-side
`visibility: "personal"` writes and the read-set's home leg land in. Unset
means no home leg; there is no derivation, just this explicit config/env pair.

### Namespace resolution

```
1. MEMINI_NAMESPACE       local env override, wins outright
2. server-side pin        set with memini:namespace, stored on the memini server
3. the `namespace` config sent to the server as the declared namespace
4. "openclaw"             the default
```

Then `namespace_prefix`, `namespace_template` and per-agent nesting apply on top,
exactly as before.

The plugin resolves its namespace through the config handshake
(`POST /v1/handshake`): each session it sends the server a small set of gateway
facts — the daemon's working directory path and the declared namespace (the
`namespace` config value, else `openclaw`) — and uses whatever the server
resolves. A **pin** recorded for this install's working directory beats the
declared value (that is the point of pinning); otherwise the declared value
stands. When the server is unreachable, the plugin falls back to the declared
value, so behavior degrades to exactly what the config says. The handshake is
memoized in memory for ten minutes; setting or clearing a pin drops the memo,
so a change applies on the very next turn.

Note what is _not_ in that list: OpenClaw does **no git derivation**, and that
is deliberate. It is a gateway harness, where the working directory is the
daemon's, not a project's, so the default is the literal `openclaw` rather
than a guess at a repo. The pin's identity is that same daemon working
directory (`path:<cwd>` on the server) — stable per install, and never
confused with a git checkout the daemon happens to sit inside.

`MEMINI_NAMESPACE` overrides everything **including a pin** for this plugin. A
gateway is a long-lived, per-machine install, and an operator's explicit local
env pin should never be silently shadowed by server state. Prefer a pin (or
the `namespace` config) for a durable choice; keep `MEMINI_NAMESPACE` as the
temporary big hammer.

### Commands

| Command            | What it does                                                         |
| ------------------ | -------------------------------------------------------------------- |
| `memini:status`    | Effective settings, resolved namespace **with provenance**, warnings |
| `memini:namespace` | Show, set, or clear the server-side namespace pin for this install   |

Recall shaping (both optional, matching the opencode/Claude Code plugins):
`recall_limit` (max memories per turn, default **3**) and `recall_max_tokens`
(hard token ceiling on the recall block, default **0** = uncapped, matching the
other integrations; set it `> 0` to cap a raised `recall_limit`, the tail is
dropped with a truncation footer). `recall_max_tokens` also reads
`MEMINI_INJECT_RECALL_MAX_TOK`, and `MEMINI_INJECT_LABELS` (`tier`, `confidence`,
`age`) toggles the per-bullet tag prefix.

`recall_position` controls where automatic recall is placed. It defaults to
`"prepend"`, preserving existing behavior. Set it to `"append"` to place recall
after the current prompt, keeping the existing conversation prefix stable for
provider prefix caching.

The **repeat-injection cooldown** keeps an already-injected memory from being
re-served on every step. It is windowed on two dimensions, and an injected id
is suppressed while inside _either_ window and re-served once **both** lapse:

- `inject_cooldown_ms` (config; also `MEMINI_INJECT_COOLDOWN_MS`, default
  **1800000** = 30 min, `0` disables it) — the **time** window an injected id
  is held back before it may re-serve.
- `inject_cooldown_prompts` (config; also `MEMINI_INJECT_COOLDOWN_PROMPTS`,
  default **3**, `0` disables it) — the **prompt** window. OpenClaw's
  `before_prompt_build` fires per agent _step_, not per user message, so this
  window **counts completed agent turns** (`agent_end`, which fires once per
  turn) — the closest thing to "user prompts" this host can honestly count.
  Steps within a turn never advance it, and until the session's first turn
  completes the prompt dimension is inert (time-only), so a host that never
  fires `agent_end` degrades to the time window rather than to
  suppress-forever.

Setting **both** knobs to `0` restores the prior suppress-forever behavior.

Capture filtering: cron/heartbeat turns are skipped by default (`skip_system_turns`),
runtime `(untrusted metadata)` preambles are stripped, and turns beginning with a
`[cron:` / `[Subagent Context]` marker (even behind a `User:` label) are dropped.
As an extra backstop, `min_capture_chars` (env `MEMINI_MIN_CAPTURE_CHARS`, default
**0** = off) drops a capture whose stripped user turn is shorter than N characters —
set it (e.g. `30`) if a gateway still emits short residual-noise turns.

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

**On by default as of 0.6.9** (it was opt-in before). The plugin fills the memory
slot — recall and capture are automatic — and _also_ registers explicit tools the
agent can call on demand. Set `"expose_tools": false` to restore the slot-only
surface.

The default flipped because the slot cannot express what the tools carry: `scope`
(how wide to read), `visibility` (who should know a fact), and the session
briefing with its ancestor `Scope:` line. With the tools off, an agent on this
harness simply does not have those capabilities — its only fallback is the
curl-based [memory skill](../skills/memory/SKILL.md), which sends the _base_
namespace and so cannot see per-agent memory at all.

- **`memory_briefing`** — query-less session-start orientation: pinned context,
  durable facts, how-to procedures, recent activity, plus a `scope_header`
  (`Scope: acme/phoenix/api ← acme/phoenix(3) ← acme(4) ← personal(2)`) naming
  the ancestor chain this namespace inherits from. Those ancestor names are what
  `visibility` accepts — the agent reads them, never guesses them.
- **`memory_recall`** — search, with optional `tags` / `metadata` filters and
  `scope`: `project` (this project only), `full` (default — plus ancestors, the
  user's personal namespace, and links), or `everywhere` (plus nested
  sub-projects). Results carry `from` provenance; an absent `from` means the
  memory is this project's own. A `degraded: "keyword_only"` field means semantic
  search was unavailable and results came from keyword matching alone.
- **`memory_list`** — query-less browse by tier / tags / metadata category
  (e.g. "all procedural memories" or "everything categorized `bug_fixes`").
- **`memory_remember`** — store a fact, with optional `tags`, a `category`, and
  `visibility`: `project` (default), `personal` (follows the user everywhere), or
  an ancestor name read off the briefing's Scope line. An unrecognized name is
  rejected with an error listing the valid chain. Episodic/working writes always
  stay in the project regardless. To correct an existing memory, pass its `id`
  (from recall/list) — the write updates it in place (`POST /v1/memories` upserts
  by id). A `reinforced: true` result means the fact was already known and no new
  memory was created.
- **`memory_forget`** — permanently delete a memory by `id` (from recall/list)
  when it's wrong, outdated, or poisoned. There is no `memory_update` here —
  this plugin talks to memini over REST, which has no partial-update
  endpoint; re-remember with the memory's `id` to correct it, and reserve
  forget for memories that shouldn't exist at all.

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
