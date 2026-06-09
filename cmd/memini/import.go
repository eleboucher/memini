package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/eleboucher/memini/internal/config"
	"github.com/eleboucher/memini/internal/importer"
)

// runImport handles `memini import [flags] <file>`: it loads an export from
// another memory system (or memini's own format) and bulk-loads it either into
// the locally-configured store or — with --remote — into a running memini
// server over its REST API. Reads stdin when the path is "-" or omitted.
func runImport(ctx context.Context, cfg *config.Config, log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	source := fs.String("source", "memini",
		"export format: "+strings.Join(importer.Sources(), "|"))
	namespace := fs.String("namespace", cfg.DefaultNamespace,
		"namespace for records whose source carried none")
	batch := fs.Int("batch", 0, "records per batch (0 = backend default)")
	remote := fs.String("remote", "",
		"target a running memini server (e.g. https://memini.example.com) instead of the local store")
	token := fs.String("token", cfg.APIKey,
		"bearer token for the remote server (defaults to MEMINI_API_KEY)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	data, err := readInput(fs.Arg(0))
	if err != nil {
		return err
	}
	recs, err := importer.Parse(importer.Source(*source), data)
	if err != nil {
		return err
	}

	im, target, closeFn, err := buildImporter(ctx, cfg, log, *remote, *token)
	if err != nil {
		return err
	}
	defer closeFn()

	rep, err := im.Import(ctx, recs, importer.Options{
		DefaultNamespace: *namespace,
		BatchSize:        *batch,
	})
	fmt.Printf("import %s -> %s: %d imported, %d skipped, %d total\n",
		*source, target, rep.Imported, rep.Skipped, rep.Total)
	for _, e := range rep.Errors {
		fmt.Fprintln(os.Stderr, "  error:", e)
	}
	return err
}

// buildImporter wires a remote (REST) or local (store+embedder) importer,
// returning a human label for the target and a cleanup func.
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

// readInput reads the named file, or stdin when path is empty or "-".
func readInput(path string) ([]byte, error) {
	if path == "" || path == "-" {
		return io.ReadAll(os.Stdin)
	}
	return os.ReadFile(path)
}
