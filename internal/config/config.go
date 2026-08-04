package config

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"

	"github.com/eleboucher/memini/internal/extract"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/store"
)

// Backend selects the storage driver.
type Backend string

const (
	BackendSQLite   Backend = "sqlite"
	BackendPostgres Backend = "postgres"

	// defaultNamespace is the ultimate fallback when env, git, and cwd all
	// fail to produce a usable name.
	defaultNamespace = "default"
)

// The truncation budgets' defaults, exported for the tools that build an
// embedder or reranker outside the server — cmd/bench and cmd/qa — so they
// start from the same numbers the server does instead of copied literals. A
// benchmark run against a stale budget measures a deployment nobody has.
//
// These duplicate the matching `envDefault` tags below, which cannot reference
// a constant. cmd/memini's TestTruncationDefaultsMatchPackageConstants pins the
// two together; keep that test passing rather than editing one side alone.
const (
	DefaultEmbedMaxBatch       = 20
	DefaultEmbedMaxBatchChars  = 24000
	DefaultEmbedMaxItemChars   = 8000
	DefaultRerankMaxDocChars   = 2048
	DefaultRerankMaxBatchChars = 6000

	// The chunking defaults, exported for cmd/bench for the same reason. These
	// mirror chunk.DefaultConfig(); TestChunkDefaultsAreCoherent pins the two.
	DefaultChunkSize         = 1200
	DefaultChunkOverlap      = 200
	DefaultChunkMinContent   = 1200
	DefaultChunkMaxPerMemory = 64
)

// NamespaceSource records how DefaultNamespace was resolved, useful for
// startup logging and debug surfaces.
type NamespaceSource string

const (
	NamespaceFromEnv     NamespaceSource = "env"      // MEMINI_DEFAULT_NAMESPACE / MEMINI_NAMESPACE
	NamespaceFromGit     NamespaceSource = "git"      // git rev-parse --show-toplevel basename
	NamespaceFromCWD     NamespaceSource = "cwd"      // filepath.Base(cwd)
	NamespaceFromLiteral NamespaceSource = "fallback" // literal "default"
)

