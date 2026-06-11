package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/eleboucher/memini/internal/config"
	"github.com/eleboucher/memini/internal/importer"
	"github.com/eleboucher/memini/internal/logging"
)

var (
	importSource    string
	importNamespace string
	importBatch     int
	importRemote    string
	importToken     string
)

var importCmd = &cobra.Command{
	Use:   "import [file]",
	Short: "Bulk-load an export from another memory system",
	Long: "Bulk-load an export from another memory system (or memini's own format) " +
		"into the local store or a remote server (--remote). Reads stdin when the path is \"-\" or omitted.",
	Args: cobra.MaximumNArgs(1),
	RunE: runImport,
}

func init() {
	importCmd.Flags().StringVar(&importSource, "source", "memini",
		"export format: "+strings.Join(importer.Sources(), "|"))
	importCmd.Flags().StringVar(&importNamespace, "namespace", "",
		"namespace for records whose source carried none")
	importCmd.Flags().IntVar(&importBatch, "batch", 0,
		"records per batch (0 = backend default)")
	importCmd.Flags().StringVar(&importRemote, "remote", "",
		"target a running memini server (e.g. https://memini.example.com) instead of the local store")
	importCmd.Flags().StringVar(&importToken, "token", "",
		"bearer token for the remote server (defaults to MEMINI_API_KEY)")

	rootCmd.AddCommand(importCmd)
}

func runImport(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	log := logging.New(cfg.LogLevel, cfg.LogFormat)

	if importNamespace == "" {
		importNamespace = cfg.DefaultNamespace
	}
	if importToken == "" {
		importToken = cfg.APIKey
	}

	var path string
	if len(args) > 0 {
		path = args[0]
	}

	w := cmd.ErrOrStderr()
	isTerm := isTerminal(w)

	if isTerm {
		fmt.Fprint(w, "loading records...") //nolint:errcheck
	}
	recs, err := loadRecords(importer.Source(importSource), path)
	if err != nil {
		return err
	}
	if isTerm {
		fmt.Fprintf(w, "\r\033[Kloaded %d records\n", len(recs)) //nolint:errcheck
	}
	if cmd.Flags().Changed("namespace") {
		for i := range recs {
			recs[i].Namespace = ""
		}
	}

	im, target, closeFn, err := buildImporter(cmd.Context(), cfg, log, importRemote, importToken)
	if err != nil {
		return err
	}
	defer closeFn()

	rep, err := im.Import(cmd.Context(), recs, importer.Options{
		DefaultNamespace: importNamespace,
		BatchSize:        importBatch,
		OnProgress:       newProgressWriter(w),
	})
	fmt.Fprintf(cmd.OutOrStdout(), "import %s -> %s: %d imported, %d skipped, %d total\n", //nolint:errcheck
		importSource, target, rep.Imported, rep.Skipped, rep.Total)
	for _, e := range rep.Errors {
		fmt.Fprintln(cmd.ErrOrStderr(), "  error:", e) //nolint:errcheck
	}
	if err != nil {
		return err
	}
	if len(rep.Errors) > 0 {
		return fmt.Errorf("import completed with %d errors", len(rep.Errors))
	}
	return nil
}

func buildImporter(
	ctx context.Context, cfg *config.Config, log *slog.Logger, remote, token string,
) (*importer.Importer, string, func(), error) {
	if remote != "" {
		client := importer.NewRemoteClient(remote, token, cfg.NamespaceHeader)
		return importer.NewRemote(client), remote, func() {}, nil
	}
	st, err := buildStore(ctx, cfg)
	if err != nil {
		return nil, "", nil, err
	}
	embedder, err := buildEmbedder(cfg, log)
	if err != nil {
		_ = st.Close()
		return nil, "", nil, err
	}
	return importer.NewLocal(st, embedder), "local store", func() { _ = st.Close() }, nil
}

func readInput(path string) ([]byte, error) {
	if path == "" || path == "-" {
		return io.ReadAll(os.Stdin)
	}
	return os.ReadFile(path)
}

func loadRecords(src importer.Source, path string) ([]importer.Record, error) {
	if src == importer.SourceClaudeCode && path != "" && path != "-" {
		if info, err := os.Stat(path); err == nil && info.IsDir() {
			recs, warns, err := importer.LoadClaudeCode(path)
			for _, w := range warns {
				fmt.Fprintln(os.Stderr, "  warning:", w)
			}
			return recs, err
		}
	}
	data, err := readInput(path)
	if err != nil {
		return nil, err
	}
	return importer.Parse(src, data)
}
