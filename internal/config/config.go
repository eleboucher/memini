package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"

	"github.com/eleboucher/memini/internal/memory"
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
	HTTPAddr        string        `env:"MEMINI_HTTP_ADDR" envDefault:":8080"`
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
	// admin UI on its own listener instead of the main HTTP port. The shell
	// embeds MEMINI_API_KEY, so isolating it to a dedicated port lets operators
	// expose that port only on a trusted (LAN) gateway while the main port
	// carries the token-free API. The UI listener also serves the API so the
	// same-origin SPA can call /v1. Empty (the default) keeps the UI on the main
	// port when MEMINI_UI_ENABLED is true.
	UIAddr string `env:"MEMINI_UI_ADDR"`

	// Logging.
	LogLevel  string `env:"MEMINI_LOG_LEVEL" envDefault:"info"`  // debug|info|warn|error
	LogFormat string `env:"MEMINI_LOG_FORMAT" envDefault:"json"` // json|text

	// Storage.
	Backend     Backend `env:"MEMINI_BACKEND" envDefault:"sqlite"`
	SQLitePath  string  `env:"MEMINI_SQLITE_PATH" envDefault:"memini.db"`
	PostgresDSN string  `env:"MEMINI_POSTGRES_DSN"`

	// Embeddings (external OpenAI-compatible endpoint, required for vector search).
	EmbedBaseURL string `env:"MEMINI_EMBED_BASE_URL"`
	EmbedAPIKey  string `env:"MEMINI_EMBED_API_KEY"`
	EmbedModel   string `env:"MEMINI_EMBED_MODEL" envDefault:"text-embedding-3-small"`
	EmbedDims    int    `env:"MEMINI_EMBED_DIMS" envDefault:"1536"`
	// EmbedQueryPrefix is prepended to recall queries before embedding, for
	// instruction-tuned asymmetric embedders (e.g. Qwen3-Embedding, bge).
	// Documents are always embedded without it. Empty disables.
	EmbedQueryPrefix string `env:"MEMINI_EMBED_QUERY_PREFIX"`
	// EmbedMaxBatch caps items per /embeddings request so bulk callers (dedup
	// over a whole namespace) can't exceed the server's max client batch and
	// fail with 422. The TEI default is 32; 20 leaves headroom.
	EmbedMaxBatch int `env:"MEMINI_EMBED_MAX_BATCH" envDefault:"20"`
	// EmbedMaxBatchChars caps total characters per request (0 disables).
	EmbedMaxBatchChars int `env:"MEMINI_EMBED_MAX_BATCH_CHARS" envDefault:"24000"`
	// EmbedMaxConcurrency caps in-flight calls to the embeddings backend. 0
	// is unbounded. Set to 1-2 for self-hosted backends that can't service a
	// recall burst in parallel.
	EmbedMaxConcurrency int `env:"MEMINI_EMBED_MAX_CONCURRENCY" envDefault:"0"`
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
	LLMBaseURL string `env:"MEMINI_LLM_BASE_URL"`
	LLMAPIKey  string `env:"MEMINI_LLM_API_KEY"`
	LLMModel   string `env:"MEMINI_LLM_MODEL" envDefault:"gpt-4o-mini"`
	// LLMAPI selects the chat backend: "openai" (default) or "anthropic".
	LLMAPI string `env:"MEMINI_LLM_API" envDefault:"openai"`

	// Rerank selects recall reranking: "off" (default), "llm" (reorder with the
	// chat LLM), or a cross-encoder /rerank base URL (e.g. http://host:8002/v1).
	// Reranking reorders the top k composite-ranked candidates; it adds one
	// reranker call per recall.
	Rerank string `env:"MEMINI_RERANK" envDefault:"off"`
	// RerankModel / RerankAPIKey configure the cross-encoder when Rerank is a URL.
	RerankModel  string `env:"MEMINI_RERANK_MODEL"`
	RerankAPIKey string `env:"MEMINI_RERANK_API_KEY"`
	// RerankMaxBatchChars caps the total characters across the query and all
	// documents in a single /rerank request, so a deep candidate pool can never
	// exceed the model's context window. Set just below the model's effective
	// context in characters (≈ n_ctx × chars-per-token × (1 − template reserve)).
	// 6000 keeps ~2 max-size docs per request at the 2048-char doc cap above.
	// 0 disables proactive batching.
	RerankMaxBatchChars int `env:"MEMINI_RERANK_MAX_BATCH_CHARS" envDefault:"6000"`
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
	// marked pending_embed for background backfill. 0 restores fail-fast writes.
	WriteEmbedTimeout time.Duration `env:"MEMINI_WRITE_EMBED_TIMEOUT" envDefault:"5s"`
	// RecallMinScore is the fused-score floor: candidates below it are dropped
	// before ranking. The default (0.1) is the benched value; it is exposed so a
	// deployment on a different embedder can raise it to trim loosely-relevant
	// injection. Only meaningful with score fusion.
	RecallMinScore float64 `env:"MEMINI_RECALL_MIN_SCORE" envDefault:"0.1"`
	// RecallSemanticReserve reserves up to N of the recall slots for durable
	// tiers (semantic/procedural) so consolidated knowledge is not crowded out by
	// episodic chatter. Exposed because it changes recall composition per
	// deployment: set 0 for pure-relevance recall (no forced durable slots).
	// Reserved slots are relevance-gated — a durable memory is only promoted in
	// when it is relevance-competitive with the entry it displaces.
	RecallSemanticReserve int `env:"MEMINI_RECALL_SEMANTIC_RESERVE" envDefault:"2"`
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

	// BackfillInterval is how often the vector backfill loop re-embeds
	// memories left vectorless by a degraded write (metadata pending_embed);
	// 0 disables it.
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
	DedupInterval   time.Duration `env:"MEMINI_DEDUP_INTERVAL" envDefault:"24h"`
	DedupSimilarity float64       `env:"MEMINI_DEDUP_SIMILARITY" envDefault:"0.85"`
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

	// UIEnabled mounts the embedded admin UI at /. Enabled by default; set
	// MEMINI_UI_ENABLED=false to run a headless API/MCP-only service.
	UIEnabled bool `env:"MEMINI_UI_ENABLED" envDefault:"true"`

	// Auth (optional). When APIKey is set, requests must present it as a bearer token.
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

	// Multi-tenancy. The fallback namespace when no header is sent; the header
	// name itself is fixed (DefaultNamespaceHeader).
	DefaultNamespace string
	NamespaceSrc     NamespaceSource

	// Home is the caller's personal namespace: merged read-only (durable
	// tiers only) into the default read set on every recall/briefing/answer,
	// on top of the request namespace and its ancestors. Client-side only —
	// the server never derives it. On HTTP transports it is carried per-request
	// by the X-Memini-Home header (DefaultHomeHeader); this env var is what the
	// stdio MCP server (`memini mcp`) resolves instead, since stdio has no
	// headers. Empty means no home leg (unset by default).
	Home string `env:"MEMINI_HOME"`
}

