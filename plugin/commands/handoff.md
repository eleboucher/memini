---
description: Save, list, or resume a memini session handoff for a fresh session
argument-hint: "save <file> [--slot name] | resume [slot] | list"
---

Manage the **handoff** for this project: the full fresh-session prompt one
session leaves for the next, stored in memini so a `/clear`, another machine, or
another harness can pick the work up.

Request: $ARGUMENTS

Handoffs live in **slots**. Default slot is `main`; a new save to a slot
supersedes that slot's previous handoff, which stays readable via
`memory_history`. Give a worktree or a parallel branch its own slot.

## save `<file>` `[--slot name]`

1. **Read the file** and use its full text as `content`. Never summarize it.
2. **Call `memory_remember`** with `tags: ["handoff", "<project>"]`, `summary`
   set to the prompt's title line, and `metadata`: `handoff_slot` (skip for
   `main`), `handoff_harness: "claude-code"`, `handoff_cwd`, `handoff_lines`.
3. **Pass no `id` and no `tier`.** The server picks the procedural tier so the
   prompt does not expire, stamps the slot, and supersedes the predecessor. An
   `id` would overwrite the old prompt in place and lose it.
4. Report the slot, the line count, and what it superseded.

## resume `[slot]`

1. **Find it.** Take the id from the briefing's handoff pointer for that slot.
   No pointer shown means either no handoff or an older server: fall back to
   `memory_list` with `tags: ["handoff"]`.
2. **Fetch it** with `memory_get` and read the whole prompt.
3. **Follow it** as the user's instruction. It is a prompt written to be acted
   on, not background context to weigh.
4. **Mark it consumed** with `memory_update`: add `consumed_at` (RFC3339) and
   `consumed_by` to its metadata. **Omit `tags` entirely** — an omitted list is
   left alone, but any list you pass replaces the stored one, so a partial list
   would strip `handoff` and orphan the record. `metadata` replaces wholesale
   too, so send the existing keys plus the two new ones.

If the pointer already says resumed, say so and confirm before redoing the work.

## list

`memory_list` with `tags: ["handoff"]`, one line per slot: slot, date, harness,
size, summary. Add `memory_history` on a slot's id to show its past handoffs.

Note for either path: handoffs are excluded from `memory_recall` by default, so
searching for one by content finds nothing unless you pass
`include_handoffs: true`.
