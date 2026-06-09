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
go run ./cmd/bench -suite longmemeval -data ./longmemeval_s.json -k 5
go run ./cmd/bench -suite locomo      -data ./locomo.json        -k 5

# Isolate the recency-aware re-ranker against pure RRF on the same candidates,
# using each question's date as "now" (needs a timestamped dataset):
go run ./cmd/bench -suite longmemeval -data ./longmemeval_s.json -rerank -k 5
```

## Results: memini vs other memory systems

All memini numbers below are **measured by this harness** against a live
**Qwen3-Embedding-8B** (4096-d) endpoint. Competitor numbers are **cited from
their own publications** — we cannot re-run their systems here, and they use
different embedding models, readers, and judges. Treat cross-system rows as
**directional**, not a controlled head-to-head. (This mirrors how
[agentmemory documents its comparison](https://github.com/rohitg00/agentmemory/blob/main/benchmark/COMPARISON.md).)

### LongMemEval-S — retrieval `recall_any@K`

Full **500-question** [LongMemEval-S](https://arxiv.org/abs/2410.10813) (~48
sessions/question), same metric agentmemory reports: does **any** gold session
appear in the top-K retrieved? No LLM in the loop — pure retrieval. Both runs
below are the **full 500 questions**; the first uses the **identical embedding
model agentmemory benchmarks with** (all-MiniLM-L6-v2, 384-d) for a true
apples-to-apples comparison, the second a premium model (Qwen3-Embedding-8B).

| System                         | Embedding model             |       R@5 |      R@10 | Source                                                                                  |
| ------------------------------ | --------------------------- | --------: | --------: | --------------------------------------------------------------------------------------- |
| **memini — hybrid (RRF)**      | all-MiniLM-L6-v2 (384-d)    |     96.4% |     98.4% | measured                                                                                |
| memini — keyword (Porter BM25) | —                           |     97.6% |     99.0% | measured                                                                                |
| memini — vector                | all-MiniLM-L6-v2            |     92.6% |     95.4% | measured                                                                                |
| **memini — hybrid (RRF)**      | Qwen3-Embedding-8B (4096-d) | **97.6%** | **98.4%** | measured                                                                                |
| memini — keyword (Porter BM25) | —                           |     97.2% |     98.2% | measured                                                                                |
| memini — vector                | Qwen3-Embedding-8B          |     96.0% |     97.8% | measured                                                                                |
| agentmemory — BM25 + Vector    | all-MiniLM-L6-v2            |     95.2% |     98.6% | [published](https://github.com/rohitg00/agentmemory/blob/main/benchmark/LONGMEMEVAL.md) |
| agentmemory — BM25 only        | —                           |     86.2% |     94.6% | published                                                                               |
| MemPalace (vector only)        | larger model                |    ~96.6% |         — | self-reported                                                                           |

On the **same model/dataset/metric** (full 500 questions), memini hybrid
**beats agentmemory at R@5 (96.4% vs 95.2%)** and MRR (88.6% vs 88.2%), ties at
R@10 (98.4% vs 98.6%) and R@20 (99.6% vs 99.4%); with the premium model it
reaches **97.6% R@5**. memini's keyword leg is **+11.4pp over agentmemory's
BM25-only** (97.6% vs 86.2%) thanks to Porter stemming. On the small model the
keyword leg alone is so strong it edges the fused R@5 — we deliberately **do
not** tune RRF to the test set; with the premium model the vector leg
strengthens and hybrid wins outright.

### Recency-aware re-ranking (`-rerank`)

memini re-ranks the fused candidates by a composite of relevance, **recency**,
and **importance**. The recency weight is deliberately light (0.05): a sweep on
LongMemEval-S (knowledge-update + temporal-reasoning, q.Now = question date,
sessions timestamped from `haystack_dates`) shows recency is a net win only as a
tie-breaker, and actively harmful when over-weighted.

| recency weight | R@1 (both cats) | knowledge-update R@1 | temporal R@1 | MRR |
| -------------- | --------------: | -------------------: | -----------: | --: |
| 0 (pure RRF)   |           79.6% |                88.5% |        74.4% | 87.6% |
| **0.05** (default) |       **81.0%** |            85.9% |        78.2% | **88.4%** |
| 0.15           |           77.7% |                73.1% |        80.5% | 86.4% |
| 0.25           |           71.6% |                56.4% |        80.5% | 82.6% |

At 0.05 the re-ranker is **+1.4pp R@1 / +0.8pp MRR** over pure RRF (gaining
+3.8pp on temporal at a −2.6pp knowledge-update cost). Heavier recency buries
correct-but-older memories — the gold session is **not** always the most recent
— so the default keeps relevance dominant. Recall@5 is unchanged across all
weights (the re-rank only reorders within the top results).

memini hybrid per-category (Qwen3-8B, recall_any@10): multi-session 100%,
single-session-user 100%, single-session-assistant 100%, knowledge-update 97.4%,
single-session-preference 96.7%, temporal-reasoning 96.2%.

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

| System           | Retrieval                                         |
| ---------------- | ------------------------------------------------- |
| `memini-hybrid`  | vector + keyword fused with RRF (production path) |
| `memini-vector`  | dense vector only                                 |
| `memini-keyword` | BM25 keyword only                                 |

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
