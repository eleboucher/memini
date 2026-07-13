---
name: memory
description: >-
  Persistent long-term memory via memini. Use to remember durable facts,
  decisions, and preferences, and to recall relevant context before acting.
  Triggers when the user says "remember", "what do you know about", or when
  starting a task that may have prior context.
---

# memory (memini)

memini is a memory service.

**If the `memory_briefing` / `memory_recall` / `memory_remember` / `memory_list` /
`memory_forget` tools are in your tool list, use them and ignore the rest of this
file.** They take the same `scope` and `visibility` arguments described below,
and they resolve the namespace for you. The curl recipes here are the fallback
for installs that run memini as a skill _without_ the plugin — see
[the README's Alternatives](../../README.md#alternatives) — where there is no
tool to call.

Do not mix the two. The plugin namespaces memory per agent by default
(`{namespace}-{agent}`), and the curl calls below cannot see that — they send the
base `$MEMINI_NAMESPACE`, so a fact written by curl lands somewhere the plugin's
own recall will never look. If the plugin is installed but its tools are not
exposed, ask the operator to set `expose_tools: true` rather than reaching for
curl.

The rest of this file assumes the no-plugin install.

The service is reachable at `${MEMINI_BASE_URL:-${MEMINI_URL:-http://localhost:8080}}`.
If a bearer token is configured, send `Authorization: Bearer $MEMINI_API_KEY`.

**Namespaces are managed for you.** Every request carries the
`X-Memini-Namespace` header, whose value is `$MEMINI_NAMESPACE` — configuration
your operator exports, not something you choose. Never invent a namespace, and
never type a raw namespace path into a request. You make two semantic choices
instead — `scope` when you read, `visibility` when you write — and you learn the
topology by reading what comes back. (With `MEMINI_NAMESPACE` unset the header is
empty and the server falls back to its configured default namespace,
`MEMINI_DEFAULT_NAMESPACE` — see `docs/reference/configuration.md`.)

Export it once, then use `$NS` in every call below:

```sh
NS="$MEMINI_NAMESPACE"
```

## Orient first: the briefing

At the start of a task, get the session briefing — pinned context, durable facts,
how-to procedures, and recent activity, in one query-less call. Prefer it over a
broad recall query:

```sh
curl -sf "${MEMINI_BASE_URL:-$MEMINI_URL}/v1/namespaces/briefing" \
  -H "X-Memini-Namespace: $NS"
```

The response's `scope_header` is the line to read — e.g.
`Scope: acme/phoenix/api ← acme/phoenix(3) ← acme(4) ← personal(2)`. It spells
out the ancestor chain this namespace inherits from. That is where the ancestor
names you may pass as `visibility` come from; do not guess them.

## Recall before acting

```sh
curl -sf -X POST "${MEMINI_BASE_URL:-$MEMINI_URL}/v1/search" \
  -H 'Content-Type: application/json' \
  -H "X-Memini-Namespace: $NS" \
  -d '{"query":"<what you are about to work on>","limit":5}'
```

`scope` is the only lever on how wide you read:

- `"project"` — just this project's own memories.
- `"full"` (default) — project plus inherited context: ancestors, the user's
  personal namespace, and links.
- `"everywhere"` — `full` plus nested sub-projects.

```sh
curl -sf -X POST "${MEMINI_BASE_URL:-$MEMINI_URL}/v1/search" \
  -H 'Content-Type: application/json' \
  -H "X-Memini-Namespace: $NS" \
  -d '{"query":"deploy window","scope":"everywhere"}'
```

Read the returned `results[].memory.content` and factor them into your plan. Each
result carries **provenance, not a choice**: an absent `from` means the memory is
this project's own; otherwise `from` names the ancestor or personal namespace it
came from (`"acme"`, `"personal/kit"`, or `"link:shared/golang"`). Read `from`
and the briefing's Scope line to learn where knowledge actually lives — never
construct a namespace path from them.

`results[].memory` also carries `created_at` and `tags` — on conflicting memories
prefer the most recent and surface the conflict. A top-level
`degraded: "keyword_only"` (with a `note`) means the query embed was unavailable
and the results came from keyword matching alone; treat them as incomplete, not
exhaustive. Empty results mean nothing is known — proceed from first principles,
never invent a remembered fact.

Narrow recall with `tags` (a memory must carry every listed tag) and/or
`metadata` (top-level key=value pairs), e.g. only auth bug-fix memories:

```sh
curl -sf -X POST "${MEMINI_BASE_URL:-$MEMINI_URL}/v1/search" \
  -H 'Content-Type: application/json' \
  -H "X-Memini-Namespace: $NS" \
  -d '{"query":"auth","tags":["auth"],"metadata":{"category":"bug_fixes"}}'
```

Two more levers worth knowing: `"query_rewrite":true` expands the query into 2-3
variants and fuses the results (slower, better recall — reach for it when a first
attempt comes back thin), and `"as_of":"<RFC3339>"` is time-travel recall,
returning the facts that were true at that instant, including ones since
superseded.

## Browse without a query

To list memories by tier/tag/category instead of searching — "show me everything
categorized `bug_fixes`", "all procedural memories" — use `GET /v1/memories` with
repeatable `tag` and `meta=key=value` filters:

```sh
curl -sf "${MEMINI_BASE_URL:-$MEMINI_URL}/v1/memories?tier=procedural&meta=category=bug_fixes" \
  -H "X-Memini-Namespace: $NS"
```

## Remember durable facts

```sh
curl -sf -X POST "${MEMINI_BASE_URL:-$MEMINI_URL}/v1/memories" \
  -H 'Content-Type: application/json' \
  -H "X-Memini-Namespace: $NS" \
  -d '{"content":"<the fact to remember>","tier":"semantic","metadata":{"category":"architecture_decisions"}}'
```

Use `tier: "semantic"` for durable knowledge, `procedural` for how-to steps,
`episodic` for what happened, `working` for transient notes. Omit `tier` to let
the server classify from the content.

`visibility` decides who should know the fact:

- `"project"` (default) — stays in this project.
- `"personal"` — routes to the user's own namespace and follows them everywhere.
  Use it for preferences about the user, not about the code.
- an **ancestor name read off the briefing's Scope line** (e.g. the team or org
  level) — shares the fact up that chain. An unrecognized name is rejected with
  an error listing the valid options, which is how you learn the chain; do not
  guess.

