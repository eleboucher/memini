---
description: Diagnose memini namespace mismatches and store health
---

Run `memini doctor` in a shell and report its output to the user.

`memini doctor` is read-only. It shows how the namespace resolves for writes
versus recall and lists per-namespace memory counts, flagging the two failure
modes behind "my agent stopped remembering things":

- the plugin and the server resolving different namespaces (writes land where
  recall doesn't look), and
- a catch-all namespace (`default` / `openclaw`) that has ballooned from a bulk
  import collapsing many sources into one pool.

If it reports warnings, explain them plainly and point the user at the suggested
fix (`memini namespace split` for a collapsed pool, or setting
`MEMINI_DEFAULT_NAMESPACE` for a resolution mismatch). To let memini remediate a
poisoned store itself, run `memini doctor --fix` (preview) and then
`memini doctor --fix --yes` to apply. The fix chain also backfills nil
confidence on legacy (pre-0.0.11) durable memories, so they enter the demote
lifecycle instead of staying permanently trusted.
