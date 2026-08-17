# The memory lifecycle

What happens to a memory _after_ [the write path](./write-path.md) commits it. Nothing in the store is static: recall keeps memories alive, usage promotes them, neglect demotes and expires them, and corrections supersede them without erasing history.

The short version:

- Every recall reinforces its results: access counts climb and short-term expiries slide forward. Session briefings deliberately never reinforce.
- Usage moves memories up the [tier](../tiers.md) ladder; a daily promoter distills what keeps getting recalled into durable facts.
- An hourly sweeper purges the expired, caps the short-term pool, and demotes durable debris that never earned its keep.
- Durable facts carry a confidence that grows on corroboration and decays with silence — and a validity window, so a superseded fact leaves live recall while staying reachable through time travel.

## Reinforcement: recall keeps memories alive

When a recall returns a memory, that memory was _used_, and memini records it in the background: `access_count` is bumped, `last_accessed_at` is set, and — for memories that expire — the expiry slides forward by the memory's own lifetime (the caller's custom TTL if one was set at write time, else the tier default: 72h for `working`, 30 days for `episodic`). A `working` note recalled every two days effectively never expires; one nobody asks about is gone in three. Durable memories never gain a TTL from reinforcement — sliding only ever applies to rows that already expire.

**Briefings never reinforce.** A session briefing fires on every session start over the same top-ranked set regardless of what the session is about. Counting that as "used" would inflate `access_count` uniformly across the board and distort every decision that reads it — promotion eligibility, retention scoring, ranking. So a briefing logs what it served but bumps no counters; only genuine, query-driven recall (and the per-prompt injection built on it — see [recall](./recall.md)) earns reinforcement.

## Moving up: intake to durable

Two mechanisms turn short-term intake into lasting knowledge. (A third — write-time distillation/extraction — already ran on [the write path](./write-path.md); the ones here are the usage-driven backstop.)

**Working to episodic.** A `working` memory recalled at least `MEMINI_PROMOTE_MIN_ACCESS` times (default 3) has proven it is worth more than the 72-hour intake window: it is retiered to `episodic` and given the 30-day TTL. The move is stamped in metadata so it happens once.

**The promoter.** Every `MEMINI_PROMOTE_INTERVAL` (default 24h), each namespace's frequently-accessed, not-yet-promoted short-term memories (`working` and `episodic`, same access threshold) are distilled into durable `semantic`/`procedural` facts — by the LLM when one is configured, by the marker extractor otherwise, so usage-earned promotion also works on LLM-less deployments. Distilled facts are written through the normal write path, so they get classification, dedup, and consolidation like any other write, and they carry provenance (`metadata.source_ids` / `metadata.promoted_from`) back to the episodes they came from.

One design choice worth knowing: sources are stamped `promoted_at` **before** distillation, not after. If the fact-write then fails, that batch's facts are lost (logged, recoverable by hand) rather than re-distilled next tick into near-duplicate, non-deterministically reworded facts. Idempotency over completeness — duplicates poison ranking; a missed fact just waits to be restated.

## Moving down: demotion

The mirror image of promotion. During each sweep, a durable (`semantic`/`procedural`) memory is demoted to `episodic` — picking up the 30-day TTL, after which it ages out — when _all_ of these hold:

- it has **never been recalled** (`access_count` 0);
- its importance is **below 0.75** (so default-seeded facts are not immune, but anything marked important is);
- it was last updated longer ago than `MEMINI_DEMOTE_AFTER` (default 7 days);
- its current confidence has decayed **below 0.35** — uncorroborated debris, not an established fact;
- it is not tagged `pinned`.

A single recall resets the clock permanently (the access count never goes back to zero), and corroboration keeps confidence above the floor. What demotion actually removes is the sediment: low-quality bulk imports and misclassified "facts" that nothing ever asked about.

## The sweeper

An hourly background sweep (`MEMINI_SWEEP_INTERVAL`, first pass at boot) runs five stages, in order:

1. **Expiry purge.** Memories whose TTL has passed are deleted, in batches. The delete is conditional on the expiry still being in the past — so a concurrent recall that just slid the expiry forward _wins the race_, on purpose. A memory being used at the moment of its scheduled death is exactly the memory reinforcement exists to save.
2. **Short-term cap.** Each namespace holding more than `MEMINI_SHORT_TERM_CAP` (default 1000) `working`+`episodic` memories has its lowest-retention ones evicted, ranked by the same quality score recall uses — so eviction and ranking agree on what matters least.
3. **Tombstone GC.** When `MEMINI_TOMBSTONE_TTL` is set, superseded rows tombstoned longer ago than the TTL are hard-deleted (see [bi-temporality](#bi-temporality-supersession-contradiction-and-time-travel) below). The default is 0: tombstones are kept forever.
4. **Demotion**, as above (when `MEMINI_DEMOTE_AFTER` is set; it is by default).
5. **Event pruning.** The activity log is bounded by age and row count.

## Confidence: how facts earn and lose trust

Durable facts carry a confidence in the spirit of "how corroborated is this?", and it drives ranking, demotion, and contradiction handling.

- **Seeds.** A fresh durable write starts at **0.4** — some basis, not yet corroborated. A bulk import starts lower, at **0.25**, so imported claims must earn trust before outranking facts the agent actually established. A caller-supplied seed is clamped to at most 0.7: the top of the range is reserved for trust earned over time, not asserted up front.
- **Growth.** Each corroboration — an exact restatement, a coalesced duplicate, or a short-term write that restates the fact — closes 10% of the remaining gap to 1.0, so confidence asymptotes and never overshoots. Growth is rate-limited to once per 24 hours per fact: a session restating the same thing five times is one observation, because restatement echo must not manufacture confidence — only re-observation spread over time may.
- **Decay.** After a one-week grace period, confidence decays by 0.05 per week of silence, down to a floor of 0.05. Decay is computed **lazily at read time** from the last corroboration or recall — no background job rewrites rows just to make numbers smaller.
- **Shrink on contradiction.** When a fresh durable write contradicts a fact (changed value or flipped polarity, not a mere restatement), the stale fact's confidence is cut — scaled down harder the more it was used, so a heavily reinforced stale fact cannot coast on its usage bonus — and its validity window is closed (next section).

Legacy rows written before confidence tracking read as fully trusted; they are never retroactively penalized.

## Ranking: what quality means per tier

Two scores decide what surfaces and what survives, built from the same parts: **salience** (a tier weight — `procedural` 0.95, `semantic` 0.90, `episodic` 0.55, `working` 0.30 — modulated by importance), confidence, and a usage bonus that grows logarithmically with access count.

- **Durable memories** score salience × confidence × usage, with **no recency decay**. Durable facts already age through confidence decay; an exponential recency factor on top would zero out any fact not recalled this week and bury core knowledge under fresh session trivia. A briefing ranks purely on this score.
- **Short-term memories** multiply in a recency factor: an exponential forgetting curve with a 7-day half-life, _flattened_ by access — each recall stretches the curve's stability, so a frequently-used note forgets slower rather than merely getting a one-time bump. This is the score cap-eviction uses, so the cap always evicts the stalest, least-used scratch first.

How these scores combine with query relevance at recall time is covered in [recall](./recall.md).

## Bi-temporality: supersession, contradiction, and time travel

Every memory carries a validity window: `valid_from` (when the fact became true — defaults to write time, backdatable) and `valid_to` (when it stopped being true — open while it still holds). Corrections therefore never delete: they close windows and link replacements.

Two related but distinct events end a fact's live life:

- **Supersession** — "this record was replaced." The old row gets a tombstone pointer to its successor. Produced by explicit supersede calls, the write path's auto-supersede, and duplicate coalescing.
- **Contradiction** — "this fact stopped being true." The stale fact's `valid_to` is stamped, its confidence shrunk, and it records what contradicted it. The fact is invalidated, not replaced row-for-row.

**Two independent exits from live queries.** Default recall excludes a row if _either_ it has been superseded _or_ its validity window has closed — two separate conditions, either alone suffices. That is how a contradiction can retire a fact without any supersession, and a supersession can retire a record whose window says nothing.

**Time travel.** Passing `as_of` asks "what was believed true at that instant?", and it changes the filter in two deliberate ways:

- The superseded filter is **bypassed**: a fact that was true then and has since been replaced is exactly what you asked for. Only the validity window is checked — `valid_from ≤ as_of < valid_to`.
- Expiry is evaluated **at the `as_of` instant**, not the current clock: a memory that has since expired was still alive then.

```
memory_recall query="staging database engine" as_of="2026-06-01T00:00:00Z"
```

returns the SQLite-era fact even though the live store now says Postgres — walk the worked example in [the life story of a memory](../examples/memory-life-story.md).

**History.** A memory's history is its full supersession lineage, walked in _both_ directions — everything it superseded, and everything that superseded it, including merges where several memories fold into one — tombstoned rows included, ordered oldest-first. Each entry's validity window bounds when it was believed, and its supersession link names what replaced it.

**How far back can you go?** As far as tombstones live. `MEMINI_TOMBSTONE_TTL` defaults to 0 — tombstones are kept indefinitely, and `as_of` can reach the beginning of the store. Setting a TTL trades that reach for space: the sweeper hard-deletes tombstones older than the TTL (aged from when the row was invalidated, not when it was written), and history beyond that horizon is gone. See [upgrading](../operations/upgrading.md) for migration-adjacent knobs.

## Source map

For contributors; the body above deliberately names no code.

| mechanism                                 | where                                                                                                                 |
| ----------------------------------------- | --------------------------------------------------------------------------------------------------------------------- |
| reinforcement on recall                   | `internal/service/service.go` — `reinforceResults`, `reinforce`, `reinforceTTL`                                       |
| briefings never reinforce                 | `internal/service/query.go` — `Briefing` (comment at `logBriefingEvent`); pinned by `internal/service/events_test.go` |
| working→episodic retier + promoter        | `internal/service/promote.go` — `Promote`, `retierWorkingToEpisodic`, `promote`, `promoteHeuristic`, `stampPromoted`  |
| demotion                                  | `internal/maintenance/demote.go` — `DemoteStale`                                                                      |
| sweeper stages                            | `internal/maintenance/maintenance.go` — `Sweeper.sweep`, `PurgeExpired`, `EnforceShortTermCap`                        |
| tombstone GC                              | `internal/maintenance/tombstone.go` — `PurgeTombstones`                                                               |
| confidence math                           | `internal/memory/types.go` — `EffectiveConfidence`, `GrowConfidence`, `decayConfidence`, seed constants               |
| corroboration / contradiction             | `internal/service/service.go` — `corroborateNearestAsync`, `contradictNearestAsync`, `invalidate`                     |
| quality / durable score / salience        | `internal/memory/types.go` — `Quality`, `DurableScore`, `Salience`, `tierSalience`                                    |
| bi-temporal query semantics               | `internal/store/sqlitevec/helpers.go` — `filterClause`                                                                |
| validity resolution / supersede / history | `internal/service/service.go` — `resolveValidity`, `Supersede`, `History`                                             |
