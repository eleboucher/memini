---
description: Show memini's effective settings, resolved namespace, and read set
argument-hint: "[--json]"
---

Run this in a shell and report the output to the user:

```
"${CLAUDE_PLUGIN_ROOT}/scripts/run.sh" "${CLAUDE_PLUGIN_ROOT}/scripts/status.mjs" $ARGUMENTS
```

It is read-only. It answers "what is this plugin actually doing right now?" —
and, more usefully, _why_. Three things have to agree for memory to work:

1. the namespace the **hooks** write to (session digests, turn capture),
2. the namespace the **MCP tools** write to (`memory_remember` / `memory_recall`),
3. the namespace the **server** reads from when it assembles a read set.

When the first two diverge, memory silently half-works: the agent saves things it
can never recall. `status` reports that split directly.

Every setting carries its **provenance** — `<- env` versus `(default)` — which is
the whole point. A plain list of values would show `namespace: default` and look
fine; what catches the real problem is the line underneath saying git would have
given `memini`. That is how a forgotten `MEMINI_NAMESPACE` export — a shell rc, or
a fish _universal_ variable set months ago — gets caught, quietly collapsing every
repo on the machine into one shared memory pool.

Secrets are always redacted (`sk-…4f2a`), so the output is safe to paste into an
issue.

When reporting back:

- Lead with the namespace and any **WARNINGS**. Those are the findings; the rest
  is reference.
- Explain each warning plainly and give the suggested fix. Do not just echo the
  table.
- `global-namespace-pin` is the most consequential one: it means every project on
  the machine shares a namespace. Say so in those terms.
- If it reports `namespace-split`, the hooks and the MCP tools are targeting
  different namespaces — memory is actively broken. Treat it as urgent.
- `server-unreachable` means recall and capture are both failing, not just slow.

Pass `--json` for machine-readable output.
