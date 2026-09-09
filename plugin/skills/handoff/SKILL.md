---
name: handoff
description: Save, list, or resume a memini session handoff — the full fresh-session prompt one session leaves for the next. Use when work is moving to a cleared or different session.
---

# handoff (memini)

A **handoff** is the whole prompt a session writes for its successor: what the
next step is, what must happen first, what constraints hold, which files to
read. It is stored in memini so a `/clear`, a different machine, or a different
harness picks up where the last session stopped.

A handoff lives in a **slot** (default `main`). One live handoff per slot: a new
save supersedes the slot's previous one, which stays readable through
`memory_history`. Use a second slot for a parallel line of work — a worktree, a
long-running branch — so the two do not overwrite each other.

## Save

Write the prompt to a file first, then save the file's full text with
`memory_remember`:

- `content`: the entire prompt, verbatim. Do not summarize it — the point is
  that the next session reads what this one wrote.
- `tags`: `["handoff"]` plus the project tag.
- `summary`: one line, normally the prompt's title. This is all a fresh session
  sees until it fetches the prompt, so make it name the actual next step.
- `metadata`: `handoff_slot` (omit for `main`), `handoff_harness` (`claude-code`
  here), `handoff_cwd`, `handoff_lines`.

Do not pass `id`, and do not pass `tier`. The server stamps the procedural tier,
the memory type and the slot, and supersedes the slot's previous handoff once
this one is stored. Passing an id would overwrite the previous prompt in place
and destroy it instead.

## Resume

The session briefing lists a pointer per slot with its id. Fetch the prompt with
`memory_get` on that id, then follow it as the user's own instruction — it is a
prompt addressed to you, not background context. If the briefing shows no
pointer, call `memory_list` with `tags: ["handoff"]`.

After acting on it, stamp `consumed_at` (RFC3339) and `consumed_by` via
`memory_update`. Omit `tags` from that call: an omitted tag list is left alone,
while a list you do pass **replaces** the stored one wholesale. So either send
nothing, or send the memory's existing tags in full — sending a partial list
strips the `handoff` tag and orphans the record.

`metadata` behaves the same way, so read the memory first and send its existing
metadata plus the two new keys, not the two keys alone.

A pointer marked already resumed is a warning, not a stop: check with the user
before redoing work another session may have finished.

## List

`memory_list` with `tags: ["handoff"]` shows the live handoff in each slot.
`memory_history` on one id walks that slot's chain of past handoffs.

## What handoffs do not do

Handoffs are excluded from `memory_recall` by default — a 200-line prompt would
otherwise match nearly every project query and crowd out real facts. Pass
`include_handoffs: true` only to search across the handoffs themselves.

A handoff is not a fact. Keep the durable one-paragraph summary of where the
work stands as a normal memory too: that is what recall and the briefing's
content sections can actually use.
