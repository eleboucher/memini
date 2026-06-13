# Memory categories

Tiers (`working` / `episodic` / `semantic` / `procedural`) classify a memory by
_kind_ — how consolidated and durable it is. **Categories** are an orthogonal
_topic_ axis: what the memory is about. Together they let recall and browse
target a precise slice ("all `procedural` memories in the `deployment_runbook`
category").

Categories ride on the existing `metadata` field under the conventional key
`category`. Any string is allowed; the list below is the recommended set for
coding agents.

## Convention

When saving a memory, set `metadata.category` to one of the canonical values:

```jsonc
{
  "content": "auth middleware uses jose, not jsonwebtoken (Cloudflare Workers can't run native bindings)",
  "tier": "semantic",
  "metadata": { "category": "architecture_decisions" },
}
```

## Canonical categories

| category                 | use for                                                         |
| ------------------------ | --------------------------------------------------------------- |
| `architecture_decisions` | why a structural choice was made (and the rejected alternative) |
| `anti_patterns`          | approaches that failed here; what not to do again               |
| `task_learnings`         | non-obvious things learned completing a task                    |
| `tooling_setup`          | how the local/dev toolchain is configured                       |
| `bug_fixes`              | a bug's root cause and its fix                                  |
| `coding_conventions`     | code style, naming, file layout rules                           |
| `user_preferences`       | how the user wants you to work or respond                       |
| `dependency_decisions`   | why a library/version was chosen or pinned                      |
| `performance_findings`   | measured bottlenecks and optimizations                          |
| `security_constraints`   | auth, secrets, and threat-model rules                           |
| `testing_patterns`       | how this project tests, and the commands                        |
| `data_model`             | schema, entities, and their relationships                       |
| `api_contracts`          | endpoint shapes, request/response invariants                    |
| `deployment_runbook`     | how to ship, roll back, and operate                             |
| `team_norms`             | process, review, and workflow expectations                      |
| `domain_glossary`        | project-specific terms and their meaning                        |
| `experiment_results`     | outcomes of a tried approach or benchmark                       |

## Filtering by category

Because `category` lives in `metadata`, every filter surface that accepts a
metadata filter narrows by it.

MCP (`memory_recall` / `memory_list`):

```jsonc
{ "metadata": { "category": "bug_fixes" } }          // query-less browse via memory_list
{ "query": "auth race", "metadata": { "category": "bug_fixes" } }  // scoped recall
```

REST search:

```sh
curl -X POST "$MEMINI_URL/v1/search" \
  -H 'Content-Type: application/json' -H "X-Memini-Namespace: $NS" \
  -d '{"query":"auth","metadata":{"category":"bug_fixes"}}'
```

REST browse (`GET /v1/memories`) — repeatable `meta=key=value`:

```sh
curl "$MEMINI_URL/v1/memories?meta=category=bug_fixes&tier=semantic" \
  -H "X-Memini-Namespace: $NS"
```

CLI export of one category:

```sh
memini export --namespace "$NS" --meta category=bug_fixes
```

Tags and categories compose: tags are free-form keywords (AND-matched), while
`category` is a single conventional topic bucket. Filters across both are ANDed.
