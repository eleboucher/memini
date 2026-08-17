# Team sharing: two humans, one server

alice and bob share a memini server. alice works interactively with a normal
key; bob runs an unattended agent on a read-only key. This example shows how
named API keys, write visibility, and the read cascade compose into "private
by default, shared on purpose" — over plain REST, no plugin required. The key
semantics are documented in [API keys](../api-keys.md), the scoping rules in
[namespace scoping](../scopes.md).

Every payload, status code, and response field below is asserted by the Go
test in the footer. Ids are illustrative; the other fields are the asserted
shapes.

## The keys

Both keys are declared in `MEMINI_API_KEYS_FILE` (the API-managed key table
works identically):

```yaml
keys:
  - name: alice
    secret: "tok-alice"
    home: alice-home
    default_namespace: acme/phoenix/api
  - name: bob-agent
    secret: "tok-bob"
    default_namespace: acme/phoenix/api
    read_only: true
```

Both are bound to the same team default namespace, so neither caller sends a
namespace header — the key resolves it. alice additionally has a home
namespace; bob additionally has `read_only: true`.

## Stage 1: a personal write stays personal

alice saves a personal note:

```json
{
  "content": "alice keeps her personal vault notes in ~/vault",
  "tier": "semantic",
  "visibility": "personal"
}
```

```json
{
  "namespace": "alice-home",
  "metadata": { "author": "alice" }
}
```

`visibility: "personal"` routed the write to her key-bound home namespace,
not the team namespace, and the server stamped her key name as the author.
bob searches for it:

```json
{ "query": "personal vault notes", "limit": 10 }
```

```json
{ "results": [] }
```

bob's read set is `acme/phoenix/api` plus its ancestors; his key has no home
binding, so `alice-home` is simply not in any read set he can reach by
default.

## Stage 2: a team fact is shared on purpose

alice shares a fact with the whole team by naming the team ancestor —
`"phoenix"` is the unambiguous last segment of `acme/phoenix`:

```json
{
  "content": "the phoenix team ships from the release branch every tuesday",
  "tier": "semantic",
  "visibility": "phoenix"
}
```

```json
{ "namespace": "acme/phoenix" }
```

bob's next search sees it without doing anything: `acme/phoenix` is an
ancestor leg of his default (`full` scope) read set, and the hit says so via
`from` provenance:

```json
{
  "results": [
    {
      "memory": {
        "namespace": "acme/phoenix",
        "content": "the phoenix team ships from the release branch every tuesday"
      },
      "from": "acme/phoenix"
    }
  ]
}
```

## Stage 3: the read-only key cannot write

bob's agent misbehaves and attempts a write:

```json
{ "content": "bob tries to write", "tier": "semantic" }
```

```http
HTTP/1.1 403 Forbidden
```

```json
{
  "error": "read-only credential: API key \"bob-agent\" has read_only=true and cannot perform mutating requests"
}
```

The gate runs before the handler, so even a well-formed payload never
reaches validation, and the body names the credential and the reason —
`read_only` bounds what a key can change, not what it can see. bob's reads
(search, briefing, listing) are untouched.

## Stage 4: a link narrows, never widens

The team wants `acme/phoenix/api` readers to also see the shared
`ops/runbooks` namespace — which holds a semantic fact, a procedural how-to,
and an episodic entry. alice links the namespaces (`POST /v1/links`, a write,
so her key, not bob's):

```json
{ "dst": "ops/runbooks", "tiers": ["episodic"] }
```

This first attempt configures the link to carry only the episodic tier — and
bob's searches surface nothing from `ops/runbooks`. A link cannot widen past
the global rule that only durable tiers (semantic, procedural) cross a
namespace boundary; a tier list outside that set contributes nothing.

alice replaces the link with a real restriction:

```json
{ "dst": "ops/runbooks", "tiers": ["semantic"] }
```

A link with no `tiers` would carry the full durable default (semantic and
procedural); this one narrows it to semantic only. bob's search now surfaces
exactly the semantic fact, annotated as a link hit:

```json
{
  "results": [
    {
      "memory": {
        "namespace": "ops/runbooks",
        "content": "the incident runbook index lives in the ops wiki"
      },
      "from": "link:ops/runbooks"
    }
  ]
}
```

The procedural how-to is excluded by the link's tier restriction, and the
episodic entry is excluded by the global rule regardless of what the link
says. That is the whole contract: a link can narrow what crosses, and
nothing a link says can ever widen it.

## Validated by

- `TestExampleTeamSharing` in
  `internal/api/rest/example_team_sharing_test.go` — all four stages over
  the real REST handlers with file-declared keys: the personal write's
  landing namespace and invisibility to bob, the team fact's ancestor hit
  with `from` provenance, the exact 403 status and error body, and the
  link's narrow-never-widen behavior.
- The read-only gate's full endpoint matrix is pinned by
  `internal/api/rest/readonly_test.go`; key binding precedence by
  `internal/api/rest/apikey_test.go` and
  `internal/api/rest/filekey_test.go`; link tier intersection at the
  service layer by `internal/service/readset_test.go`.