// Config is the fully-resolved runtime configuration. Environment-backed
// fields are parsed by github.com/caarlos0/env via their `env` tags; an absent
// variable falls back to `envDefault`, while a set-but-empty variable is taken
// verbatim. DefaultNamespace/NamespaceSrc are resolved separately (see
// resolveDefaultNamespace) and carry no tag.
type Config struct {
	// HTTP server.

	// HTTPAddr is the address the main listener binds. It carries the REST API,
	// the MCP endpoint, and the admin UI unless UIAddr moves the UI elsewhere.
	HTTPAddr string `env:"MEMINI_HTTP_ADDR" envDefault:":8080"`
	// ShutdownTimeout is how long in-flight requests get to finish after SIGTERM
	// before the server stops waiting and exits anyway.
	ShutdownTimeout time.Duration `env:"MEMINI_SHUTDOWN_TIMEOUT" envDefault:"15s"`
	// RequestTimeout bounds how long a single /v1 REST request may run before
	// chi's Timeout middleware cancels its context (internal/api/rest.Mount).
	// It never applies to /mcp (long-lived SSE), /healthz, /readyz, or
	// /metrics. Default 60s rather than a more conservative 30s: the LLM HTTP
	// client's own timeout is 120s (internal/llm/llm.go defaultHTTPTimeout),
	// and POST /v1/answer can ride that full call chain (e.g. a
	// reasoning_level=high rewrite/answer). A 30s default would systematically
	// cut off those legitimate long-running answer calls; 60s cuts that risk
	// roughly in half without going as high as the LLM client's own ceiling.
	// It still doesn't fully cover a 120s LLM call — deployments that
	// regularly hit that ceiling should raise this explicitly. 0 disables it.
	RequestTimeout time.Duration `env:"MEMINI_REQUEST_TIMEOUT" envDefault:"60s"`
	// MetricsAddr, when set (e.g. ":9090"), serves /metrics on its own listener
	// instead of the main HTTP port. The dedicated port is meant to stay
	// in-cluster — keep it out of any public route and it needs no bearer token.
	// Empty (the default) keeps /metrics on the main port, where MEMINI_API_KEY
	// gates it.
	MetricsAddr string `env:"MEMINI_METRICS_ADDR"`
	// UIAddr, when set (e.g. ":8081") and distinct from HTTPAddr, serves the
	// admin UI on its own listener instead of the main HTTP port. The shell is
	// credential-free — it never contains MEMINI_API_KEY; the SPA signs in
	// against /v1/self in the browser and keeps its token in localStorage. A
	// dedicated listener just lets operators expose the UI on a different
	// port/gateway from the main API. The UI listener also serves the API so the
	// same-origin SPA can call /v1. Empty (the default) keeps the UI on the main
	// port when MEMINI_UI_ENABLED is true.
	UIAddr string `env:"MEMINI_UI_ADDR"`

	// Logging.

	// LogLevel is one of debug, info, warn or error.
	LogLevel string `env:"MEMINI_LOG_LEVEL" envDefault:"info"`
	// LogFormat is json (structured, for a log pipeline) or text (readable, for
	// a terminal).
	LogFormat string `env:"MEMINI_LOG_FORMAT" envDefault:"json"`

	// Storage.

	// Backend selects the storage driver: sqlite (embedded, the default, no
	// external dependency) or postgres (pgvector/VectorChord, for a shared
	// deployment). Postgres additionally requires PostgresDSN.
	Backend Backend `env:"MEMINI_BACKEND" envDefault:"sqlite"`
	// SQLitePath is where the embedded database file lives. Give it a path on a
	// volume that survives restarts; the default is relative to the working
	// directory, which in a container usually means it does not.
	SQLitePath string `env:"MEMINI_SQLITE_PATH" envDefault:"memini.db"`
	// PostgresDSN is the connection string used when Backend is postgres, e.g.
	// postgres://user:pass@host:5432/memini?sslmode=disable. Required in that
	// mode; the server refuses to start without it.
	PostgresDSN string `env:"MEMINI_POSTGRES_DSN"`

	// Embeddings (external OpenAI-compatible endpoint, required for vector search).

	// EmbedBaseURL is an OpenAI-compatible /embeddings endpoint, which you
	// deploy: text-embeddings-inference, llama.cpp, vLLM, OpenAI itself, or
	// anything else speaking that shape. Leaving it empty is supported and
	// degrades recall to keyword-only search, which works but retrieves
	// noticeably worse.
	EmbedBaseURL string `env:"MEMINI_EMBED_BASE_URL"`
	// EmbedAPIKey is the bearer token for the embeddings endpoint, when it wants
	// one. Optional.
	EmbedAPIKey string `env:"MEMINI_EMBED_API_KEY"`
	// EmbedModel is the model name sent with each embeddings request. memini
	// records which model produced a store's vectors and refuses to start when
	// this later disagrees, because vectors from different models are not
	// comparable and a silent swap degrades recall with no error. Use the
	// `memini reembed` command to migrate a store, or ReembedOnModelChange to do
	// it automatically at startup.
	EmbedModel string `env:"MEMINI_EMBED_MODEL" envDefault:"text-embedding-3-small"`
	// EmbedDims is the dimensionality of the embedding model, and it must match
	// the model EmbedBaseURL actually serves. This is the most common setup
	// mistake: the default suits text-embedding-3-small, so pointing at a 768 or
	// 1024 dimension model without changing this corrupts the store rather than
	// failing cleanly. Unlike the model name, dimensionality cannot be migrated
	// in place; changing it needs a fresh store (export, then import).
	EmbedDims int `env:"MEMINI_EMBED_DIMS" envDefault:"1536"`
	// EmbedQueryPrefix is prepended to recall queries before embedding, for
	// instruction-tuned asymmetric embedders (e.g. Qwen3-Embedding, bge).
	// Documents are always embedded without it. Empty disables.
	EmbedQueryPrefix string `env:"MEMINI_EMBED_QUERY_PREFIX"`
	// EmbedMaxBatch caps items per /embeddings request so bulk callers (dedup
	// over a whole namespace) can't exceed the server's max client batch and
	// fail with 422. The TEI default is 32; 20 leaves headroom.
	EmbedMaxBatch int `env:"MEMINI_EMBED_MAX_BATCH" envDefault:"20"`
	// EmbedMaxBatchChars caps total bytes per request (0 disables). Bytes, not
	// runes: this guards the HTTP payload the backend has to accept, and the
	// backend's limit is on the wire size.
	EmbedMaxBatchChars int `env:"MEMINI_EMBED_MAX_BATCH_CHARS" envDefault:"24000"`
	// EmbedMaxItemChars truncates any single text to this many runes before
	// embedding, so one oversized memory can't blow the per-request budget or
	// exceed the model's context.
	//
	// This bounds what is findable, not what is stored: a memory longer than
	// this is stored and returned whole, but its vector represents only the
	// prefix, so vector recall cannot match the text beyond it. Raise it toward
	// your embedder's real context window (text-embedding-3-small accepts 8191
	// tokens, roughly 32000 characters) if you store long memories, and watch
	// for the "embed: truncating over-long text" warning.
	//
	// 0 disables truncation, and is a foot-gun rather than a faster setting:
	// this is the only guard that keeps an oversized text off the wire, since
	// the batcher always sends the first item of a batch whatever its size. A
	// text past the backend's context is then rejected, the write lands with no
	// vector (metadata pending_embed), and the backfill re-sends the same
	// oversized text on every tick — so the memory is permanently unreachable by
	// vector recall while the API reports the failure as temporary. Set 0 only
	// if you are certain every memory fits your backend's context.
	EmbedMaxItemChars int `env:"MEMINI_EMBED_MAX_ITEM_CHARS" envDefault:"8000"`
	// EmbedMaxConcurrency caps in-flight calls to the embeddings backend. 0
	// is unbounded. Set to 1-2 for self-hosted backends that can't service a
	// recall burst in parallel.
	EmbedMaxConcurrency int `env:"MEMINI_EMBED_MAX_CONCURRENCY" envDefault:"0"`

	// ChunkEmbed additionally embeds long memories in overlapping segments, so
	// recall can match text past MEMINI_EMBED_MAX_ITEM_CHARS instead of only the
	// prefix that fits in one vector. Off by default.
	//
	// It is purely additive: the whole-memory vector is unchanged and still
	// searched, and chunk hits are merged into it. Turning this on can only add
	// results, never remove or re-rank away an existing one; turning it back off
	// returns exactly the previous behaviour, leaving unused rows behind.
	//
	// Chunks are built by the background loop on MEMINI_BACKFILL_INTERVAL, not
	// at write time — a long memory is many embedder round-trips, which would
	// blow MEMINI_WRITE_EMBED_TIMEOUT for precisely the writes this helps. So
	// recall improves for a long memory shortly after it is written, not at the
	// instant it is.
	ChunkEmbed bool `env:"MEMINI_CHUNK_EMBED" envDefault:"false"`
	// ChunkSize is the maximum runes in one chunk. Keep it under your embedder's
	// context: a chunk over MEMINI_EMBED_MAX_ITEM_CHARS would itself be
	// truncated, which is the failure chunking exists to remove. The default
	// suits the 512-token local models (BGE, e5) as well as OpenAI's.
	//
	// It applies to memories chunked from here on, NOT retroactively. A memory
	// that already has chunks keeps the split it was built with, because the
	// background loop looks for memories with no chunks at all rather than for
	// memories chunked differently. Its existing chunks stay valid and keep
	// serving recall; they are simply at the old granularity. To re-split
	// everything, turn MEMINI_CHUNK_EMBED off and on again is NOT enough — the
	// rows persist. Today the honest answer is that re-splitting an existing
	// corpus needs a rewrite of those memories.
	ChunkSize int `env:"MEMINI_CHUNK_SIZE" envDefault:"1200"`
	// ChunkOverlap is how many runes each chunk repeats from the previous one,
	// so a fact spanning a boundary survives whole in one of them. Must be less
	// than MEMINI_CHUNK_SIZE.
	ChunkOverlap int `env:"MEMINI_CHUNK_OVERLAP" envDefault:"200"`
	// ChunkMinContent is the content length at or below which a memory gets no
	// chunks at all. Below this the whole-memory vector already covers the text,
	// so a chunk would duplicate it: a wasted embedder call, a wasted row, and a
	// duplicate hit to merge away. Must be at or above MEMINI_CHUNK_SIZE —
	// enforced at boot, because a lower value silently produces exactly one
	// whole-content chunk per mid-sized memory, the pure waste this floor
	// exists to prevent.
	ChunkMinContent int `env:"MEMINI_CHUNK_MIN_CONTENT" envDefault:"1200"`
	// ChunkMaxPerMemory caps the chunks one memory may produce (the default
	// covers roughly 64k runes). Past it the tail stays uncovered by chunk
	// recall and the server logs a warning — an observable ceiling, unlike the
	// silent one it replaces.
	ChunkMaxPerMemory int `env:"MEMINI_CHUNK_MAX_PER_MEMORY" envDefault:"64"`
	// ChunkScoreWeight scales a chunk hit's score before it is compared with a
	// whole-memory hit. 1 leaves the two directly comparable.
	//
	// It exists because max-pooling has a length bias: a maximum over more
	// samples is higher in expectation, so a long memory with many chunks tends
	// to out-score a short one on the same query. Recall's gates
	// (MEMINI_RECALL_MIN_SCORE, the semantic reserve) are absolute thresholds
	// calibrated against the current score distribution rather than ranks, so
	// that bias shifts real behaviour rather than just reordering results.
	// Below 1 a chunk hit must beat a whole-memory hit by a margin to win. Tune
	// it with the benchmark harness (mise run bench), not by intuition.
	ChunkScoreWeight float64 `env:"MEMINI_CHUNK_SCORE_WEIGHT" envDefault:"1.0"`
	// ReembedOnModelChange makes the server re-embed every stored memory at
	// startup when MEMINI_EMBED_MODEL differs from the model the vectors were
	// produced with, instead of refusing to start. Off by default: re-embedding
	// hits the embeddings endpoint once per memory and blocks startup, so it
	// must be opted into (the `memini reembed` command is the explicit
	// alternative). Dimensionality still cannot change this way.
	ReembedOnModelChange bool `env:"MEMINI_REEMBED_ON_MODEL_CHANGE" envDefault:"false"`

	// WriteDedupScore is the fused vector similarity (0..1) at or above which a
	// fresh write is treated as a near-duplicate of its nearest same-tier memory,
	// triggering WriteDedupAction. 0 disables write-time dedup regardless of the
	// action. The right value is embedder-dependent (~0.9 collapses near-identical
	// restatements only; the default 0.625 was calibrated for merge hints in
	// bench/dedup_test.go). See WriteDedupAction for what happens at/above it.
	WriteDedupScore float64 `env:"MEMINI_WRITE_DEDUP_SCORE" envDefault:"0.625"`

	// WriteDedupAction picks what happens when a write scores >= WriteDedupScore
	// against its nearest same-tier memory:
	//   - "hint" (default): store the write and return a MergeHint so the caller
	//     (agent or human) can merge via memory_update. Non-destructive; scoped to
	//     durable tiers (semantic/procedural), where the threshold was calibrated
	//     and the hint is consumed — episodic/working writes skip the lookup.
	//   - "coalesce": reinforce the existing memory and drop the write (headless
	//     corpus hygiene; use a high score like 0.9). Applies to all tiers.
	//   - "supersede": store the write and tombstone the old memory ("new wins").
	//   - "off": no write-time dedup (the exact-restatement fingerprint pass,
	//     WriteDedupFingerprint, still runs independently).
	WriteDedupAction string `env:"MEMINI_WRITE_DEDUP_ACTION" envDefault:"hint"`

	// SplitDedupLLMMerge (opt-in, default off) routes ambiguous split-dedup
	// candidates (≥2 close neighbours) through the LLM consolidator for a
	// merge/supersede verdict before the deterministic action fires. Requires
	// a consolidator to be configured.
	SplitDedupLLMMerge bool `env:"MEMINI_SPLIT_DEDUP_LLM_MERGE" envDefault:"false"`

	// ContradictionDownrank (default on) invalidates a durable fact when a fresh
	// durable write contradicts it (changed value or flipped polarity, confirmed
	// by the lexical detector): the stale fact's valid_to is stamped so it leaves
	// live recall while AsOf time-travel can still reach it, and its confidence
	// is shrunk. Reversible (Restore clears valid_to) and precision-first (0
	// restatement misfires measured in bench/contradiction_test.go). Set false to
	// disable — the kill-switch; there is no threshold knob.
	ContradictionDownrank bool `env:"MEMINI_CONTRADICT_DOWNRANK" envDefault:"true"`

	// GlobalNamespace and TenantShared (MEMINI_GLOBAL_NAMESPACE /
	// MEMINI_TENANT_SHARED) are deleted (T12): the old opt-in shared-scope
	// model is replaced by the ancestor cascade. Both env vars are now fatal
	// at server boot — see deprecatedVars / FatalDeprecatedVars below and
	// docs/scopes.md#knobs.

	// Cascade (default true) is the server-wide switch for the ancestor/home/
	// link read cascade. When true, a recall or briefing in namespace N also
	// reads N's ancestors, the caller's home namespace, and N's stored links —
	// durable tiers only. Set false to restore pre-cascade isolation: the
	// default read set becomes N (and its subtree, when asked) only, and Scope
	// "full"/"everywhere" no longer add the cascade legs. A per-call Scope of
	// "project" already suppresses the cascade for one request; this is the
	// global default for operators (or upgraders) who want isolation without
	// setting Scope on every call. See docs/scopes.md#knobs.
	Cascade bool `env:"MEMINI_CASCADE" envDefault:"true"`

	// LLM (opt-in; empty BaseURL disables the consolidation pipeline).

	// LLMBaseURL is the master switch for everything that needs a chat model.
	// Empty (the default) is a fully supported way to run: marker heuristics
	// still do write-time extraction, tier classification, promotion,
	// corroboration and contradiction handling, so durable knowledge still
	// accumulates. Setting it adds background consolidation, POST /v1/answer,
	// the memory_answer MCP tool, and Rerank="llm".
	LLMBaseURL string `env:"MEMINI_LLM_BASE_URL"`
	// LLMAPIKey is the bearer token for the LLM endpoint, when it wants one.
	// Optional.
	LLMAPIKey string `env:"MEMINI_LLM_API_KEY"`
	// LLMModel is the chat model used for consolidation, distillation, answering
	// and LLM reranking.
	LLMModel string `env:"MEMINI_LLM_MODEL" envDefault:"gpt-4o-mini"`
	// LLMAPI selects the chat backend: "openai" (default) or "anthropic".
	LLMAPI string `env:"MEMINI_LLM_API" envDefault:"openai"`
	// LLMMaxTokens caps the completion length of every chat call. 0 (the
	// default) keeps the client's built-in budget (4096). Reasoning models
	// that spend thousands of hidden thinking tokens inside the budget need
	// more headroom here, or their JSON answers come back truncated
	// (finish_reason "length") and the affected pipeline call is lost.
	LLMMaxTokens int `env:"MEMINI_LLM_MAX_TOKENS" envDefault:"0"`
	// LLMExtraBody is a JSON object merged into every chat request body at
	// the top level, for provider-specific dialect knobs memini deliberately
	// does not model. Typical use: disabling hidden reasoning on
	// DeepSeek-style endpoints ('{"thinking":{"type":"disabled"}}') or
	// Qwen-style ones ('{"enable_thinking":false}'). Fields memini sets
	// itself (model, messages, temperature, max_tokens) always win. Invalid
	// JSON fails config loading.
	LLMExtraBody string `env:"MEMINI_LLM_EXTRA_BODY"`

	// Rerank selects recall reranking: "off" (default), "llm" (reorder with the
	// chat LLM), or a cross-encoder /rerank base URL (e.g. http://host:8002/v1).
	// Reranking reorders the top k composite-ranked candidates; it adds one
	// reranker call per recall.
	Rerank string `env:"MEMINI_RERANK" envDefault:"off"`
	// RerankModel names the cross-encoder to use when Rerank is a URL. The
	// server warns at boot if a URL is set without one.
	RerankModel string `env:"MEMINI_RERANK_MODEL"`
	// RerankAPIKey is the bearer token for the cross-encoder endpoint, when it
	// wants one. Optional.
	RerankAPIKey string `env:"MEMINI_RERANK_API_KEY"`
	// RerankPool is how many composite-ranked candidates are handed to the
	// reranker before the result is truncated to the recall limit. 0 (the
	// default) reranks exactly the limit, which reorders the results but can
	// never surface a memory that ranked below them — most of a cross-encoder's
	// value is precisely that rescue, so a deployment with a reranker wants this
	// set (RecallPoolSize, ~50, is the natural ceiling: recall never retrieves
	// more). Cost is linear — one model forward pass per candidate — so a deep
	// pool trades recall latency for accuracy.
	RerankPool int `env:"MEMINI_RERANK_POOL" envDefault:"0"`
	// RerankMinScore drops rerank candidates whose cross-encoder relevance
	// score falls below it, across the whole RerankPool before the recall
	// limit is applied. Cross-encoders emit calibrated absolute relevance
	// (unlike the fused retrieval score, whose min-max normalization inflates
	// the best of a bad pool), so an absolute floor here is what cuts the
	// noise tail on queries with no real answer: when everything gates out,
	// recall returns EMPTY rather than the least-irrelevant leftovers. The
	// response `score` field still carries the fused score — rerank scores are
	// never exposed on the wire. Cross-encoder only: the LLM reranker returns
	// an ordinal list with no scores, so combining it with this knob is a boot
	// error rather than a gate that silently never fires. 0 (the default)
	// disables the gate; no upper bound is enforced because some /rerank
	// servers emit unbounded logits. Pick a threshold with the rerank-gate
	// bench sweep (bench.RerankGateSweep) against your own reranker.
	RerankMinScore float64 `env:"MEMINI_RERANK_MIN_SCORE" envDefault:"0"`
	// RerankMaxBatchChars caps the total characters across the query and all
	// documents in a single /rerank request. This is an HTTP payload guard, not
	// a context-window guard: a Cohere-style /rerank server scores each
	// (query, document) pair in its own forward pass, so the model's context
	// bounds a single pair, never the batch. Sizing this near the model context
	// shards a deep RerankPool into many *serial* requests (see
	// rerank.CrossEncoder.Rerank), which is far more likely to blow RerankTimeout
	// than a large body is to trouble the server. 0 disables proactive batching.
	RerankMaxBatchChars int `env:"MEMINI_RERANK_MAX_BATCH_CHARS" envDefault:"6000"`
	// RerankMaxDocChars truncates each document sent to the cross-encoder to
	// this many runes, bounding a single (query, document) pair against the
	// model's context. Raise it toward your reranker's context window if your
	// memories are long and the tail carries the signal.
	//
	// RerankMaxBatchChars overrides it whenever it is smaller, so the effective
	// per-document cap is the lower of the two. That includes 0: 0 here means
	// "no cap of my own", which leaves RerankMaxBatchChars (6000 by default) as
	// the cap — NOT unlimited. Truncation is off only when both are 0.
	RerankMaxDocChars int `env:"MEMINI_RERANK_MAX_DOC_CHARS" envDefault:"2048"`
	// RerankLLMMaxDocChars truncates each candidate in the LLM reranker's prompt
	// to this many bytes (not runes — the cut lands on a rune boundary), keeping
	// a deep pool of long memories from blowing a RAM-limited local chat
	// server's context. 0 disables truncation. Only used when MEMINI_RERANK is
	// the LLM reranker; the cross-encoder uses RerankMaxDocChars.
	RerankLLMMaxDocChars int `env:"MEMINI_RERANK_LLM_MAX_DOC_CHARS" envDefault:"300"`
	// RerankTimeout bounds a single reranker call; past it, recall degrades to
	// composite order instead of stalling on a slow or congested backend.
	RerankTimeout time.Duration `env:"MEMINI_RERANK_TIMEOUT" envDefault:"10s"`
	// RerankMaxConcurrency caps in-flight rerank calls. 0 is unbounded. See
	// EmbedMaxConcurrency for the rationale.
	RerankMaxConcurrency int `env:"MEMINI_RERANK_MAX_CONCURRENCY" envDefault:"0"`
	// RecallEmbedTimeout bounds the query embed on the recall path; past it, or on
	// any embed error, recall degrades to keyword-only search instead of stalling
	// on a slow or stuck embeddings backend. Defaults to 2s so a wedged backend
	// can't hang recall indefinitely; set 0 to restore an unbounded query embed.
	RecallEmbedTimeout time.Duration `env:"MEMINI_RECALL_EMBED_TIMEOUT" envDefault:"2s"`
	// RecallRewriteTimeout bounds the LLM query-expansion call on query_rewrite
	// recalls; past it, recall proceeds with the original query alone rather
	// than riding along the LLM client's much longer HTTP timeout. Default 3s;
	// set 0 to restore an unbounded rewrite call.
	RecallRewriteTimeout time.Duration `env:"MEMINI_RECALL_REWRITE_TIMEOUT" envDefault:"3s"`
	// WriteEmbedTimeout bounds the content embed on the remember path; past it, or on
	// embed error, the memory is stored without a vector (keyword-searchable) and
	// queued for background repair. 0 restores fail-fast writes.
	//
	// Lowering it trades write latency for repair latency, not for durability:
	// a degraded write keeps its content and gets its vector and its write-time
	// enrichment back from the repair worker within seconds. Raising it makes
	// more writes keep their vector inline, which is the cheaper path when the
	// embedder is healthy.
	WriteEmbedTimeout time.Duration `env:"MEMINI_WRITE_EMBED_TIMEOUT" envDefault:"5s"`
	// BackgroundEmbedTimeout bounds the embed inside a background repair,
	// deliberately independent of MEMINI_WRITE_EMBED_TIMEOUT.
	//
	// The write budget exists to bound a waiting caller's latency; a repair has
	// no caller. Sharing one value means tightening the write budget also
	// tightens every repair, so a merely-slow embedder would make repairs fail
	// forever while the write path kept degrading — the exact failure repairs
	// exist to recover from. 0 selects the built-in default.
	BackgroundEmbedTimeout time.Duration `env:"MEMINI_BACKGROUND_EMBED_TIMEOUT" envDefault:"30s"`
	// RepairPollInterval is how often the repair worker polls for memories that
	// still owe a vector or the write-time enrichment that follows it. It is
	// only a backstop: a degraded write wakes the worker directly, so this
	// bounds how long a repair waits when that wake-up was missed (the worker
	// was mid-batch) or when another process marked the row. 0 disables the
	// worker, leaving repairs to the sweeper on MEMINI_BACKFILL_INTERVAL.
	RepairPollInterval time.Duration `env:"MEMINI_REPAIR_POLL_INTERVAL" envDefault:"5s"`
	// RecallMinScore is the fused-score floor: candidates below it are dropped
	// before ranking. The default (0.1) is the benched value; it is exposed so a
	// deployment on a different embedder can raise it to trim loosely-relevant
	// injection. Only meaningful with score fusion.
	RecallMinScore float64 `env:"MEMINI_RECALL_MIN_SCORE" envDefault:"0.1"`
	// RecallMinSemanticScore is the raw vector-score floor: candidates below it
	// are dropped before fusion, preventing the keyword leg from reintroducing
	// an off-topic candidate. 0 disables it. It is applied only when the query
	// embedding succeeds; keyword-only fallback remains available on failure.
	RecallMinSemanticScore float64 `env:"MEMINI_RECALL_MIN_SEMANTIC_SCORE" envDefault:"0.46"`
	// RecallSemanticReserve reserves up to N of the recall slots for durable
	// tiers (semantic/procedural) so consolidated knowledge is not crowded out by
	// episodic chatter. Exposed because it changes recall composition per
	// deployment: set 0 for pure-relevance recall (no forced durable slots).
	// Reserved slots are relevance-gated — a durable memory is only promoted in
	// when it is relevance-competitive with the entry it displaces.
	RecallSemanticReserve int `env:"MEMINI_RECALL_SEMANTIC_RESERVE" envDefault:"2"`
	// RecallImportanceReserve reserves up to N slots of the reranker's candidate
	// pool for high-importance candidates the fused score buried below the pool
	// cut, so an important memory still gets judged on its merits. It changes
	// pool membership only — never the composite top-N a rerank-free recall
	// returns — and is therefore structurally inert unless a reranker is
	// configured AND MEMINI_RERANK_POOL exceeds the recall limit. 0 disables it.
	RecallImportanceReserve int `env:"MEMINI_RECALL_IMPORTANCE_RESERVE" envDefault:"2"`
	// RecallImportanceMin is the effective-importance threshold (assessed value
	// when the LLM set one, else stored importance) a candidate must meet to
	// claim a slot reserved by MEMINI_RECALL_IMPORTANCE_RESERVE. The default
	// (0.75) sits above the tier-seeded baseline every memory carries, so only
	// genuinely important memories compete for a reserved slot.
	RecallImportanceMin float64 `env:"MEMINI_RECALL_IMPORTANCE_MIN" envDefault:"0.75"`
	// StabilityK is the spaced-repetition strength (Ebbinghaus stability): a
	// short-term memory's effective recall half-life stretches with reinforcement
	// as halfLife*(1+StabilityK*ln(1+access_count)), so a frequently-recalled
	// memory decays more slowly, improving recall of reinforced-but-aged facts
	// (see bench/reinforcement_test.go). Default 1; set 0 to disable (fixed
	// half-life). Only affects short-term tiers with access_count > 0 — durable
	// tiers and never-recalled memories are unchanged.
	StabilityK float64 `env:"MEMINI_STABILITY_K" envDefault:"1"`
	// AssessedSalienceWeight blends the LLM's self-assessed importance into a
	// memory's salience: above 0 the importance term becomes
	// (1-w)*importance + w*assessed_importance for rows that carry an assessment,
	// leaving unassessed rows untouched. Default 0 (exact no-op). This is a
	// ranking AND lifecycle knob, not a display preference — salience feeds
	// recall's quality term, short-term cap eviction (RetentionScore), briefing
	// order, and dedup representative selection, so raising it changes what gets
	// recalled, what gets evicted, and which duplicate survives. Enable only
	// after benching.
	AssessedSalienceWeight float64 `env:"MEMINI_ASSESSED_SALIENCE_WEIGHT" envDefault:"0"`
	// TurnEchoWindow is the server-wide temporal exclusion window for
	// freshly-captured episodic turns. A just-captured turn
	// (metadata.format="turn") younger than this is dropped from recall by
	// default — a just-captured turn is live context, not long-term memory,
	// and echoing it back makes the agent parrot itself. Callers opt out per
	// call via include_fresh_turns. Default 5m; zero disables it server-wide.
	TurnEchoWindow time.Duration `env:"MEMINI_TURN_ECHO_WINDOW" envDefault:"5m"`
	// Fusion weight stays baked in cmd/memini (tuned via the benchmark harness).

	// EpisodicMinChars drops an episodic write whose substantive content (role
	// scaffolding stripped) is below this many characters — the low-signal
	// per-turn chatter ("keep going", "ok", "hello") that otherwise dominates
	// episodic memory. Only episodic is gated. Default 120 (on); set 0 to disable.
	EpisodicMinChars int `env:"MEMINI_EPISODIC_MIN_CHARS" envDefault:"120"`

	// ClassifyMaxChars bounds a write that picked no tier, in runes: below it
	// the heuristic may label the content semantic or procedural, above it the
	// content reads as session history and falls back to the working tier.
	//
	// This is a cliff, not a truncation — nothing is cut, but a long write that
	// would have earned a durable tier silently lands in working instead and
	// expires with it. Raise it if you write long durable facts without passing
	// an explicit tier. 0 disables classification, so every untier'd write takes
	// the working default.
	//
	// Must be 0 or at least 20 (the extractor's floor, below which there is too
	// little text to be a fact). A ceiling between the two would classify
	// nothing at all while reading like a tight bound, so the server refuses it
	// rather than silently behaving as 0.
	ClassifyMaxChars int `env:"MEMINI_CLASSIFY_MAX_CHARS" envDefault:"400"`

	// Write-time fact building (LLM distill, else heuristic extract) is automatic;
	// it self-selects on LLM presence, so there is no toggle.

	// DistillBatchTokens batches distill-on-write per session: captures
	// accumulate until roughly this many (estimated) tokens, then distill as
	// one LLM call with cross-turn context. 0 restores per-capture distill.
	// Only applies with an LLM configured and to captures with a session_id.
	DistillBatchTokens int `env:"MEMINI_DISTILL_BATCH_TOKENS" envDefault:"1024"`
	// DistillBatchMaxAge flushes a session's buffered captures once the oldest
	// has waited this long, so a quiet session still distills promptly.
	DistillBatchMaxAge time.Duration `env:"MEMINI_DISTILL_BATCH_MAX_AGE" envDefault:"10m"`
	// DistillTimeout bounds one write-time distillation (the LLM call plus
	// fact writes). Distillation runs in the background, so a longer deadline
	// never blocks the write path; raise it when the chat model runs on a
	// slow or shared backend where a distill call can queue behind
	// long-running requests and blow the default before it gets compute.
	DistillTimeout time.Duration `env:"MEMINI_DISTILL_TIMEOUT" envDefault:"60s"`

	// Consolidation tuning.
	// ConsolidateMode is "async" (default), "sync", or "off".
	ConsolidateMode string `env:"MEMINI_CONSOLIDATE_MODE" envDefault:"async"`
	// ConsolidateMinScore gates the LLM: it runs only when the nearest candidate
	// scores at least this. 0 disables the gate.
	ConsolidateMinScore float64 `env:"MEMINI_CONSOLIDATE_MIN_SCORE" envDefault:"0.3"`

	// Promotion (episodic→semantic distillation). Uses the LLM when configured,
	// the marker extractor otherwise, so it also runs LLM-less.
	// PromoteInterval is how often the promoter runs; 0 disables it.
	PromoteInterval time.Duration `env:"MEMINI_PROMOTE_INTERVAL" envDefault:"24h"`
	// PromoteMinAccess is the minimum access_count for an episodic memory to be
	// considered for promotion.
	PromoteMinAccess int `env:"MEMINI_PROMOTE_MIN_ACCESS" envDefault:"3"`
	// PromoteWholeMaxChars bounds LLM-less whole-content promotion, in runes: an
	// eligible episodic memory with no extractable marker is promoted verbatim
	// only if it is this short, since a longer one is unlikely to be the single
	// statement that promotion produces.
	//
	// This is a cliff, not a truncation — a longer source is simply never
	// promoted, however often it was recalled. Raise it if your durable facts
	// are written as paragraphs rather than sentences. 0 disables whole-content
	// promotion, leaving only marker extraction. Ignored when an LLM is
	// configured: distillation replaces the heuristic.
	PromoteWholeMaxChars int `env:"MEMINI_PROMOTE_WHOLE_MAX_CHARS" envDefault:"240"`

	// BackfillInterval is how often the repair sweeper runs: it re-arms repairs
	// parked by a long embedder outage, and adopts memories that owe a vector
	// but carry no repair state — rows written by a release predating the
	// repair columns, or by a path that bypasses the normal write. Shared with
	// the chunk backfill. 0 disables it.
	//
	// It is a safety net, not the main path: a degraded write records its own
	// repair state and wakes the worker directly, so ordinary repairs never
	// wait for this tick.
	BackfillInterval time.Duration `env:"MEMINI_BACKFILL_INTERVAL" envDefault:"1m"`

	// SweepInterval is how often the decay sweeper purges expired memories.
	SweepInterval time.Duration `env:"MEMINI_SWEEP_INTERVAL" envDefault:"1h"`
	// ShortTermCap bounds short-term (working+episodic) memories per namespace;
	// the sweeper evicts the lowest-retention ones over the cap. 0 disables it.
	ShortTermCap int `env:"MEMINI_SHORT_TERM_CAP" envDefault:"1000"`
	// TombstoneTTL hard-deletes superseded (deduped/contradicted) memories last
	// updated before now-TTL, reclaiming space. Off by default: tombstones are
	// excluded from recall regardless, so GC is purely a space optimization and
	// removing it is the only irreversible maintenance action. Set e.g. 720h
	// (30d) to enable.
	TombstoneTTL time.Duration `env:"MEMINI_TOMBSTONE_TTL" envDefault:"0"`

	// ActivityLog records reads (recall/get/briefing) and writes
	// (remember/update/forget/supersede) to the activity log that backs the UI's
	// Activity page. Writes are best-effort and off the request path, so the cost
	// is storage, not latency; set false to record nothing.
	ActivityLog bool `env:"MEMINI_ACTIVITY_LOG" envDefault:"true"`
	// ActivityRetention drops activity events older than this. 0 keeps them
	// forever (bounded only by ActivityMaxRows).
	ActivityRetention time.Duration `env:"MEMINI_ACTIVITY_RETENTION" envDefault:"720h"`
	// ActivityMaxRows caps the activity log, dropping the oldest rows beyond it.
	// A busy agent writes several rows per recall, so the cap — not the retention
	// window — is usually what bounds the table. 0 disables the cap.
	ActivityMaxRows int `env:"MEMINI_ACTIVITY_MAX_ROWS" envDefault:"100000"`
	// DemoteAfter demotes durable memories older than this to the episodic tier
	// when they have never been recalled, are not important, and are
	// uncorroborated (low confidence) — so an old bulk import ages out while
	// facts the agent actually uses or establishes are kept. Default 168h (7d);
	// set to 0 to disable.
	DemoteAfter time.Duration `env:"MEMINI_DEMOTE_AFTER" envDefault:"168h"`

	// Dedup tuning. The dedup pass collapses near-duplicate memories
	// (embedding similarity ≥ DedupSimilarity) into a single representative
	// per cluster; the rest are tombstoned (SupersededBy → representative),
	// not hard-deleted, so the action is reversible. Exposed on-demand via
	// POST /v1/dedup and run as a periodic store-wide background job every
	// DedupInterval (daily by default, so a store stays clean with no manual
	// intervention). Set MEMINI_DEDUP_INTERVAL=0 to disable the periodic pass.
	DedupInterval time.Duration `env:"MEMINI_DEDUP_INTERVAL" envDefault:"24h"`
	// DedupSimilarity is the embedding-similarity threshold for two memories to
	// count as members of the same near-duplicate cluster. Higher is stricter,
	// so raise it if the pass is collapsing memories that were only superficially
	// alike, and lower it if obvious restatements survive.
	DedupSimilarity float64 `env:"MEMINI_DEDUP_SIMILARITY" envDefault:"0.85"`
	// DedupTiers is an optional comma-separated list restricting the periodic
	// pass to specific tiers (working,episodic,semantic,procedural). Empty
	// means all tiers.
	DedupTiers string `env:"MEMINI_DEDUP_TIERS" envDefault:""`
	// DedupLLMMerge (opt-in, default off) enables LLM-based content merging
	// during the periodic dedup pass. Each cluster's content is merged into a
	// single comprehensive memory before tombstoning duplicates. Requires an
	// LLM (MEMINI_LLM_BASE_URL); when false or no LLM, the representative
	// keeps its original content. Defaults off to preserve existing behavior.
	DedupLLMMerge bool `env:"MEMINI_DEDUP_LLM_MERGE" envDefault:"false"`

	// AssessInterval runs a periodic importance-backfill sweep: durable
	// (semantic/procedural) memories that never received an LLM
	// self-assessment — rows written before the feature existed, written
	// without an LLM configured, or ones the model declined to rate — are sent
	// back to the model for a score, which then drives ranking in place of the
	// tier-seeded default. Only active when an LLM is configured
	// (MEMINI_LLM_BASE_URL); without one the job never starts. A memory whose
	// importance was set explicitly is left alone. Hourly by default, spending
	// at most MEMINI_ASSESS_MAX_PER_RUN rows of LLM budget per pass; 0 disables
	// the sweep.
	AssessInterval time.Duration `env:"MEMINI_ASSESS_INTERVAL" envDefault:"1h"`
	// AssessBatch is how many memory texts go into a single LLM call. Larger
	// batches cost less per row but ask the model to hold a longer positional
	// list together, and a reply that does not line up costs the whole batch.
	AssessBatch int `env:"MEMINI_ASSESS_BATCH" envDefault:"20"`
	// AssessMaxPerRun caps the rows one pass assesses, bounding the LLM spend of
	// a single tick. A backlog larger than this drains over successive passes,
	// oldest memories first. 0 falls back to the internal default (200).
	AssessMaxPerRun int `env:"MEMINI_ASSESS_MAX_PER_RUN" envDefault:"200"`
	// AssessMinAge skips memories younger than this, so the sweep never races
	// the write path's own assessment — a fresh write is rated inline by the
	// distill/consolidate call, and a sweep arriving first would waste a slot
	// scoring a row that is about to be scored anyway. 0 falls back to the
	// internal default (1h).
	AssessMinAge time.Duration `env:"MEMINI_ASSESS_MIN_AGE" envDefault:"1h"`

	// UIEnabled mounts the embedded admin UI at /. Enabled by default; set
	// MEMINI_UI_ENABLED=false to run a headless API/MCP-only service.
	UIEnabled bool `env:"MEMINI_UI_ENABLED" envDefault:"true"`

	// Auth (optional).

	// APIKey is the break-glass admin and bootstrap credential. When set, every
	// request must present it (or a valid named key) as a bearer token, and it
	// authenticates as an admin with no principal at all — the recovery path that
	// always works even when no named admin key does. Named keys (see APIKeysFile
	// and `memini key`) can now hold their own admin capability, so this is no
	// longer the ONLY key that can manage others through /v1/keys; it is the one
	// that can bootstrap the first named key and the one to fall back on if the
	// last named admin locks itself out.
	//
	// It also gates two operator surfaces that authenticate ONLY against this
	// env key, never against a named admin key: /metrics on the main port, and
	// the verbose dependency detail of GET /healthz?verbose=1. A named admin key
	// manages other keys but does not unlock those two.
	APIKey string `env:"MEMINI_API_KEY"`

	// APIKeysFile (optional; K2b), when set, names a YAML file of
	// declaratively managed API keys — the GitOps-friendly counterpart to
	// the api_keys table (which is managed imperatively via `memini key
	// ...` / a future /v1/keys API, K3b). The file is loaded exactly ONCE at
	// boot (see internal/apiauth.LoadFileKeys); there is no live reload
	// today — a GitOps rollout restarts the pod on every change to the
	// file's content, which already picks up edits, so a SIGHUP-triggered
	// in-process reload is a reasonable future addition but is not built
	// here. Absent (the default) is a complete no-op: zero behavior change
	// versus a server built before this field existed.
	//
	// Format (see internal/apiauth/testdata/api_keys.example.yaml for a
	// runnable example):
	//
	//	keys:
	//	  - name: alex                             # required, unique within the file
	//	    hash: "<hex sha-256 of the secret>"     # exactly one of hash|secret
	//	    home: personal/alex                     # optional
	//	    default_namespace: acme                 # optional
	//	    disabled: false                         # optional, default false
	//	  - name: ci
	//	    secret: "plaintext secret"              # allowed: the file itself is
	//	                                             # the secret store (e.g.
	//	                                             # SOPS-encrypted at rest);
	//	                                             # hashed at load, never kept
	//	                                             # in memory as plaintext
	//
	// Boot validation is fail-loud: malformed YAML, a missing name, both or
	// neither of hash/secret, a hash that isn't valid hex-encoded SHA-256, a
	// duplicate name within the file, or an invalid home/default_namespace
	// (httputil.ValidateNamespace, after httputil.NormalizeNamespace) all
	// refuse the boot with a message naming this file and the offending
	// entry. A file key that shares a name with an existing api_keys table
	// row wins at auth time (internal/apiauth.Config.Authenticate: file
	// checked before the table); the server logs a warning at boot listing
	// which table keys are shadowed this way.
	APIKeysFile string `env:"MEMINI_API_KEYS_FILE"`

	// ClientDefaultsRaw (MEMINI_CLIENT_DEFAULTS; optional; config-handshake
	// redesign), when set, is a
	// JSON-encoded ClientSettings object (e.g. `{"capture_turns":false}`) that
	// becomes the server's GLOBAL default behavioral-settings layer — the layer
	// between the built-in defaults and any per-key override, which
	// POST /v1/handshake and GET /v1/self resolve through. Managing it here,
	// via the environment, is the GitOps-friendly counterpart to editing it at
	// runtime through PUT /v1/settings/defaults: when this is set, that endpoint
	// is refused (409) and the KV store is not consulted for globals, so the
	// env is the single source of truth and can't be silently overridden.
	//
	// Boot validation is fail-loud, matching MEMINI_API_KEYS_FILE: invalid JSON,
	// an unknown field, or a value that fails ClientSettings' range/enum checks
	// refuses the boot with a message naming this variable. Absent (the default)
	// is a complete no-op: ClientDefaults stays nil and the KV-backed global
	// defaults apply unchanged. Only the fields you set are stored; the rest
	// keep inheriting the built-in defaults.
	ClientDefaultsRaw string `env:"MEMINI_CLIENT_DEFAULTS"`

	// Multi-tenancy. The fallback namespace when no header is sent; the header
	// name itself is fixed (DefaultNamespaceHeader).
	DefaultNamespace string
	NamespaceSrc     NamespaceSource

	// ClientDefaults is the parsed, validated MEMINI_CLIENT_DEFAULTS (see
	// ClientDefaultsRaw), resolved separately in Load() like DefaultNamespace so
	// it carries no env tag. nil means the variable was unset — the KV-backed
	// global-defaults layer applies instead.
	ClientDefaults *store.ClientSettings

	// Home is the caller's personal namespace: merged read-only (durable
	// tiers only) into the default read set on every recall/briefing/answer,
	// on top of the request namespace and its ancestors. Client-side only —
	// the server never derives it. On HTTP transports it is carried per-request
	// by the X-Memini-Home header (DefaultHomeHeader); this env var is what the
	// stdio MCP server (`memini mcp`) resolves instead, since stdio has no
	// headers. Empty means no home leg (unset by default).
	Home string `env:"MEMINI_HOME"`
}

