package main

import (
	"os/signal"
	"syscall"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/spf13/cobra"

	mcpapi "github.com/eleboucher/memini/internal/api/mcp"
	"github.com/eleboucher/memini/internal/config"
	"github.com/eleboucher/memini/internal/logging"
	"github.com/eleboucher/memini/internal/version"
)

var mcpCmd = &cobra.Command{
	Use:   "mcp",
	Short: "Serve MCP tools over stdio",
	RunE:  runMCP,
}

func init() {
	rootCmd.AddCommand(mcpCmd)
}

func runMCP(cmd *cobra.Command, _ []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	log := logging.New(cfg.LogLevel, cfg.LogFormat)
	log.Info("starting memini mcp",
		"version", version.String(),
		"backend", cfg.Backend,
		"default_namespace", cfg.DefaultNamespace,
	)

	ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	reg := prometheus.NewRegistry()
	svc, _, _, joinWorkers, cleanup, err := buildServiceStack(ctx, cfg, log, reg)
	if err != nil {
		return err
	}
	defer cleanup()
	defer joinWorkers()

	log.Info("serving MCP over stdio")
	return mcpapi.RunStdio(ctx, svc, cfg.DefaultNamespace)
}
