package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/spf13/cobra"

	"github.com/eleboucher/memini/internal/api/rest"
	"github.com/eleboucher/memini/internal/api/ui"
	"github.com/eleboucher/memini/internal/config"
	"github.com/eleboucher/memini/internal/embed"
	"github.com/eleboucher/memini/internal/llm"
	"github.com/eleboucher/memini/internal/logging"
	"github.com/eleboucher/memini/internal/maintenance"
	"github.com/eleboucher/memini/internal/rerank"
	"github.com/eleboucher/memini/internal/search"
	"github.com/eleboucher/memini/internal/server"
	"github.com/eleboucher/memini/internal/service"
	"github.com/eleboucher/memini/internal/store"
	"github.com/eleboucher/memini/internal/store/postgres"
	"github.com/eleboucher/memini/internal/store/sqlitevec"
	"github.com/eleboucher/memini/internal/version"

	mcpapi "github.com/eleboucher/memini/internal/api/mcp"
)

var rootCmd = &cobra.Command{
	Use:           "memini",
	Short:         "memini — a memory service for AI agents",
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runServer,
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func runServer(cmd *cobra.Command, _ []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	log := logging.New(cfg.LogLevel, cfg.LogFormat)
	log.Info("starting memini",
		"version", version.String(),
		"backend", cfg.Backend,
		"llm_enabled", cfg.LLMEnabled(),
		"default_namespace", cfg.DefaultNamespace,
		"namespace_source", string(cfg.NamespaceSrc),
	)

	for _, w := range config.DeprecationWarnings() {
		log.Warn("deprecated configuration", "detail", w)
	}

	if cfg.NamespaceSrc != config.NamespaceFromEnv {
		log.Warn("default namespace derived from server's cwd — likely wrong in HTTP mode. "+
			"Set X-Memini-Namespace on every request (plugin does this) or set MEMINI_DEFAULT_NAMESPACE",
			"namespace", cfg.DefaultNamespace, "source", cfg.NamespaceSrc)
	}

	ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// One registry shared by the service stack (app/store/embed metrics) AND
	// the HTTP server's /metrics handler. Without this the two would create
	// independent registries and every series registered on the service one
	// would be invisible at /metrics.
	reg := prometheus.NewRegistry()

	svc, st, joinWorkers, cleanup, err := buildServiceStack(ctx, cfg, log, reg)
	if err != nil {
		return err
	}
	defer cleanup()
	defer joinWorkers()

	if cfg.APIKey == "" && !loopbackAddr(cfg.HTTPAddr) {
		log.Warn("MEMINI_API_KEY is not set and the listen address is not loopback: " +
			"the full API (including deletes and cross-namespace reads) is open to " +
			"anyone who can reach the port")
	}

	srv, err := newServer(cfg, svc, st, log, reg)
	if err != nil {
		return err
	}

	return srv.Run(ctx)
}

// newServer mounts the REST API, MCP handler, and optional admin UI onto a
// fresh server and returns it ready to Run. Kept separate from runServer so
// integration tests exercise the exact same HTTP wiring without re-running the
// process bootstrap. reg must be the same registry the service stack writes
// to, otherwise /metrics exposes only HTTP-side series and every
// app/store/embed collector stays invisible.
func newServer(
	cfg *config.Config, svc *service.Service, st store.Store, log *slog.Logger, reg *prometheus.Registry,
) (*server.Server, error) {
	srv := server.New(server.Options{
		Addr:            cfg.HTTPAddr,
		ShutdownTimeout: cfg.ShutdownTimeout,
		APIKey:          cfg.APIKey,
		MetricsAddr:     cfg.MetricsAddr,
	}, log, reg)
	srv.SetReady(func(ctx context.Context) error { return st.Ping(ctx) })

	rest.New(svc, rest.AuthConfig{
		APIKey:           cfg.APIKey,
		NamespaceHeader:  config.DefaultNamespaceHeader,
		DefaultNamespace: cfg.DefaultNamespace,
	}).Mount(srv.Router())

	mcpHandler := mcpapi.HTTPHandler(svc, config.DefaultNamespaceHeader, cfg.DefaultNamespace, cfg.APIKey)
	srv.Router().Handle("/mcp", mcpHandler)
	srv.Router().Handle("/mcp/*", mcpHandler)

	if cfg.UIEnabled {
		if cfg.APIKey != "" {
			log.Warn("the admin UI embeds MEMINI_API_KEY in its unauthenticated shell " +
				"so the browser can call the API; anyone who can load / can read the key. " +
				"Set MEMINI_UI_ENABLED=false if the port is reachable by untrusted clients")
		}
		if err := ui.Mount(srv.Router(), cfg.APIKey); err != nil {
			return nil, fmt.Errorf("mount ui: %w", err)
		}
		log.Info("admin UI mounted at /")
	}

	return srv, nil
}

// buildServiceStack constructs the store, embedder, service, and starts
// background workers. Returns the service, a join function for workers,
// and a cleanup function that closes the store. reg is the shared
// Prometheus registry the service-side metrics are written to; the HTTP
// server reads from the same registry to expose them at /metrics.
func buildServiceStack(
	ctx context.Context, cfg *config.Config, log *slog.Logger, reg *prometheus.Registry,
) (*service.Service, store.Store, func(), func(), error) {
	// openStore (not buildStore): the embed-model reconciliation below needs the
	// embedder to re-embed on a model change, which the buildStore guard can't do.
	st, err := openStore(ctx, cfg)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	metricsImpl := newConsolidateMetrics(reg)

	embedder, err := buildEmbedder(cfg, log, metricsImpl.EmbedInFlight)
	if err != nil {
		_ = st.Close()
		return nil, nil, nil, nil, err
	}

	if sm, ok := st.(interface{ SetMetrics(store.Metrics) }); ok {
		sm.SetMetrics(metricsImpl)
	}
	if em, ok := embedder.(interface{ SetMetrics(embed.Metrics) }); ok {
		em.SetMetrics(metricsImpl)
	}
	embedder = embed.Instrument(embedder, outerBackendLabel(embedder), metricsImpl)

	if err := reconcileEmbedModel(ctx, st, embedder, cfg, log); err != nil {
		_ = st.Close()
		return nil, nil, nil, nil, err
	}

	var svcOpts []service.Option
	var chatClient llm.Client
	if cfg.LLMEnabled() {
		client, err := llm.New(llm.API(cfg.LLMAPI), llm.Config{
			BaseURL: cfg.LLMBaseURL, APIKey: cfg.LLMAPIKey, Model: cfg.LLMModel,
		})
		if err != nil {
			_ = st.Close()
			return nil, nil, nil, nil, err
		}
		chatClient = client
		// The distiller is shared by write-time distillation and the batch
		// promoter, so wire it whenever an LLM is configured — not only when the
		// promoter is on, or MEMINI_PROMOTE_INTERVAL=0 would silently disable
		// distill-on-write too.
		svcOpts = append(svcOpts, service.WithConsolidator(client), service.WithAnswerer(client),
			service.WithDistiller(client))
		log.Info("LLM consolidation + answering enabled",
			"api", cfg.LLMAPI, "model", cfg.LLMModel, "mode", cfg.ConsolidateMode)
	}
	// Promotion self-selects its engine: the LLM distiller when configured, the
	// marker extractor otherwise. Wired outside the LLM block so LLM-less
	// deployments get the usage-earned backstop too.
	if cfg.PromoteInterval > 0 {
		svcOpts = append(svcOpts, service.WithPromoteMinAccess(cfg.PromoteMinAccess))
		log.Info("episodic→semantic promotion enabled",
			"interval", cfg.PromoteInterval, "min_access", cfg.PromoteMinAccess)
	}
	if cfg.RerankEnabled() {
		reranker, name, err := buildReranker(cfg, chatClient, log, metricsImpl.RerankInFlight)
		if err != nil {
			_ = st.Close()
			return nil, nil, nil, nil, err
		}
		if reranker != nil {
			svcOpts = append(svcOpts,
				service.WithReranker(reranker, name),
				service.WithRerankTimeout(cfg.RerankTimeout),
			)
		}
	}
	svcOpts = append(svcOpts,
		service.WithShortTermCap(cfg.ShortTermCap),
		service.WithConsolidateMode(service.ConsolidateMode(cfg.ConsolidateMode)),
		service.WithConsolidateMinScore(cfg.ConsolidateMinScore),
		service.WithQueryPrefix(cfg.EmbedQueryPrefix),
		service.WithScoreFusion(search.DefaultFusionAlpha),
		service.WithWriteDedup(cfg.WriteDedupScore, service.WriteDedupAction(cfg.WriteDedupAction)),
		service.WithCorroboration(defaultCorroborateMinScore),
		service.WithGlobalNamespace(cfg.GlobalNamespace),
		service.WithTemporalTargeting(defaultTemporalBoost, search.RegexAnchorExtractor{}),
		service.WithRecallEmbedTimeout(cfg.RecallEmbedTimeout),
		service.WithRecallMinScore(defaultRecallMinScore),
		service.WithRecallSemanticReserve(defaultRecallSemanticReserve),
		service.WithEpisodicMinChars(cfg.EpisodicMinChars),
		// Write-time fact building self-selects: distill (LLM) no-ops without a
		// consolidator; extract (heuristic) only fires when no LLM is configured.
		service.WithDistillOnWrite(true),
		service.WithExtractOnWrite(true),
		service.WithMetrics(metricsImpl),
	)
	svc := service.New(st, embedder, svcOpts...)

	workerCtx, stopWorkers := context.WithCancel(ctx)
	var workers sync.WaitGroup
	joinWorkers := func() {
		stopWorkers()
		workers.Wait()
		svc.WaitBackground()
	}
	workers.Go(func() { svc.StartConsolidator(workerCtx) })
	workers.Go(func() { svc.RunPromoter(workerCtx, cfg.PromoteInterval) })
	sweeper := maintenance.NewSweeper(st, log, maintenance.SweeperConfig{
		Interval:     cfg.SweepInterval,
		ShortTermCap: cfg.ShortTermCap,
		TombstoneTTL: cfg.TombstoneTTL,
		DemoteAfter:  cfg.DemoteAfter,
	})
	workers.Go(func() { sweeper.Run(workerCtx) })
	if cfg.DedupInterval > 0 {
		dedupJob := maintenance.NewDedupJob(st, embedder, metricsImpl, log, cfg.DedupInterval, maintenance.DedupOptions{
			Similarity: cfg.DedupSimilarity,
			Tiers:      cfg.DedupTierList(),
		})
		workers.Go(func() { dedupJob.Run(workerCtx) })
		log.Info("periodic dedup enabled",
			"interval", cfg.DedupInterval,
			"similarity", cfg.DedupSimilarity)
	}

	cleanup := func() {
		if err := st.Close(); err != nil {
			log.Warn("closing store", "err", err)
		}
	}

	return svc, st, joinWorkers, cleanup, nil
}

func loopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func buildReranker(cfg *config.Config, chat llm.Client, log *slog.Logger, onInFlight func(n int64)) (rerank.Reranker, string, error) {
	if cfg.RerankIsLLM() {
		if chat == nil {
			log.Warn("MEMINI_RERANK=llm but no LLM is configured; set MEMINI_LLM_BASE_URL")
			return nil, "", nil
		}
		log.Info("LLM recall reranking enabled (adds one LLM call per recall)",
			"model", cfg.LLMModel)
		return wrapRerank(rerank.NewLLM(chat), cfg.RerankMaxConcurrency, onInFlight, log, "llm"), "llm", nil
	}
	ce, err := rerank.New(rerank.Config{
		BaseURL:       cfg.Rerank,
		Model:         cfg.RerankModel,
		APIKey:        cfg.RerankAPIKey,
		MaxDocChars:   rerankMaxDocChars,
		MaxBatchChars: cfg.RerankMaxBatchChars,
	})
	if err != nil {
		return nil, "", err
	}
	log.Info("cross-encoder recall reranking enabled (adds one reranker call per recall)",
		"base_url", cfg.Rerank, "model", cfg.RerankModel)
	return wrapRerank(ce, cfg.RerankMaxConcurrency, onInFlight, log, "cross_encoder"), "cross_encoder", nil
}

// wrapRerank applies the optional concurrency cap. max <= 0 is a no-op.
func wrapRerank(r rerank.Reranker, max int, onInFlight func(n int64), log *slog.Logger, name string) rerank.Reranker {
	if max <= 0 {
		return r
	}
	log.Info("rerank concurrency cap", "backend", name, "max_in_flight", max)
	return rerank.NewLimited(r, max, onInFlight)
}

// buildStore opens the configured store and verifies the recorded embedding
// model matches MEMINI_EMBED_MODEL, so a silent model swap (same dims, vectors
// in an incomparable space) fails loudly instead of quietly degrading recall.
// The `reembed` command uses openStore directly to bypass this guard.
func buildStore(ctx context.Context, cfg *config.Config) (store.Store, error) {
	st, err := openStore(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := guardEmbedModel(ctx, st, cfg.EmbedModel); err != nil {
		_ = st.Close()
		return nil, err
	}
	return st, nil
}

// openStore opens the configured store without the embed-model guard.
func openStore(ctx context.Context, cfg *config.Config) (store.Store, error) {
	switch cfg.Backend {
	case config.BackendSQLite:
		return sqlitevec.Open(ctx, cfg.SQLitePath, cfg.EmbedDims)
	case config.BackendPostgres:
		return postgres.Open(ctx, cfg.PostgresDSN, cfg.EmbedDims)
	default:
		return nil, fmt.Errorf("unknown backend %q", cfg.Backend)
	}
}

// guardEmbedModel records the configured embedding model on a fresh store and,
// on an existing one, refuses to proceed when it differs from what the vectors
// were produced with. A pre-existing store with no recorded model adopts the
// current one (it is the best guess available). Stores that don't track the
// model are left untouched.
func guardEmbedModel(ctx context.Context, st store.Store, model string) error {
	ems, ok := st.(store.EmbedModelStore)
	if !ok {
		return nil
	}
	recorded, err := ems.EmbedModel(ctx)
	if err != nil {
		return err
	}
	if recorded == "" {
		return ems.SetEmbedModel(ctx, model)
	}
	if recorded != model {
		return embedModelMismatchErr(recorded, model)
	}
	return nil
}

// embedModelMismatchErr describes a recorded-vs-configured embedding model
// conflict and the three ways out (match the env, re-embed once, or opt into
// automatic re-embedding).
func embedModelMismatchErr(recorded, configured string) error {
	return fmt.Errorf("store was created with embedding model %q but is configured for %q; "+
		"vectors from different models are not comparable, so recall would silently degrade. "+
		"Set MEMINI_EMBED_MODEL=%s to match the existing data, run `memini reembed` to re-embed "+
		"every memory under %q, or set MEMINI_REEMBED_ON_MODEL_CHANGE=true to re-embed automatically "+
		"at startup", recorded, configured, recorded, configured)
}

// reconcileEmbedModel handles an embedding-model change for the long-running
// server, where an embedder is available. It adopts the configured model on a
// fresh store, and on a changed model either re-embeds every memory in place
// (when MEMINI_REEMBED_ON_MODEL_CHANGE is set) or refuses to start. Stores that
// don't track the model are left untouched.
func reconcileEmbedModel(
	ctx context.Context, st store.Store, embedder embed.Embedder, cfg *config.Config, log *slog.Logger,
) error {
	ems, ok := st.(store.EmbedModelStore)
	if !ok {
		return nil
	}
	recorded, err := ems.EmbedModel(ctx)
	if err != nil {
		return err
	}
	if recorded == "" {
		return ems.SetEmbedModel(ctx, cfg.EmbedModel)
	}
	if recorded == cfg.EmbedModel {
		return nil
	}
	if !cfg.ReembedOnModelChange {
		return embedModelMismatchErr(recorded, cfg.EmbedModel)
	}
	if cfg.EmbedBaseURL == "" {
		return fmt.Errorf("MEMINI_REEMBED_ON_MODEL_CHANGE is set and the model changed from %q to %q, "+
			"but no embeddings endpoint is configured; set MEMINI_EMBED_BASE_URL", recorded, cfg.EmbedModel)
	}
	log.Warn("embedding model changed; re-embedding every memory at startup "+
		"(MEMINI_REEMBED_ON_MODEL_CHANGE is set) — this blocks startup and calls the embeddings endpoint once per memory",
		"from", recorded, "to", cfg.EmbedModel)
	rep, err := maintenance.Reembed(ctx, st, embedder, nil, 0, nil)
	if err != nil {
		return fmt.Errorf("auto re-embed after model change: %w", err)
	}
	if err := ems.SetEmbedModel(ctx, cfg.EmbedModel); err != nil {
		return fmt.Errorf("re-embedded %d memories but recording the model failed: %w", rep.Reembedded, err)
	}
	log.Info("re-embedding complete",
		"memories", rep.Reembedded, "namespaces", rep.Namespaces, "model", cfg.EmbedModel)
	return nil
}

func buildEmbedder(cfg *config.Config, log *slog.Logger, onInFlight func(n int64)) (embed.Embedder, error) {
	if cfg.EmbedBaseURL == "" {
		log.Warn("no embeddings endpoint configured; remember/recall will error until MEMINI_EMBED_BASE_URL is set")
		return embed.Disabled{D: cfg.EmbedDims}, nil
	}
	client, err := embed.NewOpenAI(embed.OpenAIConfig{
		BaseURL: cfg.EmbedBaseURL,
		APIKey:  cfg.EmbedAPIKey,
		Model:   cfg.EmbedModel,
		Dims:    cfg.EmbedDims,
	})
	if err != nil {
		return nil, err
	}
	// Limited sits inside Batched and Cached so cache hits skip the slot.
	limited := embed.NewLimited(client, cfg.EmbedMaxConcurrency, onInFlight)
	if cfg.EmbedMaxConcurrency > 0 {
		log.Info("embed concurrency cap", "max_in_flight", cfg.EmbedMaxConcurrency)
	}
	batched := embed.NewBatched(limited, cfg.EmbedMaxBatch, cfg.EmbedMaxBatchChars, embedMaxItemChars)
	return embed.NewCached(batched, 4096)
}

// Fixed internal defaults (no env override). The benchmark harness overrides the
// retrieval knobs via service.Option; production runs these baked values.
const (
	// embedMaxItemChars truncates any single text before embedding so one
	// oversized memory can't blow the per-request budget. The per-request and
	// per-batch budgets remain configurable.
	embedMaxItemChars = 8000
	// rerankMaxDocChars truncates each document sent to the cross-encoder.
	// MEMINI_RERANK_MAX_BATCH_CHARS remains configurable.
	rerankMaxDocChars = 2048
	// Baked retrieval defaults (formerly MEMINI_RECALL_* / MEMINI_TEMPORAL_BOOST).
	defaultRecallMinScore        = 0.1
	defaultRecallSemanticReserve = 2
	defaultTemporalBoost         = 0.40
	// defaultCorroborateMinScore gates corroboration routing (short-term write →
	// confidence growth on the durable fact it restates). Above the 0.664
	// nearest-neighbour ceiling the dedup bench measured for genuinely distinct
	// memories, while low enough to catch reworded restatements (in the
	// sqlitevec 1/(1+L2) score space 0.70 ≈ cosine 0.91). The action is mild
	// (one bounded confidence step per 24h), so a rare false positive costs
	// little.
	defaultCorroborateMinScore = 0.70
)

func outerBackendLabel(e embed.Embedder) string {
	switch e.(type) {
	case *embed.Cached:
		return "cached"
	case *embed.DiskCache:
		return "diskcache"
	case *embed.Batched:
		return "batched"
	case *embed.OpenAIClient:
		return "openai"
	case embed.Disabled:
		return "disabled"
	default:
		return "wrapped"
	}
}
