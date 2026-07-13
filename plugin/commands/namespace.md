---
description: Show, set, or clear memini's namespace override for this project
argument-hint: "[<namespace> | --clear]"
---

Run this in a shell and report the output to the user:

```
"${CLAUDE_PLUGIN_ROOT}/scripts/run.sh" "${CLAUDE_PLUGIN_ROOT}/scripts/namespace.mjs" $ARGUMENTS
```

- no arguments — show the current namespace and where it came from
- `<namespace>` — override the namespace for this project
- `--clear` — remove the override and go back to automatic resolution

The override **beats `MEMINI_NAMESPACE`**. That ordering is deliberate: a globally
exported `MEMINI_NAMESPACE` pins every repo on the machine to one namespace, and if
the environment won, this command would silently do nothing on exactly the machines
that most need it.

**Two things to tell the user after setting or clearing an override:**

1. **The hooks pick it up immediately.** Session digests, turn capture, and recall
   injection all re-resolve on every invocation.

2. **The MCP tools do not — they need `/reload-plugins`.** Claude Code runs the MCP
   `headersHelper` only when the server _connects_, so `memory_remember` and
   `memory_recall` keep targeting the old namespace until the plugin reconnects.
   Until then the hooks and the MCP tools are pointed at different namespaces, which
   is precisely the split that makes memory half-work. **Tell the user to run
   `/reload-plugins`** — do not let this go unmentioned.

Scope is the **project** (keyed by the git repository root), not the session. Two
sessions open in the same repo share the override. This is a real limitation rather
than an oversight: the MCP `headersHelper` is given no session id, so it cannot tell
two sessions in one repo apart, and pretending otherwise would produce a setting
that appears to work and quietly does not.

The override is stored in `~/.config/memini/overrides.json` and persists across
sessions until cleared, so if a project's namespace looks wrong later, check here
first — `/memini:status` reports it too.
