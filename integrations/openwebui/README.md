# memini + Open WebUI

[Open WebUI](https://openwebui.com) is extensible through
[Functions](https://docs.openwebui.com/features/plugin/functions/) (Python
modules that hook the request lifecycle) and
[Tools](https://docs.openwebui.com/features/plugin/tools/) (Python functions the
model can call). memini ships both:

- a **Filter function** ([`filter/memini_memory.py`](filter/memini_memory.py))
  that makes memory automatic — recall before each turn, capture after — with no
  tool calls from the model. This is the recommended path, and the analog of the
  [opencode plugin](../opencode/).
- a **Tools** module ([`tools/memini_tools.py`](tools/memini_tools.py)) that
  exposes `recall_memory` / `remember_memory` / `forget_memory` (delete a
  wrong/outdated memory by the id shown in recall) for the model to call on demand.

Both talk to memini over REST (`POST /v1/search`, `POST /v1/memories`), scoped by
the `X-Memini-Namespace` header. The API key is read from the `MEMINI_API_KEY`
environment variable of the Open WebUI process, so the secret never lands in the
Open WebUI database.

## Recommended: the memory filter

What it wires (two methods on the `Filter` class):

- **`inlet`** — searches memini for the latest user message and inserts the
  matches as a system message before the turn runs. It excludes this chat's own
  captured turns (already in the live transcript), so they aren't echoed back as
  memory a turn behind; other chats still recall. What a chat has already been
  shown is deduped by the **windowed repeat-injection cooldown** (below), so an
  unchanged match isn't re-served turn after turn.
- **`outlet`** — once the response completes, stores the user/assistant turn
  back into memini (episodic, tagged with the chat id) so it can be recalled
  later.

### Install

Open WebUI functions are single Python files. Either import from the community
library or paste the code in directly:

1. **Admin Panel → Functions → `+` (New Function)**.
2. Paste the contents of [`filter/memini_memory.py`](filter/memini_memory.py)
   and **Save**.
3. Toggle the function **active**, then make it apply to chats:
   - **Globally:** Functions → Filter Management → the three-dot menu on the
     filter → click the **Globe** icon (highlighted = global, all models).
   - **Per model:** Workspace → Models → edit a model → select it under
     **Filters**.

The API key (if memini needs auth) goes in the environment that launches Open
WebUI — `MEMINI_API_KEY=…` — not in a Valve, so it stays out of the DB.

### Configure (Valves)

Everything else is a [Valve](https://docs.openwebui.com/features/plugin/valves/)
you set in the function's settings (the gear on the function), no code edit
needed:

| Valve               | Default                 | Purpose                                                                                                                                                                                                                      |
| ------------------- | ----------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `base_url`          | `http://localhost:8080` | memini REST base URL (default seeds from `MEMINI_BASE_URL` env)                                                                                                                                                              |
| `namespace`         | `openwebui`             | project the memory is scoped to (`X-Memini-Namespace`; see Namespace resolution below)                                                                                                                                       |
| `home`              | unset (`MEMINI_HOME`)   | caller's personal namespace, sent as `X-Memini-Home`; unset = no home leg                                                                                                                                                    |
| `recall`            | on                      | recall memories before each turn                                                                                                                                                                                             |
| `capture`           | on                      | capture the completed turn after each response                                                                                                                                                                               |
| `recall_limit`      | `3`                     | max memories injected per turn                                                                                                                                                                                               |
| `timeout_ms`        | `30000`                 | per-request timeout (for `/v1/search`/`/v1/memories`; the handshake has its own ~2.5s timeout). Must exceed the server's `MEMINI_RERANK_TIMEOUT`, or a slow reranker returns nothing instead of degrading to composite order |
| `fallback_on_error` | on                      | degrade silently on memini errors instead of surfacing them                                                                                                                                                                  |
| `require_https`     | off                     | refuse to send the API key over plaintext HTTP to a remote                                                                                                                                                                   |
| `scope_by_user`     | off                     | isolate memory per Open WebUI user (suffix namespace with id)                                                                                                                                                                |
| `priority`          | `0`                     | filter execution order                                                                                                                                                                                                       |

Open WebUI is multi-user, unlike a local agent. Set the same `namespace` to pool
one shared memory across your agents, or flip `scope_by_user` on to give each
Open WebUI account its own private memory.

### Repeat-injection cooldown

The filter keeps a per-chat map of what it already injected (bounded; keyed by
the chat id) and applies the **windowed repeat-injection cooldown** shared with
the Claude Code / opencode / hermes / openclaw integrations: an
already-injected memory is excluded from recall (server-side via `exclude_ids`,
with a client-side backstop for older servers) while it is inside _either_
window — the **time** window (`inject_cooldown_ms`) or the **prompt** window
(`inject_cooldown_prompts`, one prompt = one `inlet` call) — and is re-served
once **both** have lapsed. A memory whose content was updated in place
re-injects immediately (the content-hash bypass), so a correction is never
withheld for the window.

There is no valve for these two knobs: like the capture bounds, the cooldown
policy is the server's call and arrives via the handshake's `ClientSettings`
(`inject_cooldown_ms` / `inject_cooldown_prompts` — configure them server-side
or per-key). When the handshake is unavailable the filter falls back to the
same built-in defaults the server ships: **30 min / 3 prompts**. Setting both
to `0` server-side suppresses a shown memory for the chat's whole lifetime
(the legacy behavior). Chats without a chat id (rare) have no key to dedupe by
and stay un-deduped, mirroring the capture path's chat-id rule.

### Namespace resolution

Open WebUI is a server, not a local agent — there is no meaningful per-request
working directory the way there is for opencode or Hermes, so a cwd-keyed
override was never meaningful here and that mechanism is gone. The `namespace`
valve is instead the **declared namespace**: on each recall/capture (and each
`memini_status` call), the Filter or Tools instance calls the server's
`POST /v1/handshake` (api/openapi.yaml) with `project.declared_namespace` set
to the valve's value, and the server echoes it back verbatim unless an
explicit pin overrides it. The call is fail-soft — any error or a ~2.5s
timeout falls back to the valve value alone, so an unreachable or older
memini never breaks a turn — and memoized per Filter/Tools instance for 10
minutes.

`scope_by_user` still appends the per-user suffix (`<namespace>-<id>`) on top
of whatever namespace was resolved (valve or server): it isolates _who_, not
_what_, and dropping it would collapse every account on a shared server into
one namespace.

Both files read the API key from `MEMINI_API_KEY` in the server's environment,
never from a valve, so the secret stays out of the Open WebUI database.

## Alternative: the memory tools (on demand)

Instead of (or alongside) the filter, expose memini's `recall_memory`,
`remember_memory`, `forget_memory` and `memini_status` (read-only: which
namespace is in force and why, secrets redacted) as tools the model calls itself.

1. **Workspace → Tools → `+`**, paste
   [`tools/memini_tools.py`](tools/memini_tools.py), **Save**.
2. Enable the tool for a model in **Workspace → Models** (the tool's checkbox),
   and set the same Valves (`base_url`, `namespace`, `home`, …) in the tool's settings.
3. For reliable tool use, set **Function Calling → Native** (Admin Panel →
   Settings → Models, globally or per model); the default prompt-based mode works
   too but is less consistent.

The filter is automatic and invisible; the tools give the model explicit control
but depend on it choosing to call them. Pick one as your default — running both
double-writes each turn.

## Alternative: MCP via mcpo

To use memini's full MCP toolset instead of the REST wrappers, put memini behind
[`mcpo`](https://github.com/open-webui/mcpo) (Open WebUI's MCP-to-OpenAPI proxy)
and register it as an OpenAPI tool server:

```bash
# Expose memini's stdio MCP server as an OpenAPI tool server on :8000
uvx mcpo --port 8000 -- memini mcp
```

Then **Settings → Tools → Add Connection** (type `openapi`,
url `http://localhost:8000`), or set `TOOL_SERVER_CONNECTIONS` in the Open WebUI
environment. memini's MCP server can't infer the namespace the way the filter
does — set `MEMINI_DEFAULT_NAMESPACE` on the `memini mcp` process (or pass
`namespace` per tool call). Mind the context budget: only register the MCP
servers you actually use.

## Tests

The pure helpers, the filter's recall/capture flow, and the `POST /v1/handshake`
wire contract (namespace precedence, fail-soft, memoization) are unit-tested:

```bash
cd integrations/openwebui && python -m unittest
```

(Requires `pydantic` and `aiohttp`, both bundled with Open WebUI. The flow and
handshake tests skip automatically if they aren't installed.)

## Publishing

The two files carry the YAML frontmatter Open WebUI parses (`title`, `author`,
`version`, `license`, `required_open_webui_version`, …). To share them on the
[Open WebUI Community Library](https://openwebui.com) so others can install with
one click: sign in at openwebui.com, create a Function / Tool, paste the code,
and publish. Users then hit **Get** on the listing, enter their instance URL, and
**Import to Open WebUI**. Bump the `version` field on each release — Open WebUI
shows it in the UI so users can tell when an update is available.
