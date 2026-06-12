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

`memory_recall` runs hybrid retrieval (vector + keyword, fused with RRF), so
natural-language queries work as well as exact keywords. Prefer a short
descriptive query ("JWT auth setup") over a single keyword.

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

Read the returned `results[].memory.content`. Don't dump the raw list to
the user — synthesize: "I remember we chose X because Y, and last time we
hit Z." When you state a fact that came from memory, quote the stored
content verbatim rather than paraphrasing it into something it didn't say;
if a memory is ambiguous, say so instead of guessing.

## Unhappy paths

- **No results**: say memini has nothing on this and proceed from first
  principles — never invent a "remembered" fact to fill the gap.
- **Conflicting results**: prefer the most recent (a newer memory may
  supersede an older one), but surface the conflict and let the user
  resolve it.
- **Tool errors / server unreachable**: report that memini is unavailable
  and suggest running `memini doctor`; continue without memory rather than
  blocking.
