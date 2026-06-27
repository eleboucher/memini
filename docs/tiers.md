# Memory tiers

Every memory lives in one of four **tiers**. A tier classifies a memory by
_kind_ — how consolidated and durable it is — and sets how long it survives
before it ages out. (Tiers are orthogonal to [categories](categories.md), which
classify a memory by _topic_.)

| tier         | holds                                           | lifetime      |
| ------------ | ----------------------------------------------- | ------------- |
| `working`    | raw, short-lived observations (session scratch) | 24h TTL       |
| `episodic`   | summaries of what happened in a session         | 90-day TTL    |
| `semantic`   | durable extracted facts — "what I know"         | never expires |
| `procedural` | workflows and how-to knowledge                  | never expires |

`working` and `episodic` are **short-term** (TTL'd); `semantic` and
`procedural` are **long-term** (durable, curated). A store is normally
lopsided toward `episodic` — most rows are session history, with a smaller core
of `semantic`/`procedural` knowledge.

## The lifecycle

Memories move between tiers as usage patterns change:

```
working ──▶ episodic ──(distilled at write / recalled repeatedly)──▶ semantic / procedural
                 ▲                                                         │
                 └──────────────────(unused, low-value)────────────────────┘
```

- **Distillation (write-time)** — each fresh `episodic` capture is distilled by
  the LLM into durable `semantic`/`procedural` facts immediately, so a fact
  stated once is durable without first having to be recalled. On by default
  (`MEMINI_DISTILL_ON_WRITE`); needs an LLM. The raw episodic is kept unless
  `MEMINI_DISTILL_DROP_NO_FACT` is set.
- **Heuristic extraction (write-time, no LLM)** — when no LLM is configured (the
  embedder-only and embedder+reranker deployments), a marker-based extractor
  pulls decisions/preferences/problems out of each fresh `episodic` capture into
  durable typed facts. On by default (`MEMINI_EXTRACT_ON_WRITE`); the LLM
  distiller supersedes it when present. Conservative — a miss just means no extra
  fact, and the raw episodic is always kept.
- **Promotion** — a backstop for episodic memories that were not distilled at
  write time (e.g. written before an LLM was configured): frequently-recalled
  `episodic` memories are periodically distilled into durable `semantic` facts.
  Eligibility kicks in at `MEMINI_PROMOTE_MIN_ACCESS` recalls (default `3`); the
  pass runs every `MEMINI_PROMOTE_INTERVAL` (default `24h`). Needs an LLM configured.
- **Demotion** — durable memories that were never recalled, are low-importance,
  and are older than `MEMINI_DEMOTE_AFTER` get pushed back down to `episodic`,
  so unused "durable" debris (e.g. a low-quality bulk import) ages out on its
  own. Disabled by default (`0`); anything recalled even once is reinforced and
  kept.
- **Eviction** — the short-term tiers are capped per namespace at
  `MEMINI_SHORT_TERM_CAP` (default `1000`); over the cap, the sweeper evicts the
  lowest-retention `working`/`episodic` rows.

Without an LLM, memini still runs: promotion is a no-op and tiers stay where
they were written.

## Setting a tier

On write, set `tier` explicitly; it defaults to `episodic` when omitted.

```jsonc
{
  "content": "auth middleware uses jose, not jsonwebtoken (Workers can't run native bindings)",
  "tier": "semantic",
}
```

Bulk imports map each source's native kind onto a tier (e.g. agentmemory
`workflow` → `procedural`, mem0 facts → `semantic`); sources with no recognized
kind default to `episodic` with a fresh 90-day TTL.

## Filtering by tier

Every browse/search surface narrows by `tier`:

```sh
# REST browse
curl "$MEMINI_URL/v1/memories?tier=semantic" -H "X-Memini-Namespace: $NS"

# CLI export of one tier
memini export --namespace "$NS" --tier procedural
```
