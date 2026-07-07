# Coding-agent-memory pilot benchmark

A pilot benchmark whose corpus is memini's _own_ development history, built to do
what LongMemEval and LoCoMo cannot: discriminate memory configurations on an
in-domain, update-rich, distractor-dense corpus with real retrieval headroom.

## 1. Motivation

Three consecutive LLM experiments — Phase-4 multi-hop, the WP6 agentic answer
loop, and the distill-on-write A/B — each bottomed out in "at the 100% recall
ceiling" or "inside the n=50 noise floor." The instrument was the bottleneck:

- **LongMemEval is retrieval-saturated.** Hybrid recall@5 is ~100% on the held
  split, so nothing downstream (a better answerer, a better write path) can move
  a number that is already maxed.
- **It is off-domain.** Persona-conversation QA ("what did the user say about
  their dog") is not coding-agent memory (decisions, conventions, rationale,
  evolving config).

No LLM feature can prove its worth against a bench that can't distinguish
interventions. This pilot is the instrument: a small, honest, hand-verified
benchmark that reflects coding-agent memory and separates configurations
LongMemEval couldn't.

## 2. Corpus

**Source (signed off): memini's own dev history.** It is genuinely in-domain
(a coding project's memory), and it carries real, dated contradictions and
evolving state — the hard part for a memory benchmark. Sources mined:

- **git log** (433 commits, 2026-06-09 → 2026-07-06): rationale-bearing
  conventional-commit subjects and bodies. ID `c-*` / `n-*` with `source: git:<hash>`.
- **memory notes** (19 curated notes under the project memory dir): decision
  reversals and negative results, already modeling temporal supersession.
  `source: note:<file>`.
- **plan files** and **bench/README.md** results where a decision/number is stated.

**Mining rule (anti-circularity).** Item content is a faithful transcription /
normalization of a primary source (expand pronouns, add a `[date]` prefix, split
multi-fact notes into atomic statements). **No LLM generated any corpus fact, and
the LLM judge (the `MEMINI_LLM_*` model) authored none of the corpus.** Questions
and gold answers are hand-authored and hand-verified against each item's `source`
pointer. Abstention questions were verified genuinely-absent with a recorded grep
(see each abstention `provenance`).

**Pilot corpus:** 103 items, one namespace (`memini-dev`) so every question
retrieves against the whole corpus — the structural difference from LongMemEval's
per-question private haystack, and the source of the retrieval headroom. Dense
topic overlap (many recall/dedup/rerank/contradict items) supplies natural
near-duplicate distractors.

**Scaled corpus (`codingagent_v1.json`, 2026-07-07):** 206 items / 166 questions,
same mining rules and namespace, built by mining previously-unmined commits, new
temporal-supersession chains, docs, and memory notes, plus a synthesis-question
pass over the whole item set. Every mined item carries a `verify` verbatim
excerpt that the merge step (`.bench-mining/merge_verify.py`) checks against its
cited source (`git show` / note / doc) before admission — 0 rejects. Selected via
`MEMINI_CODINGAGENT_DATA`. This is the corpus the §7.1 result runs on; the pilot
(`codingagent_pilot.json`) is retained as the default.

**Contradiction chains (10)** — each version is its own dated item with a
`superseded_by` link; `gold` for the temporal-update question is the _latest_
version only:

| fact                                     | old → new                                 | source            |
| ---------------------------------------- | ----------------------------------------- | ----------------- |
| `MEMINI_RECALL_SEMANTIC_RESERVE` default | 0 → 2                                     | git:fe0c9d4       |
| contradiction action                     | confidence-shrink → valid_to invalidation | git:662c632       |
| contradiction cooldown key               | UpdatedAt → CreatedAt                     | git:ced00b4       |
| `MEMINI_DEMOTE_AFTER` default            | 0/off → 168h                              | git:2033bd0, note |
| integration recall default               | 5-uncapped & 3/800 → 3-uncapped           | git:01fb279       |
| tier-omitted write tier                  | working → episodic                        | git:a8c11f8       |
| `MEMINI_RERANK_TIMEOUT` default          | 3s → 10s                                  | git:f8e5315       |

## 3. Schema

Extends the normalized bench schema (`bench/dataset.go`), loaded by
`bench.LoadCodingAgent` (`bench/codingagent.go`):

- **items**: `id, content, group, time` (RFC3339, required), `session`
  (dev-session key, passed as `session_id` metadata on write ingest), `source`
  (provenance), `kind`, optional `superseded_by`.
- **questions**: `query, gold, group, answer, category, now` (required),
  optional `gold_all` (full evidence set for synthesis; drives coverage@k),
  `provenance`.
- **gold semantics** — `gold` = item ids for recall (any-hit); for
  temporal-update it holds only the latest version, so retrieving the stale item
  is not credited. `gold_all` = the whole evidence set for synthesis. `answer` =
  hand-authored gold string for the LLM judge; abstention has empty gold and
  empty answer.

**Categories (7):** decision, convention, rationale, current-state, synthesis
(≥2 evidence memories), temporal-update (answer depends on when / what
superseded what), abstention (genuinely not in the corpus). Pilot: 45 questions
(decision 8, convention 7, rationale 8, current-state 6, synthesis 6,
temporal-update 6, abstention 4). Scaled v1: 166 questions (decision 28,
convention 23, rationale 26, current-state 26, synthesis 32, temporal-update 16,
abstention 15) — weighted toward synthesis, the highest-headroom category.

## 4. Metrics

- **recall@{1,5,10}** and per-category recall via the existing `bench.Run`.
- **coverage@k** over `gold_all` for synthesis (recall_any credits one hit;
  synthesis needs the whole set).
- **LLM-judged QA accuracy** per category, with per-category rubrics
  (`bench.JudgeSystemFor`): base fact-equivalence with a paraphrase clause
  (distill rewrites content), knowledge-update/temporal-update must state the
  updated value, abstention must decline.
- **token cost** — `CountingDistiller` (distill ingest) and `CountingChat`
  (answer path).
- **paired McNemar exact p** on the discordant counts for each contrast
  (`bench.McNemarExact`), because n≈45 is inside the chi-square approximation's
  unreliable range.

## 5. Pre-registered criteria (committed before Test C is run)

**Headroom gate (Test B):** hybrid-upsert recall@5 must land in **[50%, 88%]**.
≥90% → add synthetic distractors and re-run once. <40% → gold too narrow,
re-mine. Only a corpus in band proceeds.

**Observed headroom (2026-07-06, qwen3-embedding-0.6b, 1024-d):**

| ingest | recall@5 (hybrid) | verdict |
| ------ | ----------------: | ------- |
| upsert |         **86.7%** | IN BAND |
| write  |             84.4% | —       |

Per-category recall@5 is ~100% for the single-fact categories (decision,
convention, current-state, rationale, temporal-update) but synthesis
coverage@5 = 56% / coverage@10 = 67%. So retrieval is _not_ the bottleneck for
single-fact lookup (as intended — that mirrors the real world), and the
retrieval headroom that remains is concentrated in synthesis. The discrimination
for the single-fact categories therefore has to come from **answer quality**
(does the model state the _updated_ value for temporal-update; does it _decline_
for abstention; does it _combine_ memories for synthesis), not from recall — which
is exactly the axis LongMemEval's saturation hid.

**Discrimination (Test C).** After the pilot (§7.0) the design changed twice, for
diagnosable reasons:

- The **distill ingest axis was retired.** The pilot distiller emitted 0 facts on
  all 67 calls (the corpus is fact-shaped commit/note text with nothing to
  compress), so the distill cells were a tie by construction. Keeping it would
  only re-measure that inertness.
- A third **answer strategy** was added: **`expand`** — one query-rewrite
  completion, a unioned multi-query recall, and one synthesis pass, with no tool
  loop. It is the cheap form of what the agentic loop does the expensive way, and
  the pre-registered question became: does the full ReAct loop beat this cheap
  arm enough to justify its cost?

So Test C is a **3-way over the extract ingest**: single-shot vs `expand` vs the
gated agentic loop. Pre-registered **go/no-go** (the standing stop condition):

> The agentic loop ships **on** only if it (C2) clears **p < 0.05 over single-shot**
> **and** (C3) justifies its cost over the cheap `expand` arm. If either fails,
> **LLM-answer is settled**: it ships **opt-in / off-by-default** and we stop
> investing. A null is a valid, decision-closing result — not a failure to iterate.

## 6. Run commands

The answerer must be a **capable** model — a mini tier (the local 9B) makes the
loop look better than it is by leaving a weak single-shot baseline for it to beat
(see §7). The scaled run used the embedder local and the LLM via the litellm
gateway.

```sh
# Embedder local (keep co-resident to avoid JIT thrash); LLM via litellm.
export MEMINI_EMBED_BASE_URL=http://127.0.0.1:8001/v1 \
       MEMINI_EMBED_MODEL=text-embedding-qwen3-embedding-0.6b MEMINI_EMBED_DIMS=1024
export MEMINI_LLM_API=openai MEMINI_LLM_BASE_URL=https://litellm.erwanleboucher.dev/v1 \
       MEMINI_LLM_API_KEY=$LITELLM_API_KEY MEMINI_LLM_MODEL=neuralwatt/qwen3.6-35b-fast

# scaled corpus (166 q); omit to use the 45-q pilot
export MEMINI_CODINGAGENT_DATA=data/codingagent_v1.json

# 1. gold audit (offline, instant — run after every dataset edit)
go test -tags bench ./bench -run TestCodingAgentGoldAudit -v
# 2. headroom (embedder only)
go test -tags bench ./bench -run TestCodingAgentHeadroom -v -timeout 20m
# 3. discrimination 3-way (embedder + LLM; checkpointed/resumable)
go test -tags bench ./bench -run TestCodingAgentDiscrimination -v -timeout 6h
# ad-hoc single-arm rerun
go run ./cmd/qa -suite codingagent -data bench/data/codingagent_v1.json \
  -ingest write -reasoning [""|expand|medium] -k 10
```

## 7. Results

### 7.0 Pilot (2026-07-06, qwen3.5-9b, 45 q, the retired 2×2)

The pilot (45 q, extract×distill × single×agentic) put the ungated agentic loop
at **84% vs 76% single (+9pp, p=0.29)** — directionally real, underpowered, and
concentrated on decision/current-state/synthesis/temporal-update, while it
_regressed_ rationale (88→75%) by over-searching direct-answer questions. The
distiller emitted **0 facts on all 67 calls** (fact-shaped corpus → nothing to
compress), so both distill cells tied by construction. Those two findings drove
the redesign: an **early-exit gate** on the loop (answer directly unless the first
pass is insufficient), a cheap **`expand`** arm, retirement of the distill axis,
and a scaled corpus. One confound surfaced only on a capable model (below): the
reader prompt's hard "6-words-or-fewer" cap made synthesis answers impossible; the
9B masked it by disobeying, so the pilot's synthesis numbers are unreliable. Fixed
in `answerSystem` before the scaled run.

### 7.1 Scaled 3-way (2026-07-07, qwen3.6-35b via litellm, qwen3-embedding-0.6b, 166 q)

Answerer + judge = `neuralwatt/qwen3.6-35b-fast` (a **capable** tier — see §6).
Corpus `codingagent_v1.json` (206 items / 166 q; headroom recall@5 = 88.0%, IN
BAND). Per-category LLM-judged accuracy:

| category        |            single |            expand |           agentic |
| --------------- | ----------------: | ----------------: | ----------------: |
| abstention      |      100% (15/15) |      100% (15/15) |      100% (15/15) |
| convention      |       91% (21/23) |       96% (22/23) |       87% (20/23) |
| current-state   |       85% (22/26) |       85% (22/26) |       88% (23/26) |
| decision        |       82% (23/28) |       86% (24/28) |       89% (25/28) |
| rationale       |       92% (24/26) |       92% (24/26) |       96% (25/26) |
| synthesis       |       75% (24/32) |       81% (26/32) |       72% (23/32) |
| temporal-update |       81% (13/16) |       88% (14/16) |       81% (13/16) |
| **overall**     | **86% (142/166)** | **89% (147/166)** | **87% (144/166)** |

Pre-registered paired contrasts (McNemar exact):

- **C1** expand vs single: 89% vs 86%, **Δ +3pp**; A-only=6, B-only=1, tie=159; **p=0.125**.
- **C2** agentic(gated) vs single: 87% vs 86%, **Δ +1pp**; A-only=5, B-only=3, tie=158; **p=0.73**.
- **C3** agentic(gated) vs expand: 87% vs 89%, **Δ −2pp**; A-only=5, B-only=8, tie=153; **p=0.58**.

Cost (LLM only, ~4 B/token estimate):

| arm     | completes | tool-rounds | in tok | out tok |  s/q |
| ------- | --------: | ----------: | -----: | ------: | ---: |
| single  |       166 |           0 |   147k |    3.7k |  2.8 |
| expand  |       332 |           0 |   241k |    9.4k | 10.4 |
| agentic |       166 |          29 |   227k |    4.3k |  9.5 |

### 7.2 Verdict — the loop is NOT worth it (NO-GO / settle)

Both go conditions fail:

- **C2** agentic vs single is **+1pp, p=0.73** — nowhere near the p<0.05 bar.
- **C3** agentic vs expand is **−2pp** at essentially the **same cost** (9.5 vs
  10.4 s/q; 227k vs 241k input tokens) — so even against the cheap arm the full
  tool loop does not justify itself; it is slightly _worse_.

Against single-shot, the loop costs **+54% input tokens and 3.4× latency for a
non-significant +1pp.** The gate did its job — it made the loop non-regressive
(rationale 92→96, no category collapses) — but in doing so it removed almost all
of the pilot's apparent +9pp: on a capable model with the prompt confound fixed,
single-shot is already strong (86%) and the headroom the ungated loop exploited
was mostly the weak-baseline artifact the §7.0 pilot warned about.

The cheap **`expand`** arm is the best of the three (89%) and the only one that
beats single directionally, but at **+3pp, p=0.125** it too fails to clear
significance — and it doubles the completion count. It is not a win either.

**Decision (honoring the pre-registered stop condition): LLM-answer is settled.**
The agentic answer loop ships **opt-in / off-by-default** (`Reasoning` empty =
single-shot remains the default). `expand` stays available as an opt-in for
callers who want the small multi-query lift and can pay 2× completions. We stop
investing in the answer loop as a default-on feature. This is the pre-registered,
decision-closing null — the bench did its job: it turned "the loop feels helpful"
(the +9pp mirage on a weak model at n=45) into a powered, cost-aware "no."

## 8. What the instrument proved

1. **The bench discriminates and is now powered.** At n=166 the paired contrasts
   have tight confidence (tie counts ~155/166), so a +1–3pp effect is measured as
   _not significant_ rather than _unknown_ — the resolution LongMemEval's ~100%
   recall saturation could never provide.
2. **Model tier changes the answer.** The pilot's +9pp was largely a weak-baseline
   artifact: a capable answerer lifts single-shot enough to erase it. Any future
   answer-strategy claim must be measured on a capable model, not a mini tier.
3. **Prompt confounds hide in weak models.** The 6-word cap zeroed synthesis on
   the capable model while the 9B's disobedience masked it. Fixing it lifted
   single-shot synthesis to 75% — most of what the loop had appeared to add there.
4. **Retrieval is not the bottleneck** (single-fact recall ~100%); the residual
   headroom is in synthesis coverage (@5≈80%), and none of the three answer
   strategies closes it — a retrieval/consolidation problem, not an answering one.

**Do not** reopen the answer loop as a default-on investment without a materially
different corpus or model regime that moves C2 past p<0.05. The distill-on-write
axis remains parked (needs a turn-shaped corpus + a distiller verified to emit
facts) — unchanged from the pilot.

## 9. Write-time synthesis spike (negative)

Follow-up to §8.4: since the synthesis headroom is a retrieval/consolidation gap,
not an answering one, could precomputing the combined fact at **write time** —
one retrievable durable memory — beat doing it at answer time (which §7 showed
does not pay)? This is honcho's "dreamer" inductive half, minus the tool loop
(`bench/synthesize.go`, `TestCodingAgentSynthesis`). It clusters the corpus by
nearest-neighbour proximity and, per cluster, makes one LLM call that emits facts
entailed by ≥2 members, stored as new `Level=deduced` semantic memories. It is
**question-blind** (sees only the store) — the validity crux.

A/B on `codingagent_v1`, both arms single-shot (66 facts synthesized from 46
clusters, ~17k/6k tokens):

| category                |      baseline |    +synthesis |                Δ |
| ----------------------- | ------------: | ------------: | ---------------: |
| **synthesis (primary)** |   69% (22/32) |   69% (22/32) | **+0pp** (p=1.0) |
| decision                |   82% (23/28) |   96% (27/28) |  +14pp (p=0.125) |
| overall                 | 86% (142/166) | 87% (144/166) |    +1pp (p=0.80) |

_(other categories moved within noise: convention +4, current-state −4, rationale
−4, temporal −6, abstention +0 — each ≤2 questions, all p=1.0.)_

**Verdict: NO.** Precomputing combined facts did not move the primary endpoint
(synthesis +0pp). Two reasons, both diagnosable: (1) question-blind bottom-up
synthesis produces facts that don't align with the specific combinations the
questions ask — the pre-registered validity risk, realised; (2) the single-shot
reader already combines the retrieved episodics to 69% on its own (coverage@5≈80%),
so a precomputed combination adds nothing on top. The synthesis headroom is real
at the retrieval level but resists both answer-time and write-time attack on this
corpus. **Guardrail:** no systematic poisoning from the 66 added facts.

**One live thread, not chased:** synthesized durable facts lifted the _decision_
category +14pp (4 questions gained, 0 lost, p=0.125) — plausibly because a clean
"we decided X" durable fact answers a decision question more directly than the raw
episodic capture does. Off the synthesis hypothesis and underpowered; a targeted
"promote decision episodics to durable facts" experiment could revisit it, but it
is not a reason to ship the synthesizer. The spike stays bench-only (build-tagged);
it is **not** promoted to a `service.Synthesizer`.