// DefaultNamespaceHeader is the request header carrying the per-request namespace.
// Fixed (no env override): clients and plugins all send this exact header.
const DefaultNamespaceHeader = "X-Memini-Namespace"

// DefaultHomeHeader is the request header carrying the caller's personal
// namespace (see Config.Home). Fixed (no env override), same rationale as
// DefaultNamespaceHeader. Unlike the namespace header, its absence has no
// default — no header means no home leg for that request.
const DefaultHomeHeader = "X-Memini-Home"

// valueOff is the shared "disabled" token for the string-enum settings
// (MEMINI_RERANK, MEMINI_CONSOLIDATE_MODE, MEMINI_WRITE_DEDUP_ACTION).
const valueOff = "off"

// deprecatedVars maps removed environment variables to migration guidance. A
// non-fatal entry is ignored at load; DeprecationWarnings surfaces it so an
// operator upgrading from an older release learns why their tuning no longer
// applies — without the boot failing. A fatal entry is the opposite: the
// change underneath it is not safe to silently ignore (the scope model
// itself changed), so FatalDeprecatedVars refuses the boot instead of just
// warning. See FatalDeprecatedVars for where that refusal is (and, just as
// importantly, is not) enforced.
var deprecatedVars = []struct {
	name     string
	guidance string
	fatal    bool
}{
	{"MEMINI_WRITE_DEDUP_MIN_SCORE", "use MEMINI_WRITE_DEDUP_SCORE with MEMINI_WRITE_DEDUP_ACTION=coalesce", false},
	{"MEMINI_MERGE_HINT_MIN_SCORE", "use MEMINI_WRITE_DEDUP_SCORE with MEMINI_WRITE_DEDUP_ACTION=hint (the default)", false},
	{"MEMINI_AUTO_SUPERSEDE_MIN_SCORE", "use MEMINI_WRITE_DEDUP_SCORE with MEMINI_WRITE_DEDUP_ACTION=supersede", false},
	{"MEMINI_DEDUP_MIN_CLUSTER_SIZE", "now a fixed internal default (2)", false},
	{"MEMINI_DEDUP_NEIGHBOURS", "now a fixed internal default (20)", false},
	{"MEMINI_CONSOLIDATE_QUEUE_CAP", "now a fixed internal default (1024)", false},
	{"MEMINI_NAMESPACE_HEADER", "the header name is fixed to X-Memini-Namespace", false},
	{"MEMINI_FUSION_ALPHA", "now a baked retrieval default (0.5); tune via the benchmark harness, not env", false},
	{"MEMINI_TEMPORAL_BOOST", "now a baked retrieval default (0.40)", false},
	{"MEMINI_REDACT_SECRETS", "secret redaction is always on", false},
	{"MEMINI_REINFORCE_SKIP_MARKERS", "always on", false},
	{"MEMINI_WRITE_DEDUP_FINGERPRINT", "exact-restatement dedup is always on", false},
	{"MEMINI_QUARANTINE_GARBLED", "removed; garbled-content downranking is no longer configurable", false},
	{"MEMINI_DISTILL_ON_WRITE", "write-time fact building is automatic (LLM when configured, heuristic extractor otherwise)", false},
	{"MEMINI_EXTRACT_ON_WRITE", "write-time fact building is automatic (LLM when configured, heuristic extractor otherwise)", false},
	{"MEMINI_DISTILL_DROP_NO_FACT", "removed; episodic captures are always kept", false},
	{"MEMINI_GLOBAL_NAMESPACE", "the scope model changed: namespaces are now always merged via the ancestor " +
		"cascade, replacing the old opt-in global namespace. Run `memini migrate scopes` to fold any " +
		"<tenant>/_shared data forward, and adopt the old global namespace via MEMINI_HOME (single-operator) " +
		"or `memini link add <ns> <old-global>` (team-wide, per namespace that needs it) — see docs/scopes.md#knobs", true},
	{"MEMINI_TENANT_SHARED", "the scope model changed: namespaces are now always merged via the ancestor " +
		"cascade, replacing the old opt-in tenant-shared merge. Run `memini migrate scopes` to fold each " +
		"<tenant>/_shared namespace into <tenant>, and adopt MEMINI_HOME or `memini link add` if you also " +
		"relied on a global namespace — see docs/scopes.md#knobs", true},
}

