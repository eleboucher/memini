# memini + Pi

[Pi](https://pi.dev) is an open-source coding agent with a first-class
[extension API](https://pi.dev/docs/latest/extensions). Pi has **no built-in
MCP** — capabilities are added through extensions — so memini ships a native
extension in [`plugin/`](plugin/) that makes memory both automatic and
tool-callable, with no MCP layer required.

## Recommended: the memory extension

What it wires:

- **`before_agent_start`** — searches memini for the user's prompt and injects
  the matches as a persistent context message before the agent runs. It excludes
  this session's own captured turns (already in live context), so they aren't
  echoed back a turn behind; past sessions still recall.
- **`agent_settled`** — once retries, compaction recovery, and queued
  continuations are finished, stores the completed user/assistant turn back
  into memini (episodic, tagged `pi`, with the session id) so it can be recalled
  later.
- **Session lifecycle** — injects a bounded briefing at startup, writes bounded
  pre-compaction/session checkpoints, restores branch-local suppression state,
  and re-briefs after compaction without flooding the transcript.
- **Explicit tools** — registered natively via `pi.registerTool`, with complete
  model-facing JSON and compact TUI rendering: `memory_briefing`,
  `memory_recall`, `memory_list`, `memory_remember`, `memory_get`,
  `memory_history`, `memory_update`, and `memory_forget`. `memory_answer` is
  added dynamically only when authenticated `GET /healthz?verbose=1` literally
  reports `deps.llm.configured: true`; false or unavailable capability evidence
  leaves it unadvertised.

The REST-backed `memory_answer` intentionally does not expose MCP's
`reasoning_level` yet because the current `/v1/answer` OpenAPI request does not
accept that field. Likewise, REST briefing does not expose the service's
truncated-child count, so Pi returns every child rollup REST supplied but does
not fabricate MCP's `children_note`. These limitations are evidence-based and
fail closed rather than guessing server support.

### Install

The extension is published to npm as
[`@eleboucher/pi-memini`](https://www.npmjs.com/package/@eleboucher/pi-memini).
Pi has no `init`/scaffold command — extensions are just discovered from known
locations or declared in config. Pick one:

**Project / global settings** — add the package to `settings.json`
(`.pi/settings.json` for one project, or `~/.pi/agent/settings.json` globally):

```json
{
  "packages": ["npm:@eleboucher/pi-memini"]
}
```

**Discovery folder** — Pi auto-discovers and hot-reloads extensions in
`~/.pi/agent/extensions/` (global) or `.pi/extensions/` (project-local). Drop
the built `dist/index.js` (or the `src/index.ts` source) there.

**Quick test** — point Pi at a local checkout for one run:

```sh
pi -e ./integrations/pi/plugin/dist/index.js
```

### Configure

All config is via environment variables in the shell that launches Pi (secrets
stay out of any file):

| Env var                          | Default                          | Purpose                                                                                                                                            |
| -------------------------------- | -------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------- |
| `MEMINI_BASE_URL`                | `http://localhost:8080`          | memini REST base URL                                                                                                                               |
| `MEMINI_NAMESPACE`               | unset (server handshake decides) | machine-local namespace override; the offline escape hatch when the server is unreachable                                                          |
| `MEMINI_HOME`                    | unset                            | caller's personal namespace, sent as `X-Memini-Home`; unset = no home leg                                                                          |
| `MEMINI_RECALL`                  | on                               | `0`/`false` disables recall-before-turn                                                                                                            |
| `MEMINI_CAPTURE`                 | on                               | `0`/`false` disables capture-after-turn                                                                                                            |
| `MEMINI_RECALL_LIMIT`            | `3`                              | max memories injected per turn                                                                                                                     |
| `MEMINI_INJECT_RECALL_MAX_TOK`   | `0`                              | hard ceiling on recall-block tokens (`0` = unbounded); the tail is dropped with a footer                                                           |
| `MEMINI_INJECT_RECALL_MIN_SCORE` | `0`                              | fused-score floor (>=) sent as `min_score` to `/v1/search`                                                                                         |
| `MEMINI_INJECT_COOLDOWN_MS`      | `1800000`                        | repeat-injection cooldown, **time** window (ms) before an already-injected memory may re-inject; `0` disables the time dimension                   |
| `MEMINI_INJECT_COOLDOWN_PROMPTS` | `3`                              | repeat-injection cooldown, **prompt** window (per user turn); `0` disables the prompt dimension; both cooldown vars `0` = suppress for the session |
| `MEMINI_INJECT_LABELS`           | —                                | comma-separated bullet labels: `tier`, `confidence`, `age`                                                                                         |
| `MEMINI_TIMEOUT_MS`              | `30000`                          | per-request timeout                                                                                                                                |
| `MEMINI_FALLBACK`                | on                               | `0`/`false` surfaces errors instead of degrading silently                                                                                          |
| `MEMINI_API_KEY`                 | —                                | bearer token, if memini needs auth (sent as `Authorization: Bearer …`)                                                                             |
| `MEMINI_REQUIRE_HTTPS`           | —                                | `1` refuses to send the token over plaintext HTTP                                                                                                  |

The two `MEMINI_INJECT_COOLDOWN_*` vars are the windowed **repeat-injection
cooldown**: an already-injected memory is excluded from recall (server-side via
`exclude_ids`, with a client-side backstop) while it is inside _either_ window,
and re-served only once _both_ have lapsed. Pi's `before_agent_start` fires once
per user prompt, so it advances both the clock and the prompt counter — both
dimensions apply here. Both vars `0` restores the prior suppress-for-the-session
behavior.

The namespace itself is resolved by the memini **server**, not this extension:
at the first turn the extension performs the config handshake
(`POST /v1/handshake`), sending the project's facts (git remote, toplevel,
cwd basename) and using whatever the server resolves — a pin recorded for this
project, `MEMINI_NAMESPACE` if exported, or derivation from the facts (repo
name, then toplevel basename, then cwd basename). The result is memoized in
memory for ten minutes. When the server is unreachable, the extension degrades
to the same chain locally: `MEMINI_NAMESPACE`, else git/cwd derivation — which
is why the env var is best thought of as the offline escape hatch, not the
primary lever.

### Commands

| Command            | What it does                                                                    |
| ------------------ | ------------------------------------------------------------------------------- |
| `memini:status`    | Effective settings, the resolved namespace **and where it came from**, warnings |
| `memini:namespace` | Show, set, or clear the server-side namespace pin for this project              |

`memini:status` exists because a list of values is not enough to debug a namespace
problem. It shows provenance (`<- env` vs `<- server` vs `(default)`), so a
`MEMINI_NAMESPACE` exported once from a shell profile — which pins _every_ repo on
the machine to one namespace — shows up as a warning rather than as a mystery.
Secrets are redacted.

### The namespace pin

```
memini:namespace              # show the namespace and where it came from
memini:namespace acme/api     # pin this project to acme/api
memini:namespace --clear      # back to automatic resolution
```

The pin lives on the **memini server** (`PUT`/`DELETE /v1/pins`), keyed by the
project's git remote and/or toplevel path — so it follows you across machines,
and every client that handshakes for this project (Claude Code, this extension,
`memini doctor`) resolves the same value.

A pin beats `MEMINI_NAMESPACE` at handshake time, deliberately: a globally
exported `MEMINI_NAMESPACE` is exactly the problem a pin exists to solve, so if
the environment won, the command would silently do nothing on the machines that
need it. Setting or clearing a pin takes effect on the next turn — the write
drops the extension's in-memory handshake memo, so there is no restart or
ten-minute wait.

Because pins are server-side, setting one needs the server reachable. For an
offline, machine-local override, export `MEMINI_NAMESPACE` instead.

### Build & test

```sh
cd integrations/pi/plugin
npm install
npm run build      # esbuild bundle -> dist/index.js
npm test           # bundle test (node --test) + pure-helper unit tests (tsx --test)
```

## Alternative: MCP wire

Pi can also reach memini's `memory_*` tools over MCP, but — unlike Claude Code
or Codex — Pi has no native MCP client, so you first need an MCP extension for
Pi (e.g. the one prewired in the [`my-pi`](https://github.com/spences10/my-pi)
distribution), then point it at memini's server: `http://<host>:8080/mcp`
(remote) or `memini mcp` (stdio). The native extension above is simpler and adds
the automatic recall/capture loop on top of the tools, so prefer it.
