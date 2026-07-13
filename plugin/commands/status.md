---
description: Show memini's effective settings, resolved namespace, and read set
argument-hint: "[--json]"
---

Run this in a shell and report the output to the user:

```
"${CLAUDE_PLUGIN_ROOT}/scripts/run.sh" "${CLAUDE_PLUGIN_ROOT}/scripts/status.mjs" $ARGUMENTS
```

It is read-only. It runs a **live handshake** against the memini server and
reports, with provenance, what this plugin is actually doing right now — and
_why_:

- **NAMESPACE** — the effective namespace the server resolved and how
  (`server:pin`, `server:remote`, `env`, or a `local-…` degraded guess), the
  facts-only local fallback, and any active pin (its key, who set it, when).
- **IDENTITY** — the API key, its bound home, and default namespace.
- **CONNECTION** — base URL, redacted API key, and whether HTTPS is required.
- **SETTINGS** — every behavioral setting with its source: `env (overriding
server)`, `server (key|global|default)`, or `(default)`. This is the point:
  a plain list of values would look fine; the source column is what reveals a
  forgotten `MEMINI_*` export overriding what the server would otherwise send.
- **SERVER** — handshake ok / FAILED, latency, version.
- **READ SET** — the namespaces a plain recall draws from (probed live).
- **WARNINGS** — the findings.

Secrets are always redacted (`sk-…4f2a`), so the output is safe to paste into an
issue.

When reporting back:

- Lead with the namespace and any **WARNINGS**. Those are the findings; the rest
  is reference.
- Explain each warning plainly and give the suggested fix. Do not just echo the
  table.
- `global-namespace-pin` is the most consequential one: `MEMINI_NAMESPACE`
  overrides the server for every repo on this machine (unless that repo is
  pinned). Say so in those terms, and point at `/memini:namespace <ns>` (a
  server-side pin beats the environment).
- `degraded-mode` means the server was unreachable, so the namespace is a
  local-derived guess and every setting is a built-in default — recall and
  capture are both failing, not just slow.
- `cwd-split` means this command's directory and the directory the MCP tools
  resolve from have different git facts, so the hooks and the MCP tools may be
  targeting different namespaces.

Pass `--json` for machine-readable output (every setting's value + source, the
namespace provenance, and a `degraded` flag).
