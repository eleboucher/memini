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

Then tell the user what you saved, in which tier and namespace, in one line.

Read the result before you report it. Three flags change what actually happened:

- **`reinforced: true`** — the fact was **already known**. No new memory was
  created; the existing one was strengthened, and the returned `id` belongs to
  _that_ memory, not to anything you just wrote. Say "already recorded, so I
  reinforced it" rather than claiming a new save. Be careful with that id: a
  follow-up update or forget would hit a memory you did not create.
- **`stored: false`** — the value gate dropped a low-signal write. Nothing was
  saved. This is not an error, but do not tell the user you saved something.
- **`merge_hint`** — the content nearly duplicates an existing memory. Either fold
  them together with `memory_update` using `merge_hint.similar_id`, or keep both
  deliberately. Do not silently ignore it.

If the write fails outright, say so plainly and report the error. A silently
dropped memory is worse than no memory, because the user will believe it is there.
