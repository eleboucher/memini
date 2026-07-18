# Tuning recall

Recall is bad and you want to know what to turn. This guide is organized by
symptom, because that is what you actually have: a complaint, not a hypothesis.

## Before you turn anything

```sh
memini doctor
```

The single most common cause of "memini forgot everything" is not ranking. It is
a **namespace mismatch**: the agent is writing to one namespace and recalling
from another, so retrieval is working perfectly against an empty set. `doctor`
exists to catch exactly this. It prints the namespace the server resolves without
a header, the namespace the plugin resolves in this directory, the effective read
set, and the per-namespace memory counts, and it warns when the namespace recall
uses is empty while another one holds the bulk of the store.

Do not tune ranking until `doctor` is clean. A retrieval knob cannot fix a
pointer problem, and every hour spent on `MEMINI_RECALL_MIN_SCORE` while writes
land in `default` is an hour wasted.

---

## Nothing comes back at all

Three causes, in the order they are worth checking.

**Dimensionality mismatch.**
[`MEMINI_EMBED_DIMS`](../reference/configuration.md#memini_embed_dims) must match
the model your embeddings endpoint actually serves. The default (`1536`) suits
`text-embedding-3-small`. Point at a 768- or 1024-dimension model without changing
it and memini does not error: it writes meaningless vectors and recall degrades
with no signal anywhere. This is the most common setup failure in memini, full
stop. It cannot be repaired by re-embedding either, because dimensionality is
baked into the store's shape; a wrong value means export, fresh store, import.

**The embeddings endpoint is down or slow.** Recall does not fail when the query
embed fails. It falls back to keyword-only search and stamps a `degraded` field
on the result (`keyword_only`, with a note explaining it). Every agent-facing
tool description tells the model to treat a `degraded` result as incomplete
rather than a confident negative, so an empty answer under `degraded` means "I
could not look properly", not "nothing is known". If you see `degraded` in tool
output, or `memini_recall_degraded_total` climbing in metrics, fix the embedder
before touching anything else.

Note that a _slow_ embedder degrades just as silently as a dead one:
[`MEMINI_RECALL_EMBED_TIMEOUT`](../reference/configuration.md#memini_recall_embed_timeout)
defaults to `2s`, past which recall gives up on the vector leg rather than hang.
That default protects latency; on a loaded self-hosted GPU it can also quietly
turn every recall keyword-only.

**The score floor is too high.**
[`MEMINI_RECALL_MIN_SCORE`](../reference/configuration.md#memini_recall_min_score)
drops candidates below a fused score before ranking. The default is `0.1`, which
is the benched value. It is exposed so a deployment on a different embedder can
_raise_ it to trim loosely relevant injection. If someone raised it to 0.5 to
"clean up recall", that is why recall is now empty.

---

## Results come back, but they are irrelevant

This is the case reranking was built for, and it is worth being precise about
when it helps.

```sh
# server (the memini process)
export MEMINI_RERANK=http://reranker:8002/v1   # a cross-encoder /rerank endpoint
export MEMINI_RERANK_MODEL=qwen3-reranker-0.6b
export MEMINI_RERANK_POOL=50
export MEMINI_RERANK_MAX_BATCH_CHARS=120000
```

[`MEMINI_RERANK`](../reference/configuration.md#memini_rerank) takes three kinds
of value: `off` (default), a cross-encoder `/rerank` base URL, or `llm` (reorder
with the configured chat model, which requires `MEMINI_LLM_BASE_URL` and fails
loudly at boot without it).

**`MEMINI_RERANK_POOL` is the setting people miss.** It is how many
composite-ranked candidates get handed to the reranker before the list is
truncated to the recall limit. The default, `0`, reranks exactly the limit: the
reranker reorders the results you were already going to return, and can never
surface a memory that fused retrieval placed just below the cut. That rescue is
most of what a cross-encoder is for. Set the pool and it can reach down and pull
that memory up. The natural ceiling is around 50, because recall never retrieves
a deeper pool than that. Cost is linear, one model forward pass per candidate, so
a deep pool trades recall latency for accuracy.

If you set a deep pool, raise
[`MEMINI_RERANK_MAX_BATCH_CHARS`](../reference/configuration.md#memini_rerank_max_batch_chars)
too. It defaults to `6000` and is an HTTP payload guard, not a context-window
guard: a Cohere-style `/rerank` server scores each (query, document) pair in its
own forward pass, so the model's context bounds a pair, never the batch. Leave it
at 6000 with a 50-deep pool and the pool shards into many _serial_ requests, which
is a far better way to blow
[`MEMINI_RERANK_TIMEOUT`](../reference/configuration.md#memini_rerank_timeout)
(default `10s`) than a large body ever was. On timeout, recall silently falls back
to composite order, so a mis-sized batch cap presents as "the reranker does
nothing".

**Keep the client's timeout above the server's.** That composite-order fallback
only reaches the caller if the caller is still listening. Clients bound each
memini call by `MEMINI_TIMEOUT_MS` (the `request_timeout_ms` client setting,
default `30000`); if that is _below_ `MEMINI_RERANK_TIMEOUT`, a slow rerank makes
the client hang up first and recall returns **nothing at all** rather than an
unranked result — the reranker presents as "memory is broken", not "memory is
slow". Layered timeouts only degrade gracefully when the outermost one is the
longest, so raise the client ceiling whenever you raise the server's rerank
budget. A deployment can push the new value to every client at once by setting
`request_timeout_ms` in the server's global client defaults (`PUT
/v1/settings/defaults`, or the admin UI's Settings view) instead of asking each
user to export the env var.

**Reranking only helps where base recall has headroom.** From
[the benchmarks](../../bench/README.md), on the same all-MiniLM-L6-v2 endpoint:

|                  | LongMemEval (session) | LoCoMo (turn-level)    | added p50     |
| ---------------- | --------------------- | ---------------------- | ------------- |
| hybrid (default) | 98.4 / 99.2 / 93.0    | 59.7 / 69.9 / 42.4     |               |
| + cross-encoder  | 98.4 / 99.2 / 93.1    | **70.9 / 75.0 / 59.8** | 20 to 230 ms  |
| + LLM rerank     | 98.4 / 99.2 / 93.0    | **74.4 / 76.5 / 67.4** | 350 to 420 ms |

(`recall_any@5 / @10 / MRR`, in percent.) On the session-level set, where hybrid
is already at ceiling, reranking is a measured no-op: it costs latency and buys
nothing. On turn-level retrieval, where base recall has real room, the
cross-encoder is worth +11pp R@5 and +17pp MRR, and the LLM buys a few points
more for roughly ten times the latency. The cross-encoder is the recommended
production reranker: most of the lift, a fraction of the cost, and no chat model.

Before you reach for a reranker, ask whether your recall is at ceiling. If it is,
the reranker will not save you, and the problem is upstream.

### Gating on rerank score

A reranker that only reorders still returns _something_ for every query — on a
query with no real answer, that something is the least-irrelevant noise, ranked
confidently. The fused retrieval score cannot cut this tail:
[`MEMINI_RECALL_MIN_SCORE`](../reference/configuration.md#memini_recall_min_score)
operates on min-max-normalized scores, so the best of a bad pool always
normalizes into competitive territory. Cross-encoder scores are different: they
are calibrated absolute relevance, and on a well-separated model a true hit
scores ~0.9 while noise sits near zero.

[`MEMINI_RERANK_MIN_SCORE`](../reference/configuration.md#memini_rerank_min_score)
puts that separation to work: candidates scoring below the threshold are
dropped, across the whole `MEMINI_RERANK_POOL` before the recall limit applies.
When everything gates out, recall returns **empty** — "nothing relevant exists"
— instead of falling back to the ungated composite order. Empty-by-gate is
recorded as `rerank_result{result="empty"}` in the metrics, distinct from
`ok`/`fallback`, so you can watch the gate's collapse rate; a rerank _failure_
(timeout, dead backend) still falls back to composite order, because a dead
reranker never rendered a verdict.

Two constraints. It is cross-encoder-only — `MEMINI_RERANK=llm` returns an
ordinal list with no scores, so combining it with the gate is a boot error
rather than a gate that silently never fires. And the threshold is
**model-specific**: score distributions differ between rerankers, so pick it
from a sweep against your own backend rather than copying a number. The bench
harness runs the sweep directly — per threshold it reports positive recall@k
(gold answers that survive the gate) against the negative injection rate
(foreign-namespace noise that sneaks through):

```sh
go run ./cmd/bench -suite longmemeval -data ./longmemeval_s.json \
  -rerank-url http://reranker:8002/v1 -rerank-model your-reranker \
  -rerank-gate "0.05,0.1,0.2,0.3,0.5" -rerank-gate-pool 20
```

(Dataset acquisition and the other suites are covered in
[the bench README](../../bench/README.md).)

Pick the highest threshold whose positive recall is still at your no-gate
baseline; the negative injection column shows what you are buying. A
miscalibrated threshold silently empties recall — the default is `0` (off) for
exactly that reason. The response `score` field is unaffected: it still
carries the fused retrieval score, never the rerank score.

---

## It misses queries that are worded semantically

The memory is there, but a paraphrased question does not find it.

**Durable facts are being crowded out by chatter.**
[`MEMINI_RECALL_SEMANTIC_RESERVE`](../reference/configuration.md#memini_recall_semantic_reserve)
(default `2`) reserves up to N recall slots for durable tiers so consolidated
knowledge is not buried under episodic noise. The reserved slots are
relevance-gated, so a durable memory is only promoted in when it is competitive
with the entry it displaces. Set it to `0` for pure-relevance recall, or raise it
if your store is durable-heavy and you want more of it surfaced by default.

**Your embedder is asymmetric and you are not telling it so.**
Instruction-tuned embedders (Qwen3-Embedding, bge) expect a retrieval instruction
on the query and a bare document.
[`MEMINI_EMBED_QUERY_PREFIX`](../reference/configuration.md#memini_embed_query_prefix)
prepends one to recall queries only; documents are always embedded without it.
Measured on Qwen3-Embedding-8B, this was worth **+6.0pp R@5 on the LongMemEval
vector leg** (91.2 to 97.2) and +1.0pp MRR on the fused ranking:

```sh
# server (the memini process)
export MEMINI_EMBED_QUERY_PREFIX=$'Instruct: Given a user query, retrieve relevant memories that answer it\nQuery:'
```

**Or your embedding model is simply weak.** The vector leg is doing most of the
work on paraphrase, and a small model is bad at it. Move to a better one, then run
`memini reembed` to migrate the store. Bear in mind that changing the model is
allowed; changing the _dimensionality_ still is not.

---

## Stale facts outrank current ones

Somebody changed a decision, and recall keeps serving the old one.

[`MEMINI_CONTRADICT_DOWNRANK`](../reference/configuration.md#memini_contradict_downrank)
is **on by default** and is what should be handling this. When a fresh durable
write contradicts a stored durable fact (a changed value or a flipped polarity,
confirmed by a lexical detector), memini stamps the stale fact's `valid_to`: it
leaves live recall while time-travel (`as_of`) can still reach it, and its
confidence is shrunk. It is reversible and precision-first, with zero restatement
misfires measured in the benchmark suite. It has no threshold to tune. It has an
off switch, and if someone flipped it, that is your bug.

The benchmark for this is worth quoting, because it explains why confidence alone
was never going to be enough: with an entrenched old fact and a fresh contradicting
write, the stale fact outranked the fresh one in 10 of 12 topics. Shrinking
confidence only halved that. Stamping `valid_to` took it to 0 of 12.

Two other things in this neighborhood:

- [`MEMINI_STABILITY_K`](../reference/configuration.md#memini_stability_k)
  (default `1`) stretches a short-term memory's recall half-life with
  reinforcement, so a frequently recalled memory decays more slowly. It only
  affects short-term tiers with a nonzero access count. Set `0` for a fixed
  half-life, if you want aged episodic memories to fall away regardless of how
  often they were used.
- The **`memory_history`** MCP tool traces a memory's bi-temporal supersession
  lineage by ID: the fact, everything it superseded, and everything that
  superseded it, oldest first, tombstones included. When you want to know what was
  believed before and what replaced it, that is the tool. It is the fastest way to
  confirm a contradiction actually fired.

---

## My durable facts vanished after a week

Read this one carefully, because the old documentation got it wrong.

[`MEMINI_DEMOTE_AFTER`](../reference/configuration.md#memini_demote_after) defaults
to **`168h`** (seven days). It is **not** `0`, and it is **not** disabled by
default. The sweeper demotes a durable memory back to the episodic tier when all
of the following are true:

- it is older than `MEMINI_DEMOTE_AFTER`, and
- it has never been recalled, and
- it is not marked important, and
- it is uncorroborated (its confidence is below the demote floor).

The intent is that a low-quality bulk import ages out on its own while facts the
agent actually uses, or that were established through corroboration, are kept.
Anything recalled even once is reinforced and survives. But a semantic fact that
was written, never read, and never restated will be episodic seven days later, and
episodic memories carry a 30-day TTL, so eventually it is gone.

If you are importing a corpus you want to keep unconditionally, or you simply
want durable to mean durable:

```sh
# server (the memini process)
export MEMINI_DEMOTE_AFTER=0        # disable demotion entirely
```

`memini doctor` prints a `LOW-CONF` column per namespace: durable memories whose
confidence sits below the demote floor. That column is your exposure. If it is
large and your store is mostly imported, demotion is going to eat it.

---

## Duplicates everywhere

Two independent mechanisms, and they are easy to confuse.

**Write-time dedup** fires on every fresh write, comparing it to its nearest
same-tier memory. It is one threshold plus one action:

- [`MEMINI_WRITE_DEDUP_SCORE`](../reference/configuration.md#memini_write_dedup_score)
  (default `0.625`) is the fused similarity at or above which the write counts as
  a near-duplicate. `0` disables write-time dedup entirely. The right value is
  embedder-dependent: around 0.9 collapses near-identical restatements only, while
  the 0.625 default was calibrated for merge hints in `bench/dedup_test.go`.
- [`MEMINI_WRITE_DEDUP_ACTION`](../reference/configuration.md#memini_write_dedup_action)
  decides what happens at or above it. `hint` (default) stores the write and
  returns a merge hint the agent can act on with `memory_update`; it is
  non-destructive and applies to durable tiers only. `coalesce` reinforces the
  existing memory and drops the write, which is the headless corpus-hygiene
  choice and wants a high score like 0.9. `supersede` stores the write and
  tombstones the old memory (new wins). `off` disables it (the exact-restatement
  fingerprint pass still runs, always).

**The periodic dedup pass** collapses whole near-duplicate clusters across the
store, keeps one representative, and tombstones the rest (reversibly, pointing
each at the representative).
[`MEMINI_DEDUP_SIMILARITY`](../reference/configuration.md#memini_dedup_similarity)
(default `0.85`) is cluster membership: raise it if the pass is collapsing
memories that were only superficially alike, lower it if obvious restatements
survive. It runs daily by default. To try a threshold without waiting, run it
on demand and read the report:

```sh
curl -X POST "$MEMINI_URL/v1/dedup" -H "X-Memini-Namespace: acme/phoenix" \
  -H 'Content-Type: application/json' -d '{"similarity": 0.9, "dry_run": true}'
```

---

## Agent noise drowns out the real facts

Recall is full of "ok", "keep going", and the agent parroting back what it said
thirty seconds ago.

- [`MEMINI_EPISODIC_MIN_CHARS`](../reference/configuration.md#memini_episodic_min_chars)
  (default `120`) drops an episodic write whose substantive content, once role
  scaffolding is stripped, is shorter than this. Only episodic is gated. Raise it
  if per-turn chatter still dominates; set `0` to keep everything.
- [`MEMINI_TURN_ECHO_WINDOW`](../reference/configuration.md#memini_turn_echo_window)
  (default `5m`) drops freshly captured turns from recall. A turn captured two
  minutes ago is live context, not memory, and returning it makes the agent
  repeat itself. Callers can opt out per call with `include_fresh_turns`. Zero
  disables it server-wide.
- [`MEMINI_SHORT_TERM_CAP`](../reference/configuration.md#memini_short_term_cap)
  (default `1000`) bounds working plus episodic memories per namespace; over the
  cap, the sweeper evicts the lowest-retention rows. This is the backstop that
  keeps one chatty agent from burying a project's durable knowledge under its own
  transcript.

---

## The same memory keeps getting injected

A memory that is already in context keeps coming back: the session-start
briefing surfaces it, the next prompt's recall surfaces it again, then a burst
of `Edit`s on one file surfaces it a third time. It is the right memory; it is
just being restated into context it already occupies.

This is what the **repeat-injection cooldown** is for, and the case for it is
measured. On one production namespace, 68% of the memories a recall served had
already been served to the same session within the previous minute, and a
30-minute window covers 89% of the observed re-serves. Left unchecked, that is
context budget spent repeating what the model can already see.

Three client behavior settings shape it. Like every behavior setting they are
server-resolved: set them globally (`PUT /v1/settings/defaults`), per key
(`PUT /v1/self/settings`), or as a per-client debug override in the env.

- **`inject_cooldown_ms`** (`MEMINI_INJECT_COOLDOWN_MS`, default `1800000`, 30
  minutes) is the **time** window: an injected memory is held back until this
  long after its last injection. `0` disables the time dimension.
- **`inject_cooldown_prompts`** (`MEMINI_INJECT_COOLDOWN_PROMPTS`, default `3`)
  is the **prompt** window, counted in user turns. `0` disables the prompt
  dimension. Some harnesses have no user-prompt boundary — a gateway that runs
  per agent step, for one — and there the prompt dimension is inert and the time
  window carries re-admission alone.
- **`inject_pretool_gate_ms`** (`MEMINI_INJECT_PRETOOL_GATE_MS`, default `90000`,
  90 seconds) is a separate lever on the pretool hook: it skips the recall
  **server call** for a file whose last call was younger than the gate. `0`
  always calls.

**The re-admission rule is suppress-on-either, re-admit-on-both.** A memory
stays suppressed while it is inside _either_ window, and re-surfaces only once
_both_ have lapsed — so a fact comes back after the conversation has genuinely
moved on, not five seconds later and not never. Set both cooldown knobs to `0`
and the cooldown collapses to "suppress for the rest of the session", the
pre-cooldown behavior. One thing bypasses the windows on the plugin's per-file
(PreToolUse) surface: an in-place edit to a memory's content (`memory_update`)
re-injects it immediately there, because its content changed and the model should
see the new version. That is the only surface that carries a content identity to
compare; the plugin's prompt-recall path and the id-only surfaces (hermes, pi,
openclaw) re-admit an updated memory only once the windows lapse.

**The gate's tradeoff is real.** The pretool call gate saves the round-trip on a
file being edited repeatedly, but it also means a memory you just saved about
that file can be invisible on it for up to `inject_pretool_gate_ms` — no call is
made, so nothing finds it. Shorten the gate if same-session save-then-see matters
more than the saved calls; lengthen it on a slow server or a heavy-edit workflow.

To confirm the cooldown is working, read the activity log: a recall event that
applied the exclusion carries `detail.excluded_count`, the number of in-cooldown
ids the client asked the server to skip.

```sh
curl "$MEMINI_URL/v1/activity?kind=recall&limit=20" -H "X-Memini-Namespace: acme/phoenix"
```

The re-serve fraction — a memory served to a session it was already served to
minutes ago — should sit near zero with the defaults in place, where before it
was most of the traffic.

---

## Do not reach for these

Three settings that a stale blog post, an old README, or a confident LLM will tell
you to set. They are **removed**. Setting them does nothing except produce a
startup warning, and they are now baked defaults tuned through the benchmark
harness:

| Removed                            | Now                                                                      |
| ---------------------------------- | ------------------------------------------------------------------------ |
| `MEMINI_FUSION_ALPHA`              | a baked retrieval default (0.5); tune via the benchmark harness, not env |
| `MEMINI_TEMPORAL_BOOST`            | a baked retrieval default (0.40)                                         |
| `MEMINI_RECALL_MIN_SEMANTIC_SCORE` | a baked retrieval default (0, off)                                       |

A separate pair, `MEMINI_GLOBAL_NAMESPACE` and `MEMINI_TENANT_SHARED`, are
**fatal at boot** rather than ignored: the scope model underneath them changed, so
booting as though they were unset would silently change what every recall sees.
The full list, and what replaced each one, is in
[upgrading](../operations/upgrading.md) and the
[removed settings](../reference/configuration.md#removed-settings) table. Read the
boot log after any upgrade: a deprecation warning means a tuning value you thought
was applying is not.

---

## Measure, do not guess

Every number in this guide came from
[`bench/README.md`](../../bench/README.md), which is a real harness you can run
against your own embedder and your own data. If you are about to change a
retrieval setting for a whole team, run the scoreboard first. Ranking changes in
memini itself are not made until the quality scoreboard moves in the right
direction on at least two embedders, and your deployment deserves the same bar.

For a single bad recall, the activity log answers "why did it return that".
[`MEMINI_ACTIVITY_LOG`](../reference/configuration.md#memini_activity_log) is on
by default and records every read and write: a recall event carries the query that
ran and, for each memory it served, **the rank and the composite score it was
served at**. That is the "why" no per-memory access counter can reconstruct.

```sh
curl "$MEMINI_URL/v1/activity?kind=recall&limit=20" -H "X-Memini-Namespace: acme/phoenix"
```

The same feed backs the Activity page in the admin UI. Between `memini doctor` for
the pointer questions and `/v1/activity` for the ranking ones, you should rarely
have to guess.
