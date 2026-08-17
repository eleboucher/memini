# A namespace's whole life

We start a fresh server and follow one namespace — `acme/phoenix/api` — from
nonexistence to disappearance. There is no "create namespace" call anywhere in
memini, and this example shows why none is missing: a namespace exists exactly
while at least one memory row carries its string. The mechanism is laid out in
[how namespaces work](../how-it-works/namespaces.md); this page walks it.

Every request payload and response field quoted below is asserted by the Go
test in the footer. Response ids are illustrative; the other fields are the
asserted shapes.

## Stage 1: an empty server lists nothing

```http
GET /v1/namespaces
```

```json
{ "namespaces": [] }
```

Not "no namespaces created yet" — there is nothing to create. The listing is
the set of distinct namespace strings on stored rows, and there are no rows.

## Stage 2: the first write materializes the namespace

We write the first memory with the request namespace set to
`acme/phoenix/api` (the `X-Memini-Namespace` header over REST, the resolved
namespace under the plugin):

```json
{
  "content": "the api service reads feature flags from postgres",
  "tier": "semantic"
}
```

```json
{
  "id": "9f2c1a...   (illustrative)",
  "namespace": "acme/phoenix/api"
}
```

The listing now has exactly one entry:

```json
{ "namespaces": ["acme/phoenix/api"] }
```

That is the whole ceremony. Writing to a name that never existed just works —
which also means a typo'd namespace silently becomes a real one, the failure
mode pins and `memini doctor` exist to catch.

## Stage 3: ancestors are readable before they exist

`acme/phoenix` and `acme` hold no rows. Yet the child's default read set
already cascades through both, because ancestors are computed lexically from
the path — there is no existence check:

```http
GET /v1/namespaces/readset
X-Memini-Namespace: acme/phoenix/api
```

```json
{
  "entries": [
    { "namespace": "acme/phoenix/api", "origin": "primary" },
    { "namespace": "acme/phoenix", "origin": "ancestor", "tiers": ["semantic", "procedural"] },
    { "namespace": "acme", "origin": "ancestor", "tiers": ["semantic", "procedural"] }
  ]
}
```

The ancestor legs are restricted to the two durable tiers: only semantic and
procedural memories ever cross a namespace boundary. Searching those empty
legs costs a lookup that finds nothing — an ancestor becomes useful the
moment something lands in it, with no setup step in between.

## Stage 4: a team fact travels up

From the child, we share a fact with the whole team by naming the ancestor in
`visibility` — `"phoenix"` is the unambiguous last segment of
`acme/phoenix`, so the full path is not required:

```json
{
  "content": "the phoenix team deploys through forgejo actions",
  "tier": "semantic",
  "visibility": "phoenix"
}
```

```json
{ "namespace": "acme/phoenix" }
```

The write landed one level up, and `acme/phoenix` now exists — materialized
by a visibility write, not by any create call:

```json
{ "namespaces": ["acme/phoenix", "acme/phoenix/api"] }
```

Only durable tiers travel: had this been an episodic or working write, the
tier clamp would have kept it in `acme/phoenix/api` silently (see
[scoping](../scopes.md)).

## Stage 5: scope "everywhere" discovers the subtree

From the root of the tree, a search at scope `"everywhere"` expands the
primary namespace's subtree:

```json
{ "query": "feature flags postgres", "scope": "everywhere", "limit": 10 }
```

with `X-Memini-Namespace: acme`. The resolved read set is:

```json
{
  "entries": [
    { "namespace": "acme", "origin": "primary" },
    { "namespace": "acme/phoenix", "origin": "primary" },
    { "namespace": "acme/phoenix/api", "origin": "primary" }
  ]
}
```

Two things to notice. First, every subtree member is origin `"primary"` —
subtree expansion widens the primary leg, it is not a cascade leg of its own.
Second, the subtree contains only namespaces that actually have rows (plus
the primary itself, always): unlike the lexical ancestor cascade of stage 3,
subtree expansion consults the namespace listing, so there are no phantom
entries. The search surfaces the grandchild's fact from
`acme/phoenix/api`.

## Stage 6: delete the memories, and the namespace vanishes

We delete both memories:

```http
DELETE /v1/memories/{id}
```

and the listing is empty again:

```json
{ "namespaces": [] }
```

No delete-namespace bookkeeping ran, because there was never a namespace
object — "deleting a namespace" is deleting its memories, nothing more.

## Stage 7: pins, from the plugin's side

The stages above are the server's half. The client's half is deciding which
namespace to propose in the first place — normally derived from the
project's git remote, but pinnable per project:

```
/memini:namespace acme/phoenix/api

Pinned this project to acme/phoenix/api (was: memini, derived from git
remote). Hooks pick this up on their next invocation; run /reload-plugins so
the MCP tools reconnect and pick it up too.
```

Pins live on the server, keyed by the project's git remote and toplevel
path, so a pin follows you across machines and every client resolves the
same namespace from it. One caveat matters enough to repeat: hooks re-resolve
from the handshake immediately, but Claude Code wires the MCP tools' headers
only when the plugin connects — until `/reload-plugins`, the hooks and the
MCP tools can point at different namespaces, the split `memini doctor`
diagnoses. See [how the plugin works](../how-it-works/plugin.md) for the
mechanism.

The pin/derivation chain is covered by the client test suite, not re-tested
here.

## Validated by

- `TestExampleNamespaceLifecycle` in
  `internal/service/example_namespace_lifecycle_test.go` — stages 1-6: the
  empty listing, materialization on first write, the lexical ancestor read
  set, the visibility write landing up-tree, subtree discovery at scope
  "everywhere", and the listing emptying after the deletes.
- Stage 7's client-side resolution and pin precedence are pinned by
  `packages/memini-client/test/resolve.test.ts` and
  `packages/memini-client/test/derivation-vectors.test.ts`; the visibility
  resolution rules in depth by `internal/service/visibility_test.go`. The
  `/memini:namespace` transcript above is illustrative.