// DeprecationWarnings returns one message per removed, non-fatal environment
// variable that is currently set, telling the operator what to use instead.
// These variables are ignored either way; this only explains the change.
// Empty when none are set. Fatal deprecated vars (see FatalDeprecatedVars)
// are excluded — they refuse the boot instead of just warning, so a warning
// here would never be reached in that path anyway.
func DeprecationWarnings() []string {
	var w []string
	for _, d := range deprecatedVars {
		if d.fatal {
			continue
		}
		if _, ok := os.LookupEnv(d.name); ok {
			w = append(w, fmt.Sprintf("%s is removed and ignored; %s", d.name, d.guidance))
		}
	}
	return w
}

// FatalDeprecatedVars returns one refusal message per fatal deprecated
// environment variable (MEMINI_GLOBAL_NAMESPACE, MEMINI_TENANT_SHARED) that
// is currently set. Unlike DeprecationWarnings, these are not safe to boot
// through silently: both named the old opt-in shared-scope model, which the
// always-on ancestor cascade replaced outright, so booting as if the var
// were never set would silently drop the operator's expectation of shared
// visibility. Empty when neither is set.
//
// Deliberately NOT checked inside Load(): `memini migrate scopes`
// (cmd/memini/migrate.go) also calls config.Load() and separately reads
// MEMINI_GLOBAL_NAMESPACE via os.Getenv to print adoption instructions for
// exactly this case. If the refusal lived in Load(), the one command that
// handles the migration could never run while the var that triggers it is
// set — an operator deadlock. Instead this is called explicitly from the
// server-boot paths (cmd/memini/root.go runServer and runMCP), the callers
// that need the refusal: booting a long-running server (REST or MCP) with a
// stale shared-scope expectation baked into the operator's env is the
// dangerous case; one-shot CLI commands (migrate, doctor, reembed, ...) are
// not "booting" anything and are unaffected.
func FatalDeprecatedVars() []string {
	var msgs []string
	for _, d := range deprecatedVars {
		if !d.fatal {
			continue
		}
		if _, ok := os.LookupEnv(d.name); ok {
			msgs = append(msgs, fmt.Sprintf("%s is set; %s", d.name, d.guidance))
		}
	}
	return msgs
}

