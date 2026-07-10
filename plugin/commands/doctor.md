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

It also reports the **retrieval scope**: `MEMINI_GLOBAL_NAMESPACE`, the
tenant-shared namespace (`<tenant>/_shared`, shown as `(off)` unless
`MEMINI_TENANT_SHARED` is enabled), and the resolved effective read
set (namespace, tier access, and source: default/global/tenant-shared) for the
plugin namespace, answering "why does recall see/miss X". It warns on a
read-set entry holding zero memories (e.g. an empty tenant-shared namespace)
and a redundant or self-referencing entry.

If it reports warnings, explain them plainly and point the user at the suggested
fix (`memini namespace split` for a collapsed pool, setting
`MEMINI_DEFAULT_NAMESPACE` for a resolution mismatch, or writing shared facts
into `<tenant>/_shared` for an empty tenant-shared namespace). To let memini
remediate a poisoned store itself, run `memini
doctor --fix` (preview) and then `memini doctor --fix --yes` to apply. The fix
chain also backfills nil confidence on legacy (pre-0.0.11) durable memories,
so they enter the demote lifecycle instead of staying permanently trusted.
