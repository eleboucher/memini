package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
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

// Config is the fully-resolved runtime configuration.
type Config struct {
	// HTTP server.
	HTTPAddr        string
	ShutdownTimeout time.Duration

	// Logging.
	LogLevel  string // debug|info|warn|error
	LogFormat string // json|text

	// Storage.
	Backend     Backend
	SQLitePath  string
	PostgresDSN string

	// Embeddings (external OpenAI-compatible endpoint, required for vector search).
	EmbedBaseURL string
	EmbedAPIKey  string
	EmbedModel   string
	EmbedDims    int

	// LLM (opt-in; empty BaseURL disables the consolidation pipeline).
	LLMBaseURL string
	LLMAPIKey  string
	LLMModel   string
	// LLMAPI selects the chat backend: "openai" (default) or "anthropic".
	LLMAPI string

	// Consolidation tuning.
	// ConsolidateMode is "async" (default), "sync", or "off".
	ConsolidateMode string
	// ConsolidateMinScore gates the LLM: it runs only when the nearest candidate
	// scores at least this. 0 disables the gate.
	ConsolidateMinScore float64

	// Promotion (episodic→semantic distillation). Requires an LLM.
	// PromoteInterval is how often the promoter runs; 0 disables it.
	PromoteInterval time.Duration
	// PromoteMinAccess is the minimum access_count for an episodic memory to be
	// considered for promotion.
	PromoteMinAccess int

	// SweepInterval is how often the decay sweeper purges expired memories.
	SweepInterval time.Duration
	// ShortTermCap bounds short-term (working+episodic) memories per namespace;
	// the sweeper evicts the lowest-retention ones over the cap. 0 disables it.
	ShortTermCap int

	// Auth (optional). When APIKey is set, requests must present it as a bearer token.
	APIKey string

	// Multi-tenancy. Namespace resolution header and the fallback namespace.
	NamespaceHeader  string
	DefaultNamespace string
	NamespaceSrc     NamespaceSource
}

// LLMEnabled reports whether the opt-in LLM pipeline is configured.
func (c *Config) LLMEnabled() bool { return c.LLMBaseURL != "" }

// Load reads configuration from the environment and validates it.
func Load() (*Config, error) {
	c := &Config{
		HTTPAddr:            env("MEMINI_HTTP_ADDR", ":8080"),
		ShutdownTimeout:     envDuration("MEMINI_SHUTDOWN_TIMEOUT", 15*time.Second),
		LogLevel:            env("MEMINI_LOG_LEVEL", "info"),
		LogFormat:           env("MEMINI_LOG_FORMAT", "json"),
		Backend:             Backend(env("MEMINI_BACKEND", string(BackendSQLite))),
		SQLitePath:          env("MEMINI_SQLITE_PATH", "memini.db"),
		PostgresDSN:         env("MEMINI_POSTGRES_DSN", ""),
		EmbedBaseURL:        env("MEMINI_EMBED_BASE_URL", ""),
		EmbedAPIKey:         env("MEMINI_EMBED_API_KEY", ""),
		EmbedModel:          env("MEMINI_EMBED_MODEL", "text-embedding-3-small"),
		EmbedDims:           envInt("MEMINI_EMBED_DIMS", 1536),
		LLMBaseURL:          env("MEMINI_LLM_BASE_URL", ""),
		LLMAPIKey:           env("MEMINI_LLM_API_KEY", ""),
		LLMModel:            env("MEMINI_LLM_MODEL", "gpt-4o-mini"),
		LLMAPI:              env("MEMINI_LLM_API", "openai"),
		ConsolidateMode:     env("MEMINI_CONSOLIDATE_MODE", "async"),
		ConsolidateMinScore: envFloat("MEMINI_CONSOLIDATE_MIN_SCORE", 0.6),
		PromoteInterval:     envDuration("MEMINI_PROMOTE_INTERVAL", 24*time.Hour),
		PromoteMinAccess:    envInt("MEMINI_PROMOTE_MIN_ACCESS", 3),
		SweepInterval:       envDuration("MEMINI_SWEEP_INTERVAL", time.Hour),
		ShortTermCap:        envInt("MEMINI_SHORT_TERM_CAP", 1000),
		APIKey:              env("MEMINI_API_KEY", ""),
		NamespaceHeader:     env("MEMINI_NAMESPACE_HEADER", "X-Memini-Namespace"),
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

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v, ok := os.LookupEnv(key); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
	}
	return def
}

func envFloat(key string, def float64) float64 {
	if v, ok := os.LookupEnv(key); ok {
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			return f
		}
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v, ok := os.LookupEnv(key); ok {
		if d, err := time.ParseDuration(strings.TrimSpace(v)); err == nil {
			return d
		}
	}
	return def
}
