# Rust port migration map

The Go implementation at commit `81a2789` was the behavioral reference during the port. It was removed after its complete unit suite and tagged benchmark compile-check passed alongside the Rust differential suite.

## Definition of full parity

- The `memini` CLI exposes the same commands, flags, environment variables, exit behavior, and generated reference documentation.
- REST and MCP preserve their schemas, authentication rules, headers, status/error behavior, and cross-surface interoperability.
- SQLite databases and PostgreSQL schemas remain readable and writable without export/import, including vector, FTS, API-key, namespace-link, and event data.
- Recall, ranking, consolidation, contradiction, scopes, maintenance, import/export, metrics, and dependency-degradation behavior match the Go tests and benchmark acceptance thresholds.
- Existing UI, plugins, Docker/Compose, Helm, and integration packages work without client changes.

## Migration slices

1. `memini-core`: domain types, normalization/fingerprints, sanitization, fusion/ranking, temporal logic.
2. `memini-config`: environment schema, namespace resolution, validation, deprecated-setting failures.
3. `memini-store`: shared contracts and conformance suite, then SQLite and PostgreSQL implementations using the existing schemas.
4. AI and heuristics: `memini-embed`, `memini-llm`, `memini-rerank`, and
   `memini-intelligence` (extraction, entities, contradiction, redaction).
5. `memini-service`: read sets/scopes, remember/recall/answer, consolidation, briefing, events, lifecycle maintenance.
6. `memini-api`: API-key auth, REST from `api/openapi.yaml`, MCP tools/resources, health and metrics.
7. `memini-cli`: server bootstrap and every operational command, including doctor, migrate, keys, links, import/export, and re-embedding.
8. Packaging and cutover: differential tests, benchmarks, UI embedding, docs generation, container/Helm changes, removal of Go sources.

## Implemented compatibility slices

- Domain types, fingerprints, sanitization, ranking/fusion, and temporal scoring.
- Full environment configuration surface and namespace/agent resolution.
- SQLite and PostgreSQL stores, including vectors, FTS, links, API keys, events,
  legacy backfills, and a shared conformance suite.
- OpenAI-compatible embeddings with batching, limits, retries, memory and disk
  caches; OpenAI/Anthropic LLM adapters; cross-encoder and LLM reranking.
- Marker-based durable extraction, named entities, precision-first
  contradiction classification, and recursive credential redaction.
- Service-level remember/recall/list/answer/briefing, including ancestor/home/link
  read sets, temporal recall, query expansion and bounded tool-loop reasoning,
  confidence routing, consolidation modes, fuzzy write dedup, promotion, event
  history, session-batched distillation with age/shutdown flushing, and
  child-namespace briefing rollups.
- Axum REST and JSON-RPC MCP surfaces, API-key/file-key authentication, embedded
  UI, dedicated UI/metrics listeners, readiness checks, verbose dependency
  health, shared service/embed/rerank/store metrics, and stdio MCP transport.
- Operational CLI and import adapters (memini, mem0, AgentMemory, Mnemory, and
  Claude Code), including extraction, remote authenticated imports, export,
  keys, links, namespace repair, migrations, dedup, and re-embedding.
- Rust-native CI, release versioning, multi-architecture container builds, and
  stable embedded UI asset names.

## Gates for every slice

- Port the reference Go tests before or with production code.
- Add fixture-based differential tests where floating point, JSON, SQL, HTTP, or protocol behavior can drift.
- Keep `cargo test --workspace`, Clippy, docs drift checks, plugin tests, storage conformance, and container smoke tests green after cutover.
- Do not change public contracts merely to simplify the Rust implementation.

## Cutover gate

Cutover happens only when every Go package is accounted for, all Go and Rust tests pass against both storage backends, generated OpenAPI/MCP/CLI docs have no unexplained diff, integration plugins pass against the Rust server, and benchmark changes are explicitly accepted.

## Cutover status

- CLI, configuration, runtime MCP schemas, and OpenAPI reference docs are generated or validated by the Rust `memini gen-docs` command.
- Streamable HTTP MCP sessions bind namespace, home, and principal context at initialization and validate POST/GET/DELETE session IDs.
- Retrieval, QA, temporal/rerank, rewrite, distill, vector-gate, and rerank-gate harnesses are Rust-native. The committed offline sample pins the former Go oracle's exact recall/MRR values.
- Go sources, `go.mod`, `go.sum`, and Go-only build tooling have been removed.

Release verification is complete: plugin suites, SQLite and live PostgreSQL
conformance, generated-doc drift, strict Clippy/formatting, Helm lint, the
offline benchmark gates, and a clean container build/runtime smoke test pass on
the Rust implementation.
