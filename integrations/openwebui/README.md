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
  memory a turn behind; other chats still recall.
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
WebUI — `MEMINI_API_KEY=…` (alias: `MEMINI_TOKEN`) — not in a Valve, so it stays
out of the DB.

### Configure (Valves)

Everything else is a [Valve](https://docs.openwebui.com/features/plugin/valves/)
you set in the function's settings (the gear on the function), no code edit
needed:

| Valve               | Default                 | Purpose                                                                      |
| ------------------- | ----------------------- | ---------------------------------------------------------------------------- |
| `base_url`          | `http://localhost:8080` | memini REST base URL (default seeds from `MEMINI_BASE_URL`/`MEMINI_URL` env) |
| `namespace`         | `openwebui`             | tenant the memory is scoped to (`X-Memini-Namespace`)                        |
| `recall`            | on                      | recall memories before each turn                                             |
| `capture`           | on                      | capture the completed turn after each response                               |
| `recall_limit`      | `3`                     | max memories injected per turn                                               |
| `timeout_ms`        | `5000`                  | per-request timeout                                                          |
| `fallback_on_error` | on                      | degrade silently on memini errors instead of surfacing them                  |
| `require_https`     | off                     | refuse to send the API key over plaintext HTTP to a remote                   |
| `scope_by_user`     | off                     | isolate memory per Open WebUI user (suffix namespace with id)                |
| `priority`          | `0`                     | filter execution order                                                       |

Open WebUI is multi-user, unlike a local agent. Set the same `namespace` to pool
one shared memory across your agents, or flip `scope_by_user` on to give each
Open WebUI account its own private memory.

## Alternative: the memory tools (on demand)

Instead of (or alongside) the filter, expose memini's `recall_memory` and
`remember_memory` as tools the model calls itself.

1. **Workspace → Tools → `+`**, paste
   [`tools/memini_tools.py`](tools/memini_tools.py), **Save**.
2. Enable the tool for a model in **Workspace → Models** (the tool's checkbox),
   and set the same Valves (`base_url`, `namespace`, …) in the tool's settings.
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

The pure helpers and the filter's recall/capture flow are unit-tested:

```bash
cd integrations/openwebui && python -m unittest
```

(Requires `pydantic` and `aiohttp`, both bundled with Open WebUI. The filter-flow
tests skip automatically if they aren't installed.)

## Publishing

The two files carry the YAML frontmatter Open WebUI parses (`title`, `author`,
`version`, `license`, `required_open_webui_version`, …). To share them on the
[Open WebUI Community Library](https://openwebui.com) so others can install with
one click: sign in at openwebui.com, create a Function / Tool, paste the code,
and publish. Users then hit **Get** on the listing, enter their instance URL, and
**Import to Open WebUI**. Bump the `version` field on each release — Open WebUI
shows it in the UI so users can tell when an update is available.
