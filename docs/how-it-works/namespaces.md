# Namespaces

**The short version.** A namespace exists exactly when at least one memory row carries its string — there is no create step, no registry, and no delete operation, only rows. The hierarchy is purely lexical: `acme` is an ancestor of `acme/phoenix/api` because it is a path prefix, nothing more. Clients propose a namespace through a precedence chain (pin, env, declared, derived from the project, key default, server default), the server is the authoritative resolver, and on the wire an explicit header beats a key's default — except for the home namespace, where a key binding beats the header, deliberately.

## Namespaces are never created

There is no "create namespace" call anywhere in memini, and none is missing. The set of namespaces is defined by the memories themselves: a namespace exists when at least one memory row carries the string, and the namespace listing is nothing more than the distinct namespace strings currently on rows. The consequences fall out directly:

- **Writing to any name just works.** The first memory written under `acme/phoenix/api` materializes the namespace; nothing needed to exist beforehand. This is also why a typo'd namespace "works" — `acme/phenix` silently becomes a real, separate namespace with one memory in it, which is the failure mode `memini doctor` and the pin mechanism exist to catch.
- **Namespaces vanish on their own.** Delete the last memory in a namespace and it disappears from the listing; there is no empty husk to clean up. "Deleting a namespace" means deleting its memories (the CLI and API expose exactly that), not removing an entry from a registry.
- **Repair is row surgery.** Moving a namespace (`memini namespace move`) rewrites the namespace string on its rows — see the [namespace lifecycle example](../examples/namespace-lifecycle.md) for the full arc from empty server to vanished namespace.

## Hierarchy is lexical

Nothing stores a parent pointer. Ancestors are derived by splitting the path on `/`: the ancestors of `acme/phoenix/api` are `acme/phoenix` and `acme`, in that order, whether or not any memory has ever been written to either. A read in `acme/phoenix/api` cascades to `acme/phoenix` and `acme` even when both are empty — searching an empty namespace just contributes nothing.

