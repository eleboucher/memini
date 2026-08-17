# Recall

**The short version.** memini pulls memories on three occasions: once when a session opens (the briefing), once per user prompt (the injection), and whenever the agent explicitly asks (`memory_recall`). Every pull resolves a read set (which namespaces to search), runs vector and keyword search over each namespace in parallel, fuses the results, and re-ranks them with a relevance-dominant composite before reserves, guards, floors, and a token budget shape the final list. If the embedder is down, recall degrades to keyword-only and says so instead of failing.

## The three pull surfaces

| Surface                  | Trigger                                                                | Query             | Default scope | Default budget                              | Reinforces? |
| ------------------------ | ---------------------------------------------------------------------- | ----------------- | ------------- | ------------------------------------------- | ----------- |
| Session-start briefing   | the session opens (plugin hook)                                        | none — query-less | `full`        | 600 tokens (plugin default, server-trimmed) | **never**   |
| Per-prompt injection     | each user prompt, after shape gates (length, not a command, cooldowns) | the prompt text   | `full`        | 3 hits, 250 tokens, composite floor 0.5     | yes         |
| Explicit `memory_recall` | the agent decides ("check memory before touching this file")           | the agent's query | `full`        | 10 hits, unbounded tokens unless asked      | yes         |

The briefing's refusal to reinforce is deliberate, not an omission: it fires on every session start over the same top-N regardless of relevance, so counting a briefing serve as "this memory was used" would inflate access counts uniformly and distort the promotion and ranking that depend on them. The activity log still records what each briefing served; the counters stay clean. Explicit recall and the per-prompt injection do reinforce: each served memory's access count bumps and its expiry slides forward by its own lifetime, so what actually gets used stops decaying.

The plugin adds a fourth, narrower surface — a pre-tool-use lookup before file edits — with its own gates and a 200-token budget; see [the plugin doc](./plugin.md) for its mechanics and [recall in practice](../examples/recall-in-practice.md) for a worked comparison of the surfaces.

## Building the read set

Every pull first resolves the set of namespaces to search. The per-call `scope` keyword picks the shape:

| `scope`            | Read set                                                            |
| ------------------ | ------------------------------------------------------------------- |
| `"project"`        | the primary namespace only — no cascade                             |
| `"full"` (default) | primary, then the cascade: ancestors, home, links                   |
| `"everywhere"`     | `full`, plus every namespace nested under the primary (its subtree) |

The cascade appends in a fixed order: the primary namespace first, then its ancestors nearest-first (`acme/phoenix` before `acme` for a recall in `acme/phoenix/api`), then the caller's home namespace, then any stored links. Every non-primary leg is restricted to durable tiers (`semantic`, `procedural`) — episodic and working memories never cross a namespace boundary on a read. A link may narrow that further, never widen it. Subtree members are the one exception: they count as part of the primary leg and are searched at the caller's full tier filter.

An explicit per-call `namespaces: [...]` list (REST) replaces the whole default set — capped at 16 entries, with `/*` suffixes expanding subtrees — and the resolved set is clamped at 64 namespaces after expansion, keeping the front of the list (the primary, and the home leg, survive the clamp by construction). The full scoping model, provenance annotations, and the isolation contract live in [scopes.md](../scopes.md); the vocabulary is pinned in the [glossary](../glossary.md).

## The search

With the read set in hand, recall embeds the query and searches. The query embedding and the read-set resolution run concurrently; then every namespace in the read set gets two search legs — vector similarity and keyword — run in parallel across the whole set.

Failure is isolated per leg, on purpose. One unreachable ancestor or link namespace cannot take down a recall the primary namespace has already answered: a failed secondary namespace is dropped and named in the response's degradation note, and a failed vector leg falls back to that namespace's keyword results. Only losing the primary namespace's keyword leg — the one leg every recall has — fails the call.

Before any fusion, an absolute semantic gate drops vector candidates whose raw similarity score is below a floor (0.46 by default, per-call overridable). This is an absolute bar, not a relative one: without it, fusion would min-max-normalize a batch of uniformly irrelevant candidates into competitive-looking scores. Keyword hits that never appeared in the vector leg — vectorless memories, or hits outside the bounded vector pool — have no semantic score to gate on and stay eligible; a keyword hit whose vector score is known and below the floor is dropped from both legs.

