package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/eleboucher/memini/internal/config"
	"github.com/eleboucher/memini/internal/maintenance"
	"github.com/eleboucher/memini/internal/store"
)

var (
	forgetTag       string
	forgetNamespace string
)

var forgetCmd = &cobra.Command{
	Use:   "forget",
	Short: "Bulk-delete memories by tag (e.g. undo a bad import)",
	Long: "Delete every memory in a namespace carrying the given tag. The import " +
		"command stamps each record with import:<source>:<date>, so a single " +
		"`memini forget --tag import:mem0:2026-06-12` undoes that import.",
	RunE: runForget,
}

func init() {
	forgetCmd.Flags().StringVar(&forgetTag, "tag", "", "delete memories carrying this exact tag (required)")
	forgetCmd.Flags().StringVar(&forgetNamespace, "namespace", "", "namespace to delete from (defaults to the resolved default)")
	_ = forgetCmd.MarkFlagRequired("tag")
	rootCmd.AddCommand(forgetCmd)
}

func runForget(cmd *cobra.Command, _ []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ns := forgetNamespace
	if ns == "" {
		ns = cfg.DefaultNamespace
	}
	return withLocalStore(cmd.Context(), func(st store.Store) error {
		n, err := maintenance.ForgetByTag(cmd.Context(), st, ns, forgetTag)
		if err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "forget: deleted %d memories tagged %q in namespace %q\n", //nolint:errcheck
			n, forgetTag, ns)
		return nil
	})
}
