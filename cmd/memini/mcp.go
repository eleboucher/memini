package main

import (
	"errors"
	"os/signal"
	"strings"
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
	// Same fatal guard as runServer, for the same reason: `memini mcp` builds
	// the identical service stack and runs as a persistent server (the
	// standard plugin deployment mode), so a stale deleted scope-model knob
	// must refuse the boot here too — not slip through on exactly the path
	// most users run. See config.FatalDeprecatedVars for the rationale and
	// the migrate-scopes exemption.
	if fatal := config.FatalDeprecatedVars(); len(fatal) > 0 {
		return errors.New(strings.Join(fatal, "\n"))
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	log := logging.New(cfg.LogLevel, cfg.LogFormat)
	log.Info("starting memini mcp",
		"version", version.String(),
		"backend", cfg.Backend,
		"default_namespace", cfg.DefaultNamespace,
		"home", cfg.Home,
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
	return mcpapi.RunStdio(ctx, svc, cfg.DefaultNamespace, cfg.Home)
}
