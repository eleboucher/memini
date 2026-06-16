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
		NamespaceHeader:  cfg.NamespaceHeader,
		DefaultNamespace: cfg.DefaultNamespace,
	}).Mount(srv.Router())

	mcpHandler := mcpapi.HTTPHandler(svc, cfg.NamespaceHeader, cfg.DefaultNamespace, cfg.APIKey)
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
	st, err := buildStore(ctx, cfg)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	embedder, err := buildEmbedder(cfg, log)
	if err != nil {
		_ = st.Close()
		return nil, nil, nil, nil, err
	}

	metricsImpl := newConsolidateMetrics(reg)

	if sm, ok := st.(interface{ SetMetrics(store.Metrics) }); ok {
		sm.SetMetrics(metricsImpl)
	}
	if em, ok := embedder.(interface{ SetMetrics(embed.Metrics) }); ok {
		em.SetMetrics(metricsImpl)
	}
	embedder = embed.Instrument(embedder, outerBackendLabel(embedder), metricsImpl)

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
		svcOpts = append(svcOpts, service.WithConsolidator(client), service.WithAnswerer(client))
		log.Info("LLM consolidation + answering enabled",
			"api", cfg.LLMAPI, "model", cfg.LLMModel, "mode", cfg.ConsolidateMode)
		if cfg.PromoteInterval > 0 {
			svcOpts = append(svcOpts,
				service.WithDistiller(client),
				service.WithPromoteMinAccess(cfg.PromoteMinAccess))
			log.Info("episodic→semantic promotion enabled",
				"interval", cfg.PromoteInterval, "min_access", cfg.PromoteMinAccess)
		}
	}
	if cfg.RerankEnabled() {
		reranker, name, err := buildReranker(cfg, chatClient, log)
		if err != nil {
			_ = st.Close()
			return nil, nil, nil, nil, err
		}
		if reranker != nil {
			svcOpts = append(svcOpts,
				service.WithReranker(reranker, name, cfg.RerankTopN),
				service.WithRerankTimeout(cfg.RerankTimeout),
			)
		}
	}
	svcOpts = append(svcOpts,
		service.WithShortTermCap(cfg.ShortTermCap),
		service.WithConsolidateMode(service.ConsolidateMode(cfg.ConsolidateMode)),
		service.WithConsolidateMinScore(cfg.ConsolidateMinScore),
		service.WithConsolidateQueueCap(cfg.ConsolidateQueueCap),
		service.WithQueryPrefix(cfg.EmbedQueryPrefix),
		service.WithScoreFusion(cfg.FusionAlpha),
		service.WithWriteDedup(cfg.WriteDedupMinScore),
		service.WithFingerprintDedup(cfg.WriteDedupFingerprint),
		service.WithSecretRedaction(cfg.RedactSecrets),
		service.WithTemporalTargeting(cfg.TemporalBoost, search.RegexAnchorExtractor{}),
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
			Similarity:          cfg.DedupSimilarity,
			MinClusterSize:      cfg.DedupMinClusterSize,
			NeighboursPerAnchor: cfg.DedupNeighboursAnchor,
			Tiers:               cfg.DedupTierList(),
		})
		workers.Go(func() { dedupJob.Run(workerCtx) })
		log.Info("periodic dedup enabled",
			"interval", cfg.DedupInterval,
			"similarity", cfg.DedupSimilarity,
			"min_cluster_size", cfg.DedupMinClusterSize)
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

func buildReranker(cfg *config.Config, chat llm.Client, log *slog.Logger) (rerank.Reranker, string, error) {
	if cfg.RerankIsLLM() {
		if chat == nil {
			log.Warn("MEMINI_RERANK=llm but no LLM is configured; set MEMINI_LLM_BASE_URL")
			return nil, "", nil
		}
		log.Info("LLM recall reranking enabled (adds one LLM call per recall)",
			"model", cfg.LLMModel, "top_n", cfg.RerankTopN)
		return rerank.NewLLM(chat), "llm", nil
	}
	ce, err := rerank.New(rerank.Config{
		BaseURL:     cfg.Rerank,
		Model:       cfg.RerankModel,
		APIKey:      cfg.RerankAPIKey,
		MaxDocChars: cfg.RerankMaxDocChars,
	})
	if err != nil {
		return nil, "", err
	}
	log.Info("cross-encoder recall reranking enabled (adds one reranker call per recall)",
		"base_url", cfg.Rerank, "model", cfg.RerankModel, "top_n", cfg.RerankTopN)
	return ce, "cross_encoder", nil
}

func buildStore(ctx context.Context, cfg *config.Config) (store.Store, error) {
	switch cfg.Backend {
	case config.BackendSQLite:
		return sqlitevec.Open(ctx, cfg.SQLitePath, cfg.EmbedDims)
	case config.BackendPostgres:
		return postgres.Open(ctx, cfg.PostgresDSN, cfg.EmbedDims)
	default:
		return nil, fmt.Errorf("unknown backend %q", cfg.Backend)
	}
}

func buildEmbedder(cfg *config.Config, log *slog.Logger) (embed.Embedder, error) {
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
	batched := embed.NewBatched(client, cfg.EmbedMaxBatch, cfg.EmbedMaxBatchChars, cfg.EmbedMaxItemChars)
	return embed.NewCached(batched, 4096)
}

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
