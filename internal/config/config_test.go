package config_test

import (
	"os"
	"strings"
	"testing"
	"time"

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
	t.Setenv("MEMINI_DEFAULT_NAMESPACE", "tenant-a")

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
	if cfg.DefaultNamespace != "tenant-a" {
		t.Errorf("DefaultNamespace = %q, want tenant-a", cfg.DefaultNamespace)
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

func TestReadNamespacesParsing(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    []string
		wantErr bool
	}{
		{name: "unset disables it", value: "", want: nil},
		{name: "single entry", value: "shared", want: []string{"shared"}},
		{
			name:  "list splits and trims whitespace",
			value: " shared , rules/go ",
			want:  []string{"shared", "rules/go"},
		},
		{name: "subtree pattern preserved", value: "rules/*", want: []string{"rules/*"}},
		{
			name:  "empty entries dropped",
			value: "shared,,  ,rules/*",
			want:  []string{"shared", "rules/*"},
		},
		{name: "invalid entry rejected", value: strings.Repeat("x", 300), wantErr: true},
		{
			name:  "surrounding slashes and duplicate separators normalized",
			value: "shared/,/team,a//b,rules//*",
			want:  []string{"shared", "team", "a/b", "rules/*"},
		},
		{name: "bare pattern rejected", value: "/*", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearMeminiEnv(t)
			if tt.value != "" {
				t.Setenv("MEMINI_READ_NAMESPACES", tt.value)
			}
			cfg, err := config.Load()
			if tt.wantErr {
				if err == nil {
					t.Fatal("Load: expected error, got nil")
				}
				if !strings.Contains(err.Error(), "MEMINI_READ_NAMESPACES") {
					t.Errorf("error = %q, want it to name MEMINI_READ_NAMESPACES", err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if len(cfg.ReadNamespaces) != len(tt.want) {
				t.Fatalf("ReadNamespaces = %v, want %v", cfg.ReadNamespaces, tt.want)
			}
			for i := range tt.want {
				if cfg.ReadNamespaces[i] != tt.want[i] {
					t.Errorf("ReadNamespaces[%d] = %q, want %q", i, cfg.ReadNamespaces[i], tt.want[i])
				}
			}
		})
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
	"MEMINI_CONSOLIDATE_MODE", "MEMINI_CONSOLIDATE_MIN_SCORE",
	"MEMINI_PROMOTE_INTERVAL", "MEMINI_PROMOTE_MIN_ACCESS", "MEMINI_BACKFILL_INTERVAL",
	"MEMINI_SWEEP_INTERVAL", "MEMINI_SHORT_TERM_CAP", "MEMINI_UI_ENABLED",
	"MEMINI_API_KEY",
	"MEMINI_DEFAULT_NAMESPACE", "MEMINI_NAMESPACE",
	"MEMINI_DEDUP_INTERVAL", "MEMINI_DEDUP_SIMILARITY", "MEMINI_DEDUP_TIERS",
	"MEMINI_WRITE_EMBED_TIMEOUT", "MEMINI_RECALL_EMBED_TIMEOUT",
	"MEMINI_RECALL_REWRITE_TIMEOUT", "MEMINI_REQUEST_TIMEOUT",
	"MEMINI_GLOBAL_NAMESPACE", "MEMINI_READ_NAMESPACES",
}
