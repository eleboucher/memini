---
name: recall
description: >-
  Search memini for prior context, decisions, or facts relevant to the
  current task. Use this skill when the user asks "what do you know
  about X", "did we discuss Y before", or before starting work that may
  have prior context (a file edit, an architectural change, debugging a
  recurring issue). The MCP tool name is `memory_recall`.
---

# recall (memini)

memini is a memory service. To search for prior context, call the
`memory_recall` MCP tool with:

- `query` (required) — a natural-language description of what you're looking for
- `limit` (optional) — max results; default 10, suggested 5 for targeted search
- `tiers` (optional) — restrict to specific tiers (`semantic`, `procedural`,
  `episodic`, `working`)
- `tags` (optional) — restrict to memories carrying every listed tag (AND)
- `metadata` (optional) — restrict to memories whose metadata contains each
  key=value pair, e.g. `{"category":"bug_fixes"}` (see `docs/categories.md`)
- `scope` (optional) — `subtree` also searches namespaces nested under this one
  (a `project` namespace then also reads `project/<agent>`), for reading shared
  plus per-agent memory; default `exact`.
- `namespaces` (optional): search exactly these namespaces instead of the
  default read set (namespace, its subtree/links/env-configured namespaces,
  and the global namespace), replacing it entirely rather than adding to it.
  An entry ending in `/*` also includes namespaces nested under it. Max 16,
  and the current namespace isn't added automatically, so list it too if you
  still want it searched.
- `as_of` (optional) — an RFC3339 time for "what was true then" queries; returns
  facts valid at that instant, including ones since superseded.
- `response_format` (optional) — `concise` returns each result's summary (or the
  first ~240 characters of content, truncated) instead of full content; use it
  for a token-efficient first pass over many results, then call `memory_get` on
  the ones worth reading in full. Default `detailed` returns full content.

To browse without a query — "show me everything tagged X" or "all procedural
memories in the `deployment_runbook` category" — call `memory_list` instead. It
takes the same `tiers` / `tags` / `metadata` filters plus `limit` (default 20)
and `offset` for paging past it, and returns matching memories newest-first
with no relevance query.

`memory_recall` runs hybrid retrieval (vector + keyword), then ranks by
relevance and memory quality (a corroborated, durable, frequently-recalled fact
outranks a one-off note), so natural-language queries work as well as exact
keywords. Each result carries `confidence` (durable facts only — treat a
low-confidence memory as a weak signal), `created_at`, and `tags`. Prefer a
short descriptive query ("JWT auth setup").

To orient at the start of a session without a query, call `memory_briefing`
instead: it returns pinned context, durable facts, how-to procedures, and recent
activity in one call.

## When to call

- Before editing a file the user hasn't touched recently — surface past
  edits, gotchas, or related decisions.
- At the start of a debugging task on a recurring issue — past root
  causes are gold here.
- When the user asks "what do we know about…" or "did we already decide…".
- When the model is about to make a non-obvious decision — check whether
  the codebase has a recorded preference.

## When NOT to call

- The information is in the current session's context already.
- The user is asking about something brand-new with no prior history.
- A trivial lookup (function signature, line of code) — use the file
  tools, not memory.

## After recall

Read the returned `results[].content`. Don't dump the raw list to
the user — synthesize: "I remember we chose X because Y, and last time we
hit Z." When you state a fact that came from memory, quote the stored
content verbatim rather than paraphrasing it into something it didn't say;
if a memory is ambiguous, say so instead of guessing.

## Unhappy paths

- **No results**: say memini has nothing on this and proceed from first
  principles — never invent a "remembered" fact to fill the gap.
- **Conflicting results**: prefer the one with the later `created_at` (a
  newer memory may supersede an older one), but surface the conflict and let
  the user resolve it.
- **Degraded (keyword-only) results**: a top-level `degraded: "keyword_only"`
  (with a `note`) means semantic search was unavailable and the results came
  from keyword matching alone — treat them as incomplete, not exhaustive; a
  relevant memory may exist but not have matched. Don't report "nothing
  found" as a confident negative when this flag is set.
- **Tool errors / server unreachable**: report that memini is unavailable
  and suggest running `memini doctor`; continue without memory rather than
  blocking.
