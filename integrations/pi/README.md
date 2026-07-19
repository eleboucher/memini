# memini + Pi

[Pi](https://pi.dev) is an open-source coding agent with a first-class
[extension API](https://pi.dev/docs/latest/extensions). Pi has no built-in MCP
client, so memini ships a native Pi package in [`plugin/`](plugin/). It adds
automatic session memory, native `memory_*` tools, lifecycle persistence, and
compact transcript rendering without an MCP adapter.

## Install

The package is published as
[`@eleboucher/pi-memini`](https://www.npmjs.com/package/@eleboucher/pi-memini)
and requires Pi 0.80.6 or newer (Node.js 22.19 or newer):

```sh
pi install npm:@eleboucher/pi-memini
```

Use `-l` for a project-local install. Pi records global packages in
`~/.pi/agent/settings.json` and project packages in `.pi/settings.json`.
Equivalent manual configuration is:

```json
{
  "packages": ["npm:@eleboucher/pi-memini"]
}
```

Project-local packages load only after the project is trusted. Run `pi config`
to enable or disable installed resources.

For development, load the built extension for one run:

```sh
npm --prefix integrations/pi/plugin run build
pi -e ./integrations/pi/plugin/dist/index.js
```

Pi also auto-discovers source extensions placed in
`~/.pi/agent/extensions/` or `.pi/extensions/`; those locations can be
hot-reloaded with `/reload`.

## What the extension does

### Automatic memory and lifecycle

- **`session_start` / `session_tree`** reconstruct branch-local suppression and
  capture state, then inject one bounded layered briefing when the active model
  context does not already contain it. Startup, resume, fork, and reload do not
  duplicate an intact briefing.
- **`before_agent_start`** searches for the submitted prompt and injects useful
  matches as untrusted, read-only context. Blank text, command-shaped prompts,
  and steering text shorter than 12 characters are skipped; search queries are
  capped at 2,000 characters.
- **`agent_settled`** captures the final successful user/assistant turn only
  after retries, overflow compaction, and queued continuations have settled.
  Repeated settled events for the same assistant entry are idempotent.
- **`session_before_compact` / `session_compact`** optionally checkpoint bounded
  state-changing activity, clear context-coupled suppression, and queue a fresh
  briefing without starting an extra turn.
- **`session_shutdown`** optionally records a bounded activity digest for real
  shutdown/switch/fork events. Reload is skipped because the session continues.
- **Branch-aware dedupe** persists in Pi custom entries only after the matching
  recall/tool-result message is finalized. Automatic briefing and prompt recall,
  plus explicit recall/briefing/list/get/history/answer results, share one cooldown
  state; successful updates, deletes, and ID upserts make corrected content eligible immediately,
  even when sibling tools complete concurrently.
  Set `MEMINI_INJECT_DEDUPE=0` to disable exclusions, filtering, and recording
  together.

The server-authoritative namespace from `POST /v1/handshake` scopes automatic
and explicit requests through `X-Memini-Namespace`. Automatic lifecycle work
no-ops while authority is unavailable; explicit tools retry the handshake once
and then return an actionable error rather than request a locally guessed partition.
A compatibility retry drops `exclude_ids` only after a server explicitly rejects
that field with HTTP 400; timeouts, throttling, and unrelated failures do not disable it.

### Native tools

Pi registers these tools directly with `pi.registerTool`:

| Tool              | Purpose                                                                                      |
| ----------------- | -------------------------------------------------------------------------------------------- |
| `memory_briefing` | Read pinned context, durable facts, procedures, recent work, and nested-project rollups.     |
| `memory_recall`   | Hybrid semantic/keyword recall with filters, time travel, scope, exclusions, and provenance. |
| `memory_list`     | Browse and page newest-first memories without a search query.                                |
| `memory_remember` | Store or upsert an atomic memory, including validity, confidence, metadata, and visibility.  |
| `memory_get`      | Fetch one complete memory DTO.                                                               |
| `memory_history`  | Read a memory's supersession history oldest-first.                                           |
| `memory_update`   | Partially correct content, summary, tier, level, tags, metadata, importance, or confidence.  |
| `memory_forget`   | Permanently delete a memory.                                                                 |

`memory_answer` is added dynamically only when authenticated
`GET /healthz?verbose=1` returns the literal boolean
`deps.llm.configured: true`. Missing, malformed, false, unreachable, or
unrouted capability evidence leaves the tool unadvertised.

The REST-backed answer tool intentionally omits MCP's `reasoning_level` because
the current `/v1/answer` REST request does not accept it. REST briefing also
does not expose the service's truncated-child count, so Pi preserves every
returned child rollup but does not fabricate `children_note`.

Read results include namespace/provenance evidence. For inherited or personal
memories, copy the returned `namespace` verbatim into get/history/update/forget;
do not invent namespace paths. Writes choose semantic `visibility` instead.

### Compact rendering

All explicit tools keep their complete structured JSON in the tool result sent
to the model and stored in the session. Their TUI renderer shows a concise
single line when collapsed and, with Pi's tool-expansion key (`Ctrl+O` by
default), a bounded human-readable view with at most eight memory/source items
plus kind-specific answer, acknowledgement, merge, and child-rollup details.

Automatic briefing and recall messages likewise remain persistent model
context while registered message renderers keep the normal transcript to one
line. Rendering never truncates or rewrites the model-facing payload.

## Configure

The extension has two configuration layers:

1. **Transport and identity** come from the environment of the Pi process.
2. **Behavior settings** resolve as **environment override → handshake server
   setting → built-in default**. This lets one server set team defaults while a
   local environment can override a specific knob.

### Transport and identity environment

| Environment variable      | Default                 | Effect                                                                                                   |
| ------------------------- | ----------------------- | -------------------------------------------------------------------------------------------------------- |
| `MEMINI_BASE_URL`         | `http://localhost:8080` | memini REST base URL.                                                                                    |
| `MEMINI_API_KEY`          | unset                   | Bearer token.                                                                                            |
| `MEMINI_REQUIRE_HTTPS`    | off                     | Set to `1` to refuse a bearer token over non-loopback plaintext HTTP.                                    |
| `MEMINI_HOME`             | unset                   | Personal namespace sent as `X-Memini-Home`; unset means no personal read leg.                            |
| `MEMINI_NAMESPACE`        | unset                   | Declared namespace fact and offline machine-local fallback; a server pin still wins.                     |
| `MEMINI_NAMESPACE_PREFIX` | unset                   | Prefix applied only to derived namespaces, online and in degraded fallback.                              |
| `MEMINI_AGENT`            | unset                   | Optional per-agent namespace suffix fact.                                                                |
| `MEMINI_TIMEOUT_MS`       | `30000`                 | Per-request timeout in milliseconds.                                                                     |
| `MEMINI_FALLBACK`         | on                      | `0`/`false` makes automatic lifecycle transport failures surface instead of degrading to warnings/no-op. |

The initial handshake sends git remote, repository root, cwd basename, and the
identity facts above. The server resolves pins and other server-side policy and
returns the authoritative namespace. The result is memoized for ten minutes;
setting or clearing a pin invalidates it immediately. If the handshake is
unreachable, Pi derives a namespace only for diagnostics and marks it degraded;
automatic memory traffic pauses and explicit tools refuse to route until server
authority is restored.

### Behavior settings used by Pi

| Environment override                 | Built-in default | Effect                                                                                        |
| ------------------------------------ | ---------------- | --------------------------------------------------------------------------------------------- |
| `MEMINI_RECALL`                      | on               | Automatic prompt recall.                                                                      |
| `MEMINI_CAPTURE`                     | on               | Settled-turn capture.                                                                         |
| `MEMINI_RECALL_LIMIT`                | `3`              | Maximum automatic recall hits.                                                                |
| `MEMINI_INJECT_RECALL_MAX_TOK`       | `250`            | Recall injection token ceiling; `0` is unbounded.                                             |
| `MEMINI_INJECT_RECALL_MIN_SCORE`     | `0`              | Minimum automatic recall score.                                                               |
| `MEMINI_INJECT_DEDUPE`               | on               | Shared cross-surface suppression state.                                                       |
| `MEMINI_INJECT_COOLDOWN_MS`          | `1800000`        | Time cooldown for repeated injection; `0` disables this dimension.                            |
| `MEMINI_INJECT_COOLDOWN_PROMPTS`     | `3`              | Prompt cooldown; `0` disables this dimension. Both cooldowns at `0` suppress for the session. |
| `MEMINI_INJECT_LABELS`               | empty            | Comma/pipe-separated automatic bullet labels: `tier`, `confidence`, `age`.                    |
| `MEMINI_INJECT_BRIEFING_PINNED`      | `5`              | Startup pinned-memory cap.                                                                    |
| `MEMINI_INJECT_BRIEFING_FACTS`       | `5`              | Startup durable-fact cap.                                                                     |
| `MEMINI_INJECT_BRIEFING_PROCEDURES`  | `5`              | Startup procedure cap.                                                                        |
| `MEMINI_INJECT_BRIEFING_RECENT`      | `3`              | Startup recent-memory cap.                                                                    |
| `MEMINI_INJECT_BRIEFING_MAX_TOK`     | `600`            | Whole briefing token ceiling; `0` is unbounded.                                               |
| `MEMINI_SESSION_DIGEST`              | on               | Pre-compaction and shutdown activity checkpoints.                                             |
| `MEMINI_MIN_CAPTURE_CHARS`           | `0`              | Minimum settled user-text length required for capture.                                        |
| `MEMINI_CAPTURE_USER_MAX_CHARS`      | `1000`           | Per-turn captured user-text cap; `0` is unbounded.                                            |
| `MEMINI_CAPTURE_ASSISTANT_MAX_CHARS` | `3000`           | Per-turn captured assistant-text cap; `0` is unbounded.                                       |

An already injected memory stays suppressed while **either** configured
cooldown still holds and may return only after both lapse. Explicit read tools
use conservative suppression until a correction evicts the stale entry.

## Slash commands

Type commands with the leading `/` in Pi's editor:

| Command                         | Effect                                                                                                                |
| ------------------------------- | --------------------------------------------------------------------------------------------------------------------- |
| `/memini:status`                | Show effective settings and provenance, resolved namespace, connection status, read set, redacted auth, and warnings. |
| `/memini:namespace`             | Show the current server-resolved namespace and pin provenance.                                                        |
| `/memini:namespace <namespace>` | Set this project's server-side namespace pin.                                                                         |
| `/memini:namespace --clear`     | Clear the server-side pin and return to automatic resolution.                                                         |

Command diagnostics are persisted as TUI-only custom entries and never enter
the model context, including server-authored pin notes and read-set labels.

Pins are stored by the memini server (`PUT`/`DELETE /v1/pins`) and keyed by the
project's git remote and/or repository root. They follow the project across
machines and beat `MEMINI_NAMESPACE` deliberately. Pin writes require a
reachable server; `MEMINI_NAMESPACE` remains the offline local override.

## Build and verify

From the package directory:

```sh
cd integrations/pi/plugin
npm ci
npm test
```

`npm test` is the standard verification path. It type-checks source and tests
against Pi 0.80.6, builds `dist/index.js`, runs bundle and helper/lifecycle/tool
contract tests, packs the publication artifact, installs it into a clean
consumer, and imports the installed ESM with only Pi-provided peer modules.

Useful focused commands:

```sh
npm run typecheck
npm run build
npm run test:unit
npm run test:package
npm pack --dry-run
```

The package's `prepack` hook repeats typecheck and build so a direct `npm pack`
or publish cannot ship stale source. `@memini/client` is bundled into the
single extension file; `typebox`, `@earendil-works/pi-coding-agent`, and
`@earendil-works/pi-tui` remain host-provided peers as required for Pi packages.

Relevant cross-integration parity checks from the repository root are:

```sh
pnpm --filter @memini/client test
node --test plugin/scripts/_test.mjs
```

## Alternative: MCP wire

Pi can reach memini's `memory_*` tools through a third-party MCP extension, then
connect to `http://<host>:8080/mcp` or `memini mcp` (stdio). The native package
above is preferred because it also provides automatic recall/capture, lifecycle
state, capability gating, and compact rendering.