Fusion happens twice. First within each namespace: the vector and keyword legs merge into one best-first list (by default a weighted sum of min-max normalized scores, both legs weighted equally; a reciprocal-rank-fusion mode exists as a benchmark-tuned internal alternative). Then across namespaces: the per-namespace lists fuse into a single ranking, and **ties at equal fused score break by first-seen order across the read set**. That tie-break is exactly why the cascade appends ancestors nearest-first — at equal relevance, a memory in `acme/phoenix` outranks the same-scored one in `acme`, because its namespace was seen first. An absolute floor on the fused score (0.1 by default) then trims the loosely relevant tail before ranking.

When the caller asks for query rewriting (and an answering model is configured), the query is first expanded into two or three variants, each recalled concurrently through this same pipeline, and the variants' results fuse by reciprocal rank into the final list.

## Ranking, honestly

The fused list is re-scored with a composite that is best described as relevance with a quality modifier — not the even blend of relevance, recency, and importance that memory systems usually advertise:

```
composite = relevance × (0.80 + 0.20 × quality)
```

Relevance is the fused retrieval score, normalized to the best hit in the set. Quality folds tier salience, corroboration (confidence), reinforcement (access counts), and recency into one normalized number. In production, the standalone recency and importance weights are **literally zero** — recency and importance influence ranking only through the quality term, and the quality term multiplies the candidate's own relevance rather than adding a flat bonus. An off-topic memory has almost no score to amplify, so a high-quality durable fact can rise above comparably relevant chatter, but it can never beat genuinely more relevant results just by being old news that gets recalled a lot. On a fresh single-tier corpus the quality term is constant and ranking reduces to pure relevance order.

After the composite, a fixed sequence of adjustments runs:

1. **Durable-tier reserve.** Up to 2 of the top-k slots (by default) are held for semantic/procedural memories, promoting durables from just below the cut and evicting the lowest episodic entries — but only when the durable is relevance-competitive (at least half the score of what it evicts and 40% of the top hit). A query with no relevant durable keeps its pure-relevance window. Promoted durables slot in directly below the top hit; the top hit is never displaced.
2. **Turn-echo guard.** Conversation turns captured within the last five minutes (by default) are dropped: a just-captured turn is still in the caller's live context, and echoing it back makes the agent parrot itself. Callers can opt out per call (`include_fresh_turns`).
3. **Dedup.** Results whose normalized content matches a higher-ranked result are dropped.
4. **Optional reranker.** With a cross-encoder or LLM reranker configured, a pool deeper than k is re-judged by the reranker, which can promote a memory the retrieval score left just below the cut. Before the pool is cut, an **importance reserve** swaps up to 2 buried high-importance candidates (effective importance at least 0.75 — a bar only assessed or explicitly-set importance clears) into pool membership, relevance-gated the same way as the tier reserve. The reserve changes only which candidates the reranker sees, never their final order, and a reranker failure or timeout falls back to the composite order — recall never errors on the reranker's account.
5. **The composite floor.** A per-call `min_rank_score` in [0, 1) splits the finalized list on the final composite score: hits at or above the floor are served, the rest are logged as floored — visible in the activity feed and a metric, but never served and never reinforced.
6. **One-hop link expansion.** With `include_linked`, memories explicitly linked from a served result are fetched and merged in at a synthetic score of half the lowest direct hit, so they always rank below direct results. The floor runs _before_ this step on purpose — a floor applied after would wipe out every synthetic-scored linked hit and silently defeat the option, so linked hits are exempt from it.
7. **The token budget.** When the caller sets one, results fill in final rank order until the next would exceed it, and the tail is dropped — whole items only, with the drop count reported, and the first result always ships even if it alone busts the budget. A non-empty recall never becomes empty by budget.

Only then does reinforcement run, over exactly the results the caller received. The knobs behind most of these stages — floors, reserves, timeouts — are covered symptom-first in [tuning recall](../guides/tuning-recall.md).

## Degraded recall

Recall never fails because the embedder did. If the query embed errors or exceeds its timeout (2 seconds by default), the vector legs are skipped and the recall proceeds keyword-only, with `degraded: "keyword_only"` and a plain-language note on the response. If secondary namespaces were unreachable, the response is `degraded: "partial"` and names them. The same happens with no embedder configured at all: writes store vectorless rows and every recall is keyword-only — supported, but noisy.

