# Memory tiers

Every memory lives in one of four **tiers**. A tier classifies a memory by
_kind_ — how consolidated and durable it is — and sets how long it survives
before it ages out. (Tiers are orthogonal to [categories](categories.md), which
classify a memory by _topic_.)

| tier         | holds                                          | lifetime      |
| ------------ | ---------------------------------------------- | ------------- |
| `working`    | raw, short-lived observations (default intake) | 72h TTL       |
| `episodic`   | summaries of what happened in a session        | 30-day TTL    |
| `semantic`   | durable extracted facts — "what I know"        | never expires |
| `procedural` | workflows and how-to knowledge                 | never expires |

`working` and `episodic` are **short-term** (TTL'd); `semantic` and
`procedural` are **long-term** (durable, curated). How memini picks a tier
when a write omits one is covered in
[the write path](how-it-works/write-path.md#tier-classification), and how
memories move between tiers over time in
[lifecycle](how-it-works/lifecycle.md). A store is normally
lopsided toward `working` — most rows are raw intake — with a smaller core of
`episodic` session summaries and `semantic`/`procedural` knowledge.

## The lifecycle

Memories move between tiers as usage patterns change:

```
working ──▶ episodic ──(distilled at write / recalled repeatedly)──▶ semantic / procedural
                 ▲                                                         │
                 └──────────────────(unused, low-value)────────────────────┘
```

- **Distillation (write-time)** — each fresh short-term capture (working or
  episodic) is distilled by the LLM into durable `semantic`/`procedural` facts
  immediately, so a fact stated once is durable without first having to be
  recalled. Automatic when an LLM is configured. The raw source is always kept.
- **Heuristic extraction (write-time, no LLM)** — when no LLM is configured (the
  embedder-only and embedder+reranker deployments), a marker-based extractor
  pulls decisions/preferences/problems out of each fresh short-term capture into
  durable typed facts. Automatic when no LLM is present (the distiller supersedes
  it otherwise). Conservative — a miss just means no extra fact, and the raw
  source is always kept.
- **Working→episodic promotion** — a `working` memory recalled enough to clear
  `MEMINI_PROMOTE_MIN_ACCESS` (default `3`) is retiered to `episodic` (30d TTL)
  so content that proved valuable survives longer than the 72h intake TTL.
- **Promotion** — a backstop for short-term memories that were not distilled at
  write time: frequently-recalled `working`/`episodic` memories are periodically
  distilled into durable `semantic` facts. Eligibility kicks in at
  `MEMINI_PROMOTE_MIN_ACCESS` recalls (default `3`); the pass runs every
  `MEMINI_PROMOTE_INTERVAL` (default `24h`). Uses the LLM when configured, the
  marker extractor otherwise (a short source with no extractable segment is
  promoted whole).
- **Corroboration** — a fresh `working`/`episodic` write that restates an
  existing durable fact (nearest-neighbour score ≥ 0.70) reinforces that fact
  and grows its confidence instead of only piling up as chatter. Growth is
  rate-limited to once per 24h per fact, so a session restating the same thing
  five times counts as one observation. The write itself is still stored.
  Corroboration never resolves _contradictions_: similarity cannot distinguish
  "same fact, updated" from "related fact" (benchmarked), so conflicting
  durable facts stay with the LLM consolidator or the merge-hint +
  `memory_update` flow.
- **Demotion** — durable memories that were never recalled, are low-importance,
  and are uncorroborated get pushed back down to `episodic` once they are older
  than `MEMINI_DEMOTE_AFTER`, so unused "durable" debris (e.g. a low-quality bulk
  import) ages out on its own. **On by default** at `168h` (7d); set `0` to
  disable. Anything recalled even once is reinforced and kept.
- **Eviction** — the short-term tiers are capped per namespace at
  `MEMINI_SHORT_TERM_CAP` (default `1000`); over the cap, the sweeper evicts the
  lowest-retention `working`/`episodic` rows.

Without an LLM, memini still runs the full lifecycle: write-time extraction,
promotion, and corroboration all fall back to the marker heuristics.

## Setting a tier

On write, set `tier` explicitly. When omitted, the content defaults to `working`
(the intake tier) and is classified by the marker heuristic — a terse, unhedged
decision/preference/problem statement lands in `semantic`/`procedural` (stamped
`metadata.tier_classified=marker` for auditing). Classification only raises:
it never lowers a write to `working`, and `working` is already the default.

```jsonc
{
  "content": "auth middleware uses jose, not jsonwebtoken (Workers can't run native bindings)",
  "tier": "semantic",
}
```

Bulk imports map each source's native kind onto a tier (e.g. agentmemory
`workflow` → `procedural`, mem0 facts → `semantic`); sources with no recognized
kind default to `working` with a fresh 72h TTL.

## Filtering by tier

Every browse/search surface narrows by `tier`:

```sh
# REST browse
curl "$MEMINI_URL/v1/memories?tier=semantic" -H "X-Memini-Namespace: $NS"

# CLI export of one tier
memini export --namespace "$NS" --tier procedural
```
