package main

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/eleboucher/memini/internal/config"
	"github.com/eleboucher/memini/internal/embed"
	"github.com/eleboucher/memini/internal/extract"
	"github.com/eleboucher/memini/internal/llm"
	"github.com/eleboucher/memini/internal/rerank"
	"github.com/eleboucher/memini/internal/service"
	"github.com/spf13/cobra"
)

// TestRunServerRefusesDeletedGlobalNamespace and
// TestRunServerRefusesDeletedTenantShared pin the T12 boot guard at the
// actual call site (not just the config.FatalDeprecatedVars helper):
// runServer must refuse before doing any work when either deleted knob is
// set, and the returned error must reach the operator with the raw
// guidance (see config.FatalDeprecatedVars for the message contract) rather
// than being wrapped into generic noise.
func TestRunServerRefusesDeletedGlobalNamespace(t *testing.T) {
	t.Setenv("MEMINI_GLOBAL_NAMESPACE", "shared/golang")

	err := runServer(&cobra.Command{}, nil)
	if err == nil {
		t.Fatal("runServer: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "MEMINI_GLOBAL_NAMESPACE") {
		t.Errorf("runServer error = %q, want it to name MEMINI_GLOBAL_NAMESPACE", err.Error())
	}
}

func TestRunServerRefusesDeletedTenantShared(t *testing.T) {
	t.Setenv("MEMINI_TENANT_SHARED", "true")

	err := runServer(&cobra.Command{}, nil)
	if err == nil {
		t.Fatal("runServer: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "MEMINI_TENANT_SHARED") {
		t.Errorf("runServer error = %q, want it to name MEMINI_TENANT_SHARED", err.Error())
	}
}

// TestRunMCPRefusesDeletedGlobalNamespace and
// TestRunMCPRefusesDeletedTenantShared pin the same T12 boot guard on the
// stdio MCP entrypoint (review finding): `memini mcp` builds the identical
// service stack via buildServiceStack and runs as a persistent server —
// the standard plugin deployment mode — so it must refuse a stale deleted
// knob exactly like runServer, not boot silently past it.
func TestRunMCPRefusesDeletedGlobalNamespace(t *testing.T) {
	t.Setenv("MEMINI_GLOBAL_NAMESPACE", "shared/golang")
	t.Setenv("MEMINI_SQLITE_PATH", t.TempDir()+"/memini.db") // never reached; keeps a regressed run from touching a real db

	err := runMCP(&cobra.Command{}, nil)
	if err == nil {
		t.Fatal("runMCP: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "MEMINI_GLOBAL_NAMESPACE") {
		t.Errorf("runMCP error = %q, want it to name MEMINI_GLOBAL_NAMESPACE", err.Error())
	}
}

func TestRunMCPRefusesDeletedTenantShared(t *testing.T) {
	t.Setenv("MEMINI_TENANT_SHARED", "true")
	t.Setenv("MEMINI_SQLITE_PATH", t.TempDir()+"/memini.db")

	err := runMCP(&cobra.Command{}, nil)
	if err == nil {
		t.Fatal("runMCP: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "MEMINI_TENANT_SHARED") {
		t.Errorf("runMCP error = %q, want it to name MEMINI_TENANT_SHARED", err.Error())
	}
}

// TestRunMigrateScopesNotBlockedByGlobalNamespace pins the deadlock-avoidance
// case (brief's "IMPORTANT subtlety"): `memini migrate scopes` must keep
// running even with MEMINI_GLOBAL_NAMESPACE set, since it's the very command
// that reads that var to print adoption instructions. If the boot guard were
// enforced inside config.Load() (which runMigrateScopes also calls), this
// would deadlock the operator. Uses --yes=false (dry-run default) against an
// empty sqlite store so the command completes without needing an embedder.
// The output assertions prove the exempted path actually ran through to the
// report printer (T11's adoption instructions), not merely returned nil.
func TestRunMigrateScopesNotBlockedByGlobalNamespace(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MEMINI_BACKEND", "sqlite")
	t.Setenv("MEMINI_SQLITE_PATH", dir+"/memini.db")
	t.Setenv("MEMINI_GLOBAL_NAMESPACE", "shared/golang")
	t.Setenv("MEMINI_EMBED_DIMS", "8")

	migrateScopesYes = false
	migrateScopesCmd.SetContext(context.Background())
	var buf bytes.Buffer
	migrateScopesCmd.SetOut(&buf)
	t.Cleanup(func() { migrateScopesCmd.SetOut(nil) })

	if err := runMigrateScopes(migrateScopesCmd, nil); err != nil {
		t.Fatalf("runMigrateScopes: unexpected error with MEMINI_GLOBAL_NAMESPACE set: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"MEMINI_GLOBAL_NAMESPACE",     // names the dead knob
		`MEMINI_HOME="shared/golang"`, // single-operator adoption
		"memini link add",             // team-wide adoption
	} {
		if !strings.Contains(out, want) {
			t.Errorf("adoption instructions missing %q; got:\n%s", want, out)
		}
	}
}

// bufLog returns a logger writing to the returned buffer, for asserting that
// a construction path announced (or stayed quiet about) a wrapper.
func bufLog() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, nil)), &buf
}

// TestBuildEmbedderNoBaseURL pins the degraded mode: with no embeddings
// endpoint configured, buildEmbedder does not fail — it returns the
// embed.Disabled sentinel (carrying the configured dims) and warns, so
// read-only commands still boot and remember/recall error lazily.
func TestBuildEmbedderNoBaseURL(t *testing.T) {
	log, buf := bufLog()
	e, err := buildEmbedder(&config.Config{EmbedDims: 8}, log, nil)
	if err != nil {
		t.Fatalf("buildEmbedder: unexpected error: %v", err)
	}
	d, ok := e.(embed.Disabled)
	if !ok {
		t.Fatalf("buildEmbedder = %T, want embed.Disabled", e)
	}
	if d.D != 8 {
		t.Errorf("Disabled dims = %d, want 8", d.D)
	}
	if got := outerBackendLabel(e); got != "disabled" {
		t.Errorf("outerBackendLabel = %q, want %q", got, "disabled")
	}
	if !strings.Contains(buf.String(), "no embeddings endpoint configured") {
		t.Errorf("missing degraded-mode warning; log:\n%s", buf.String())
	}
}

// TestBuildEmbedderInvalidConfig pins that a partially-set embed config fails
// loudly at construction instead of degrading silently.
func TestBuildEmbedderInvalidConfig(t *testing.T) {
	tests := []struct {
		name    string
		cfg     *config.Config
		wantErr string
	}{
		{"missing model", &config.Config{EmbedBaseURL: "http://127.0.0.1:1/v1", EmbedDims: 8}, "Model is required"},
		{"non-positive dims", &config.Config{EmbedBaseURL: "http://127.0.0.1:1/v1", EmbedModel: "m"}, "Dims must be positive"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := buildEmbedder(tt.cfg, quietLog(), nil)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("buildEmbedder error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

// TestBuildEmbedderWrapsCacheAndBatch pins the wrapper stack of a fully
// configured embedder: the in-memory cache is always outermost (so
// outerBackendLabel reports "cached" for metrics), with batching and the
// optional concurrency limiter inside. Construction only — nothing is dialed.
func TestBuildEmbedderWrapsCacheAndBatch(t *testing.T) {
	cfg := &config.Config{
		EmbedBaseURL: "http://127.0.0.1:1/v1", EmbedModel: "m", EmbedDims: 8,
		EmbedMaxBatch: 20, EmbedMaxBatchChars: 24000,
	}

	log, buf := bufLog()
	e, err := buildEmbedder(cfg, log, nil)
	if err != nil {
		t.Fatalf("buildEmbedder: unexpected error: %v", err)
	}
	if _, ok := e.(*embed.Cached); !ok {
		t.Fatalf("buildEmbedder = %T, want *embed.Cached outermost", e)
	}
	if got := outerBackendLabel(e); got != "cached" {
		t.Errorf("outerBackendLabel = %q, want %q", got, "cached")
	}
	// No concurrency cap requested: the cap must not be announced.
	if strings.Contains(buf.String(), "embed concurrency cap") {
		t.Errorf("unexpected concurrency-cap log with EmbedMaxConcurrency=0:\n%s", buf.String())
	}

	// With the knob set, the limiter is applied (announced in the log; the
	// wrapper itself sits inside Batched/Cached so cache hits skip the slot).
	cfg.EmbedMaxConcurrency = 4
	log, buf = bufLog()
	if _, err := buildEmbedder(cfg, log, nil); err != nil {
		t.Fatalf("buildEmbedder with concurrency cap: %v", err)
	}
	if !strings.Contains(buf.String(), "embed concurrency cap") {
		t.Errorf("missing concurrency-cap log with EmbedMaxConcurrency=4:\n%s", buf.String())
	}
}

// rerankLLM is the MEMINI_RERANK value selecting the LLM reranker backend
// (hoisted so the literal stays under goconst's occurrence threshold).
const rerankLLM = "llm"

// fakeChat is a no-op llm.Client for construction-path tests.
type fakeChat struct{}

func (fakeChat) Consolidate(context.Context, llm.Input) (llm.Decision, error) {
	return llm.Decision{}, nil
}
func (fakeChat) Complete(context.Context, string, string) (string, error)      { return "", nil }
func (fakeChat) Distill(context.Context, llm.DistillInput) ([]llm.Fact, error) { return nil, nil }
func (fakeChat) MergeMemories(context.Context, []string) (string, error)       { return "", nil }

// TestRerankDisabledGate pins the disabled path: buildReranker is only
// reached behind cfg.RerankEnabled() (root.go), so "" and "off" mean no
// reranker is ever constructed.
func TestRerankDisabledGate(t *testing.T) {
	for _, v := range []string{"", "off"} {
		cfg := &config.Config{Rerank: v}
		if cfg.RerankEnabled() {
			t.Errorf("RerankEnabled() = true for Rerank=%q, want false", v)
		}
	}
}

// TestBuildRerankerLLMRequiresChat pins the fail-loud contract: an explicitly
// requested LLM reranker without a configured LLM is a startup error naming
// the misconfigured variables, not a silent degradation.
func TestBuildRerankerLLMRequiresChat(t *testing.T) {
	_, _, err := buildReranker(&config.Config{Rerank: rerankLLM}, nil, quietLog(), nil)
	if err == nil {
		t.Fatal("buildReranker: expected error, got nil")
	}
	for _, want := range []string{"MEMINI_RERANK=llm", "MEMINI_LLM_BASE_URL"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to name %s", err.Error(), want)
		}
	}
}

// TestBuildRerankerLLM pins backend selection for MEMINI_RERANK=llm with a
// chat client available, and that RerankMaxConcurrency=0 applies no limiter.
func TestBuildRerankerLLM(t *testing.T) {
	r, name, err := buildReranker(&config.Config{Rerank: rerankLLM}, fakeChat{}, quietLog(), nil)
	if err != nil {
		t.Fatalf("buildReranker: unexpected error: %v", err)
	}
	if r == nil {
		t.Fatal("buildReranker returned nil reranker")
	}
	if name != rerankLLM {
		t.Errorf("backend name = %q, want %q", name, rerankLLM)
	}
	if _, ok := r.(*rerank.Limited); ok {
		t.Errorf("unexpected *rerank.Limited wrapper with RerankMaxConcurrency=0")
	}
}

// TestBuildRerankerCrossEncoder pins backend selection for a URL-valued
// MEMINI_RERANK: a cross-encoder client, named "cross_encoder", with the
// concurrency limiter applied only when the knob is set. Construction only.
func TestBuildRerankerCrossEncoder(t *testing.T) {
	cfg := &config.Config{Rerank: "http://127.0.0.1:1/v1", RerankModel: "m"}

	r, name, err := buildReranker(cfg, nil, quietLog(), nil)
	if err != nil {
		t.Fatalf("buildReranker: unexpected error: %v", err)
	}
	if _, ok := r.(*rerank.CrossEncoder); !ok {
		t.Fatalf("buildReranker = %T, want *rerank.CrossEncoder", r)
	}
	if name != "cross_encoder" {
		t.Errorf("backend name = %q, want %q", name, "cross_encoder")
	}

	cfg.RerankMaxConcurrency = 3
	r, _, err = buildReranker(cfg, nil, quietLog(), nil)
	if err != nil {
		t.Fatalf("buildReranker with concurrency cap: %v", err)
	}
	lim, ok := r.(*rerank.Limited)
	if !ok {
		t.Fatalf("buildReranker = %T, want *rerank.Limited with RerankMaxConcurrency=3", r)
	}
	if lim.Max() != 3 {
		t.Errorf("Limited.Max() = %d, want 3", lim.Max())
	}
}

// TestBuildRerankerCrossEncoderMissingModelWarns pins the lenient path: a
// URL-valued MEMINI_RERANK without MEMINI_RERANK_MODEL still constructs (the
// model field is omitted from /rerank requests) but warns the operator.
func TestBuildRerankerCrossEncoderMissingModelWarns(t *testing.T) {
	log, buf := bufLog()
	r, _, err := buildReranker(&config.Config{Rerank: "http://127.0.0.1:1/v1"}, nil, log, nil)
	if err != nil {
		t.Fatalf("buildReranker: unexpected error: %v", err)
	}
	if r == nil {
		t.Fatal("buildReranker returned nil reranker")
	}
	if !strings.Contains(buf.String(), "MEMINI_RERANK_MODEL") {
		t.Errorf("missing model-omitted warning; log:\n%s", buf.String())
	}
}

// TestBuildRerankerEmptyURLErrors pins the defensive path inside
// buildReranker itself: called with an empty (non-llm) Rerank value — which
// the RerankEnabled gate normally prevents — the cross-encoder constructor
// rejects the empty base URL rather than building a client that can't dial.
func TestBuildRerankerEmptyURLErrors(t *testing.T) {
	_, _, err := buildReranker(&config.Config{Rerank: ""}, nil, quietLog(), nil)
	if err == nil || !strings.Contains(err.Error(), "base url is required") {
		t.Fatalf("buildReranker error = %v, want base-url error", err)
	}
}

// TestTruncationDefaultsMatchPackageConstants pins the contract that made these
// five settings safe to introduce: each replaced a hardcoded constant, so its
// env default must reproduce the old baked-in behaviour exactly. An `envDefault`
// is a struct tag and cannot reference a constant, so three of these values
// exist in two places at once — this test is the only thing keeping them equal.
// Without it, a typo ("800" for "8000") silently cuts every memory's searchable
// prefix tenfold with a green suite, which is the very failure this work exists
// to eliminate.
func TestTruncationDefaultsMatchPackageConstants(t *testing.T) {
	for _, k := range []string{
		"MEMINI_EMBED_MAX_ITEM_CHARS", "MEMINI_RERANK_MAX_DOC_CHARS",
		"MEMINI_RERANK_LLM_MAX_DOC_CHARS", "MEMINI_CLASSIFY_MAX_CHARS",
		"MEMINI_PROMOTE_WHOLE_MAX_CHARS", "MEMINI_EMBED_MAX_BATCH",
		"MEMINI_EMBED_MAX_BATCH_CHARS", "MEMINI_RERANK_MAX_BATCH_CHARS",
	} {
		t.Setenv(k, "") // records the original for restoration
		_ = os.Unsetenv(k)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, tc := range []struct {
		name string
		got  int
		want int
	}{
		// cmd/bench and cmd/qa build embedders/rerankers from these exported
		// constants rather than the server's Config, so a drift here means a
		// benchmark measures budgets production does not use.
		{"EmbedMaxItemChars", cfg.EmbedMaxItemChars, config.DefaultEmbedMaxItemChars},
		{"RerankMaxDocChars", cfg.RerankMaxDocChars, config.DefaultRerankMaxDocChars},
		{"EmbedMaxBatch", cfg.EmbedMaxBatch, config.DefaultEmbedMaxBatch},
		{"EmbedMaxBatchChars", cfg.EmbedMaxBatchChars, config.DefaultEmbedMaxBatchChars},
		{"RerankMaxBatchChars", cfg.RerankMaxBatchChars, config.DefaultRerankMaxBatchChars},
		// Cross-checks: the package default still exists for callers that build
		// these without the server's config, so the two must agree.
		{"RerankLLMMaxDocChars", cfg.RerankLLMMaxDocChars, rerank.DefaultLLMMaxChars},
		{"ClassifyMaxChars", cfg.ClassifyMaxChars, extract.ClassifyMaxChars},
		{"PromoteWholeMaxChars", cfg.PromoteWholeMaxChars, service.DefaultPromoteWholeMaxChars},
	} {
		if tc.got != tc.want {
			t.Errorf("%s default = %d, want %d — the env default and the package "+
				"constant have drifted apart", tc.name, tc.got, tc.want)
		}
	}
}
