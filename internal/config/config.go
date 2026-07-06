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
	// MetricsAddr, when set (e.g. ":9090"), serves /metrics on its own listener
	// instead of the main HTTP port. The dedicated port is meant to stay
	// in-cluster — keep it out of any public route and it needs no bearer token.
	// Empty (the default) keeps /metrics on the main port, where MEMINI_API_KEY
	// gates it.
	MetricsAddr string `env:"MEMINI_METRICS_ADDR"`

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

	// ContradictionDownrank (default on) invalidates a durable fact when a fresh
	// durable write contradicts it (changed value or flipped polarity, confirmed
	// by the lexical detector): the stale fact's valid_to is stamped so it leaves
	// live recall while AsOf time-travel can still reach it, and its confidence
	// is shrunk. Reversible (Restore clears valid_to) and precision-first (0
	// restatement misfires measured in bench/contradiction_test.go). Set false to
	// disable — the kill-switch; there is no threshold knob.
	ContradictionDownrank bool `env:"MEMINI_CONTRADICT_DOWNRANK" envDefault:"true"`

	// GlobalNamespace, when set, is a namespace whose memories are merged
	// read-only into every other namespace's recall and briefing — a shared
	// space for cross-project rules the agent should always remember ("no AI
	// slops", commit conventions, ...). Empty (the default) disables it, keeping
	// namespaces fully isolated. Pin a global memory so it stays top-of-mind.
	GlobalNamespace string `env:"MEMINI_GLOBAL_NAMESPACE" envDefault:""`

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
	ConsolidateMinScore float64 `env:"MEMINI_CONSOLIDATE_MIN_SCORE" envDefault:"0.6"`

	// Promotion (episodic→semantic distillation). Uses the LLM when configured,
	// the marker extractor otherwise, so it also runs LLM-less.
	// PromoteInterval is how often the promoter runs; 0 disables it.
	PromoteInterval time.Duration `env:"MEMINI_PROMOTE_INTERVAL" envDefault:"24h"`
	// PromoteMinAccess is the minimum access_count for an episodic memory to be
	// considered for promotion.
	PromoteMinAccess int `env:"MEMINI_PROMOTE_MIN_ACCESS" envDefault:"3"`

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

	// UIEnabled mounts the embedded admin UI at /. Enabled by default; set
	// MEMINI_UI_ENABLED=false to run a headless API/MCP-only service.
	UIEnabled bool `env:"MEMINI_UI_ENABLED" envDefault:"true"`

	// Auth (optional). When APIKey is set, requests must present it as a bearer token.
	APIKey string `env:"MEMINI_API_KEY"`

	// Multi-tenancy. The fallback namespace when no header is sent; the header
	// name itself is fixed (DefaultNamespaceHeader).
	DefaultNamespace string
	NamespaceSrc     NamespaceSource
}

// DefaultNamespaceHeader is the request header carrying the tenant namespace.
// Fixed (no env override): clients and plugins all send this exact header.
const DefaultNamespaceHeader = "X-Memini-Namespace"

// valueOff is the shared "disabled" token for the string-enum settings
// (MEMINI_RERANK, MEMINI_CONSOLIDATE_MODE, MEMINI_WRITE_DEDUP_ACTION).
const valueOff = "off"

// deprecatedVars maps removed environment variables to migration guidance. A
// value set for any of these is ignored at load; DeprecationWarnings surfaces it
// so an operator upgrading from an older release learns why their tuning no
// longer applies — without the boot failing.
var deprecatedVars = []struct{ name, guidance string }{
	{"MEMINI_WRITE_DEDUP_MIN_SCORE", "use MEMINI_WRITE_DEDUP_SCORE with MEMINI_WRITE_DEDUP_ACTION=coalesce"},
	{"MEMINI_MERGE_HINT_MIN_SCORE", "use MEMINI_WRITE_DEDUP_SCORE with MEMINI_WRITE_DEDUP_ACTION=hint (the default)"},
	{"MEMINI_AUTO_SUPERSEDE_MIN_SCORE", "use MEMINI_WRITE_DEDUP_SCORE with MEMINI_WRITE_DEDUP_ACTION=supersede"},
	{"MEMINI_DEDUP_MIN_CLUSTER_SIZE", "now a fixed internal default (2)"},
	{"MEMINI_DEDUP_NEIGHBOURS", "now a fixed internal default (20)"},
	{"MEMINI_EMBED_MAX_ITEM_CHARS", "now a fixed internal default (8000); batch-char budgets stay configurable"},
	{"MEMINI_CONSOLIDATE_QUEUE_CAP", "now a fixed internal default (1024)"},
	{"MEMINI_NAMESPACE_HEADER", "the header name is fixed to X-Memini-Namespace"},
	{"MEMINI_FUSION_ALPHA", "now a baked retrieval default (0.5); tune via the benchmark harness, not env"},
	{"MEMINI_RECALL_MIN_SEMANTIC_SCORE", "now a baked retrieval default (0, off)"},
	{"MEMINI_TEMPORAL_BOOST", "now a baked retrieval default (0.40)"},
	{"MEMINI_RERANK_MAX_DOC_CHARS", "now a fixed internal default (2048); MEMINI_RERANK_MAX_BATCH_CHARS remains configurable"},
	{"MEMINI_REDACT_SECRETS", "secret redaction is always on"},
	{"MEMINI_REINFORCE_SKIP_MARKERS", "always on"},
	{"MEMINI_WRITE_DEDUP_FINGERPRINT", "exact-restatement dedup is always on"},
	{"MEMINI_QUARANTINE_GARBLED", "removed; garbled-content downranking is no longer configurable"},
	{"MEMINI_DISTILL_ON_WRITE", "write-time fact building is automatic (LLM when configured, heuristic extractor otherwise)"},
	{"MEMINI_EXTRACT_ON_WRITE", "write-time fact building is automatic (LLM when configured, heuristic extractor otherwise)"},
	{"MEMINI_DISTILL_DROP_NO_FACT", "removed; episodic captures are always kept"},
}

// DeprecationWarnings returns one message per removed environment variable that
// is currently set, telling the operator what to use instead. The variables are
// ignored either way; this only explains the change. Empty when none are set.
func DeprecationWarnings() []string {
	var w []string
	for _, d := range deprecatedVars {
		if _, ok := os.LookupEnv(d.name); ok {
			w = append(w, fmt.Sprintf("%s is removed and ignored; %s", d.name, d.guidance))
		}
	}
	return w
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
		return sanitizeNamespace(v), NamespaceFromEnv
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
// back to the literal default.
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
