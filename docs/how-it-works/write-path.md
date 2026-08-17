# The write path

What happens between an agent calling `memory_remember` (or `POST /v1/memories`) and a row landing in the store — and in particular how memini decides the [tier](../tiers.md) when the caller doesn't set one.

The short version:

- Every write runs one pipeline: validate and resolve the tier, route the namespace, scrub and sanitize, gate low-signal captures, dedup (twice), embed, store, then kick off background follow-ups.
- An omitted tier is auto-classified by a pure regex heuristic — no LLM — that can raise a terse, confident statement straight into a durable tier.
- Some writes are accepted but not stored (`stored: false`); that is a feature, not an error, and the response still reports the resolved tier.

## The pipeline, in order

1. **Validate and resolve the tier.** Namespace and content are required. An omitted tier defaults to `working` unless the classifier (next section) raises it. An explicit tier is never overridden.
2. **Route the namespace.** `visibility` decides where the write lands — the project namespace, the caller's personal home, or an ancestor.
3. **Stamp provenance.** A heuristically classified durable write gets `metadata.tier_classified: "marker"`; a write authenticated by a named API key gets `metadata.author` (unless the caller set one).
4. **Scrub secrets.** Live credentials are redacted before anything persists.
5. **Sanitize.** Byte corruption is stripped; optionally, garbled content is quarantined (downranked, never rejected).
6. **Value gate.** Auto-captured conversation turns are stripped of harness boilerplate, then low-signal episodic writes are dropped — accepted but not stored.
7. **Exact dedup.** A content-identical live memory in the same tier is reinforced instead of duplicated (`reinforced: true`), before paying for an embedding.
8. **Embed.** The content is vectorized; a slow or absent embedder degrades the write to keyword-only instead of failing it.
9. **Near-duplicate dedup.** A vector search over the same tier decides whether the write should supersede, hint at, or coalesce into an existing memory.
10. **Store, then follow up.** The row commits (a client disconnect at this point can no longer undo an accepted write), and background jobs take over — see [After the row lands](#after-the-row-lands).

The request namespace arrives identically over REST and MCP: both transports normalize the `X-Memini-Namespace` header the same way, so `work//proj/` and `work/proj` name the same namespace on either. See [namespaces](./namespaces.md).

## How the tier is chosen

When a write names no tier, memini has to answer "is this raw scratch, or a fact worth keeping forever?" It answers with **pure regex marker heuristics — no LLM is involved**, so classification is deterministic, instant, and works on every deployment including embedder-only ones.

The default is `working`, the 72-hour intake tier. Classification can only ever _raise_ a write from that default into a durable tier — it never demotes, and it never touches a write whose caller picked a tier explicitly. A miss costs nothing: the write lands in `working` and can still earn durability later through the [lifecycle](./lifecycle.md).

Three gates run before any marker is checked. Failing any of them means "no confident call" and the write stays `working`:

- **Length: 20 to 400 runes** (the ceiling is tunable via `MEMINI_CLASSIFY_MAX_CHARS`). One durable fact is terse; anything longer is session history even when it contains decision language. Runes, not bytes, so non-ASCII prose isn't penalized.
- **Transcript veto.** Content with role scaffolding (`User:` / `Assistant:` at a line start) is a captured exchange, not a single fact, however many markers it contains.
- **Hedge veto.** Tentative phrasing — "maybe", "I think", "probably", "not sure", "might", "could be", "possibly", "reportedly", "I guess" — must stay short-term history rather than become a fact asserted confidently.

Past the gates, the content (code and command lines excluded — only prose is scored) is matched against five marker families:

| family     | example markers                                     | tier         |
| ---------- | --------------------------------------------------- | ------------ |
| decision   | "we decided", "let's go with", "instead of"         | `semantic`   |
| problem    | "root cause", "the fix was", "workaround"           | `semantic`   |
| fact       | "the server runs on", "deployed via", "env var"     | `semantic`   |
| preference | "always use", "never use", "I prefer", "my rule is" | `procedural` |
| how-to     | "to configure", "you need to run", "steps are"      | `procedural` |

The score is the count of _distinct_ markers that match (repeating one word can't inflate it), plus a small bonus for longer content, divided by 5. The result must reach the **0.3 confidence threshold** — so a lone marker on a short write is not enough, while two distinct markers (or one in a longer segment) pass. The best-scoring family wins and maps to its tier: preferences and how-tos become `procedural`, decisions, problems, and facts become `semantic`.

Concrete inputs and where they land (all verified against the classifier):

| input                                                                                                                    | result                                        |
| ------------------------------------------------------------------------------------------------------------------------ | --------------------------------------------- |
| "Always use pnpm in this repo, never use npm directly"                                                                   | `procedural` (two preference markers)         |
| "We decided to switch to Postgres instead of SQLite for the staging cluster"                                             | `semantic` (two decision markers)             |
| "Root cause of the flaky deploy: the CI runner kept a stale Docker layer cache; the fix was clearing it before each run" | `semantic` (two problem markers)              |
| "I think we should probably switch the deploy pipeline to Argo"                                                          | `working` (hedge veto: "I think", "probably") |
| "We went with Terraform for infra"                                                                                       | `working` (one marker — under the threshold)  |

A write the heuristic raised into a durable tier is stamped `metadata.tier_classified: "marker"`, so a bad call is auditable — and demotable — later. Explicit tiers and the `working` fallback carry no stamp.

## Where the write lands

`visibility` picks the namespace, and it runs _after_ tier resolution on purpose:

- `"project"` (or omitted) — the caller's primary namespace.
- `"personal"` — the caller's home namespace; an error if no home is configured.
- an ancestor name — full path (`"acme/phoenix"`) or an unambiguous last segment (`"acme"`). An unrecognized name errors, listing the valid targets.

**The tier clamp:** a non-durable write (`working`, `episodic`) never leaves the primary namespace, whatever `visibility` says — silently, with no error. Sharing a namespace up the tree is for curated knowledge, not raw session scratch. This is why visibility resolution has to see the _final_ tier: a capture the classifier left at `working` clamps to the project, but the same request classified into `semantic` is allowed to travel to the team ancestor. Deep dive in [scopes](../scopes.md).

## Scrubbing and sanitization

Live credentials (API keys, tokens) are redacted from content, summary, and metadata before anything persists. The embedding and the dedup fingerprint are both computed on the redacted text, so a leaked database yields no usable secrets anywhere — not even inside vectors.

Sanitization then strips unambiguous corruption (invalid UTF-8, control characters). Content that is empty after cleaning is rejected as invalid. With the opt-in quarantine enabled, script-salad content is _downranked_ instead of rejected — importance zeroed, `metadata.quarantined: true` — so it sinks in recall but stays inspectable.

## The value gate

Client integrations auto-capture conversation turns as episodic memories, and raw turns arrive polluted. Two hygiene steps run before storage:

1. **Boilerplate stripping** (turn captures only): injected context wrappers, `<system-reminder>` blocks, and stylized banner output are cut from the content. A capture that was boilerplate all the way through is dropped outright.
2. **The episodic floor**: an `episodic` write whose _signal_ — content with role labels and whitespace stripped — is under `MEMINI_EPISODIC_MIN_CHARS` (default 120) is dropped. "keep going" and "ok" turns never reach the store, never cost an embedding, and never pollute recall for 30 days.

A dropped write is **accepted, not stored** — it is not an error. The response says so, and reports the tier the write resolved to (including an auto-classified one), so the caller knows exactly what was decided:

```json
{ "id": "", "tier": "episodic", "stored": false }
```

Only `episodic` is gated; durable writes and promotion are unaffected, and the floor is tunable (0 disables it).

## Deduplication, twice

**Exact restatement — before embedding.** A fresh write (not an update by ID) is fingerprinted: SHA-256 over content normalized for case and whitespace. If a live memory in the same tier already carries that fingerprint, nothing new is stored — the existing memory is reinforced and corroborated in the background, and the response returns _that_ memory with `reinforced: true`. The `id` in the response names a memory the caller did not write; without the flag, an agent would believe it created a new fact. Since this runs pre-embed, an exact repeat costs no embedder call.

**Near duplicates — after embedding.** For fresh writes with a vector, the five nearest same-tier memories are checked against `MEMINI_WRITE_DEDUP_SCORE` (default 0.625). When the best candidate crosses it, `MEMINI_WRITE_DEDUP_ACTION` decides:

- `hint` (default) — the write proceeds, and the response carries a `merge_hint` naming the similar memory, its score, and a content preview, so the agent (or a human) can merge via `memory_update`. Hints are computed for durable tiers only — that is where the threshold was calibrated and where anyone reads them; the per-turn capture flood never pays the vector search.
- `coalesce` — the write folds into the existing memory (`reinforced: true`) — unless the incoming phrasing is strictly richer, in which case the new copy is stored and the old one superseded, with earned confidence carried forward.
- `supersede` — the new memory is stored and the near-duplicate is tombstoned. Deliberately deferred until _after_ the new row commits: a failed insert must never drop the old fact without a stored replacement.
- `off` — no vector dedup.

## Embedding, and life without one

The content embed is bounded by a write timeout. On a timeout or error — including an embedder that is not configured at all — the write is **not failed**. It is stored as a vectorless row flagged `pending_embed`, the response carries `degraded: "pending_embed"`, and a background repair worker is woken immediately to backfill the vector once the embedder recovers. Until then the memory is keyword-searchable only.

Running with no embedder at all is the same story continuously: a supported, degraded, keyword-only mode — noisier recall, but nothing errors. See [recall](./recall.md) for how degraded search behaves, and [deployment](../operations/deployment.md) for choosing a mode.

Vectorless writes skip everything that needs a vector: near-duplicate dedup, sync consolidation, and the corroborate/contradict routing below.

## After the row lands

Everything after the commit is asynchronous and best-effort — the write's latency never pays for it:

- **Auto-supersede.** If the dedup gate named a near-duplicate to replace, it is tombstoned now, safely after the replacement is durable.
- **Consolidation** (opt-in, LLM). A fresh durable write can be merged or contradiction-resolved against existing memories, either before the caller sees the result (sync) or in the background (async).
- **Fact building.** Fresh short-term captures are distilled (LLM) or marker-extracted (heuristic) into durable facts at write time, so knowledge doesn't have to wait for the batch promoter.
- **Corroborate / contradict nearest.** A short-term write that restates an existing durable fact grows that fact's confidence; a durable write that _contradicts_ one (changed value, flipped polarity) invalidates the stale fact so the new one outranks it.

What happens to the memory from here — reinforcement, promotion, demotion, decay, and time-travel — is the subject of [lifecycle](./lifecycle.md). For a single fact traced through its whole life, see [the life story of a memory](../examples/memory-life-story.md).

## Source map

For contributors; the body above deliberately names no code.

| mechanism                                  | where                                                                                                                     |
| ------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------- |
| pipeline orchestration                     | `internal/service/service.go` — `Remember`, `validateRememberInput`                                                       |
| tier classification                        | `internal/extract/extract.go` — `ClassifyWith`, `markerSets`, `hedgePatterns`, `roleScaffold`                             |
| classification stamp                       | `internal/service/service.go` — `stampClassifiedTier`                                                                     |
| visibility routing + tier clamp            | `internal/service/visibility.go` — `resolveVisibility`                                                                    |
| secret scrubbing / sanitization            | `internal/service/service.go` — `scrubInput`, `sanitizeContent`; `internal/redact`, `internal/sanitize`                   |
| capture stripping + episodic gate          | `internal/service/capture_hygiene.go`, `internal/service/episodic_gate.go`                                                |
| fingerprint dedup                          | `internal/service/service.go` — `fingerprintHit`; `internal/memory/types.go` — `Fingerprint`                              |
| vector dedup                               | `internal/service/service.go` — `runSplitDedup`, `dedupCheck`                                                             |
| degraded embed + repair                    | `internal/service/service.go` — `embedForRemember`; `internal/service/repair.go`                                          |
| post-commit jobs                           | `internal/service/service.go` — `autoSupersede`, `buildFactsOnWrite`, `corroborateNearestAsync`, `contradictNearestAsync` |
| effective-tier reporting on dropped writes | `internal/service/service.go` — `reportEffectiveTier`; `internal/api/mcp/mcp.go`, `internal/api/rest/rest.go`             |
