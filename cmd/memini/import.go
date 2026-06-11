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

	recs, err := loadRecords(importer.Source(*source), fs.Arg(0))
	if err != nil {
		return err
	}
	// For claude-code, each transcript's namespace is derived from its cwd. Honor
	// an explicit -namespace by blanking those so the run default takes over.
	if explicitlySet(fs, "namespace") {
		for i := range recs {
			recs[i].Namespace = ""
		}
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
	if err != nil {
		return err
	}
	// Per-record failures must fail the command too, or scripted imports
	// conclude success while records were dropped.
	if len(rep.Errors) > 0 {
		return fmt.Errorf("import completed with %d errors", len(rep.Errors))
	}
	return nil
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

// loadRecords parses an export into Records. The claude-code source accepts a
// directory (a project dir or ~/.claude/projects) and walks it for transcripts;
// every other source reads a single file or stdin and parses its bytes.
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

// explicitlySet reports whether the named flag was passed on the command line
// (as opposed to taking its default).
func explicitlySet(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	return set
}
