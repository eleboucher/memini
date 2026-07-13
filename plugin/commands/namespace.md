---
description: Show, set, or clear memini's namespace pin for this project
argument-hint: "[<namespace> | --clear | --migrate]"
---

Run this in a shell and report the output to the user:

```
"${CLAUDE_PLUGIN_ROOT}/scripts/run.sh" "${CLAUDE_PLUGIN_ROOT}/scripts/namespace.mjs" $ARGUMENTS
```

- no arguments — show the current namespace, where it came from, and any pin
- `<namespace>` — **pin** this project to a namespace
- `--clear` — remove the pin and go back to automatic resolution
- `--migrate` — bulk-migrate every entry in the retired
  `~/.config/memini/overrides.json` to a server pin (see below)

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

## `--migrate`: bulk-migrating overrides.json

Most projects never need this: `SessionStart` already auto-migrates a matching
`overrides.json` entry to a pin the first time a project's handshake succeeds
and reports no pin. `--migrate` is the manual, bulk equivalent for every
project at once — useful right after upgrading, or on a machine that has not
opened an affected project in a while.

It reads **every** entry in `~/.config/memini/overrides.json`, PUTs a pin for
each (keyed by the stored path — a directory that no longer exists still
migrates fine, since no git command is re-run against it), and prints a table
of `key -> namespace -> status`, where status is `migrated`, `already-pinned`
(a pin already exists for that project — set later via `/memini:namespace`, so
it is left alone rather than overwritten), or `failed`. **Only on full success**
(no `failed` rows) is the file renamed to `overrides.json.migrated` — a partial
failure leaves it in place so a re-run can retry just the entries that did not
land.

It then separately checks `~/.config/memini/config.json` for the older
`tenantRoots`/`template` tenancy config a couple of other integrations still
read. That format cannot be auto-translated (it encodes a tenancy decision),
so it is printed with instructions to recreate it by hand: as `namespace_prefix`
on the relevant API keys (per-credential tenancy), or as per-project pins.

`config.json` is only ever **read** — never written. `overrides.json` is read
and, on full success, **renamed** (never rewritten in place or deleted): older
clients (opencode, hermes, openwebui) may still consult it during a staged
rollout, so its content is preserved verbatim under the new name rather than
touched or removed.

`/memini:status` reports the effective namespace, its source, and any active pin —
check there first when a project's namespace looks wrong.
