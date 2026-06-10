# memini benchmark harness

A retrieval benchmark: ingest a dataset of memories, then for each question
measure how well a system retrieves the gold supporting memories.

```sh
mise run bench                 # offline sample, local embedder
go run ./cmd/bench -k 5        # same, explicit K
```

Against a real embeddings model and a real dataset:

```sh
export MEMINI_EMBED_BASE_URL=http://localhost:8081/v1
export MEMINI_EMBED_MODEL=bge-m3 MEMINI_EMBED_DIMS=1024
# Optional: instruction-tuned asymmetric embedders (Qwen3-Embedding, bge) score
# higher when queries carry a retrieval instruction; documents stay bare.
# Measured on Qwen3-Embedding-8B: +6.0pp R@5 on the LongMemEval vector leg
# (91.2% -> 97.2%), +1.0pp MRR on the fused ranking on both datasets.
export MEMINI_EMBED_QUERY_PREFIX=$'Instruct: Given a user query, retrieve relevant memories that answer it\nQuery:'
go run ./cmd/bench -suite longmemeval -data ./longmemeval_s.json -k 5
go run ./cmd/bench -suite locomo      -data ./locomo.json        -k 5

# Isolate the recency-aware re-ranker against pure RRF on the same candidates,
# using each question's date as "now" (needs a timestamped dataset):
go run ./cmd/bench -suite longmemeval -data ./longmemeval_s.json -rerank -k 5
```

## Results: memini vs other memory systems

