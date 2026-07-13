# Benchmarks

The Rust benchmark crate provides two executable harnesses over the same service, storage, embedding, reranking, and LLM implementations used by the server.

## Retrieval

```sh
cargo run -p memini-bench --bin memini-bench -- --k 5
cargo run -p memini-bench --bin memini-bench -- --suite longmemeval --data ./longmemeval_s.json --k 5,10
cargo run -p memini-bench --bin memini-bench -- --suite locomo --data ./locomo.json --limit 100
```

The default sample uses a deterministic local embedder. Set `MEMINI_EMBED_BASE_URL`, `MEMINI_EMBED_MODEL`, and `MEMINI_EMBED_DIMS` to benchmark a real OpenAI-compatible embedding endpoint. Disk-cached embeddings make repeated parameter sweeps inexpensive.

Supported corpus modes are `sample`, `file`, `longmemeval`, `locomo`, and `locomo-sessions`. LongMemEval also supports `--holdout tune|held|all` and `--session-doc full|user-only|dated`.

Useful production-path comparisons include:

```sh
# Production Remember ingestion rather than direct benchmark upserts
cargo run -p memini-bench --bin memini-bench -- --ingest write

# Temporal composite ranking versus the unboosted baseline
cargo run -p memini-bench --bin memini-bench -- --suite longmemeval --data data.json --rerank

# Cross-encoder or LLM reranking
cargo run -p memini-bench --bin memini-bench -- --suite locomo --data data.json \
  --rerank-url http://localhost:8002/v1 --rerank-model qwen3-reranker-0.6b
cargo run -p memini-bench --bin memini-bench -- --suite locomo --data data.json --llm-rerank

# LLM query rewriting or distillation through the production write path
cargo run -p memini-bench --bin memini-bench -- --suite locomo --data data.json --rewrite
cargo run -p memini-bench --bin memini-bench -- --suite locomo --data data.json --ingest write --distill

# Positive-recall versus foreign-namespace injection sweeps
cargo run -p memini-bench --bin memini-bench -- --suite locomo --data data.json --vec-gate 0,0.2,0.3,0.4
cargo run -p memini-bench --bin memini-bench -- --suite locomo --data data.json \
  --rerank-gate 0,0.2,0.4,0.6 --rerank-url http://localhost:8002/v1
```

Each ordinary retrieval run writes a self-describing JSON report under `bench/results/` and prints Markdown tables for hybrid, vector, and keyword recall@K, MRR, latency, and injected-token efficiency.

## Answer quality

`memini-qa` ingests a corpus, answers through `Service::answer`, and grades candidates using the category-specific reference rubrics.

```sh
cargo run -p memini-bench --bin memini-qa -- \
  --suite longmemeval --data bench/data/longmemeval_s.json \
  --ingest write --workers 6 --checkpoint bench/results/qa.jsonl
```

It supports LoCoMo, LongMemEval, and CodingAgent; resumable JSONL checkpoints; concurrent workers; per-question historical clocks; temporal-boost tuning; reasoning levels; debug output; and optional distill-on-write. The LLM and embedding backends use the `MEMINI_LLM_*` and `MEMINI_EMBED_*` environment variables.

## Tests

```sh
cargo test -p memini-bench
```

The committed sample acceptance test pins the Go oracle’s hybrid/vector/keyword recall and MRR values for both direct-upsert and production-write ingestion.
