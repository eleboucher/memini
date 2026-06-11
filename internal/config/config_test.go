package config_test

import (
	"os"
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
	// Land in a temp dir so `git rev-parse` fails and we fall through to the
	// cwd basename. Use a stable basename so the assertion is meaningful.
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
	if cfg.FusionAlpha != 0.5 {
		t.Errorf("FusionAlpha = %v, want 0.5 (score fusion default)", cfg.FusionAlpha)
	}
	if cfg.WriteDedupMinScore != 0 {
		t.Errorf("WriteDedupMinScore = %v, want 0 (off by default)", cfg.WriteDedupMinScore)
	}
	if cfg.TemporalBoost != 0.40 {
		t.Errorf("TemporalBoost = %v, want 0.40 (temporal targeting on by default)", cfg.TemporalBoost)
	}
	if cfg.SweepInterval != time.Hour {
		t.Errorf("SweepInterval = %v, want 1h", cfg.SweepInterval)
	}
	if cfg.ConsolidateMode != "async" {
		t.Errorf("ConsolidateMode = %q, want async", cfg.ConsolidateMode)
	}
	if cfg.ConsolidateMinScore != 0.6 {
		t.Errorf("ConsolidateMinScore = %v, want 0.6", cfg.ConsolidateMinScore)
	}
	if cfg.PromoteInterval != 24*time.Hour {
		t.Errorf("PromoteInterval = %v, want 24h", cfg.PromoteInterval)
	}
	if cfg.PromoteMinAccess != 3 {
		t.Errorf("PromoteMinAccess = %d, want 3", cfg.PromoteMinAccess)
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
		t.Error("UIEnabled = false, want true by default")
	}
}

func TestLoadOverrides(t *testing.T) {
	t.Setenv("MEMINI_HTTP_ADDR", ":9999")
	t.Setenv("MEMINI_LOG_LEVEL", "debug")
	t.Setenv("MEMINI_EMBED_DIMS", "256")
	t.Setenv("MEMINI_EMBED_QUERY_PREFIX", "Instruct: retrieve\nQuery: ")
	t.Setenv("MEMINI_FUSION_ALPHA", "-1")
	t.Setenv("MEMINI_WRITE_DEDUP_MIN_SCORE", "0.95")
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
	if cfg.FusionAlpha != -1 {
		t.Errorf("FusionAlpha = %v, want -1 (RRF override)", cfg.FusionAlpha)
	}
	if cfg.WriteDedupMinScore != 0.95 {
		t.Errorf("WriteDedupMinScore = %v, want 0.95", cfg.WriteDedupMinScore)
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
			name: "dedup min cluster size too small",
			env:  map[string]string{"MEMINI_DEDUP_MIN_CLUSTER_SIZE": "1"},
		},
		{
			name: "dedup neighbours too small",
			env:  map[string]string{"MEMINI_DEDUP_NEIGHBOURS": "0"},
		},
		{
			name: "dedup unknown tier",
			env:  map[string]string{"MEMINI_DEDUP_TIERS": "semantic,bogus"},
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
	"MEMINI_EMBED_QUERY_PREFIX", "MEMINI_FUSION_ALPHA",
	"MEMINI_WRITE_DEDUP_MIN_SCORE", "MEMINI_TEMPORAL_BOOST",
	"MEMINI_LLM_BASE_URL", "MEMINI_LLM_API_KEY", "MEMINI_LLM_MODEL",
	"MEMINI_CONSOLIDATE_MODE", "MEMINI_CONSOLIDATE_MIN_SCORE",
	"MEMINI_PROMOTE_INTERVAL", "MEMINI_PROMOTE_MIN_ACCESS",
	"MEMINI_SWEEP_INTERVAL", "MEMINI_SHORT_TERM_CAP", "MEMINI_UI_ENABLED",
	"MEMINI_API_KEY", "MEMINI_NAMESPACE_HEADER",
	"MEMINI_DEFAULT_NAMESPACE", "MEMINI_NAMESPACE",
	"MEMINI_DEDUP_INTERVAL", "MEMINI_DEDUP_SIMILARITY", "MEMINI_DEDUP_MIN_CLUSTER_SIZE",
	"MEMINI_DEDUP_NEIGHBOURS", "MEMINI_DEDUP_TIERS",
}
