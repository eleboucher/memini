# Namespace scoping

Namespaces are the slash-separated tree memini partitions memories into
(`acme/phoenix/api`, `acme/phoenix`, `acme`). **Scoping** is the separate
question of, on a write, which single namespace a memory lands in, and on a
read, which set of namespaces get searched. A write always lands in exactly
one namespace — that invariant never changes. A read composes a **read
set**: the request's own namespace (always first, never filtered out) plus,
by default, its ancestors, the caller's personal namespace, and any linked
namespaces, fused into one ranked result list annotated with where each hit
came from.

Scoping is orthogonal to [tiers](tiers.md) (how durable a memory is — tiers
decide whether a memory is even eligible to cross a namespace boundary) and
[categories](categories.md) (what topic a memory is about). This doc covers
the mechanics of the write and read paths, who's responsible for resolving
what, and the escape hatches available when the defaults aren't enough.

## Data flow: write

`memory_remember` (and `POST /v1/memories`) always writes into the caller's
primary namespace unless `visibility` says otherwise. `visibility` accepts:

- `"project"` (default, or omitted) — stays in the primary namespace.
- `"personal"` — routes to the caller's home namespace (`X-Memini-Home` /
  `MEMINI_HOME`); errors if none is configured.
- an ancestor's name — full path (`"acme/phoenix"`) or an unambiguous last
  segment (`"acme"`) — routes to that ancestor.

**Tier clamp:** episodic and working writes always land in the primary
namespace, regardless of `visibility`. Only durable tiers (`semantic`,
`procedural`) actually travel. This is silent and by design — a session
digest can never pollute a shared ancestor namespace, and the clamp is
checked before `visibility` is even validated, so a clamped write never
requires `MEMINI_HOME` to be set.

Worked example — a coding session running in `acme/phoenix/api`:

| Call                                                           | Resolved tier            | `visibility` | Lands in                     | Why                                                                                        |
| -------------------------------------------------------------- | ------------------------ | ------------ | ---------------------------- | ------------------------------------------------------------------------------------------ |
| `memory_remember(content, visibility:"acme")`                  | `semantic` (durable)     | `"acme"`     | `acme`                       | exact ancestor match; durable tiers travel                                                 |
| `memory_remember(content, tier:"episodic", visibility:"acme")` | `episodic` (non-durable) | `"acme"`     | `acme/phoenix/api` (primary) | **tier clamp**: episodic/working never leave primary, silently, regardless of `visibility` |
| `memory_remember(content, visibility:"personal")`              | `semantic`               | `"personal"` | `personal/kit`               | requires `MEMINI_HOME=personal/kit` on the client; errors without it                       |
| `memory_remember(content, visibility:"widgets")`               | `semantic`               | `"widgets"`  | error                        | `"widgets"` isn't `project`, `personal`, or an ancestor of `acme/phoenix/api`              |

The last row's actual error text (from `internal/service/visibility.go`,
`resolveVisibility`) enumerates the valid chain so the caller — usually an
LLM reading the error, not a human — can learn the topology instead of
guessing:

```
remember: visibility "widgets" not in scope; valid: project, personal, acme/phoenix, acme
```

An ancestor name that matches more than one chain segment (e.g. two
namespaces both ending in `/mid`) errors the same way — ambiguous is treated
like invalid, never guessed.

## Data flow: read

`memory_recall` (and `memory_briefing`, `memory_answer`) take a `scope`
argument. The default, `scope:"full"`, resolves a read set from the
ancestor/home/link cascade. A `memory_recall(query, scope:"full")` call
running in `acme/phoenix/api`, with `MEMINI_HOME=personal/kit` and a stored
link `acme/phoenix -> shared/golang`, resolves to:

| namespace          | origin   | tiers searched                                                                                            |
| ------------------ | -------- | --------------------------------------------------------------------------------------------------------- |
| `acme/phoenix/api` | primary  | the request's own tier filter (all, unless narrowed)                                                      |
| `acme/phoenix`     | ancestor | durable only (`semantic`, `procedural`)                                                                   |
| `acme`             | ancestor | durable only                                                                                              |
| `personal/kit`     | home     | durable only                                                                                              |
| `shared/golang`    | link     | durable only (or the link's own tighter override — a link can narrow within durable, never widen past it) |

Ancestors are appended nearest-first (`acme/phoenix` before `acme`) — this
ordering is load-bearing, not cosmetic (see the tie-break note below).

**Fusion.** Each namespace in the read set is searched with both vector and
keyword search; per-namespace results are combined, then the per-namespace
lists are fused into one ranked list. The default fusion mode is a weighted,
min-max-normalized score sum across legs (a reciprocal-rank-fusion mode also
exists as a benchmark-tuned internal alternative, not exposed as a runtime
setting). Ties at equal fused score are broken by first-seen order across
the read set, which is exactly why ancestor ordering is nearest-first: at
equal relevance, a memory in `acme/phoenix` outranks one in `acme`.

**Provenance.** Every result carries a `from` field naming where it came
from — empty/omitted for a primary-namespace hit (the common case, no
annotation needed), the bare namespace for an ancestor or home hit, and a
prefixed form for a link or explicit per-call namespace:

```jsonc
// primary namespace — no "from" at all
{ "id": "m1", "content": "the deploy window is 3am UTC", "tier": "semantic", "namespace": "acme/phoenix/api" }

// ancestor
{ "id": "m2", "content": "org billing runs on Stripe", "tier": "semantic", "namespace": "acme", "from": "acme" }

// caller's home namespace
{ "id": "m3", "content": "kit prefers terse commit messages", "tier": "semantic", "namespace": "personal/kit", "from": "personal/kit" }

// a stored link
{ "id": "m4", "content": "internal Go style: errors wrapped with %w", "tier": "procedural", "namespace": "shared/golang", "from": "link:shared/golang" }
```

The LLM is expected to learn the topology by reading `from` (and the
briefing `Scope:` header), never by constructing a namespace path itself.

**Other `scope` values:**

- `scope:"project"` — primary only, no cascade at all (the `"bare"` path;
  skips ancestors, home, and links entirely).
- `scope:"everywhere"` — the full cascade, plus the primary namespace's own
  subtree (its descendant namespaces). Subtree members are treated as an
  extension of the _primary_ namespace, not a separate cascade leg, so they
  are searched at the caller's full tier filter — not clamped to durable —
  unlike every other cascade leg. This is the one place tiers are not the
  isolation valve; it's an explicit per-call downward reach, never on by
  default.

## Isolation contract

| Relationship                   | What crosses                                                                                                                                    | Effort                                   |
| ------------------------------ | ----------------------------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------- |
| Within a namespace             | everything, all tiers                                                                                                                           | —                                        |
| Child → ancestor (up)          | durable tiers only, read-only                                                                                                                   | zero (always on, part of `scope:"full"`) |
| Ancestor → descendant (down)   | briefing's child rollup (titles/counts only); full recall needs per-call `scope:"everywhere"` (all tiers, since subtree is primary's own reach) | zero / per-call                          |
| Sibling ↔ sibling              | nothing, unless linked                                                                                                                          | one command (`memini link add`)          |
| Caller → own home              | durable tiers only, read-only                                                                                                                   | zero (client sets `MEMINI_HOME` once)    |
| Caller → another person's home | nothing                                                                                                                                         | not expressible                          |
| Any write                      | never crosses by default; `visibility` moves a durable write up the ancestor chain                                                              | one param                                |

## Ownership

Scoping is split cleanly across three layers, and each only does its own
job:

| Layer                            | Owns                                                                                                                                                                                      |
| -------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| **Client** (plugin/hook script)  | Resolving `MEMINI_NAMESPACE` and `MEMINI_HOME` from env/config, and sending them on every call as `X-Memini-Namespace` / `X-Memini-Home`                                                  |
| **Server** (`internal/service`)  | Cascade resolution (ancestors, home, links), `visibility` resolution and the tier clamp, `scope` parsing, read-set fusion                                                                 |
| **Store** (sqlitevec / postgres) | Partitioning memory rows by namespace string only — no tree logic, no cascade awareness; `namespace_links` is just a table of `(src, dst, tiers, note)` rows the service layer interprets |

