package main

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/eleboucher/memini/internal/config"
	"github.com/eleboucher/memini/internal/logging"
	"github.com/eleboucher/memini/internal/maintenance"
	"github.com/eleboucher/memini/internal/store"
)

var (
	reembedNamespace string
	reembedYes       bool
	reembedBatch     int
)

var reembedCmd = &cobra.Command{
	Use:   "reembed",
	Short: "Re-embed every memory under the currently configured embedding model",
	Long: "Recompute and rewrite the vector for every stored memory using the " +
		"model named by MEMINI_EMBED_MODEL, so the store can switch embedding models " +
		"without being rebuilt from an export. Dimensionality cannot change here " +
		"(the store is fixed at its original width); to change dims, create a new " +
		"store and re-import. On success the store's recorded embedding model is " +
		"updated so the startup guard accepts the new model.",
	RunE: runReembed,
}

func init() {
	reembedCmd.Flags().StringVar(&reembedNamespace, "namespace", "",
		"limit to a single namespace (default: every namespace)")
	reembedCmd.Flags().BoolVar(&reembedYes, "yes", false,
		"apply the re-embedding (default is a dry-run report)")
	reembedCmd.Flags().IntVar(&reembedBatch, "batch", 0,
		"memories embedded per request (0 = default)")
	rootCmd.AddCommand(reembedCmd)
}

func runReembed(cmd *cobra.Command, _ []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if cfg.EmbedBaseURL == "" {
		return fmt.Errorf("no embeddings endpoint configured; set MEMINI_EMBED_BASE_URL before re-embedding")
	}

	log := logging.New(cfg.LogLevel, cfg.LogFormat)

	// openStore (not buildStore) so the embed-model guard doesn't reject the very
	// model change this command exists to perform.
	st, err := openStore(cmd.Context(), cfg)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	out := cmd.OutOrStdout()
	if from := recordedEmbedModel(cmd.Context(), st); from != "" && from != cfg.EmbedModel {
		fmt.Fprintf(out, "embedding model: %q -> %q\n", from, cfg.EmbedModel) //nolint:errcheck
	} else {
		fmt.Fprintf(out, "embedding model: %q\n", cfg.EmbedModel) //nolint:errcheck
	}

	var namespaces []string
	if reembedNamespace != "" {
		namespaces = []string{reembedNamespace}
	}

	if !reembedYes {
		n, err := countMemories(cmd.Context(), st, namespaces)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "dry-run: %d memories would be re-embedded (re-run with --yes to apply)\n", n) //nolint:errcheck
		return nil
	}

	embedder, err := buildEmbedder(cfg, log, nil)
	if err != nil {
		return err
	}

	w := cmd.ErrOrStderr()
	var onProgress func(done, total int)
	if isTerminal(w) {
		onProgress = func(done, total int) {
			fmt.Fprintf(w, "\r\033[Kre-embedding... %d/%d", done, total) //nolint:errcheck
		}
	}
	rep, err := maintenance.Reembed(cmd.Context(), st, embedder, namespaces, reembedBatch, onProgress)
	if isTerminal(w) {
		fmt.Fprint(w, "\r\033[K") //nolint:errcheck
	}
	if err != nil {
		return err
	}

	// Record the new model only after every vector was rewritten, so an
	// interrupted run leaves the guard pointing at the old (still-majority) model.
	if ems, ok := st.(store.EmbedModelStore); ok {
		if err := ems.SetEmbedModel(cmd.Context(), cfg.EmbedModel); err != nil {
			return fmt.Errorf("reembed completed but recording the model failed: %w", err)
		}
	}

	fmt.Fprintf(out, "re-embedded %d memories across %d namespaces\n", //nolint:errcheck
		rep.Reembedded, rep.Namespaces)
	return nil
}

// recordedEmbedModel returns the store's recorded model, or "" (best-effort, for display).
func recordedEmbedModel(ctx context.Context, st store.Store) string {
	ems, ok := st.(store.EmbedModelStore)
	if !ok {
		return ""
	}
	model, err := ems.EmbedModel(ctx)
	if err != nil {
		return ""
	}
	return model
}

// countMemories totals the rows the re-embed pass would touch, for the dry-run report.
func countMemories(ctx context.Context, st store.Store, namespaces []string) (int, error) {
	if len(namespaces) == 0 {
		all, err := st.ListNamespaces(ctx)
		if err != nil {
			return 0, err
		}
		namespaces = all
	}
	f := store.Filter{IncludeExpired: true, IncludeSuperseded: true}
	var n int
	for _, ns := range namespaces {
		mems, err := st.List(ctx, ns, f, 0)
		if err != nil {
			return 0, err
		}
		n += len(mems)
	}
	return n, nil
}
