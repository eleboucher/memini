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
	// EmbedMaxItemChars truncates any single text before embedding so one
	// oversized memory can't blow the per-request budget (0 disables).
	EmbedMaxItemChars int `env:"MEMINI_EMBED_MAX_ITEM_CHARS" envDefault:"8000"`
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

	// FusionAlpha selects hybrid recall fusion: >= 0 uses convex score fusion
	// with this vector-vs-keyword weight (0.5 = balanced, the default); a
	// negative value falls back to rank fusion (RRF).
	FusionAlpha float64 `env:"MEMINI_FUSION_ALPHA" envDefault:"0.5"`

	// WriteDedupMinScore coalesces a fresh write into an existing same-tier
	// memory when their vector similarity is at or above this score, instead of
	// storing a near-duplicate. It only acts when the LLM consolidation pipeline
	// is not handling the write (no LLM, or a non-durable tier), giving headless
	// deployments basic corpus hygiene. 0 (the default) disables it; the right
	// value is embedder-dependent (~0.9 in score units collapses near-identical
	// restatements only).
	WriteDedupMinScore float64 `env:"MEMINI_WRITE_DEDUP_MIN_SCORE" envDefault:"0"`

	// WriteDedupFingerprint reinforces an exact normalized-content restatement
	// into the existing same-tier memory instead of storing a duplicate, before
	// embedding. Matches only identical content (no false positives), so it is on
	// by default; false stores every write verbatim.
	WriteDedupFingerprint bool `env:"MEMINI_WRITE_DEDUP_FINGERPRINT" envDefault:"true"`

	// ReinforceSkipMarkers drops session-end / stop marker memories from recall
	// reinforcement so their inflated access_count and TTL don't distort recall.
	// On by default.
	ReinforceSkipMarkers bool `env:"MEMINI_REINFORCE_SKIP_MARKERS" envDefault:"true"`

	// RedactSecrets scrubs live credentials (tokens, passwords, API keys, private
	// keys) from a memory's content/summary/metadata at ingestion, so a database
	// compromise exposes memory content but no usable secrets. On by default;
	// best-effort pattern matching, so it's one layer of defense, not a guarantee.
	// Set false only if redaction mangles legitimate content.
	RedactSecrets bool `env:"MEMINI_REDACT_SECRETS" envDefault:"true"`

	// QuarantineGarbled downranks writes whose content looks like script-salad
	// (garbled multilingual model/harness output) so they sink in recall instead
	// of surfacing verbatim — importance is zeroed and metadata.quarantined set.
	// Off by default: it's a heuristic that can misjudge rare legitimate
	// mixed-script text, so it only downranks (never rejects). Enable for
	// deployments where garbled digests are a problem.
	QuarantineGarbled bool `env:"MEMINI_QUARANTINE_GARBLED" envDefault:"false"`

	// TemporalBoost enables query-conditioned temporal targeting in the
	// re-ranker: when a query names a relative time ("3 weeks ago"), candidates
	// dated near the referenced point are boosted by up to this much on the
	// composite score. 0 disables it. Uses the regex anchor extractor.
	TemporalBoost float64 `env:"MEMINI_TEMPORAL_BOOST" envDefault:"0.40"`

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
	// RerankMaxDocChars truncates each document sent to the cross-encoder so one
	// oversized candidate can't exceed the server's physical batch and fail the
	// whole rerank request. 0 disables truncation. Default 2048 covers the typical
	// memory in full (mean content ≈ 2k chars) so the reranker scores the whole
	// memory, not a fragment; a longer memory is still truncated here. At this cap
	// the longest (query+doc) is ≈ 800 tokens, so the reranker server must run with
	// --ubatch-size ≥ ~1024 or it 500s on long candidates.
	RerankMaxDocChars int `env:"MEMINI_RERANK_MAX_DOC_CHARS" envDefault:"2048"`
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
	// RecallMinScore is an absolute relevance floor on the fused (vector+keyword)
	// score. Candidates whose fused score is below this threshold are dropped from
	// recall results entirely, preventing the "poison" problem where short
	// prompts fetch irrelevant memories whose fused scores are then min-max
	// normalised into competitive values. Default 0.1 (matches mem0's default);
	// set to 0 to disable. Only applies to score fusion (MEMINI_FUSION_ALPHA >= 0)
	// where the fused score is comparable. Has no effect when using RRF.
	RecallMinScore float64 `env:"MEMINI_RECALL_MIN_SCORE" envDefault:"0.1"`

	// RecallMinSemanticScore is an absolute floor on the raw vector score, applied
	// before fusion: a candidate below it is excluded and the keyword leg cannot
	// reintroduce it, so a query with nothing semantically relevant recalls empty.
	// Default 0 (off); the usable value is embedder-specific (e.g. ~0.46 for
	// qwen3-embedding-0.6b). See docs/recall-relevance-gate-2026-06-20.md.
	RecallMinSemanticScore float64 `env:"MEMINI_RECALL_MIN_SEMANTIC_SCORE" envDefault:"0"`

	// RecallSemanticReserve guarantees up to N recall slots for durable tiers
	// (semantic/procedural) so episodic chatter can't crowd out consolidated
	// memory; the rest fill by relevance. Default 0 (off).
	RecallSemanticReserve int `env:"MEMINI_RECALL_SEMANTIC_RESERVE" envDefault:"0"`

	// EpisodicMinChars drops an episodic write whose substantive content (role
	// scaffolding stripped) is below this many characters — the low-signal
	// per-turn chatter ("keep going", "ok", "hello") that otherwise dominates
	// episodic memory. Only episodic is gated. Default 120 (on); set 0 to disable.
	EpisodicMinChars int `env:"MEMINI_EPISODIC_MIN_CHARS" envDefault:"120"`

	// Consolidation tuning.
	// ConsolidateMode is "async" (default), "sync", or "off".
	ConsolidateMode string `env:"MEMINI_CONSOLIDATE_MODE" envDefault:"async"`
	// ConsolidateMinScore gates the LLM: it runs only when the nearest candidate
	// scores at least this. 0 disables the gate.
	ConsolidateMinScore float64 `env:"MEMINI_CONSOLIDATE_MIN_SCORE" envDefault:"0.6"`
	// ConsolidateQueueCap bounds the async consolidation queue. When full, the
	// dedup job (not the memory) is dropped and counted by the "dropped"
	// consolidate metric — raise it for write-bursty deployments.
	ConsolidateQueueCap int `env:"MEMINI_CONSOLIDATE_QUEUE_CAP" envDefault:"1024"`

	// Promotion (episodic→semantic distillation). Requires an LLM.
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
	DedupInterval         time.Duration `env:"MEMINI_DEDUP_INTERVAL" envDefault:"24h"`
	DedupSimilarity       float64       `env:"MEMINI_DEDUP_SIMILARITY" envDefault:"0.85"`
	DedupMinClusterSize   int           `env:"MEMINI_DEDUP_MIN_CLUSTER_SIZE" envDefault:"2"`
	DedupNeighboursAnchor int           `env:"MEMINI_DEDUP_NEIGHBOURS" envDefault:"20"`
	// DedupTiers is an optional comma-separated list restricting the periodic
	// pass to specific tiers (working,episodic,semantic,procedural). Empty
	// means all tiers.
	DedupTiers string `env:"MEMINI_DEDUP_TIERS" envDefault:""`

	// UIEnabled mounts the embedded admin UI at /. Enabled by default; set
	// MEMINI_UI_ENABLED=false to run a headless API/MCP-only service.
	UIEnabled bool `env:"MEMINI_UI_ENABLED" envDefault:"true"`

	// Auth (optional). When APIKey is set, requests must present it as a bearer token.
	APIKey string `env:"MEMINI_API_KEY"`

	// Multi-tenancy. Namespace resolution header and the fallback namespace.
	NamespaceHeader  string `env:"MEMINI_NAMESPACE_HEADER" envDefault:"X-Memini-Namespace"`
	DefaultNamespace string
	NamespaceSrc     NamespaceSource
}