The store never knows a namespace has a parent, a home, or a link — it just
filters by the namespace string(s) the service layer hands it.

## Links

Links are the escape hatch for **lateral** sharing — between namespaces that
aren't in an ancestor/descendant relationship, like two unrelated projects
that share a language convention. Where the ancestor cascade is implicit
(nesting under a shared parent is enough), a link is explicit: one command,
one row.

- **One hop, permanently.** A link from `acme/phoenix` to `shared/golang`
  means `acme/phoenix` reads `shared/golang`'s durable memories. It does
  _not_ mean `acme/phoenix` also inherits anything `shared/golang` itself
  links to — no transitive traversal, so provenance stays answerable with
  one lookup.
- **Durable-tier only by default**, same rule as everywhere else; a link can
  narrow further (e.g. `--tiers semantic`) but never admit `episodic` or
  `working`.

CLI:

```sh
memini link add acme/phoenix shared/golang --tiers semantic,procedural --note "shared Go conventions"
memini link rm acme/phoenix shared/golang
memini link ls acme/phoenix   # or `memini link ls` for every link in the store
```

REST: `POST /v1/links` (create/replace, keyed on `dst` — idempotent),
`GET /v1/links` (outgoing links from the request namespace), `DELETE
/v1/links?dst=<ns>`. All are scoped by the `X-Memini-Namespace` header as
`src`, same as recall/briefing.

## Escape hatches

- **Per-call `namespaces: [...]`** (REST `POST /v1/search`, `POST
/v1/namespaces/briefing`): replaces the entire default read set with
  exactly the listed namespaces, first-occurrence order, primary moved to
  the front if present. An entry ending in `/*` expands to that namespace
  plus everything nested under it. Capped at 16 entries. Explicit always
  wins over `scope`/`bare`/`subtree`, regardless of value.
- **`scope` values** — `"project"` / `"full"` / `"everywhere"` — are the
  main per-call lever on the MCP surface (see above).
- **REST `scope` back-compat aliases**: `"exact"` and `"subtree"` are
  deprecated but still accepted on the REST API (not MCP) for older
  integrations — `"exact"` maps to `"project"` (its original,
  pre-cascade meaning), `"subtree"` maps to `"everywhere"` (the full
  cascade plus the subtree, since every scope now inherits the cascade
  legs that didn't exist when `"subtree"` was coined). New integrations
  should use `"project"` / `"full"` / `"everywhere"` directly.
- **Raw `namespace`** stays on the REST API (for scripts and the admin UI)
  and is dropped from the MCP tool schema — the LLM never sees or
  constructs a raw namespace path, only `scope` and `visibility`.
- **`GET /v1/namespaces/read-set`** — header-scoped like briefing, returns
  the resolved structural read set (namespace + origin + tier restriction)
  for the request namespace, independent of any one query's tier filter.
  Useful for debugging what a given `MEMINI_NAMESPACE` / `MEMINI_HOME` pair
  would actually see. `memini doctor` uses the same resolution to show the
  effective read set on the command line.

## Agent segments: an automatic upgrade

`MEMINI_AGENT` nests a per-agent namespace under the project (e.g.
`acme/phoenix` becomes `acme/phoenix/reviewer` for a reviewer subagent). A
reviewer namespace is now a plain child in the tree, so it automatically
inherits `acme/phoenix`'s durable memories via the ancestor cascade on every
`scope:"full"` recall/briefing — no `MEMINI_TENANT_SHARED`, no `_shared`
sibling, no configuration at all. This is a behavior improvement over the
old model, where a depth-1-only tenant-shared merge didn't reach a
second-level nest like this: a reviewer subagent used to be isolated from
its own project's durable knowledge unless something else compensated for
it. Under the cascade it just works.
