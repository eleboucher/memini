---
description: Import past Claude Code sessions into memini as episodic memory
---

Backfill the user's existing Claude Code history into memini so sessions that
predate the plugin become searchable memory.

Run:

```
memini import --source claude-code ~/.claude/projects
```

This walks every transcript under `~/.claude/projects`, reconstructs each
user→assistant exchange as an episodic memory, and scopes it to the namespace
that project's git remote resolves to — the same namespace live writes use, so
backfilled history and new memories land together. Record IDs are deterministic,
so re-running it is safe and idempotent (already-imported exchanges are skipped,
not duplicated).

After it finishes, report the per-namespace histogram it prints. If the user
only wants one project, point the command at that project's transcript directory
instead. To preview without writing, add `--dry-run`.