// LLMEnabled reports whether the opt-in LLM pipeline is configured.
func (c *Config) LLMEnabled() bool { return c.LLMBaseURL != "" }

// RerankEnabled reports whether recall reranking is configured.
func (c *Config) RerankEnabled() bool { return c.Rerank != "" && c.Rerank != "off" }

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
	case "async", "sync", "off":
	default:
		return fmt.Errorf("unknown MEMINI_CONSOLIDATE_MODE %q (want async|sync|off)", c.ConsolidateMode)
	}
	if c.DedupSimilarity < 0 || c.DedupSimilarity > 1 {
		return fmt.Errorf("MEMINI_DEDUP_SIMILARITY must be in [0,1], got %v", c.DedupSimilarity)
	}
	if c.DedupMinClusterSize < 2 {
		return fmt.Errorf("MEMINI_DEDUP_MIN_CLUSTER_SIZE must be >= 2, got %d", c.DedupMinClusterSize)
	}
	if c.DedupNeighboursAnchor < 1 {
		return fmt.Errorf("MEMINI_DEDUP_NEIGHBOURS must be >= 1, got %d", c.DedupNeighboursAnchor)
	}
	for _, t := range c.dedupTiers() {
		if !t.Valid() {
			return fmt.Errorf("unknown tier %q in MEMINI_DEDUP_TIERS (want working|episodic|semantic|procedural)", t)
		}
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
