---
description: Show, set, or clear memini's namespace pin for this project
argument-hint: "[<namespace> | --clear]"
---

Run this in a shell and report the output to the user:

```
"${CLAUDE_PLUGIN_ROOT}/scripts/run.sh" "${CLAUDE_PLUGIN_ROOT}/scripts/namespace.mjs" $ARGUMENTS
```

- no arguments — show the current namespace, where it came from, and any pin
- `<namespace>` — **pin** this project to a namespace
- `--clear` — remove the pin and go back to automatic resolution

**Pins live on the memini server**, keyed by the project's git remote and/or
toplevel path (`PUT`/`DELETE /v1/pins`). Because they are server-side, a pin
**follows you across machines** and every client — the hooks, the MCP tools, and
the `memini` CLI — resolves the same namespace from it. There is no per-machine
file anymore.

A pin **beats `MEMINI_NAMESPACE`**. That ordering is deliberate: a globally
exported `MEMINI_NAMESPACE` (a shell rc, or a fish universal variable) pins every
repo on the machine to one namespace, and if the environment won, this command
would silently do nothing on exactly the machines that most need it.

**Two things to tell the user after setting or clearing a pin:**

1. **The hooks pick it up on their next invocation.** Session digests, turn
   capture, and recall injection all re-resolve from the server handshake, whose
   per-session cache is invalidated the moment the pin changes.

2. **The MCP tools need `/reload-plugins`.** Claude Code runs the MCP
   `headersHelper` only when the server _connects_, so `memory_remember` and
   `memory_recall` keep targeting the old namespace until the plugin reconnects.
   Until then the hooks and the MCP tools point at different namespaces — the
   split that makes memory half-work. **Tell the user to run `/reload-plugins`** —
   do not let this go unmentioned.

Scope is the **project** (its git remote / toplevel), not the session. Two
sessions open in the same repo share the pin — the MCP `headersHelper` is given
no session id, so per-project is the honest granularity.

**Server must be reachable to pin.** Pins are stored server-side, so setting or
clearing one requires the memini server. When it is unreachable the command says
so and points you at the offline escape hatch: export `MEMINI_NAMESPACE=<ns>` for
a machine-local override until the server is back.

`/memini:status` reports the effective namespace, its source, and any active pin —
check there first when a project's namespace looks wrong.
