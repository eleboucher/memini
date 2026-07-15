package config_test

import (
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/eleboucher/memini/internal/chunk"
	"github.com/eleboucher/memini/internal/config"
	"github.com/eleboucher/memini/internal/memory"
)

// clearMeminiEnv makes every memini env var absent for the duration of the
// test (and restores the originals on cleanup). With caarlos0/env, "absent"
// is what triggers envDefault — a set-but-empty var would be taken verbatim.
func clearMeminiEnv(t *testing.T) {
	t.Helper()
	for _, k := range meminiEnvKeys {
		t.Setenv(k, "") // records the original for restoration
		_ = os.Unsetenv(k)
	}
}

func TestLoadDefaults(t *testing.T) {
	clearMeminiEnv(t)
	// Stable temp dir so the cwd-basename assertion is meaningful (a real
	// repo root would resolve via git instead).
	prev, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })
	leaf := t.TempDir() + "/stable-test-cwd"
	if err := os.MkdirAll(leaf, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.Chdir(leaf); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	// Git hooks (e.g. lefthook's pre-push test run from a linked worktree)
	// export an absolute GIT_DIR, which would resolve the repo from inside
	// the temp dir and turn the cwd assertion below into a git one.
	for _, k := range []string{"GIT_DIR", "GIT_WORK_TREE"} {
		t.Setenv(k, "") // records the original for restoration
		_ = os.Unsetenv(k)
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.HTTPAddr != ":8080" {
		t.Errorf("HTTPAddr = %q, want :8080", cfg.HTTPAddr)
	}
	if cfg.Backend != config.BackendSQLite {
		t.Errorf("Backend = %q, want sqlite", cfg.Backend)
	}
	if cfg.SQLitePath != "memini.db" {
		t.Errorf("SQLitePath = %q, want memini.db", cfg.SQLitePath)
	}
	if cfg.EmbedDims != 1536 {
		t.Errorf("EmbedDims = %d, want 1536", cfg.EmbedDims)
	}
	if cfg.WriteDedupScore != 0.625 {
		t.Errorf("WriteDedupScore = %v, want 0.625", cfg.WriteDedupScore)
	}
	if cfg.WriteDedupAction != "hint" {
		t.Errorf("WriteDedupAction = %q, want hint", cfg.WriteDedupAction)
	}
	if cfg.SweepInterval != time.Hour {
		t.Errorf("SweepInterval = %v, want 1h", cfg.SweepInterval)
	}
	if cfg.ConsolidateMode != "async" {
		t.Errorf("ConsolidateMode = %q, want async", cfg.ConsolidateMode)
	}
	if cfg.ConsolidateMinScore != 0.3 {
		t.Errorf("ConsolidateMinScore = %v, want 0.3", cfg.ConsolidateMinScore)
	}
	if cfg.RerankMaxBatchChars != 6000 {
		t.Errorf("RerankMaxBatchChars = %d, want 6000", cfg.RerankMaxBatchChars)
	}
	if cfg.StabilityK != 1 {
		t.Errorf("StabilityK = %v, want 1", cfg.StabilityK)
	}
	if cfg.PromoteInterval != 24*time.Hour {
		t.Errorf("PromoteInterval = %v, want 24h", cfg.PromoteInterval)
	}
	if cfg.PromoteMinAccess != 3 {
		t.Errorf("PromoteMinAccess = %d, want 3", cfg.PromoteMinAccess)
	}
	if cfg.BackfillInterval != time.Minute {
		t.Errorf("BackfillInterval = %v, want 1m", cfg.BackfillInterval)
	}
	if cfg.DefaultNamespace != "stable-test-cwd" {
		t.Errorf("DefaultNamespace = %q, want stable-test-cwd", cfg.DefaultNamespace)
	}
	if cfg.NamespaceSrc != config.NamespaceFromCWD {
		t.Errorf("NamespaceSrc = %q, want cwd", cfg.NamespaceSrc)
	}
	if cfg.LLMEnabled() {
		t.Error("LLMEnabled() = true, want false with no base URL")
	}
	if !cfg.UIEnabled {
		t.Error("UIEnabled = false, want true")
	}
	if cfg.Home != "" {
		t.Errorf("Home = %q, want empty (no MEMINI_HOME set)", cfg.Home)
	}
	if cfg.APIKeysFile != "" {
		t.Errorf("APIKeysFile = %q, want empty (no MEMINI_API_KEYS_FILE set) — feature-off must be a no-op", cfg.APIKeysFile)
	}
}

// TestLoadHome pins that MEMINI_HOME is resolved into Config.Home the same
// way other simple env-backed settings are — this is what the stdio MCP
// server (`memini mcp`) reads to fill in the home leg it has no header for.
func TestLoadHome(t *testing.T) {
	clearMeminiEnv(t)
	t.Setenv("MEMINI_HOME", "personal/kit")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Home != "personal/kit" {
		t.Errorf("Home = %q, want personal/kit", cfg.Home)
	}
}

// TestLoadAPIKeysFile pins that MEMINI_API_KEYS_FILE (K2b) is resolved into
// Config.APIKeysFile verbatim, like any other simple env-backed setting.
// config.Load itself never opens the file — that's internal/apiauth.LoadFileKeys,
// called once at server boot (cmd/memini/root.go).
func TestLoadAPIKeysFile(t *testing.T) {
	clearMeminiEnv(t)
	t.Setenv("MEMINI_API_KEYS_FILE", "/etc/memini/api-keys.yaml")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.APIKeysFile != "/etc/memini/api-keys.yaml" {
		t.Errorf("APIKeysFile = %q, want /etc/memini/api-keys.yaml", cfg.APIKeysFile)
	}
}

func TestLoadOverrides(t *testing.T) {
	t.Setenv("MEMINI_HTTP_ADDR", ":9999")
	t.Setenv("MEMINI_LOG_LEVEL", "debug")
	t.Setenv("MEMINI_EMBED_DIMS", "256")
	t.Setenv("MEMINI_EMBED_QUERY_PREFIX", "Instruct: retrieve\nQuery: ")
	t.Setenv("MEMINI_WRITE_DEDUP_SCORE", "0.95")
	t.Setenv("MEMINI_WRITE_DEDUP_ACTION", "coalesce")
	t.Setenv("MEMINI_SWEEP_INTERVAL", "5m")
	t.Setenv("MEMINI_LLM_BASE_URL", "http://localhost:8000/v1")
	t.Setenv("MEMINI_DEFAULT_NAMESPACE", "team-a")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.HTTPAddr != ":9999" {
		t.Errorf("HTTPAddr = %q, want :9999", cfg.HTTPAddr)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want debug", cfg.LogLevel)
	}
	if cfg.EmbedDims != 256 {
		t.Errorf("EmbedDims = %d, want 256", cfg.EmbedDims)
	}
	if cfg.EmbedQueryPrefix != "Instruct: retrieve\nQuery: " {
		t.Errorf("EmbedQueryPrefix = %q, want the instruct prefix", cfg.EmbedQueryPrefix)
	}
	if cfg.WriteDedupScore != 0.95 {
		t.Errorf("WriteDedupScore = %v, want 0.95", cfg.WriteDedupScore)
	}
	if cfg.WriteDedupAction != "coalesce" {
		t.Errorf("WriteDedupAction = %q, want coalesce", cfg.WriteDedupAction)
	}
	if cfg.SweepInterval != 5*time.Minute {
		t.Errorf("SweepInterval = %v, want 5m", cfg.SweepInterval)
	}
	if cfg.DefaultNamespace != "team-a" {
		t.Errorf("DefaultNamespace = %q, want team-a", cfg.DefaultNamespace)
	}
	if cfg.NamespaceSrc != config.NamespaceFromEnv {
		t.Errorf("NamespaceSrc = %q, want env", cfg.NamespaceSrc)
	}
	if !cfg.LLMEnabled() {
		t.Error("LLMEnabled() = false, want true")
	}
}

// TestLoadDefaultNamespacePreservesSlash is the sanitize-slash-fix regression
// through the public Load() entry point: MEMINI_DEFAULT_NAMESPACE=team/proj
// must resolve to "team/proj", not be flattened to "proj".
func TestLoadDefaultNamespacePreservesSlash(t *testing.T) {
	clearMeminiEnv(t)
	t.Setenv("MEMINI_DEFAULT_NAMESPACE", "team/proj")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DefaultNamespace != "team/proj" {
		t.Errorf("DefaultNamespace = %q, want team/proj", cfg.DefaultNamespace)
	}
	if cfg.NamespaceSrc != config.NamespaceFromEnv {
		t.Errorf("NamespaceSrc = %q, want env", cfg.NamespaceSrc)
	}
}

func TestLoadValidationErrors(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
	}{
		{
			name: "postgres without dsn",
			env:  map[string]string{"MEMINI_BACKEND": "postgres", "MEMINI_POSTGRES_DSN": ""},
		},
		{
			name: "unknown backend",
			env:  map[string]string{"MEMINI_BACKEND": "mysql"},
		},
		{
			name: "non-positive dims",
			env:  map[string]string{"MEMINI_EMBED_DIMS": "0"},
		},
		{
			name: "negative dims",
			env:  map[string]string{"MEMINI_EMBED_DIMS": "-4"},
		},
		{
			name: "unknown consolidate mode",
			env:  map[string]string{"MEMINI_CONSOLIDATE_MODE": "eventually"},
		},
		{
			name: "negative embed max item chars",
			env:  map[string]string{"MEMINI_EMBED_MAX_ITEM_CHARS": "-1"},
		},
		{
			name: "negative promote whole max chars",
			env:  map[string]string{"MEMINI_PROMOTE_WHOLE_MAX_CHARS": "-1"},
		},
		{
			name: "negative rerank llm max doc chars",
			env:  map[string]string{"MEMINI_RERANK_LLM_MAX_DOC_CHARS": "-1"},
		},
		// A classify ceiling under the extractor's 20-rune floor admits nothing:
		// it reads as a tight bound but silently acts as an off switch, so it is
		// refused rather than accepted. 0 (plainly "off") stays legal, and is
		// covered by TestLoadOverrides.
		{
			name: "classify max chars below the extractor floor",
			env:  map[string]string{"MEMINI_CLASSIFY_MAX_CHARS": "19"},
		},
		{
			name: "classify max chars just above zero",
			env:  map[string]string{"MEMINI_CLASSIFY_MAX_CHARS": "1"},
		},
		// The chunk knobs are only checked when chunking is on; a stale value
		// must not block a boot that never uses it (covered positively in
		// TestChunkKnobsInertWhenOff).
		{
			name: "chunk overlap at the chunk size",
			env:  map[string]string{"MEMINI_CHUNK_EMBED": "true", "MEMINI_CHUNK_SIZE": "100", "MEMINI_CHUNK_OVERLAP": "100"},
		},
		{
			name: "chunk overlap past the chunk size",
			env:  map[string]string{"MEMINI_CHUNK_EMBED": "true", "MEMINI_CHUNK_SIZE": "100", "MEMINI_CHUNK_OVERLAP": "500"},
		},
		{
			name: "zero chunk size with chunking on",
			env:  map[string]string{"MEMINI_CHUNK_EMBED": "true", "MEMINI_CHUNK_SIZE": "0"},
		},
		{
			name: "zero chunks per memory",
			env:  map[string]string{"MEMINI_CHUNK_EMBED": "true", "MEMINI_CHUNK_MAX_PER_MEMORY": "0"},
		},
		// A chunk over the per-item embed budget would itself be truncated,
		// silently reintroducing the bug chunking removes.
		{
			name: "chunk size over the embed item budget",
			env: map[string]string{
				"MEMINI_CHUNK_EMBED": "true", "MEMINI_CHUNK_SIZE": "9000",
				"MEMINI_EMBED_MAX_ITEM_CHARS": "8000",
			},
		},
		{
			name: "dedup similarity out of range",
			env:  map[string]string{"MEMINI_DEDUP_SIMILARITY": "1.5"},
		},
		{
			name: "dedup unknown tier",
			env:  map[string]string{"MEMINI_DEDUP_TIERS": "semantic,bogus"},
		},
		{
			name: "zero sweep interval",
			env:  map[string]string{"MEMINI_SWEEP_INTERVAL": "0"},
		},
		{
			name: "negative sweep interval",
			env:  map[string]string{"MEMINI_SWEEP_INTERVAL": "-1m"},
		},
		{
			name: "write-dedup score out of range",
			env:  map[string]string{"MEMINI_WRITE_DEDUP_SCORE": "1.5"},
		},
		{
			name: "write-dedup score negative",
			env:  map[string]string{"MEMINI_WRITE_DEDUP_SCORE": "-0.1"},
		},
		{
			name: "unknown write-dedup action",
			env:  map[string]string{"MEMINI_WRITE_DEDUP_ACTION": "merge"},
		},
		{
			name: "recall min score out of range",
			env:  map[string]string{"MEMINI_RECALL_MIN_SCORE": "1.5"},
		},
		{
			name: "consolidate min score negative",
			env:  map[string]string{"MEMINI_CONSOLIDATE_MIN_SCORE": "-0.2"},
		},
		{
			name: "negative semantic reserve",
			env:  map[string]string{"MEMINI_RECALL_SEMANTIC_RESERVE": "-1"},
		},
		{
			name: "negative episodic min chars",
			env:  map[string]string{"MEMINI_EPISODIC_MIN_CHARS": "-10"},
		},
		{
			name: "negative stability k",
			env:  map[string]string{"MEMINI_STABILITY_K": "-0.5"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearMeminiEnv(t)
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			if _, err := config.Load(); err == nil {
				t.Fatal("Load: expected error, got nil")
			}
		})
	}
}