All memini numbers below are **measured by this harness** against a live
**all-MiniLM-L6-v2** (384-d) endpoint — the same embedding model agentmemory
benchmarks with. Competitor numbers are **cited from
their own publications** — we cannot re-run their systems here, and they use
different embedding models, readers, and judges. Treat cross-system rows as
**directional**, not a controlled head-to-head. (This mirrors how
[agentmemory documents its comparison](https://github.com/rohitg00/agentmemory/blob/main/benchmark/COMPARISON.md).)

### LongMemEval-S — retrieval `recall_any@K`

Full **500-question** [LongMemEval-S](https://arxiv.org/abs/2410.10813) (~48
sessions/question), same metric agentmemory reports: does **any** gold session
appear in the top-K retrieved? No LLM in the loop — pure retrieval. The run is
the **full 500 questions** with the **identical embedding model agentmemory
benchmarks with** (all-MiniLM-L6-v2, 384-d) for a true apples-to-apples
comparison.

Hybrid recall over-fetches a deep candidate pool per leg (`max(k*5, 50)`)
before fusing, so a memory just outside the top-k of both legs can still win —
the production `Recall` path does the same. Fusion defaults to **convex score
fusion** (`MEMINI_FUSION_ALPHA=0.5`): each leg's scores are min-max normalized
to [0,1] and combined `0.5·vector + 0.5·keyword`, keeping score _magnitude_ so a
memory a leg ranks far above its runners-up dominates one that is merely
middling in both. A negative alpha falls back to **Reciprocal Rank Fusion**;
deep pools then need a steep decay (`rrfK=5`, not the classic 60), since a flat
decay lets both-leg mediocrity outscore single-leg excellence
(`2/(60+20) > 1/(60+0)`). Score fusion reaches the same end principledly and
beat RRF on 3 of 4 model×dataset cells (and on MRR in all 4).

| System                         | Embedding model          |       R@5 |      R@10 | Source                                                                                  |
| ------------------------------ | ------------------------ | --------: | --------: | --------------------------------------------------------------------------------------- |
| **memini — hybrid (score)**    | all-MiniLM-L6-v2 (384-d) | **98.4%** | **99.4%** | measured                                                                                |
| memini — keyword (Porter BM25) | —                        |     97.6% |     99.0% | measured                                                                                |
| memini — vector                | all-MiniLM-L6-v2         |     91.8% |     96.6% | measured                                                                                |
| agentmemory — BM25 + Vector    | all-MiniLM-L6-v2         |     95.2% |     98.6% | [published](https://github.com/rohitg00/agentmemory/blob/main/benchmark/LONGMEMEVAL.md) |
| agentmemory — BM25 only        | —                        |     86.2% |     94.6% | published                                                                               |
| MemPalace (vector only)        | larger model             |    ~96.6% |         — | self-reported                                                                           |

On the **same model/dataset/metric** (full 500 questions), memini hybrid
**beats agentmemory at R@5 (98.4% vs 95.2%)**, R@10 (99.4% vs 98.6%), and MRR
(92.3% vs 88.2%). memini's keyword leg is **+11.4pp over agentmemory's
BM25-only** (97.6% vs 86.2%) thanks to Porter stemming, and hybrid fusion now
beats either leg alone. Relative to fetching only k per leg with the classic
`rrfK=60`, the deep-pool + score fusion is worth **+2.0pp R@5 / +1.0pp
R@10**.

### LoCoMo — retrieval `recall_any@K`

[LoCoMo](https://snap-research.github.io/locomo/) retrieval at **dialogue-turn
granularity** (1,982 questions over 10 long conversations, gold = exact
evidence turns among ~590 turns/conversation) — a much harder target than
LongMemEval's session granularity, and the regime where flat-decay RRF over
deep pools degrades badly.

| System (all-MiniLM-L6-v2)      |   R@5 |  R@10 |
| ------------------------------ | ----: | ----: |
| **memini — hybrid (score)**    | 59.8% | 69.8% |
| memini — keyword (Porter BM25) | 58.7% | 67.1% |
| memini — vector                | 41.5% | 52.1% |

No published turn-level retrieval baselines exist to compare against (mem0 /
Letta report LLM-judged QA accuracy, below). This is the one cell where the
default score fusion is edged by RRF (60.1% / 71.0%): when the vector leg is
near-noise (MiniLM scores only 41.5% here), giving it an equal-weight normalized
vote hurts, whereas RRF's rank-only vote is more robust. Score fusion still wins
this cell on MRR and wins outright on every cell with a stronger embedder — so
it is the default, and `MEMINI_FUSION_ALPHA=-1` selects RRF for weak-vector
deployments. (Ablation: `rrfK=60` over the same deep pools scored just **52.8%
R@5**, _below_ the keyword leg alone — both score fusion and `rrfK=5` fix that.)

### Pool-depth robustness (`-pool-factor` / `-pool-floor`)

Min-max normalization could in principle be fragile to pool depth (the score at
the bottom of the pool sets each leg's zero point), so score fusion was swept at
per-leg depths 30 / 50 / 80 on both datasets and both embedders (hybrid
R@5 / R@10 / MRR):

| cell                  | depth 30           | depth 50 (default) | depth 80           |
| --------------------- | ------------------ | ------------------ | ------------------ |
| LME · MiniLM          | 97.8 / 99.4 / 92.0 | 98.4 / 99.4 / 92.3 | 98.6 / 99.4 / 92.6 |
| LME · Qwen3+prefix    | 98.8 / 99.4 / 94.5 | 98.8 / 99.6 / 94.6 | 98.8 / 99.6 / 94.6 |
| LoCoMo · MiniLM       | 60.0 / 70.1 / 42.1 | 59.8 / 69.8 / 42.6 | 59.3 / 69.6 / 42.7 |
| LoCoMo · Qwen3+prefix | 70.1 / 77.9 / 52.1 | 70.1 / 78.5 / 52.4 | 70.1 / 78.7 / 52.5 |

Quality moves at most ±0.6pp R@5 across a 2.7× depth range — no tail collapse —
with the two datasets drifting in opposite directions (deeper pools help
session-granularity LongMemEval slightly and hurt turn-granularity LoCoMo
slightly), so the default `max(k*5, 50)` sits at the crossover.

### Recency-aware re-ranking (`-rerank`)

memini re-ranks the fused candidates by a composite of relevance, **recency**,
and **importance**. The recency weight is deliberately light (0.05): a sweep on
LongMemEval-S (knowledge-update + temporal-reasoning, q.Now = question date,
sessions timestamped from `haystack_dates`) shows recency is a net win only as a
tie-breaker, and actively harmful when over-weighted.

| recency weight     | R@1 (both cats) | knowledge-update R@1 | temporal R@1 |       MRR |
| ------------------ | --------------: | -------------------: | -----------: | --------: |
| 0 (pure RRF)       |           82.9% |                91.0% |        78.2% |     90.1% |
| **0.05** (default) |       **83.4%** |            **91.0%** |        78.9% | **90.5%** |
| 0.15               |           83.9% |                89.7% |        80.5% |     90.7% |
| 0.25               |           83.4% |                87.2% |        81.2% |     90.4% |

At 0.05 the re-ranker is **+0.5pp R@1 / +0.4pp MRR** over pure RRF with **no**
knowledge-update cost, and recall@5 is identical across all weights (the
re-rank only reorders within the top results). The steep RRF decay made the
composite far more robust to the recency weight than the flat `rrfK=60` decay
was (where 0.15+ buried correct-but-older memories); the default stays at the
conservative 0.05 since the gains beyond it are within noise.

memini hybrid per-category (all-MiniLM, recall_any@10): multi-session 100%,
knowledge-update 100%, single-session-user 98.6%, single-session-assistant
98.2%, temporal-reasoning 97.0%, single-session-preference 96.7%.

### LoCoMo — end-to-end QA accuracy (LLM-judge)

The metric mem0/Letta publish: retrieve → generate an answer → an LLM judges it
against the gold answer. memini's number uses a fast instruct reader+judge
(Llama-3.3-70B-Instruct); the competitor numbers use their own readers/judges,
so this is directional.

| System                                      | LoCoMo QA accuracy | Source                                                           |
| ------------------------------------------- | -----------------: | ---------------------------------------------------------------- |
| memini (hybrid retrieval + instruct reader) | _full run pending_ | measured                                                         |
| Letta / MemGPT                              |              83.2% | [published](https://letta.com/blog/benchmarking-ai-agent-memory) |
| Mem0                                        |              68.5% | [published](https://mem0.ai)                                     |

> Sources: agentmemory COMPARISON.md/LONGMEMEVAL.md; LongMemEval (arXiv 2410.10813);
> LoCoMo (snap-stanford.github.io/LoCoMo); mem0.ai; letta.com.

## Metrics

- **Recall@K** — fraction of questions whose gold memory appears in the top K.
- **MRR** — mean reciprocal rank of the first gold hit.
- **p50/p95** — recall latency; **ingest** — total ingest time.

Output is a Markdown table (stdout) plus JSON under `bench/results/`.

## What it compares today

Three memini retrieval strategies over the same ingested store, to show the
value of hybrid fusion:

| System           | Retrieval                                        |
| ---------------- | ------------------------------------------------ |
| `memini-hybrid`  | vector + keyword, score fusion (production path) |
| `memini-vector`  | dense vector only                                |
| `memini-keyword` | BM25 keyword only                                |

`memini-hybrid` should never score below either single strategy.

## Datasets

- **sample** — committed at `bench/data/sample.json`, runs fully offline.
- **Normalized schema** (`-suite file`) — `{name, items:[{id,content}], questions:[{query,gold:[id]}]}`.
- **LongMemEval / LoCoMo** — loaders map the published JSON shapes to the
  normalized schema (each session/turn becomes an item; answer/evidence ids
  become gold). Download the datasets and pass `-data`.

> Recall@K on LongMemEval/LoCoMo is easy to overfit — treat scores as directional.

## External baselines

`bench.System` is the extension point. To compare against mem0, Zep/Graphiti,
Letta, Cognee, agentmemory, or supermemory, implement `System` (Name / Ingest /
Recall) over each service's API and add it to the run list in `cmd/bench`. These
require the respective services/keys and are intentionally not vendored here.