// DefaultNamespaceHeader is the request header carrying the tenant namespace.
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
	{"MEMINI_EMBED_MAX_ITEM_CHARS", "now a fixed internal default (8000); batch-char budgets stay configurable", false},
	{"MEMINI_CONSOLIDATE_QUEUE_CAP", "now a fixed internal default (1024)", false},
	{"MEMINI_NAMESPACE_HEADER", "the header name is fixed to X-Memini-Namespace", false},
	{"MEMINI_FUSION_ALPHA", "now a baked retrieval default (0.5); tune via the benchmark harness, not env", false},
	{"MEMINI_RECALL_MIN_SEMANTIC_SCORE", "now a baked retrieval default (0, off)", false},
	{"MEMINI_TEMPORAL_BOOST", "now a baked retrieval default (0.40)", false},
	{"MEMINI_RERANK_MAX_DOC_CHARS", "now a fixed internal default (2048); MEMINI_RERANK_MAX_BATCH_CHARS remains configurable", false},
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
	if err := c.validate(); err != nil {
		return nil, err
	}
	return c, nil
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

func (c *Config) validate() error {
	switch c.Backend {
	case BackendSQLite:
	case BackendPostgres:
		if c.PostgresDSN == "" {
			return fmt.Errorf("MEMINI_POSTGRES_DSN is required when MEMINI_BACKEND=postgres")
		}
	default:
		return fmt.Errorf("unknown MEMINI_BACKEND %q (want sqlite|postgres)", c.Backend)
	}
	if c.EmbedDims <= 0 {
		return fmt.Errorf("MEMINI_EMBED_DIMS must be positive, got %d", c.EmbedDims)
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
	// Write-time dedup: one similarity threshold + one action. No band ordering
	// to get wrong, so no config combination can make startup fail.
	if c.WriteDedupScore < 0 || c.WriteDedupScore > 1 {
		return fmt.Errorf("MEMINI_WRITE_DEDUP_SCORE must be in [0,1], got %v", c.WriteDedupScore)
	}
	if c.RecallMinScore < 0 || c.RecallMinScore > 1 {
		return fmt.Errorf("MEMINI_RECALL_MIN_SCORE must be in [0,1], got %v", c.RecallMinScore)
	}
	if c.ConsolidateMinScore < 0 || c.ConsolidateMinScore > 1 {
		return fmt.Errorf("MEMINI_CONSOLIDATE_MIN_SCORE must be in [0,1], got %v", c.ConsolidateMinScore)
	}
	if c.RecallSemanticReserve < 0 {
		return fmt.Errorf("MEMINI_RECALL_SEMANTIC_RESERVE must be >= 0, got %d", c.RecallSemanticReserve)
	}
	if c.EpisodicMinChars < 0 {
		return fmt.Errorf("MEMINI_EPISODIC_MIN_CHARS must be >= 0, got %d", c.EpisodicMinChars)
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
