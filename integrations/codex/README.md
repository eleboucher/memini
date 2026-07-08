# memini + Codex CLI

Codex reads MCP servers from `~/.codex/config.toml` (or a project-scoped
`.codex/config.toml`) under `[mcp_servers.<name>]`.

**Local (stdio)** — widely supported; see [`config.stdio.toml`](config.stdio.toml):

```sh
codex mcp add memini -- memini mcp
```

**Remote (Streamable HTTP)** — recent Codex; see [`config.remote.toml`](config.remote.toml).

Verify:

```sh
codex mcp list
```

## What you get

Codex sees memini's full `memory_*` tool set — no extra config beyond the
server registration above:

- **`memory_recall`** — hybrid (semantic + keyword) search, with optional
  `tags` / `metadata` filters, `response_format: "concise"` for a token-cheap
  first pass, and time-travel / nested-namespace options. Results carry
  `created_at` and `tags`; a `degraded: "keyword_only"` field means semantic
  search was unavailable and the results are keyword-only.
- **`memory_list`** — query-less browse by tier / tags / metadata category
  (e.g. all procedural memories, or everything categorized `bug_fixes`; see
  [`docs/categories.md`](../../docs/categories.md)). Returns at most `limit`
  (default 20) newest-first; page past it with `offset`.
- **`memory_get`** — fetch one memory with full metadata, tags, and
  timestamps by ID.
- **`memory_remember`** — store a fact, with optional `tags` and a `category`.
  Set `metadata.category` on writes to browse by subject later.
- **`memory_update`** — partial update of an existing memory by ID (only
  provided fields change); MCP-only, so this is the one tool in the set that
  the REST-backed integrations elsewhere in this repo don't get.
- **`memory_forget`** — permanently delete a memory by ID.
- **`memory_briefing`** — pinned context, durable facts, procedures, and
  recent activity for the namespace in one query-less call.
- **`memory_answer`** — ask a question and get an answer grounded on recalled
  memories, with the same `tags` / `metadata` filters (only advertised when
  the server has an LLM configured).

Use the same namespace as your other agents (the `MEMINI_DEFAULT_NAMESPACE` env
for stdio, or the `X-Memini-Namespace` header for remote) to share memory.
The `my-project` placeholder can be replaced with your real project name,
or removed entirely — memini auto-resolves the namespace from the git repo
basename of its own working directory when unset.