```sh
curl -sf -X POST "${MEMINI_BASE_URL:-$MEMINI_URL}/v1/memories" \
  -H 'Content-Type: application/json' \
  -H "X-Memini-Namespace: $NS" \
  -d '{"content":"the org bills through Stripe","tier":"semantic","visibility":"acme"}'
```

Episodic and working writes always stay in this project regardless of
`visibility`, so session detail can never leak into a shared ancestor or the
user's personal namespace.

Set `metadata.category` to a topic bucket so the memory can be browsed and
filtered by subject later. Any string is accepted, but pick a canonical value:
`architecture_decisions`, `anti_patterns`, `task_learnings`, `tooling_setup`,
`bug_fixes`, `coding_conventions`, `user_preferences`, `dependency_decisions`,
`performance_findings`, `security_constraints`, `testing_patterns`, `data_model`,
`api_contracts`, `deployment_runbook`, `team_norms`, `domain_glossary`,
`experiment_results`.

The response may carry `reinforced: true` — the fact was already known, no new
memory was created, and `id` names the pre-existing memory that got strengthened.
Don't report that to the user as a new save.

## Guidance

- Brief at the start of a goal loop; recall before work that may have history;
  remember when you learn something durable.
- Keep memories atomic (one fact each) so dedup/consolidation works well.
- Durable facts worth sharing go up the chain, personal preferences go personal,
  and session detail stays in the project.
