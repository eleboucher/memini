---
name: doctor
description: Run read-only memini store and namespace diagnostics. Requires the local memini binary.
---

# doctor (memini)

First verify `memini` is available on `PATH`. If not, say this workflow requires
the local memini binary and point the user to the repository installation docs.
Do not substitute an MCP call.

Run `memini doctor` and explain its namespace, read-set, store-health, and
warning output. `memini doctor` is read-only. Do not run `--fix --yes` without
separate explicit authorization; `--fix` alone is only a preview.