// LLMEnabled reports whether the opt-in LLM pipeline is configured.
func (c *Config) LLMEnabled() bool { return c.LLMBaseURL != "" }

// RerankEnabled reports whether recall reranking is configured.
func (c *Config) RerankEnabled() bool { return c.Rerank != "" && c.Rerank != valueOff }

// RerankIsLLM reports whether reranking uses the chat LLM rather than a
// cross-encoder URL.
func (c *Config) RerankIsLLM() bool { return c.Rerank == "llm" }

// Load reads configuration from the environment and validates it.
func Load() (*Config, error) {
	c := &Config{}
	if err := env.Parse(c); err != nil {
		return nil, fmt.Errorf("parsing config from environment: %w", err)
	}
	ns, src := resolveDefaultNamespace()
	c.DefaultNamespace = ns
	c.NamespaceSrc = src
	if err := c.resolveClientDefaults(); err != nil {
		return nil, err
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// resolveClientDefaults parses MEMINI_CLIENT_DEFAULTS (see ClientDefaultsRaw)
// into c.ClientDefaults. Fail-loud at boot like MEMINI_API_KEYS_FILE: invalid
// JSON, an unknown field (a typo the operator wants caught, not silently
// dropped), or a value failing ClientSettings.Validate refuses the boot with a
// message naming the variable. An unset/blank value is a no-op — ClientDefaults
// stays nil and the KV-backed global-defaults layer applies unchanged.
func (c *Config) resolveClientDefaults() error {
	raw := strings.TrimSpace(c.ClientDefaultsRaw)
	if raw == "" {
		return nil
	}
	var s store.ClientSettings
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return fmt.Errorf("MEMINI_CLIENT_DEFAULTS: invalid JSON: %w", err)
	}
	if err := s.Validate(); err != nil {
		return fmt.Errorf("MEMINI_CLIENT_DEFAULTS: %w", err)
	}
	c.ClientDefaults = &s
	return nil
}

// resolveDefaultNamespace picks the fallback namespace when no
// X-Memini-Namespace header is sent. Resolution order, matching agentmemory's
// resolveProject helper:
//
//  1. MEMINI_DEFAULT_NAMESPACE (or MEMINI_NAMESPACE) env, if non-empty
//  2. basename of `git rev-parse --show-toplevel` in the current working dir
//  3. basename of the current working dir
//  4. literal "default"
//
// The git lookup is bounded by a short timeout and never errors out: failure
// simply falls through to the cwd basename.
func resolveDefaultNamespace() (string, NamespaceSource) {
	if v := firstNonEmpty(
		os.Getenv("MEMINI_DEFAULT_NAMESPACE"),
		os.Getenv("MEMINI_NAMESPACE"),
	); v != "" {
		return sanitizeNamespacePath(v), NamespaceFromEnv
	}
	cwd, err := os.Getwd()
	if err != nil {
		return defaultNamespace, NamespaceFromLiteral
	}
	if top := gitToplevel(cwd); top != "" {
		return sanitizeNamespace(filepath.Base(top)), NamespaceFromGit
	}
	return sanitizeNamespace(filepath.Base(cwd)), NamespaceFromCWD
}

// sanitizeNamespace strips path separators and trims whitespace so a
// basename like "my-project" survives but a user-supplied multi-segment
// value gets reduced to its last segment. Empty after sanitization falls
// back to the literal default. Only appropriate for git/cwd-derived values,
// where taking the basename is the intent — see sanitizeNamespacePath for
// env-sourced values, which are a deliberate multi-segment namespace, not a
// path to flatten.
func sanitizeNamespace(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return defaultNamespace
	}
	s = filepath.Base(s)
	if s == "" || s == "." || s == string(filepath.Separator) {
		return defaultNamespace
	}
	return s
}

