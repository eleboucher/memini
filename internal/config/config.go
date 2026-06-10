package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"
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

	// LLM (opt-in; empty BaseURL disables the consolidation pipeline).
	LLMBaseURL string `env:"MEMINI_LLM_BASE_URL"`
	LLMAPIKey  string `env:"MEMINI_LLM_API_KEY"`
	LLMModel   string `env:"MEMINI_LLM_MODEL" envDefault:"gpt-4o-mini"`
	// LLMAPI selects the chat backend: "openai" (default) or "anthropic".
	LLMAPI string `env:"MEMINI_LLM_API" envDefault:"openai"`

	// Consolidation tuning.
	// ConsolidateMode is "async" (default), "sync", or "off".
	ConsolidateMode string `env:"MEMINI_CONSOLIDATE_MODE" envDefault:"async"`
	// ConsolidateMinScore gates the LLM: it runs only when the nearest candidate
	// scores at least this. 0 disables the gate.
	ConsolidateMinScore float64 `env:"MEMINI_CONSOLIDATE_MIN_SCORE" envDefault:"0.6"`

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
	return nil
}
