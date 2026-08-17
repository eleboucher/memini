# The life story of a memory

One fact, birth to supersession. This example follows a single deployment
decision in the `acme/payments` namespace through everything the write and
read paths can do to it: auto-classification, reinforcement, the value gate,
a merge hint, a correction that supersedes it, time travel back to the old
belief, and the history chain that records all of it.

Every request here is shown as its REST wire shape (`X-Memini-Namespace:
acme/payments` on each call); the MCP tools `memory_remember`,
`memory_recall`, and `memory_history` produce the same shapes. Responses are
trimmed to the fields the pinning test asserts; ids and timestamps are
illustrative. The behavior shown is the production default — merge hints at
`MEMINI_WRITE_DEDUP_SCORE` 0.625 and the episodic floor
`MEMINI_EPISODIC_MIN_CHARS` 120 are both on out of the box; the test tunes
the thresholds down only because its deterministic test embedder has
different score geometry.

## 1. Birth: a write with no tier

The agent saves a decision and does not say what kind of memory it is:

```json
POST /v1/memories
{
  "content": "We decided to deploy the payments service with rolling updates instead of blue-green, the reason is the database migrations must run exactly once."
}
```

```json
201 Created
{
  "id": "01J9",                        // illustrative
  "tier": "semantic",
  "metadata": { "tier_classified": "marker" }
}
```

No LLM was consulted. The marker classifier saw a short, unhedged,
prose-shaped statement carrying several distinct decision markers ("we
decided", "instead of", "the reason is") and raised it from the working-tier
default to `semantic` — durable, no `expires_at`. The
`metadata.tier_classified: "marker"` stamp records that the tier was
classified, not chosen, so a bad call is auditable later. Hedged phrasing
("maybe we should go with...") would have stayed working-tier. The full
pipeline is in [the write path](../how-it-works/write-path.md).

## 2. Saying it again: reinforced, not duplicated

Later, another session saves the exact same sentence. The response is a
201, but look closer:

```json
201 Created
{
  "id": "01J9",                        // the EXISTING memory's id
  "reinforced": true
}
```

Nothing new was written. The normalized content matched an existing live
memory's fingerprint, so the write strengthened that memory instead of
storing a duplicate — the store still holds exactly one copy of the fact.
`reinforced: true` is the only way to tell this apart from a genuine create:
without it the agent would believe it stored something new and would hold an
id it might later update or forget, clobbering a memory it never wrote.

## 3. A low-signal write: accepted, not stored

Mid-session the harness captures a throwaway turn:

```json
POST /v1/memories
{ "content": "ok keep going", "tier": "episodic" }
```

```json
200 OK
{ "stored": false, "reason": "low_signal", "tier": "episodic" }
```

The episodic value gate (`MEMINI_EPISODIC_MIN_CHARS`) dropped it: too short
to ever be worth recalling. Note the status — a drop is a 200, not an error;
the write was accepted and judged, not rejected. The response still reports
the tier the write _resolved to_, so an agent that omitted the tier learns
what the write would have been. The gate never touches durable tiers: a
short semantic fact ("jose not jsonwebtoken") stores fine.

## 4. A reworded near-duplicate: the merge hint

Another session states the same fact in different words:

```json
POST /v1/memories
{
  "content": "We chose rolling updates rather than blue-green for the payments service because the database migrations must run exactly once."
}
```

```json
201 Created
{
  "id": "01JB",                        // a NEW memory — both copies now live
  "tier": "semantic",
  "merge_hint": {
    "similar_id": "01J9",
    "score": 0.87,                     // illustrative; fused similarity
    "tier": "semantic"
  }
}
```

The wording differs, so the exact-fingerprint path missed — but the vector
dedup gate found the original as a near-duplicate above the dedup score. In
the default `hint` mode the server stores the write anyway and hands the
caller the evidence: the similar memory's id, its tier, and the similarity
score. Deciding is the agent's job — fold the two together, or keep both
because they genuinely differ. Here the fresh copy adds nothing, so the
agent keeps the original and deletes the new one (`DELETE
/v1/memories/01JB`). Merge hints are durable-tier only; episodic captures
never pay for one.

## 5. Recall reinforces

Later, a session asks:

```json
POST /v1/search
{ "query": "rolling updates for the payments service", "limit": 5 }
```

The fact comes back — and returning it is not free of consequence. Every
recall reinforces its results: the memory's `access_count` rises and its
last-access time slides forward, which is what keeps a useful short-term
memory alive and eventually promotes it. Fetch the memory before and after
and the counter has moved:

```json
GET /v1/memories/01J9   →   { "access_count": 2 }    // was 1
```

Session-start briefings deliberately do _not_ reinforce — showing up in
every briefing is not evidence of usefulness. The asymmetry is explained in
[recall](../how-it-works/recall.md).

## 6. The correction: supersede, don't delete

Rolling updates turn out to be the wrong call. The agent records the
correction as a normal write (classified `semantic` the same way), then
marks the old fact as replaced:

```json
POST /v1/memories
{
  "content": "We decided to go with blue-green deploys for the payments service instead of rolling updates because migrations kept colliding mid-rollout."
}
```

```json
POST /v1/memories/01J9/supersede
{ "by": "01JC" }                       // the correction's id
```

From this instant, live recall for the same query returns **only** the
correction. The original is not deleted — it is tombstoned: hidden from
default recall, kept for the audit chain, and hard-deleted only after the
tombstone TTL.

## 7. Time travel: what did we believe before?

The supersede stamped the original's validity window closed. Ask for the
world as it was before the correction:

```json
POST /v1/search
{
  "query": "rolling updates for the payments service",
  "as_of": "2023-11-14T20:13:20Z",     // illustrative; any instant pre-correction
  "limit": 5
}
```

The _original_ fact comes back and the correction does not: `as_of` returns
the facts whose validity window contained that instant, superseded or not.
This is how "what was the plan in March" stays answerable after the plan
changes.

## 8. The chain

```json
GET /v1/memories/01JC/history
{
  "memories": [
    { "id": "01J9", "superseded_by": "01JC" },   // the original, oldest first
    { "id": "01JC" }                             // the correction
  ]
}
```

History walks the supersession lineage in both directions — asking from the
original's id yields the same chain — ordered oldest-first, tombstones
included. The story so far is the whole story.

What this example does _not_ show is aging: working memories promoting to
episodic after repeated access, the nightly promoter distilling episodes
into facts, stale memories demoting, confidence decaying. Those run on
background timers rather than request paths; see
[lifecycle](../how-it-works/lifecycle.md).

## Validated by

- `TestExampleMemoryLifeStory` in
  `internal/service/example_memory_life_test.go` — walks stages 1-8 above in
  order, asserting every quoted field.
- `TestRememberLowSignalReportsEffectiveTier` in
  `internal/api/rest/lowsignal_tier_test.go` — the exact stage-3 REST shape
  (`stored:false` with the resolved tier).
- `TestRememberReportsReinforcedWhenNothingWasWritten` and
  `TestMergeHintReturnedWithoutSuppression` in
  `internal/service/service_test.go` — the reinforced flag and the hint
  band, independent of this narrative.
- The aging stages deliberately not retold here are pinned by
  `internal/service/consolidate_internal_test.go` (promotion) and
  `internal/maintenance/demote_test.go` (demotion).
