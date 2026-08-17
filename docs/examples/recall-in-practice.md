# Recall in practice: one question, three scopes

The same query returns different result sets depending on `scope`, and the
differences are the whole point of the read-set design: `project` is
isolation, `full` is inheritance, `everywhere` is inheritance plus the
subtree. This example seeds a small tree, asks one question three times, and
reads the answers. The ranking and read-set mechanics behind it are in
[how recall works](../how-it-works/recall.md).

Every payload and response field quoted below is asserted by the Go test in
the footer. Ids are illustrative; the other fields are the asserted shapes.

## The setup

Our working namespace is `acme/phoenix/api`, and the caller's home namespace
(`X-Memini-Home` / `MEMINI_HOME`) is `jon-personal`. We seed four namespaces:

| Namespace                 | Content                                                           | Tier       |
| ------------------------- | ----------------------------------------------------------------- | ---------- |
| `acme/phoenix/api`        | "the api service reads feature flags from postgres"               | `semantic` |
| `acme/phoenix`            | "the phoenix team standardized on postgres 16 for every service"  | `semantic` |
| `acme/phoenix`            | "migrated the shared postgres cluster to new hardware on tuesday" | `episodic` |
| `jon-personal`            | "jon prefers postgres over mysql for side projects"               | `semantic` |
| `acme/phoenix/api/worker` | "the worker drains the postgres outbox table every minute"        | `semantic` |

Note the ancestor holds two memories — a durable fact and an episodic event —
and the fifth fact lives in a child of the working namespace, one level
deeper.

## Scope "project": just this namespace

```json
{ "query": "postgres", "scope": "project", "limit": 10 }
```

```json
{
  "results": [
    {
      "memory": {
        "namespace": "acme/phoenix/api",
        "content": "the api service reads feature flags from postgres"
      }
    }
  ]
}
```

One hit. No ancestors, no home, no links — the request namespace and nothing
else. This is the scope for "what does this project itself know".

## Scope "full": the default cascade

```json
{ "query": "postgres", "scope": "full", "limit": 10 }
```

Three hits now — the project fact plus the ancestor's durable fact and the
home namespace's preference, each annotated with `from` provenance saying
which read-set leg produced it (empty for the primary namespace):

```json
{
  "results": [
    { "memory": { "namespace": "acme/phoenix/api" } },
    { "memory": { "namespace": "acme/phoenix" }, "from": "acme/phoenix" },
    { "memory": { "namespace": "jon-personal" }, "from": "jon-personal" }
  ]
}
```

What is NOT here matters as much as what is:

- **The ancestor's episodic event is absent.** Non-primary read-set legs are
  durable-only — only semantic and procedural memories cross a namespace
  boundary. The migration event is reachable only when `acme/phoenix` is the
  request namespace itself.
- **The child's fact is absent.** `full` cascades up (ancestors) and
  sideways (home, links), never down. Subtrees are opt-in.

## Scope "everywhere": full plus the subtree

```json
{ "query": "postgres", "scope": "everywhere", "limit": 10 }
```

Four hits: the three from `full`, plus the child's fact. `everywhere` expands
the subtree of the PRIMARY namespace — everything nested under
`acme/phoenix/api` — so `acme/phoenix/api/worker` joins. A sibling such as
`acme/phoenix/other` would not: the subtree grows under the request
namespace, not under its parent.

```json
{
  "results": [{ "memory": { "namespace": "acme/phoenix/api/worker" } }]
}
```

(Only the new hit shown.) Subtree members join as part of the primary leg,
so they carry no `from` annotation — `memory.namespace` is where you see
which child a hit came from.

## The briefing for the same namespace

A session-start briefing is the query-less sibling of `full`-scope recall: it
reads the same cascade, buckets by tier, and reports which legs contributed
via a one-line scope header:

```json
{
  "namespace": "acme/phoenix/api",
  "scope_header": "Scope: acme/phoenix/api ← acme/phoenix(1) ← jon-personal(1)",
  "facts": ["... the 3 durable facts, across primary + ancestor + home ..."],
  "recent": [],
  "children": [{ "namespace": "acme/phoenix/api/worker", "total": 1 }]
}
```

- `facts` holds all three durable facts; `recent` is empty because the
  ancestor's episodic event does not cascade, same rule as recall.
- The scope header lists only legs that contributed durable memories:
  `acme` is a real ancestor leg but held nothing, so it is omitted.
- `children` is a rollup of direct child namespaces — the subtree is not
  searched, but you are told it exists.

## Recall reinforces; briefings do not

An explicit recall is a relevance signal: every served memory's access count
goes up, which feeds promotion and durable ranking. A briefing is not — it
runs at every session start, and counting it would inflate every memory's
apparent usefulness just for existing. The test asserts both directions: the
project fact's `access_count` grows across the three recalls above, then
stays exactly flat through the briefing.

Both still appear in the activity feed — the split is log-everything,
reinforce-only-real-signals.

## Where the plugin plugs in

Under the Claude Code plugin these calls are not typed by anyone: the
briefing is injected at session start, `full`-scope recall runs per prompt,
and a stricter pre-tool recall gates on relevance. See
[how recall works](../how-it-works/recall.md) for budgets and ranking, and
[how the plugin works](../how-it-works/plugin.md) for the three injection
surfaces.

## Validated by

- `TestExampleRecallScopes` in
  `internal/service/example_recall_scopes_test.go` — the three scoped result
  sets, the `from` provenance values, the briefing sections and exact scope
  header, and the reinforce-vs-not access-count asymmetry.
- The briefing-never-reinforces invariant is pinned in depth by
  `TestBriefingAndGetLogButDoNotReinforce` in
  `internal/service/events_test.go`; scope threading on the answer path by
  `internal/service/answer_test.go`.
