---
name: backfill
description: Import older Claude Code sessions into memini. Requires the local memini binary.
---

# backfill (memini)

First verify `memini` is available on `PATH`. If it is missing, say this
workflow requires the local binary.

Preview before writing:

```sh
memini import --source claude-code --dry-run ~/.claude/projects
```

After showing the preview, run the import only when the user asked to perform
the backfill. The importer reconstructs exchanges, assigns project namespaces,
and uses deterministic ids, so reruns are idempotent. Report the per-namespace
histogram. If only one project is wanted, use that project's transcript
directory instead.
