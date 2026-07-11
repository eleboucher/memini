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

It also reports the **effective read set**: a NAMESPACE/ORIGIN/TIERS table
showing every namespace a plain recall/briefing pulls from for the plugin
namespace — the namespace itself (`primary`), each ancestor in its path
(`ancestor`, nearest first), its personal namespace when `MEMINI_HOME` is set
(`home`), and any stored namespace links (`link`) — answering "why does
recall see/miss X". Ancestor/home/link legs only ever carry durable tiers
(semantic, procedural); episodic/working memories never cross a namespace
boundary. It warns when `MEMINI_HOME` is unset (so `visibility: "personal"`
writes would error and no personal leg merges in) and when a link points at a
namespace holding no memories yet (a note, not a warning — linking ahead of a
namespace's first write is legal).

If it reports warnings, explain them plainly and point the user at the suggested
fix (`memini namespace split` for a collapsed pool, setting
`MEMINI_DEFAULT_NAMESPACE` for a resolution mismatch, or `MEMINI_HOME` for the
missing personal leg). To let memini remediate a poisoned store itself, run
`memini doctor --fix` (preview) and then `memini doctor --fix --yes` to apply.
The fix chain also backfills nil confidence on legacy (pre-0.0.11) durable
memories, so they enter the demote lifecycle instead of staying permanently
trusted.
