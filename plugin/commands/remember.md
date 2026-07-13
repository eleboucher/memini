---
description: Save a fact to memini's long-term memory
argument-hint: "<the fact to remember>"
---

Save this to memini using the `memory_remember` MCP tool:

$ARGUMENTS

Rules:

- **One atomic fact per call.** If the user gave you several, make several calls.
- Rewrite it to be **self-contained** — readable in six months by someone who was
  never in this conversation. "Use tabs" becomes "This project indents with tabs,
  not spaces." Resolve pronouns and relative dates ("yesterday" → the date).
- Pick the tier: `semantic` for facts, decisions, conventions and preferences;
  `procedural` for how-tos and commands. Omit it to let the server classify.
- Pick the visibility: `project` (the default) for anything specific to this
  codebase; `personal` for something true of the _user_ wherever they go. If they
  said "remember that I prefer X", that is `personal`.
- If this **contradicts or updates** something already in memory, call
  `memory_update` on the existing memory instead of saving a duplicate. Recall
  first if you are unsure.
- Tag it `pinned` only if it should surface in _every_ future session briefing.
  That budget is small; reserve it for durable identity and preferences.

Then tell the user what you saved, in which tier and namespace, in one line. If
the write fails, say so plainly and report the error — a silently dropped memory
is worse than no memory, because they will believe it is there.