The distinction matters more than it looks: an agent handed fewer results with no explanation concludes the memory does not exist, which is worse than being told the search was partial. The plugin's per-prompt injection renders the note inline (a `[memini: ...]` line above the injected memories) so the model treats an empty degraded result as "could not look properly", not "nothing is known". See [the plugin doc](./plugin.md) for how each hook surface handles degradation, and [the write path](./write-path.md) for what an embedder outage does to writes.

## Briefing anatomy

The briefing is the query-less pull: when a session opens, the plugin asks for a structured summary of the namespace instead of searching for anything in particular. It has four sections plus a subtree index:

- **Pinned** — memories tagged `pinned`, any tier, always surfaced.
- **Facts** — semantic memories, ranked by durable score (a quality ranking with no recency decay).
- **Procedures** — procedural how-tos, same ranking.
- **Recent** — episodic entries, newest first, with just-captured turns filtered out (the briefing's version of the turn-echo guard).
- **Children** — a rollup of direct child namespaces (counts and a few highlights each), so a parent namespace's briefing indexes its subtree without searching it.

Each section is capped (5 items by default; the plugin asks for 3 recent). Cascade legs contribute durable memories only, so an ancestor's facts show up but its chatter never does. The token budget fills whole items in section order — **pinned, then facts, then procedures, then recent** — so fill order is priority order and the recent section starves first when the budget is tight. The first item overall always ships.

Two details keep briefings from going stale. First, the **exploration slot**: when every item in a durable section's top-n was already served by a briefing in the last seven days (ignoring the last hour, so a re-fired session doesn't count against itself), the section's last slot rotates to the highest-ranked memory _not_ recently shown — breaking the rich-get-richer loop where the same five facts open every session. Only the last slot ever moves. Second, the **scope header** — the `Scope: acme/phoenix/api ← acme/phoenix(3) ← acme(4) ← personal/kit(2), +1 link` line — is built from the resolved read set with per-leg counts of what each leg actually contributed, and a leg that contributed nothing is omitted so the header never advertises empty breadth. Only the primary namespace is unconditional; links collapse into a `+K links` suffix.

A failing cascade leg costs the briefing breadth, not existence: the briefing ships without it and lists the namespace as degraded, because a briefing that silently omits an ancestor's facts is precisely the case where an agent does not know what it is missing. Losing the primary namespace is fatal. And, as above: briefings log what they served, and never reinforce.

## Source map

| Area                                                                                 | Where                                                                                                                                                                                                                    |
| ------------------------------------------------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| Recall pipeline, semantic gate, reserves, echo guard, floors, link expansion, rerank | `internal/service/service.go` — `Recall`, `tryQueryRewrite`, `gateSemantic`, `fuseLegs`, `reserveDurableTiers`, `applyTurnEchoGuard`, `finalizeRecall`, `reserveImportantPool`, `applyMinRankScore`, `maybeExpandLinked` |
| Token budgets (recall + briefing)                                                    | `internal/service/budget.go` — `applyRecallBudget`, `applyBriefingBudget`                                                                                                                                                |
| Read-set resolution, scope parsing, cascade order, clamps                            | `internal/service/readset.go` — `parseScope`, `resolveReadSet`, `resolveDefaultReadSet`, `ancestorsOf`, `promoteProtected`, `clampReadSet`                                                                               |
| Briefing, sections, exploration slot, scope header, child rollup                     | `internal/service/query.go` — `Briefing`, `reserveExplorationSlot`, `scopeHeader`, `childRollup`; served-window lookup in `internal/service/events.go` — `recentlyServedIDs`                                             |
| Fusion and ranking                                                                   | `internal/search/fusion.go` — `FuseScores` (first-seen tie-break), `internal/search/rrf.go` — `Fuse`, `internal/search/rank.go` — `RerankWith`, `DefaultRerankWeights` (recency/importance weights are 0), `Dedup`       |
| Degradation wire format                                                              | `internal/service/query.go` — `DegradedWire`; leg failure handling in `internal/service/service.go` — `reportRecallLegs`                                                                                                 |
| Plugin surfaces and their defaults                                                   | `plugin/scripts/session-start.mjs`, `plugin/scripts/user-prompt-submit.mjs`, `packages/memini-client/src/settings.ts` — `BEHAVIOR_KNOBS`                                                                                 |