// sanitizeNamespacePath trims whitespace and leading/trailing "/", and
// collapses runs of "/" to one — cleanup without a basename, so a
// deliberate multi-segment value like "project/agent" survives intact
// instead of being flattened to "agent". Empty after cleanup falls back to
// the literal default. Used for env-sourced namespace values
// (MEMINI_DEFAULT_NAMESPACE / MEMINI_NAMESPACE); see sanitizeNamespace for
// git/cwd-derived values, where a basename is the intent.
func sanitizeNamespacePath(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, "/")
	for strings.Contains(s, "//") {
		s = strings.ReplaceAll(s, "//", "/")
	}
	if s == "" {
		return defaultNamespace
	}
	return s
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// gitToplevel returns the absolute path of the git worktree root for dir,
// or "" if dir is not inside a git repo or the lookup fails/times out.
func gitToplevel(dir string) string {
	ctx, cancel := execContext()
	defer cancel()
	out, err := runGit(ctx, dir, "git", "rev-parse", "--show-toplevel")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// validateChunking checks the chunked-embedding knobs. Split out of validate()
// rather than inlined: validate() is already at the cyclomatic ceiling, and
// these five rules only relate to each other.
func (c *Config) validateChunking() error {
	if !c.ChunkEmbed {
		return nil // the knobs are inert; a stale value must not block boot
	}
	if c.ChunkSize <= 0 {
		return fmt.Errorf("MEMINI_CHUNK_SIZE must be > 0 when MEMINI_CHUNK_EMBED is on, got %d", c.ChunkSize)
	}
	if c.ChunkOverlap < 0 || c.ChunkOverlap >= c.ChunkSize {
		// An overlap at or past the size rewinds at least as far as each chunk
		// advances, so the split could never reach the end of the content.
		return fmt.Errorf("MEMINI_CHUNK_OVERLAP must be >= 0 and < MEMINI_CHUNK_SIZE (%d), got %d",
			c.ChunkSize, c.ChunkOverlap)
	}
	if c.ChunkMaxPerMemory <= 0 {
		return fmt.Errorf("MEMINI_CHUNK_MAX_PER_MEMORY must be > 0, got %d", c.ChunkMaxPerMemory)
	}
	if c.ChunkMinContent < c.ChunkSize {
		// Content in (MinContent, Size] fits one window, so Split emits a
		// single chunk equal to the whole content — a verbatim duplicate of
		// the document vector, once per mid-sized memory, corpus-wide. Every
		// document (and the default) keeps MinContent >= Size; refuse a value
		// that quietly buys nothing but embedder spend.
		return fmt.Errorf("MEMINI_CHUNK_MIN_CONTENT must be >= MEMINI_CHUNK_SIZE (%d), got %d: "+
			"a lower floor makes every mid-sized memory a single whole-content chunk that duplicates "+
			"its own document vector", c.ChunkSize, c.ChunkMinContent)
	}
	if c.ChunkScoreWeight <= 0 {
		// 0 would score every chunk hit at 0, which is not "off" but "collect
		// the rows, embed them, then discard every result" — set
		// MEMINI_CHUNK_EMBED=false to turn the feature off.
		return fmt.Errorf("MEMINI_CHUNK_SCORE_WEIGHT must be > 0, got %v: "+
			"set MEMINI_CHUNK_EMBED=false to disable chunked recall", c.ChunkScoreWeight)
	}
	if c.EmbedMaxItemChars > 0 && c.ChunkSize > c.EmbedMaxItemChars {
		// A chunk over the per-item budget is truncated on its way to the
		// embedder — the exact bug chunking removes, reintroduced one layer
		// down. Refuse rather than ship a feature that silently does nothing.
		return fmt.Errorf("MEMINI_CHUNK_SIZE (%d) exceeds MEMINI_EMBED_MAX_ITEM_CHARS (%d): "+
			"chunks would themselves be truncated before embedding", c.ChunkSize, c.EmbedMaxItemChars)
	}
	return nil
}

// validateRecallScores checks the recall-path score floors and ranking weights.
// The fused floor and the salience blend weight are [0,1] range checks; the
// rerank gate additionally rejects a configuration the runtime could never
// honor — the LLM backend returns an ordinal list with no scores, so accepting
// the combination would configure a gate that silently never fires, which reads
// as "the gate is broken" with nothing to debug.
func (c *Config) validateRecallScores() error {
	if c.RecallMinScore < 0 || c.RecallMinScore > 1 {
		return fmt.Errorf("MEMINI_RECALL_MIN_SCORE must be in [0,1], got %v", c.RecallMinScore)
	}
	if math.IsNaN(c.RecallMinSemanticScore) || math.IsInf(c.RecallMinSemanticScore, 0) ||
		c.RecallMinSemanticScore < 0 || c.RecallMinSemanticScore > 1 {
		return fmt.Errorf("MEMINI_RECALL_MIN_SEMANTIC_SCORE must be finite and in [0,1], got %v", c.RecallMinSemanticScore)
	}
	if c.RerankMinScore < 0 {
		return fmt.Errorf("MEMINI_RERANK_MIN_SCORE must be >= 0, got %v", c.RerankMinScore)
	}
	if c.RerankMinScore > 0 && c.RerankIsLLM() {
		return fmt.Errorf("MEMINI_RERANK_MIN_SCORE requires a cross-encoder reranker: " +
			"the LLM reranker returns an ordinal list with no scores, so the gate would " +
			"silently never fire (unset it, or point MEMINI_RERANK at a /rerank endpoint)")
	}
	if c.AssessedSalienceWeight < 0 || c.AssessedSalienceWeight > 1 {
		return fmt.Errorf("MEMINI_ASSESSED_SALIENCE_WEIGHT must be in [0,1], got %v", c.AssessedSalienceWeight)
	}
	if c.RecallImportanceReserve < 0 {
		return fmt.Errorf("MEMINI_RECALL_IMPORTANCE_RESERVE must be >= 0, got %d", c.RecallImportanceReserve)
	}
	if c.RecallImportanceMin < 0 || c.RecallImportanceMin > 1 {
		return fmt.Errorf("MEMINI_RECALL_IMPORTANCE_MIN must be in [0,1], got %v", c.RecallImportanceMin)
	}
	return nil
}

func (c *Config) validateLLM() error {
	if c.LLMMaxTokens < 0 {
		return fmt.Errorf("MEMINI_LLM_MAX_TOKENS must be >= 0, got %d", c.LLMMaxTokens)
	}
	if _, err := c.LLMExtraBodyMap(); err != nil {
		return err
	}
	return nil
}

// validateRepair checks the deferred-repair knobs. Split out of validate to
// keep it under the cyclomatic budget, matching validateChunking and
// validateRecallScores.
//
// 0 is legal for both: it means "no repair worker" (repairs fall back to the
// sweeper) and "use the built-in background embed budget". Only a negative
// value is a misconfiguration.
func (c *Config) validateRepair() error {
	if c.RepairPollInterval < 0 {
		return fmt.Errorf("MEMINI_REPAIR_POLL_INTERVAL must be >= 0, got %v", c.RepairPollInterval)
	}
	if c.BackgroundEmbedTimeout < 0 {
		return fmt.Errorf("MEMINI_BACKGROUND_EMBED_TIMEOUT must be >= 0, got %v", c.BackgroundEmbedTimeout)
	}
	return nil
}

// validateAssess checks the importance-backfill knobs. Split out of validate to
// keep it under the cyclomatic budget, matching validateChunking, validateRepair
// and validateRecallScores.
//
// A zero interval is the documented "off" switch and a zero cap or min-age falls
// back to the job's own defaults, so only negatives are misconfigurations. The
// batch size is the exception: it divides the work, and a zero would mean
// "assess nothing, forever" rather than anything an operator could want.
func (c *Config) validateAssess() error {
	if c.AssessInterval < 0 {
		return fmt.Errorf("MEMINI_ASSESS_INTERVAL must be >= 0, got %v", c.AssessInterval)
	}
	if c.AssessBatch <= 0 {
		return fmt.Errorf("MEMINI_ASSESS_BATCH must be > 0, got %d", c.AssessBatch)
	}
	if c.AssessMaxPerRun < 0 {
		return fmt.Errorf("MEMINI_ASSESS_MAX_PER_RUN must be >= 0, got %d", c.AssessMaxPerRun)
	}
	if c.AssessMinAge < 0 {
		return fmt.Errorf("MEMINI_ASSESS_MIN_AGE must be >= 0, got %v", c.AssessMinAge)
	}
	return nil
}

func (c *Config) validateBackend() error {
	switch c.Backend {
	case BackendSQLite:
	case BackendPostgres:
		if c.PostgresDSN == "" {
			return fmt.Errorf("MEMINI_POSTGRES_DSN is required when MEMINI_BACKEND=postgres")
		}
	default:
		return fmt.Errorf("unknown MEMINI_BACKEND %q (want sqlite|postgres)", c.Backend)
	}
	return nil
}

func (c *Config) validate() error {
	if err := c.validateBackend(); err != nil {
		return err
	}
	if c.EmbedDims <= 0 {
		return fmt.Errorf("MEMINI_EMBED_DIMS must be positive, got %d", c.EmbedDims)
	}
	if err := c.validateLLM(); err != nil {
		return err
	}
	if c.RequestTimeout < 0 {
		return fmt.Errorf("MEMINI_REQUEST_TIMEOUT must be >= 0, got %v", c.RequestTimeout)
	}
	// The sweeper always runs (there is no "disabled" mode), and time.NewTicker
	// panics on a non-positive duration, so a zero/negative interval must be
	// rejected at load time rather than crash the sweeper goroutine.
	if c.SweepInterval <= 0 {
		return fmt.Errorf("MEMINI_SWEEP_INTERVAL must be positive, got %v", c.SweepInterval)
	}
	if err := c.validateRepair(); err != nil {
		return err
	}
	switch c.ConsolidateMode {
	case "async", "sync", valueOff:
	default:
		return fmt.Errorf("unknown MEMINI_CONSOLIDATE_MODE %q (want async|sync|off)", c.ConsolidateMode)
	}
	if c.DedupSimilarity < 0 || c.DedupSimilarity > 1 {
		return fmt.Errorf("MEMINI_DEDUP_SIMILARITY must be in [0,1], got %v", c.DedupSimilarity)
	}
	for _, t := range c.dedupTiers() {
		if !t.Valid() {
			return fmt.Errorf("unknown tier %q in MEMINI_DEDUP_TIERS (want working|episodic|semantic|procedural)", t)
		}
	}
	if err := c.validateAssess(); err != nil {
		return err
	}
	// Write-time dedup: one similarity threshold + one action. No band ordering
	// to get wrong, so no config combination can make startup fail.
	if c.WriteDedupScore < 0 || c.WriteDedupScore > 1 {
		return fmt.Errorf("MEMINI_WRITE_DEDUP_SCORE must be in [0,1], got %v", c.WriteDedupScore)
	}
	if err := c.validateRecallScores(); err != nil {
		return err
	}
	if c.ConsolidateMinScore < 0 || c.ConsolidateMinScore > 1 {
		return fmt.Errorf("MEMINI_CONSOLIDATE_MIN_SCORE must be in [0,1], got %v", c.ConsolidateMinScore)
	}
	if c.RecallSemanticReserve < 0 {
		return fmt.Errorf("MEMINI_RECALL_SEMANTIC_RESERVE must be >= 0, got %d", c.RecallSemanticReserve)
	}
	if c.StabilityK < 0 {
		return fmt.Errorf("MEMINI_STABILITY_K must be >= 0, got %v", c.StabilityK)
	}
	if c.EpisodicMinChars < 0 {
		return fmt.Errorf("MEMINI_EPISODIC_MIN_CHARS must be >= 0, got %d", c.EpisodicMinChars)
	}
	for _, f := range []struct {
		name string
		v    int
	}{
		{"MEMINI_EMBED_MAX_ITEM_CHARS", c.EmbedMaxItemChars},
		{"MEMINI_RERANK_MAX_DOC_CHARS", c.RerankMaxDocChars},
		{"MEMINI_RERANK_LLM_MAX_DOC_CHARS", c.RerankLLMMaxDocChars},
		{"MEMINI_CLASSIFY_MAX_CHARS", c.ClassifyMaxChars},
		{"MEMINI_PROMOTE_WHOLE_MAX_CHARS", c.PromoteWholeMaxChars},
	} {
		if f.v < 0 {
			return fmt.Errorf("%s must be >= 0, got %d", f.name, f.v)
		}
	}
	// A classify ceiling below the extractor's floor admits nothing: every write
	// is either under the floor or over the ceiling. That reads like a tight
	// bound and silently acts as an off switch, sending every untier'd write to
	// the working tier, so refuse it. 0 stays legal — it says "off" plainly.
	if err := c.validateChunking(); err != nil {
		return err
	}
	if c.ClassifyMaxChars > 0 && c.ClassifyMaxChars < extract.MinFactChars {
		return fmt.Errorf(
			"MEMINI_CLASSIFY_MAX_CHARS must be 0 (off) or >= %d, got %d: a ceiling below "+
				"the %d-character floor classifies nothing at all",
			extract.MinFactChars, c.ClassifyMaxChars, extract.MinFactChars)
	}
	if c.DistillBatchTokens < 0 {
		return fmt.Errorf("MEMINI_DISTILL_BATCH_TOKENS must be >= 0, got %d", c.DistillBatchTokens)
	}
	if c.DistillBatchTokens > 0 && c.DistillBatchMaxAge <= 0 {
		return fmt.Errorf("MEMINI_DISTILL_BATCH_MAX_AGE must be positive when batching is on, got %v", c.DistillBatchMaxAge)
	}
	switch c.WriteDedupAction {
	case valueOff, "hint", "coalesce", "supersede":
	default:
		return fmt.Errorf("unknown MEMINI_WRITE_DEDUP_ACTION %q (want off|hint|coalesce|supersede)", c.WriteDedupAction)
	}
	return nil
}

// DedupTierList parses MEMINI_DEDUP_TIERS into the tiers the periodic dedup
// pass is restricted to. Empty/unset returns nil, meaning all tiers. Values
// are validated in validate(), so the result is safe to use directly.
func (c *Config) DedupTierList() []memory.Tier {
	return c.dedupTiers()
}

func (c *Config) dedupTiers() []memory.Tier {
	if strings.TrimSpace(c.DedupTiers) == "" {
		return nil
	}
	var tiers []memory.Tier
	for p := range strings.SplitSeq(c.DedupTiers, ",") {
		if p = strings.TrimSpace(p); p != "" {
			tiers = append(tiers, memory.Tier(p))
		}
	}
	return tiers
}

// LLMExtraBodyMap parses LLMExtraBody into the raw top-level fields to merge
// into every chat request, or nil when unset. A non-object or invalid JSON
// value is a configuration error.
func (c *Config) LLMExtraBodyMap() (map[string]json.RawMessage, error) {
	raw := strings.TrimSpace(c.LLMExtraBody)
	if raw == "" {
		return nil, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, fmt.Errorf("MEMINI_LLM_EXTRA_BODY: invalid JSON object: %w", err)
	}
	return m, nil
}