func TestDedupTierList(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  []memory.Tier
	}{
		{name: "empty means all tiers", value: "", want: nil},
		{name: "single tier", value: "semantic", want: []memory.Tier{memory.TierSemantic}},
		{
			name:  "comma list with whitespace",
			value: " semantic , episodic ",
			want:  []memory.Tier{memory.TierSemantic, memory.TierEpisodic},
		},
		{name: "trailing comma is ignored", value: "semantic,", want: []memory.Tier{memory.TierSemantic}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearMeminiEnv(t)
			if tt.value != "" {
				t.Setenv("MEMINI_DEDUP_TIERS", tt.value)
			}
			cfg, err := config.Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			got := cfg.DedupTierList()
			if len(got) != len(tt.want) {
				t.Fatalf("DedupTierList() = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("DedupTierList()[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

var meminiEnvKeys = []string{
	"MEMINI_HTTP_ADDR", "MEMINI_SHUTDOWN_TIMEOUT", "MEMINI_LOG_LEVEL", "MEMINI_LOG_FORMAT",
	"MEMINI_BACKEND", "MEMINI_SQLITE_PATH", "MEMINI_POSTGRES_DSN",
	"MEMINI_EMBED_BASE_URL", "MEMINI_EMBED_API_KEY", "MEMINI_EMBED_MODEL", "MEMINI_EMBED_DIMS",
	"MEMINI_EMBED_QUERY_PREFIX",
	"MEMINI_WRITE_DEDUP_SCORE", "MEMINI_WRITE_DEDUP_ACTION",
	"MEMINI_LLM_BASE_URL", "MEMINI_LLM_API_KEY", "MEMINI_LLM_MODEL",
	"MEMINI_RERANK", "MEMINI_RERANK_MODEL", "MEMINI_RERANK_API_KEY",
	"MEMINI_RERANK_TIMEOUT", "MEMINI_RERANK_MAX_BATCH_CHARS",
	"MEMINI_EMBED_MAX_ITEM_CHARS", "MEMINI_RERANK_MAX_DOC_CHARS", "MEMINI_RERANK_LLM_MAX_DOC_CHARS",
	"MEMINI_CLASSIFY_MAX_CHARS", "MEMINI_PROMOTE_WHOLE_MAX_CHARS",
	"MEMINI_CHUNK_EMBED", "MEMINI_CHUNK_SIZE", "MEMINI_CHUNK_OVERLAP",
	"MEMINI_CHUNK_MIN_CONTENT", "MEMINI_CHUNK_MAX_PER_MEMORY",
	"MEMINI_CONSOLIDATE_MODE", "MEMINI_CONSOLIDATE_MIN_SCORE",
	"MEMINI_PROMOTE_INTERVAL", "MEMINI_PROMOTE_MIN_ACCESS", "MEMINI_BACKFILL_INTERVAL",
	"MEMINI_SWEEP_INTERVAL", "MEMINI_SHORT_TERM_CAP", "MEMINI_UI_ENABLED",
	"MEMINI_API_KEY", "MEMINI_API_KEYS_FILE",
	"MEMINI_DEFAULT_NAMESPACE", "MEMINI_NAMESPACE",
	"MEMINI_DEDUP_INTERVAL", "MEMINI_DEDUP_SIMILARITY", "MEMINI_DEDUP_TIERS",
	"MEMINI_WRITE_EMBED_TIMEOUT", "MEMINI_RECALL_EMBED_TIMEOUT",
	"MEMINI_RECALL_REWRITE_TIMEOUT", "MEMINI_REQUEST_TIMEOUT",
	"MEMINI_GLOBAL_NAMESPACE", "MEMINI_TENANT_SHARED",
	"MEMINI_HOME",
	"MEMINI_CLIENT_DEFAULTS",
}

// TestUndeprecatedVarsAreLive pins that a variable which is read is never also
// listed as removed. Both of these were once fixed internal defaults and are
// configurable again; leaving the deprecation entry behind would warn that the
// operator's setting is ignored while it silently took effect, and would render
// the same name into both the settings table and the "Removed" table of the
// generated reference.
func TestUndeprecatedVarsAreLive(t *testing.T) {
	for _, tc := range []struct {
		env  string
		want int
		got  func(*config.Config) int
	}{
		{"MEMINI_EMBED_MAX_ITEM_CHARS", 12345, func(c *config.Config) int { return c.EmbedMaxItemChars }},
		{"MEMINI_RERANK_MAX_DOC_CHARS", 4096, func(c *config.Config) int { return c.RerankMaxDocChars }},
	} {
		t.Run(tc.env, func(t *testing.T) {
			clearMeminiEnv(t)
			t.Setenv(tc.env, strconv.Itoa(tc.want))
			for _, w := range config.DeprecationWarnings() {
				if strings.Contains(w, tc.env) {
					t.Fatalf("%s is read but still warns as deprecated: %q", tc.env, w)
				}
			}
			cfg, err := config.Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got := tc.got(cfg); got != tc.want {
				t.Fatalf("%s = %d, want %d", tc.env, got, tc.want)
			}
		})
	}
}

// TestChunkKnobsInertWhenOff pins that the chunk knobs are only validated when
// chunking is on: a value left over from an experiment must not refuse a boot
// that does not use it.
func TestChunkKnobsInertWhenOff(t *testing.T) {
	clearMeminiEnv(t)
	t.Setenv("MEMINI_CHUNK_EMBED", "false")
	t.Setenv("MEMINI_CHUNK_OVERLAP", "99999") // nonsense, and irrelevant while off
	if _, err := config.Load(); err != nil {
		t.Fatalf("Load refused a boot over an inert chunk knob: %v", err)
	}
}

// TestChunkDefaultsAreCoherent pins the defaults against internal/chunk's own,
// so the server and the splitter cannot drift apart.
func TestChunkDefaultsAreCoherent(t *testing.T) {
	clearMeminiEnv(t)
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	d := chunk.DefaultConfig()
	for _, tc := range []struct {
		name      string
		got, want int
	}{
		{"MEMINI_CHUNK_SIZE", cfg.ChunkSize, d.Size},
		{"MEMINI_CHUNK_OVERLAP", cfg.ChunkOverlap, d.Overlap},
		{"MEMINI_CHUNK_MIN_CONTENT", cfg.ChunkMinContent, d.MinContent},
		{"MEMINI_CHUNK_MAX_PER_MEMORY", cfg.ChunkMaxPerMemory, d.MaxChunks},
	} {
		if tc.got != tc.want {
			t.Errorf("%s default = %d, but chunk.DefaultConfig says %d", tc.name, tc.got, tc.want)
		}
	}
	if cfg.ChunkEmbed {
		t.Error("MEMINI_CHUNK_EMBED defaults on; it must be opt-in")
	}
}

// TestFatalDeprecatedVarsUnset pins the clean-boot case: with neither deleted
// knob set, FatalDeprecatedVars reports nothing to refuse on.
func TestFatalDeprecatedVarsUnset(t *testing.T) {
	clearMeminiEnv(t)
	if got := config.FatalDeprecatedVars(); len(got) != 0 {
		t.Errorf("FatalDeprecatedVars() = %v, want empty", got)
	}
}

// TestFatalDeprecatedVarsGlobalNamespace and TestFatalDeprecatedVarsTenantShared
// pin the T12 boot guard: setting either deleted knob produces a refusal
// message naming the variable (a), explaining the scope model change to the
// always-on ancestor cascade (b), pointing at `memini migrate scopes` and/or
// MEMINI_HOME / `memini link add` as the way forward (c), and citing
// docs/scopes.md#knobs (d).
func TestFatalDeprecatedVarsGlobalNamespace(t *testing.T) {
	clearMeminiEnv(t)
	t.Setenv("MEMINI_GLOBAL_NAMESPACE", "shared/golang")

	got := config.FatalDeprecatedVars()
	if len(got) != 1 {
		t.Fatalf("FatalDeprecatedVars() = %v, want exactly one message", got)
	}
	msg := got[0]
	assertFatalMessageComplete(t, msg, "MEMINI_GLOBAL_NAMESPACE")
}

func TestFatalDeprecatedVarsTenantShared(t *testing.T) {
	clearMeminiEnv(t)
	t.Setenv("MEMINI_TENANT_SHARED", "true")

	got := config.FatalDeprecatedVars()
	if len(got) != 1 {
		t.Fatalf("FatalDeprecatedVars() = %v, want exactly one message", got)
	}
	msg := got[0]
	assertFatalMessageComplete(t, msg, "MEMINI_TENANT_SHARED")
}

// TestFatalDeprecatedVarsBoth pins that both knobs set at once produce two
// distinct messages (not a single collapsed one), so an operator with both
// stale exports sees guidance for each.
func TestFatalDeprecatedVarsBoth(t *testing.T) {
	clearMeminiEnv(t)
	t.Setenv("MEMINI_GLOBAL_NAMESPACE", "acme")
	t.Setenv("MEMINI_TENANT_SHARED", "true")

	got := config.FatalDeprecatedVars()
	if len(got) != 2 {
		t.Fatalf("FatalDeprecatedVars() = %v, want exactly two messages", got)
	}
}

// TestLoadIgnoresFatalDeprecatedVars pins the migrate-scopes exemption: the
// fatal boot guard is NOT enforced inside config.Load() itself. `memini
// migrate scopes` calls config.Load() directly (cmd/memini/migrate.go) to
// read MEMINI_GLOBAL_NAMESPACE via os.Getenv and print adoption instructions
// — if Load() itself refused to return a config when the var is set, the
// migration command that handles it could never run, deadlocking the
// operator. The refusal is enforced instead at the server-start call site
// (cmd/memini/root.go runServer), which explicitly calls
// FatalDeprecatedVars() before doing anything else.
func TestLoadIgnoresFatalDeprecatedVars(t *testing.T) {
	clearMeminiEnv(t)
	t.Setenv("MEMINI_GLOBAL_NAMESPACE", "acme")
	t.Setenv("MEMINI_TENANT_SHARED", "true")

	if _, err := config.Load(); err != nil {
		t.Fatalf("Load: unexpected error with deleted vars set: %v", err)
	}
}

// assertFatalMessageComplete checks all four required elements of a fatal
// deprecation message: (a) names the variable, (b) explains the always-on
// ancestor cascade, (c) points at `memini migrate scopes` and MEMINI_HOME /
// `memini link add`, (d) cites docs/scopes.md#knobs.
func assertFatalMessageComplete(t *testing.T, msg, varName string) {
	t.Helper()
	checks := []struct {
		label string
		want  string
	}{
		{"names the variable", varName},
		{"explains the ancestor cascade", "ancestor cascade"},
		{"points at migrate scopes", "memini migrate scopes"},
		{"points at MEMINI_HOME", "MEMINI_HOME"},
		{"points at link add", "memini link add"},
		{"cites docs", "docs/scopes.md#knobs"},
	}
	for _, c := range checks {
		if !strings.Contains(msg, c.want) {
			t.Errorf("message missing %s (want substring %q); got: %s", c.label, c.want, msg)
		}
	}
}

// TestLoadClientDefaultsUnset pins the feature-off no-op: with
// MEMINI_CLIENT_DEFAULTS absent, ClientDefaults is nil and the KV-backed global
// defaults apply unchanged — zero behavior change versus before this existed.
func TestLoadClientDefaultsUnset(t *testing.T) {
	clearMeminiEnv(t)
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ClientDefaults != nil {
		t.Errorf("ClientDefaults = %+v, want nil when MEMINI_CLIENT_DEFAULTS is unset", cfg.ClientDefaults)
	}
}

// TestLoadClientDefaultsHappy pins the happy path: a valid JSON ClientSettings
// object parses into ClientDefaults with exactly the fields it set (and only
// those — omitted fields stay nil to keep inheriting the built-ins).
func TestLoadClientDefaultsHappy(t *testing.T) {
	clearMeminiEnv(t)
	t.Setenv("MEMINI_CLIENT_DEFAULTS", `{"capture_turns":false,"recall_limit":7,"namespace_scope":"owner_repo"}`)

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ClientDefaults == nil {
		t.Fatal("ClientDefaults is nil, want the parsed object")
	}
	cd := cfg.ClientDefaults
	if cd.CaptureTurns == nil || *cd.CaptureTurns {
		t.Errorf("CaptureTurns = %v, want a set false", cd.CaptureTurns)
	}
	if cd.RecallLimit == nil || *cd.RecallLimit != 7 {
		t.Errorf("RecallLimit = %v, want 7", cd.RecallLimit)
	}
	if cd.NamespaceScope == nil || *cd.NamespaceScope != "owner_repo" {
		t.Errorf("NamespaceScope = %v, want owner_repo", cd.NamespaceScope)
	}
	// An omitted field must stay nil (inherit), not be defaulted here.
	if cd.SessionDigest != nil {
		t.Errorf("SessionDigest = %v, want nil (omitted → inherit)", cd.SessionDigest)
	}
}

// TestLoadClientDefaultsFatal pins the fail-loud boot: invalid JSON, an unknown
// field, or a value failing ClientSettings.Validate all refuse the boot with a
// message naming the variable — never a silent fallback to the built-ins.
func TestLoadClientDefaultsFatal(t *testing.T) {
	cases := []struct {
		name, raw string
	}{
		{"invalid JSON", `{not json`},
		{"unknown field", `{"bogus_field":true}`},
		{"out-of-range value", `{"auto_save_interval":0}`},
		{"bad enum value", `{"namespace_scope":"nonsense"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clearMeminiEnv(t)
			t.Setenv("MEMINI_CLIENT_DEFAULTS", c.raw)
			_, err := config.Load()
			if err == nil {
				t.Fatalf("Load should be fatal for %s, got nil error", c.name)
			}
			if !strings.Contains(err.Error(), "MEMINI_CLIENT_DEFAULTS") {
				t.Errorf("error should name the variable, got: %v", err)
			}
		})
	}
}
