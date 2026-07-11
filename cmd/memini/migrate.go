package main

import (
	"context"
	"fmt"
	"io"
	"sort"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/eleboucher/memini/internal/config"
	"github.com/eleboucher/memini/internal/logging"
	"github.com/eleboucher/memini/internal/maintenance"
	"github.com/eleboucher/memini/internal/store"
)

// migrateScopesYes gates whether `memini migrate scopes` applies its changes.
// Mirrors reembed's confirm pattern (dry-run report by default, --yes to
// apply) rather than namespace move's bare --dry-run flag: like reembed,
// this command both moves data AND tombstones records (the post-merge dedup
// pass), so it gets the stronger, opt-in-to-write default.
var migrateScopesYes bool

var migrateCmd = &cobra.Command{
	Use:   "migrate",
	Short: "One-shot data migrations between memini scope models",
}

var migrateScopesCmd = &cobra.Command{
	Use:   "scopes",
	Short: "Merge <tenant>/_shared namespaces into <tenant> (old shared-scope model -> ancestor cascade)",
	Long: "The old scope model merged a `<tenant>/_shared` namespace read-only into every " +
		"sibling under <tenant> (and, separately, a MEMINI_GLOBAL_NAMESPACE merged into every " +
		"namespace store-wide). The new model has interior namespaces AS the shared layer via " +
		"the ancestor cascade, so this command folds the old data forward: every namespace " +
		"literally named \"<prefix>/_shared\" is moved into \"<prefix>\" (link endpoints follow " +
		"along), then a dedup pass runs against the target to collapse any facts the merge just " +
		"duplicated. A bare \"_shared\" (no prefix) has no parent to merge into and is left " +
		"alone, reported as a note. Idempotent: once no \"<prefix>/_shared\" namespace holds any " +
		"memories, re-running finds nothing to do.\n\n" +
		"Defaults to a dry-run report; pass --yes to apply.",
	RunE: runMigrateScopes,
}

func init() {
	migrateScopesCmd.Flags().BoolVar(&migrateScopesYes, "yes", false,
		"apply the migration, including the post-merge dedup pass (default is a dry-run report)")
	migrateCmd.AddCommand(migrateScopesCmd)
	rootCmd.AddCommand(migrateCmd)
}

func runMigrateScopes(cmd *cobra.Command, _ []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	dryRun := !migrateScopesYes
	// The post-merge dedup pass (gap G14) is mandatory whenever data actually
	// moves, so an apply run needs a working embedder up front: fail before
	// touching anything rather than moving data and then erroring mid-dedup.
	if !dryRun && cfg.EmbedBaseURL == "" {
		return fmt.Errorf("no embeddings endpoint configured; set MEMINI_EMBED_BASE_URL before running " +
			"migrate scopes with --yes (the post-merge dedup pass needs it)")
	}

	log := logging.New(cfg.LogLevel, cfg.LogFormat)

	return withLocalStore(cmd.Context(), func(st store.Store) error {
		opts := maintenance.ScopesOptions{DryRun: dryRun}
		if !dryRun {
			embedder, err := buildEmbedder(cfg, log, nil)
			if err != nil {
				return err
			}
			opts.Embedder = embedder
		}
		return migrateScopesOn(cmd.Context(), cmd.OutOrStdout(), st, opts)
	})
}

// migrateScopesOn runs MigrateScopes against st and prints the report. On a
// mid-migration error the partial report still prints first — merges already
// committed by then must reach the operator (an aborted 3rd of 5 merges would
// otherwise leave no record of the 2 that completed) — then the error is
// returned for the usual non-zero exit.
func migrateScopesOn(ctx context.Context, out io.Writer, st store.Store, opts maintenance.ScopesOptions) error {
	rep, err := maintenance.MigrateScopes(ctx, st, opts)
	if err != nil {
		if len(rep.Merges) > 0 || len(rep.BareShared) > 0 || rep.GlobalNamespaceEnv != "" {
			printScopesReport(out, rep)
		}
		fmt.Fprintf(out, "migration stopped early: the merges above (if any) have committed; "+ //nolint:errcheck
			"fix the error and re-run to continue (the command is idempotent)\n")
		return err
	}
	printScopesReport(out, rep)
	return nil
}

func printScopesReport(out io.Writer, rep maintenance.ScopesReport) {
	if rep.GlobalNamespaceEnv != "" {
		fmt.Fprintf(out, "MEMINI_GLOBAL_NAMESPACE=%q is set; it is a dead knob under the ancestor/home/link "+ //nolint:errcheck
			"cascade and this command does NOT rewrite it. Adopt manually:\n", rep.GlobalNamespaceEnv)
		fmt.Fprintf(out, "  single-operator: set MEMINI_HOME=%q on clients that read this global\n", //nolint:errcheck
			rep.GlobalNamespaceEnv)
		fmt.Fprintf(out, "  team-wide: for each consumer namespace <ns>, run: "+ //nolint:errcheck
			"memini link add <ns> %q\n\n", rep.GlobalNamespaceEnv)
	}

	if len(rep.Merges) == 0 && len(rep.BareShared) == 0 {
		fmt.Fprintln(out, "nothing to do: no <tenant>/_shared namespaces found") //nolint:errcheck
		return
	}

	verb := "migrate scopes"
	if rep.DryRun {
		verb += " (dry-run)"
	}
	fmt.Fprintf(out, "%s: %d merge(s)\n", verb, len(rep.Merges)) //nolint:errcheck
	if len(rep.Merges) > 0 {
		tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "  FROM\tTO\tMOVED\tDEDUP CLUSTERS\tDEDUP TOMBSTONED") //nolint:errcheck
		for _, m := range rep.Merges {
			fmt.Fprintf(tw, "  %s\t%s\t%d\t%d\t%d\n", //nolint:errcheck
				m.From, m.To, m.Moved, m.DedupClusters, m.DedupTombstoned)
		}
		_ = tw.Flush()
	}

	if len(rep.BareShared) > 0 {
		sort.Strings(rep.BareShared)
		for _, ns := range rep.BareShared {
			fmt.Fprintf(out, "note: namespace %q has no parent to merge into; left untouched "+ //nolint:errcheck
				"(it's likely meant to be someone's home namespace, or a link source/target).\n", ns)
		}
	}
}
