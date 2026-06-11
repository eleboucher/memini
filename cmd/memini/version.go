package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/eleboucher/memini/internal/version"
)

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print version information",
	Run: func(cmd *cobra.Command, _ []string) {
		fmt.Fprintln(cmd.OutOrStdout(), version.String()) //nolint:errcheck
	},
}

func init() {
	rootCmd.AddCommand(versionCmd)
}
