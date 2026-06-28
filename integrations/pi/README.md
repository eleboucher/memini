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
- **`agent_end`** — once the agent finishes a prompt, stores the completed
  user/assistant turn back into memini (episodic, tagged `pi`, with the session
  id) so it can be recalled later.
- **Explicit tools** — the same set Claude Code gets from memini's MCP server,
  registered natively via `pi.registerTool`: `memory_recall`, `memory_list`,
  `memory_remember`, `memory_forget`. The model can call them on demand even
  though the automatic loop already runs.

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

| Env var                          | Default                 | Purpose                                                                                       |
| -------------------------------- | ----------------------- | --------------------------------------------------------------------------------------------- |
| `MEMINI_BASE_URL`                | `http://localhost:8080` | memini REST base URL (alias: `MEMINI_URL`)                                                    |
| `MEMINI_NAMESPACE`               | cwd basename            | tenant the memory is scoped to (`X-Memini-Namespace`)                                         |
| `MEMINI_RECALL`                  | on                      | `0`/`false` disables recall-before-turn                                                       |
| `MEMINI_CAPTURE`                 | on                      | `0`/`false` disables capture-after-turn                                                       |
| `MEMINI_RECALL_LIMIT`            | `3`                     | max memories injected per turn                                                                |
| `MEMINI_INJECT_RECALL_MAX_TOK`   | `0`                     | hard ceiling on recall-block tokens (`0` = unbounded); the tail is dropped with a footer      |
| `MEMINI_INJECT_RECALL_MIN_SCORE` | `0`                     | fused-score floor (>=) sent as `min_score` to `/v1/search`                                    |
| `MEMINI_INJECT_LABELS`           | —                       | comma-separated bullet labels: `tier`, `confidence`, `age`                                    |
| `MEMINI_TIMEOUT_MS`              | `30000`                 | per-request timeout                                                                           |
| `MEMINI_FALLBACK`                | on                      | `0`/`false` surfaces errors instead of degrading silently                                     |
| `MEMINI_API_KEY`                 | —                       | bearer token, if memini needs auth (sent as `Authorization: Bearer …`; alias: `MEMINI_TOKEN`) |
| `MEMINI_REQUIRE_HTTPS`           | —                       | `1` refuses to send the token over plaintext HTTP                                             |

Unset, the namespace is derived from the working-directory basename and sent as
the `X-Memini-Namespace` header — set it to share one memory pool with your
other agents (Claude Code, opencode, …).

### Build & test

```sh
cd integrations/pi/plugin
npm install
npm run build      # tsc -> dist/index.js
npm test           # pure-helper unit tests (tsx --test)
```

## Alternative: MCP wire

Pi can also reach memini's `memory_*` tools over MCP, but — unlike Claude Code
or Codex — Pi has no native MCP client, so you first need an MCP extension for
Pi (e.g. the one prewired in the [`my-pi`](https://github.com/spences10/my-pi)
distribution), then point it at memini's server: `http://<host>:8080/mcp`
(remote) or `memini mcp` (stdio). The native extension above is simpler and adds
the automatic recall/capture loop on top of the tools, so prefer it.
