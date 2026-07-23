---
name: status
description: Show memini connection health, effective settings, namespace provenance, and read set. Use for configuration or recall diagnostics.
---

# status (memini)

Resolve this skill's directory, then run its bundled script without assuming a
global plugin path:

```sh
"<skill-directory>/../../scripts/run.sh" "<skill-directory>/../../scripts/status.mjs"
```

Pass `--json` when machine-readable output is requested. This command is
read-only and redacts secrets. Lead with the effective namespace and warnings;
explain degraded mode, a global `MEMINI_NAMESPACE` pin, and cwd split-brain in
plain language.
