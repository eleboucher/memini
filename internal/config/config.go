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
	// Reranking reorders the top RerankTopN candidates before capping at the
	// limit; it adds one reranker call per recall.
	Rerank string `env:"MEMINI_RERANK" envDefault:"off"`
	// RerankModel / RerankAPIKey configure the cross-encoder when Rerank is a URL.
	RerankModel  string `env:"MEMINI_RERANK_MODEL"`
	RerankAPIKey string `env:"MEMINI_RERANK_API_KEY"`
	// RerankTopN is how many composite-ranked candidates the reranker sees.
	RerankTopN int `env:"MEMINI_RERANK_TOP_N" envDefault:"20"`

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
