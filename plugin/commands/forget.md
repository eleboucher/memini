---
description: Delete a memory from memini
argument-hint: "<memory id, or a description of the memory>"
---

Delete a memory with the `memory_forget` MCP tool.

Target: $ARGUMENTS

`memory_forget` is **destructive and permanent**. Treat it that way:

1. **Find it first.** If the argument is a memory id, call `memory_get`. If it is a
   description, call `memory_recall` and identify the candidates. Copy the
   `namespace` field off the result verbatim — never construct one.

2. **Show the user the full content of what you are about to delete, and get
   explicit confirmation.** Do not skip this even when the match looks obvious, and
   never delete more than one memory from a single confirmation. If recall returned
   several plausible candidates, list them and ask — do not guess.

3. Only then call `memory_forget` with the id and namespace.

**Prefer correcting over deleting.** If the memory is merely _wrong_ or _outdated_
rather than unwanted, `memory_update` fixes it in place and preserves the history
chain, so recall can still explain what was believed and when. Deleting throws that
away. Suggest the update path when it applies, and let the user choose.

If the user is trying to remove something they consider sensitive, deleting the
memory is the right call — do it, and tell them plainly that it is gone from
memini, but that it may still exist in this session's transcript.
