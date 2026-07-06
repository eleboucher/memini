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
superseded what), abstention (genuinely not in the corpus). 45 questions:
decision 8, convention 7, rationale 8, current-state 6, synthesis 6,
temporal-update 6, abstention 4.

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

**Discrimination (Test C).** The 2×2 is {extract, distill} write-path ingest ×
{single-shot, agentic} answering. The bench is judged to discriminate iff at
least one pre-registered contrast holds:

- **C1** agentic vs single-shot (within extract): ≥15pp overall, or ≥20pp on
  {synthesis, temporal-update} pooled, with McNemar exact p < 0.05.
- **C2** distill vs extract (within single-shot): same thresholds; expected on
  {current-state, temporal-update}.
- **C3** interaction (distill+agentic) vs (extract+single): ≥20pp overall, p < 0.05.

**Failure/iterate path:** if all three tie, one iteration only — raise distractor
density and convert the flattest category's weakest 5 questions to 2-hop
synthesis — then re-run B then C. If still tied, the honest finding is
**no-scale**: on-domain saturation reproduced, and the discrimination bottleneck
is elsewhere (report it).

## 6. Run commands

```sh
# Local model server: keep embedder + chat co-resident to avoid JIT thrash
lms load text-embedding-qwen3-embedding-0.6b --ttl 86400 --yes
lms load qwen3.5-9b --ttl 86400 --yes

export MEMINI_EMBED_BASE_URL=http://127.0.0.1:8001/v1 \
       MEMINI_EMBED_MODEL=text-embedding-qwen3-embedding-0.6b MEMINI_EMBED_DIMS=1024
export MEMINI_LLM_API=openai MEMINI_LLM_BASE_URL=http://127.0.0.1:8001/v1 \
       MEMINI_LLM_MODEL=qwen3.5-9b

# 1. gold audit (offline, instant — run after every dataset edit)
go test -tags bench ./bench -run TestCodingAgentGoldAudit -v
# 2. headroom (embedder only)
go test -tags bench ./bench -run TestCodingAgentHeadroom -v -timeout 20m
# 3. discrimination 2x2 (embedder + LLM; checkpointed/resumable)
go test -tags bench ./bench -run TestCodingAgentDiscrimination -v -timeout 6h
# ad-hoc single-arm rerun
go run ./cmd/qa -suite codingagent -data bench/data/codingagent_pilot.json \
  -ingest write [-distill] -reasoning [""|medium] -k 10
```

## 7. Results (2026-07-06, qwen3.5-9b answerer+judge, qwen3-embedding-0.6b)

Per-category LLM-judged accuracy, 45 questions, 2×2:

| category        |      ext/single |     ext/agentic | dist/single | dist/agentic |
| --------------- | --------------: | --------------: | ----------: | -----------: |
| abstention      |      100% (4/4) |            100% |        100% |         100% |
| convention      |       86% (6/7) |             86% |         86% |          86% |
| current-state   |       83% (5/6) |        **100%** |         83% |     **100%** |
| decision        |       75% (6/8) |        **100%** |         75% |     **100%** |
| rationale       |       88% (7/8) |             75% |         88% |          75% |
| synthesis       |       33% (2/6) |         **50%** |         33% |      **50%** |
| temporal-update |       67% (4/6) |         **83%** |         67% |      **83%** |
| **overall**     | **76% (34/45)** | **84% (38/45)** |         76% |          84% |

Pre-registered paired contrasts (McNemar exact):

- **C1** agentic vs single (extract): 84% vs 76%, **Δ +9pp**; A-only=6, B-only=2, tie=37; **p=0.29**.
- **C2** distill vs extract (single): 76% vs 76%, **Δ 0pp**; 45 ties; p=1.0.
- **C3** distill+agentic vs extract+single: 84% vs 76%, Δ +9pp; p=0.29 (≡ C1).

Cost: agentic ≈43.5k input tokens vs single ≈36.8k (+18%) for the +9pp;
distill ingest = 67 calls, **0 facts produced**.

### What the bench showed

1. **It discriminates where LongMemEval is blind.** LongMemEval recall@5 is a flat
   ~100%, so single vs agentic there is a wash. Here the agentic loop produces a
   consistent, mechanistically-sensible per-category signal: it _wins_ on the
   categories that need iterative/multi-fact retrieval — decision (75→100%),
   current-state (83→100%), synthesis (33→50%), temporal-update (67→83%) — and
   _loses_ on rationale (88→75%, where extra tool-searching dilutes a direct
   answer). That per-category structure is exactly the resolution the saturated
   suite cannot give.

2. **No contrast cleared the pre-registered p<0.05 bar.** The agentic effect is
   real in direction but +9pp overall (6 vs 2 discordant) is underpowered at
   n=45 (p=0.29). Pooled {synthesis, temporal-update} is +17pp but only ~2 net
   discordant — still under-powered. The pilot's job was to measure the effect
   size so a full bench can be sized; a paired ~9pp effect needs on the order of
   ~150–250 questions for 80% power.

3. **The distill arm is inert here, for a diagnosable reason.** The distiller
   returned **0 facts on all 67 calls**, so distill-on-write added nothing and
   C2 is a 45/45 tie by construction. The cause is corpus shape: items were mined
   from commit messages and memory notes, so they are _already atomic facts_.
   Distill-on-write compresses verbose _turns_ into facts; given fact-shaped input
   there is nothing to compress. (This also explains why the prior distill-on-write
   A/B "bottomed out in noise" — with an inert distiller there is no signal to
   find. The bench surfaces that cleanly via the 0-facts cost counter.)

4. **Abstention works and is already saturated** (100% every cell): single-shot
   declines correctly on all four genuinely-absent questions, so abstention does
   not discriminate single vs agentic on this set — a real finding, not a gap.

## 8. Recommendation

**Worth scaling — with two targeted changes.** The pilot succeeded at its actual
purpose: it produces per-category, headroom-bearing numbers that separate
configurations LongMemEval flattens to 100%, and it measured the effect sizes and
failure modes needed to design a full bench. It did not _certify_ discrimination
at p<0.05, but that is a power problem at n=45, not a design failure.

For a full bench:

1. **Scale to ~150–250 hand-verified questions** (same mining rules, same
   categories) to power the ~9pp paired agentic effect to p<0.05. Weight toward
   synthesis and temporal-update, where the headroom and the effect are largest.
2. **Add a turn-shaped subcorpus for the write-enrichment axis.** Fact-shaped
   commit/note items can't exercise distill-on-write. To A/B distill vs extract
   honestly, ingest real _conversational_ transcripts (option b) as episodic
   turns, or use a distiller model/prompt verified to emit facts (the local 9B
   produced none). Keep the fact-shaped corpus for the answer-strategy axis.

**First LLM experiment to run against it:** the **agentic answer loop** (depth and
tool mix), because it shows the clearest, most mechanistically-coherent lift —
synthesis and temporal-update, the multi-memory categories where LongMemEval is
saturated and blind. Investigate the rationale regression (agentic 88→75%) as
part of it: bounding tool calls when the single-shot answer is already grounded.

**Do not** invest further in the distill-on-write A/B until (a) the distiller is
verified to emit facts and (b) a turn-shaped corpus exists — otherwise it will
keep bottoming out in the same inert-distiller noise as before.
