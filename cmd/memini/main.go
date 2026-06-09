package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/prometheus/client_golang/prometheus"

	mcpapi "github.com/eleboucher/memini/internal/api/mcp"
	"github.com/eleboucher/memini/internal/api/rest"
	"github.com/eleboucher/memini/internal/config"
	"github.com/eleboucher/memini/internal/embed"
	"github.com/eleboucher/memini/internal/llm"
	"github.com/eleboucher/memini/internal/logging"
	"github.com/eleboucher/memini/internal/maintenance"
	"github.com/eleboucher/memini/internal/server"
	"github.com/eleboucher/memini/internal/service"
	"github.com/eleboucher/memini/internal/store"
	"github.com/eleboucher/memini/internal/store/postgres"
	"github.com/eleboucher/memini/internal/store/sqlitevec"
	"github.com/eleboucher/memini/internal/version"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func run() error {
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

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// `memini import` bulk-loads an export from another memory system, into the
	// local store or a remote server (--remote). Handled before opening the
	// local store so remote imports need no local backend.
	if len(os.Args) > 1 && os.Args[1] == "import" {
		return runImport(ctx, cfg, log, os.Args[2:])
	}

	st, err := buildStore(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	embedder, err := buildEmbedder(cfg, log)
	if err != nil {
		return err
	}

	var svcOpts []service.Option
	if cfg.LLMEnabled() {
		consolidator, err := llm.New(llm.API(cfg.LLMAPI), llm.Config{
			BaseURL: cfg.LLMBaseURL, APIKey: cfg.LLMAPIKey, Model: cfg.LLMModel,
		})
		if err != nil {
			return err
		}
		svcOpts = append(svcOpts, service.WithConsolidator(consolidator))
		log.Info("LLM consolidation enabled", "api", cfg.LLMAPI, "model", cfg.LLMModel)
	}
	svcOpts = append(svcOpts, service.WithShortTermCap(cfg.ShortTermCap))
	svc := service.New(st, embedder, svcOpts...)

	// `memini mcp` serves MCP tools over stdio.
	if len(os.Args) > 1 && os.Args[1] == "mcp" {
		log.Info("serving MCP over stdio")
		return mcpapi.RunStdio(ctx, svc, cfg.DefaultNamespace)
	}

	// Background decay sweeper purges expired memories.
	go maintenance.NewSweeper(st, log, cfg.SweepInterval, cfg.ShortTermCap).Run(ctx)

	reg := prometheus.NewRegistry()
	srv := server.New(server.Options{
		Addr:            cfg.HTTPAddr,
		ShutdownTimeout: cfg.ShutdownTimeout,
		APIKey:          cfg.APIKey,
	}, log, reg)
	srv.SetReady(func(ctx context.Context) error { return st.Ping(ctx) })

	rest.New(svc, rest.AuthConfig{
		APIKey:           cfg.APIKey,
		NamespaceHeader:  cfg.NamespaceHeader,
		DefaultNamespace: cfg.DefaultNamespace,
	}).Mount(srv.Router())

	// MCP over Streamable HTTP for remote agents.
	mcpHandler := mcpapi.HTTPHandler(svc, cfg.NamespaceHeader, cfg.DefaultNamespace, cfg.APIKey)
	srv.Router().Handle("/mcp", mcpHandler)
	srv.Router().Handle("/mcp/*", mcpHandler)

	return srv.Run(ctx)
}

// buildStore constructs the configured storage backend.
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

// buildEmbedder returns the embeddings client, or a Disabled stub when no
// endpoint is configured.
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
	return embed.NewCached(client, 4096)
}
