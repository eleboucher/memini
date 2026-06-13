---
name: memory
description: >-
  Persistent long-term memory via memini. Use to remember durable facts,
  decisions, and preferences, and to recall relevant context before acting.
  Triggers when the user says "remember", "what do you know about", or when
  starting a task that may have prior context.
---

# memory (memini)

memini is a memory service reachable at `${MEMINI_URL:-http://localhost:8080}`.
All requests are scoped by the `X-Memini-Namespace` header. Set it to a
project name (e.g. `acme-web`) or to `${MEMINI_NS}` from the gateway env.
If the header is omitted, the server falls back to the git-repo basename of
its own working directory — see the top-level README's "Namespace
resolution" section. If a bearer token is configured, send `Authorization:
Bearer $MEMINI_TOKEN`.

## Recall before acting

```sh
curl -sf -X POST "$MEMINI_URL/v1/search" \
  -H 'Content-Type: application/json' \
  -H "X-Memini-Namespace: $MEMINI_NS" \
  -d '{"query":"<what you are about to work on>","limit":5}'
```

Read the returned `results[].memory.content` and factor them into your plan.

Narrow recall with `tags` (a memory must carry every listed tag) and/or
`metadata` (top-level key=value pairs), e.g. only auth bug-fix memories:

```sh
curl -sf -X POST "$MEMINI_URL/v1/search" \
  -H 'Content-Type: application/json' \
  -H "X-Memini-Namespace: $MEMINI_NS" \
  -d '{"query":"auth","tags":["auth"],"metadata":{"category":"bug_fixes"}}'
```

## Browse without a query

To list memories by tier/tag/category instead of searching — "show me
everything categorized `bug_fixes`", "all procedural memories" — use
`GET /v1/memories` with repeatable `tag` and `meta=key=value` filters:

```sh
curl -sf "$MEMINI_URL/v1/memories?tier=procedural&meta=category=bug_fixes" \
  -H "X-Memini-Namespace: $MEMINI_NS"
```

## Remember durable facts

```sh
curl -sf -X POST "$MEMINI_URL/v1/memories" \
  -H 'Content-Type: application/json' \
  -H "X-Memini-Namespace: $MEMINI_NS" \
  -d '{"content":"<the fact to remember>","tier":"semantic","metadata":{"category":"architecture_decisions"}}'
```

Use `tier: "semantic"` for durable knowledge, `procedural` for how-to steps,
`episodic` for what happened, `working` for transient notes. Set
`metadata.category` to a canonical topic bucket (see `docs/categories.md`, e.g.
`bug_fixes`, `architecture_decisions`, `coding_conventions`) so the memory can
be browsed and filtered by subject later.

## Guidance

- Recall at the start of a goal loop; remember when you learn something durable.
- Keep memories atomic (one fact each) so dedup/consolidation works well.
