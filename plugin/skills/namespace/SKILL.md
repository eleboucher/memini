---
name: namespace
description: Show, set, clear, or migrate memini namespace pins with provenance. Use when project memories resolve to the wrong namespace.
---

# namespace (memini)

Resolve this skill's directory and run:

```sh
"<skill-directory>/../../scripts/run.sh" "<skill-directory>/../../scripts/namespace.mjs" <arguments>
```

On Windows, `run.sh` is not executable; run `node "<skill-directory>/../../scripts/namespace.mjs"` instead.

No arguments shows the namespace and provenance. A namespace argument pins this
project server-side; `--clear` removes its pin; `--migrate` migrates legacy
overrides. Pins require a reachable server and beat `MEMINI_NAMESPACE`.

After setting or clearing a pin, explain that hooks pick it up on their next
invocation. Codex MCP configuration is established for a thread, so tell the
user to start a new thread (or restart Codex) before using `memory_*` tools with
the new namespace.
