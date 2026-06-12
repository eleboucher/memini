package main

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/eleboucher/memini/internal/maintenance"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/store"
)

var backfillYes bool

var backfillCmd = &cobra.Command{
	Use:   "backfill",
	Short: "Seed nil confidence on legacy durable memories (pre-0.0.11 recovery)",
	Long: "Walk every namespace and seed ConfidenceSeedImported on durable " +
		"(semantic / procedural) memories written before the confidence field " +
		"existed, so they enter the decay + demotion lifecycle. Idempotent.",
	RunE: runBackfill,
}

func init() {
	backfillCmd.Flags().BoolVar(&backfillYes, "yes", false,
		"apply the backfill (default is a dry-run report)")
	rootCmd.AddCommand(backfillCmd)
}

func runBackfill(cmd *cobra.Command, _ []string) error {
	return withLocalStore(cmd.Context(), func(st store.Store) error {
		var (
			rep maintenance.BackfillConfidenceReport
			err error
		)
		now := time.Now().UTC()
		if backfillYes {
			rep, err = maintenance.BackfillConfidence(cmd.Context(), st, now)
		} else {
			rep, err = maintenance.BackfillConfidencePreview(cmd.Context(), st, now)
		}
		if err != nil {
			return err
		}
		mode := "preview (re-run with --yes to apply)"
		if backfillYes {
			mode = "applied"
		}
		out := cmd.OutOrStdout()
		fmt.Fprintf(out, "backfill confidence: %s\n", mode)                   //nolint:errcheck
		fmt.Fprintf(out, "  inspected: %d durable memories\n", rep.Inspected) //nolint:errcheck
		fmt.Fprintf(out, "  seeded:    %d (confidence was nil, now %.2f)\n",  //nolint:errcheck
			rep.Seeded, memory.ConfidenceSeedImported)
		fmt.Fprintf(out, "  skipped:   %d (already had a confidence)\n", rep.Skipped) //nolint:errcheck
		return nil
	})
}