There is one observable asymmetry. Walking **up** the tree (the ancestor cascade) is pure string math and needs no existence check. Walking **down** (`scope:"everywhere"`, `/*` patterns, the briefing's child rollup) has to discover which descendants exist, and that discovery reads the real rows: a subtree expansion finds exactly the descendant namespaces that currently hold memories. You can read _from_ an ancestor that does not exist; you can only enumerate descendants that do. See [scopes.md](../scopes.md) for how both directions compose into a read set and [recall.md](./recall.md) for what happens to that read set at query time.

## How a client proposes a namespace

The client-side resolution chain, highest precedence first:

1. **Pin** — an operator-created project-to-namespace binding, stored on the server. Beats everything below, including the environment.
2. **`MEMINI_NAMESPACE`** — the client's environment override.
3. **Declared** — a namespace a gateway or CI caller states outright.
4. **Derived** — from the project itself: the git remote's repo name (or an `owner-repo` slug under the `namespace_scope=owner_repo` setting), else the git toplevel directory's basename, else the working directory's basename. Only a _derived_ name is then decorated: the configured namespace prefix is prepended and the `MEMINI_AGENT` segment appended. A pinned, env, or declared namespace is used verbatim — no prefix, no agent suffix.
5. **Key default** — the API key's bound default namespace.
6. **Server default** — the last resort.

Worked example — Jon works in `~/src/memini`, a clone of `git@github.com:jon/memini.git`, with a namespace prefix of `jon/dev` and `MEMINI_AGENT=reviewer` set for a review subagent:

| Situation                           | Resolved namespace        | Why                                                                          |
| ----------------------------------- | ------------------------- | ---------------------------------------------------------------------------- |
| nothing else set                    | `jon/dev/memini/reviewer` | derived from the remote (`memini`), prefix and agent applied                 |
| `MEMINI_NAMESPACE=scratch` exported | `scratch`                 | env wins over derivation — and is used verbatim, no prefix, no agent segment |
| a pin to `jon/dev/memini` exists    | `jon/dev/memini`          | the pin beats even the env — also verbatim                                   |
| not a git repo, run from `~/notes`  | `jon/dev/notes/reviewer`  | falls to the cwd basename, still decorated (it's derived)                    |

The prefix-and-agent rule is the part worth internalizing: decoration applies only when memini invented the name. The moment a human (or a pin) chose one, it is respected byte-for-byte. Per-agent trees built this way are covered in [multi-agent namespaces](../guides/multi-agent-namespaces.md).

## The handshake

Clients do not run this chain themselves. On session start, the plugin gathers the raw facts — remote URL, toplevel path, cwd basename, agent suffix, `MEMINI_NAMESPACE` if set — and sends them to the server's handshake endpoint, which applies the chain (it alone can see pins and key defaults) and returns the resolved namespace plus its source (`pin`, `env`, `declared`, `remote`, `toplevel`, `cwd`, `key_default`, `server_default`). Resolution is deterministic and side-effect-free: same facts, same answer.

Only when the server is unreachable does the client fall back to deriving locally, with the same ordering minus the legs only the server knows (pins, key default) — and every such fallback is marked degraded, because it is a guess the server has not confirmed. The hook-and-cache machinery around this — and how the MCP tools recover the project directory to hand the handshake the right facts — is in [the plugin doc](./plugin.md).

## Arbitration on the wire

Whatever a client resolved, each request carries it as a header, and the server arbitrates per request:

- **Namespace** (`X-Memini-Namespace`): the header wins; absent that, the authenticated key's default namespace; absent that, the server default. The namespace is _context_ — the caller picks it per request, and a key's default only fills the absence of a choice.
- **Home** (`X-Memini-Home`): the precedence is inverted. A key bound to a home namespace **always wins over the header** — the binding is _identity_, who the caller is, not a default to be overridden per request. The server does not overrule silently: when a bound key's home conflicts with a sent header, the response carries an `X-Memini-Warning` header saying which home was ignored and why, so nobody stares at the wrong namespace's memories wondering what happened.

This context-versus-identity asymmetry is deliberate and documented in [api-keys.md](../api-keys.md).

## Pins

A pin is the stored answer to "this project maps to that namespace". Pins live on the server, keyed two ways per project — by canonicalized git remote (`remote:github.com/jon/memini`) and by absolute toplevel path (`path:/home/jon/src/memini`) — so a pin survives both a folder move (the remote is stable) and a dropped remote (the path is stable). The remote key is preferred, which is why the same repo resolves to the same namespace on every machine and in every client.

Set, inspect, or clear one with `/memini:namespace` in the plugin (or the pins API directly). A pin outranks everything, including a globally exported `MEMINI_NAMESPACE` — deliberately, since a machine-wide env var would otherwise silently pin every repo to one namespace on exactly the machines that most need per-project pins.

> **After changing a pin, reconnect the MCP server.** Hooks re-resolve on
> their next invocation, but the MCP tools resolve their headers only when
> the connection is established — so until you run `/reload-plugins` (or
> restart the session), the hooks and the MCP tools write to _different
> namespaces_. This split-brain lasts the rest of the session if you skip
> the reload; `/memini:status` and `memini doctor` both surface it.

## What is a valid name

The server is deliberately permissive: a namespace is any string of 1 to 256 bytes containing no NUL byte. Spaces, unicode, punctuation, and any nesting depth are all fine — `équipe produit/expérience` is a legal namespace. The server refuses to have an opinion about naming taste.

Clients are stricter, for a mechanical reason: the namespace travels as an HTTP header, so the client rejects control characters (a CR or LF would split the header) and non-ASCII (header values would not round-trip byte-identically). If you want unicode namespaces, the REST body fields will take them; the header path will not.

Both transports normalize identically: leading and trailing whitespace and slashes are trimmed, and doubled separators collapse, so `work//memini/` addresses the same rows as `work/memini` whether it arrives over REST or MCP. (MCP trimmed only whitespace before v0.8 — rows written over MCP under a non-canonical header live in a sibling namespace after upgrading; `memini namespace move` is the remedy, see [upgrading](../operations/upgrading.md).)

## Where a write lands

A write always lands in exactly one namespace: the resolved primary, unless `visibility` routes a durable memory elsewhere (`"personal"` to the caller's home, or an ancestor's name up the chain). Episodic and working writes never leave the primary namespace regardless of `visibility` — the tier clamp is silent and by design, so session chatter cannot pollute a shared ancestor. The clamp, the ancestor-matching rules, and the error vocabulary are covered in [scopes.md](../scopes.md#data-flow-write); the full write pipeline is [the write path](./write-path.md).

## Source map

| Area                                                           | Where                                                                                                                                           |
| -------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------- |
| Namespace listing (exists iff a row carries the string)        | `internal/store/sqlitevec/store.go` — `ListNamespaces` (`SELECT DISTINCT namespace`)                                                            |
| Resolution chain, derivation, prefix/agent decoration          | `internal/nsresolve/resolve.go` — `Resolve`, `derive`, `applyPrefix`, `withAgent`                                                               |
| Pin keys (remote + toplevel path)                              | `internal/nsresolve/keys.go` — `PinKeys`                                                                                                        |
| Header arbitration, inverted home precedence                   | `internal/api/rest/middleware.go` — `namespaceMiddleware`, `homeMiddleware`; `internal/api/mcp/mcp.go` — `HTTPHandlerWithAuth`                  |
| Normalization and server-side validation                       | `internal/httputil/httputil.go` — `NormalizeNamespace`, `ValidateNamespace`, `HomeConflictWarning`                                              |
| Ancestor derivation and subtree discovery                      | `internal/service/readset.go` — `ancestorsOf`, `subtreeFrom`, `resolveDefaultReadSet`                                                           |
| Write placement and the tier clamp                             | `internal/service/visibility.go` — `resolveVisibility`                                                                                          |
| Client-side mirror of derivation (fixture-verified against Go) | `packages/memini-client/src/resolve.ts` — `deriveLocalNamespace`; shared fixture `packages/memini-client/test/fixtures/derivation-vectors.json` |
| Client-side (stricter) validation                              | `packages/memini-client/src/namespace-validate.ts`                                                                                              |
