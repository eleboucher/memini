package main

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/eleboucher/memini/internal/config"
	"github.com/eleboucher/memini/internal/maintenance"
	"github.com/eleboucher/memini/internal/service"
	"github.com/eleboucher/memini/internal/store"
)

var (
	nsFrom      string
	nsTo        string
	nsByCSV     string
	nsDryRun    bool
	nsLinkTiers string
	nsNamespace string
)

var namespaceCmd = &cobra.Command{
	Use:   "namespace",
	Short: "Inspect and repair memory namespaces (recovery from botched imports / shared pools)",
}

var namespaceListCmd = &cobra.Command{
	Use:   "list",
	Short: "List namespaces and their memory counts",
	RunE:  runNamespaceList,
}

var namespaceMoveCmd = &cobra.Command{
	Use:   "move",
	Short: "Move every memory from one namespace to another",
	RunE:  runNamespaceMove,
}

var namespaceSplitCmd = &cobra.Command{
	Use:   "split",
	Short: "Regroup a pooled namespace back into per-tenant namespaces by metadata",
	Long: "Regroup a namespace by metadata, moving each memory to the namespace named by " +
		"the first grouping key it carries (default: " + strings.Join(maintenance.DefaultSplitKeys, ", ") + "). " +
		"Memories with no grouping key stay put. This recovers a store whose imports collapsed into one pool.",
	RunE: runNamespaceSplit,
}

var namespaceLinkCmd = &cobra.Command{
	Use:   "link",
	Short: "Attach a read-only namespace link (reads only — writes always land in the request namespace)",
	Long: "Reads in --from's default read set (recall/briefing without an explicit per-call " +
		"namespaces list) will also see --to. 1-hop only: --to's own links are never followed, " +
		"so linking A to B does not also expose whatever B links to. Idempotent: running it again " +
		"for the same --from/--to with a different --tiers overwrites rather than duplicating.",
	RunE: runNamespaceLink,
}

var namespaceUnlinkCmd = &cobra.Command{
	Use:   "unlink",
	Short: "Remove a namespace link",
	RunE:  runNamespaceUnlink,
}

var namespaceLinksCmd = &cobra.Command{
	Use:   "links",
	Short: "List namespace links (every namespace, or one with --namespace)",
	RunE:  runNamespaceLinks,
}

func init() {
	namespaceMoveCmd.Flags().StringVar(&nsFrom, "from", "", "source namespace (required)")
	namespaceMoveCmd.Flags().StringVar(&nsTo, "to", "", "destination namespace (required)")
	namespaceMoveCmd.Flags().BoolVar(&nsDryRun, "dry-run", false, "report what would move without writing")
	_ = namespaceMoveCmd.MarkFlagRequired("from")
	_ = namespaceMoveCmd.MarkFlagRequired("to")

	namespaceSplitCmd.Flags().StringVar(&nsFrom, "from", "", "namespace to split (required)")
	namespaceSplitCmd.Flags().StringVar(&nsByCSV, "by", "",
		"comma-separated metadata keys to group by (default: "+strings.Join(maintenance.DefaultSplitKeys, ",")+")")
	namespaceSplitCmd.Flags().BoolVar(&nsDryRun, "dry-run", false, "report the split without writing")
	_ = namespaceSplitCmd.MarkFlagRequired("from")

	namespaceLinkCmd.Flags().StringVar(&nsFrom, "from", "", "namespace whose read set expands (required)")
	namespaceLinkCmd.Flags().StringVar(&nsTo, "to", "",
		"namespace to attach read-only; \"ns/*\" also includes namespaces nested under ns (required)")
	namespaceLinkCmd.Flags().StringVar(&nsLinkTiers, "tiers", "durable",
		`"durable" (default: semantic+procedural only) or "all"`)
	_ = namespaceLinkCmd.MarkFlagRequired("from")
	_ = namespaceLinkCmd.MarkFlagRequired("to")

	namespaceUnlinkCmd.Flags().StringVar(&nsFrom, "from", "", "namespace the link is removed from (required)")
	namespaceUnlinkCmd.Flags().StringVar(&nsTo, "to", "", "linked namespace to detach (required)")
	_ = namespaceUnlinkCmd.MarkFlagRequired("from")
	_ = namespaceUnlinkCmd.MarkFlagRequired("to")

	namespaceLinksCmd.Flags().StringVar(&nsNamespace, "namespace", "", "show only this namespace's links (default: every namespace)")

	namespaceCmd.AddCommand(namespaceListCmd, namespaceMoveCmd, namespaceSplitCmd,
		namespaceLinkCmd, namespaceUnlinkCmd, namespaceLinksCmd)
	rootCmd.AddCommand(namespaceCmd)
}

// withLocalStore opens the local store, runs fn, and closes it. Namespace
// repair operates on the local store directly (not via a remote server).
func withLocalStore(ctx context.Context, fn func(st store.Store) error) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	st, err := buildStore(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	return fn(st)
}

func runNamespaceList(cmd *cobra.Command, _ []string) error {
	return withLocalStore(cmd.Context(), func(st store.Store) error {
		names, err := st.ListNamespaces(cmd.Context())
		if err != nil {
			return err
		}
		sort.Strings(names)
		out := cmd.OutOrStdout()
		if len(names) == 0 {
			fmt.Fprintln(out, "no namespaces") //nolint:errcheck
			return nil
		}
		for _, ns := range names {
			mems, err := st.List(cmd.Context(), ns, store.Filter{IncludeSuperseded: true, IncludeExpired: true}, 0)
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "%-40s %d\n", ns, len(mems)) //nolint:errcheck
		}
		return nil
	})
}

func runNamespaceMove(cmd *cobra.Command, _ []string) error {
	return withLocalStore(cmd.Context(), func(st store.Store) error {
		rep, err := maintenance.Move(cmd.Context(), st, nsFrom, nsTo, nsDryRun)
		if err != nil {
			return err
		}
		printRenamespace(cmd, "move", rep)
		return nil
	})
}

func runNamespaceSplit(cmd *cobra.Command, _ []string) error {
	var keys []string
	if nsByCSV != "" {
		for k := range strings.SplitSeq(nsByCSV, ",") {
			if k = strings.TrimSpace(k); k != "" {
				keys = append(keys, k)
			}
		}
	}
	return withLocalStore(cmd.Context(), func(st store.Store) error {
		rep, err := maintenance.Split(cmd.Context(), st, nsFrom, keys, nsDryRun)
		if err != nil {
			return err
		}
		printRenamespace(cmd, "split", rep)
		return nil
	})
}

// linkService builds a bare service.Service over the local store for the
// validated link CRUD wrappers (self-link rejection, tiers validation, the
// per-namespace cap) — the same validation REST/MCP use, so the CLI doesn't
// re-implement it. No embedder is needed: LinkNamespaces/UnlinkNamespaces/
// NamespaceLinks never touch it.
func linkService(st store.Store) *service.Service {
	return service.New(st, nil)
}

func runNamespaceLink(cmd *cobra.Command, _ []string) error {
	return withLocalStore(cmd.Context(), func(st store.Store) error {
		svc := linkService(st)
		if err := svc.LinkNamespaces(cmd.Context(), nsFrom, nsTo, nsLinkTiers); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "linked %q -> %q (tiers=%s)\n", nsFrom, nsTo, nsLinkTiers) //nolint:errcheck
		return nil
	})
}

func runNamespaceUnlink(cmd *cobra.Command, _ []string) error {
	return withLocalStore(cmd.Context(), func(st store.Store) error {
		svc := linkService(st)
		if err := svc.UnlinkNamespaces(cmd.Context(), nsFrom, nsTo); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "unlinked %q -> %q\n", nsFrom, nsTo) //nolint:errcheck
		return nil
	})
}

func runNamespaceLinks(cmd *cobra.Command, _ []string) error {
	return withLocalStore(cmd.Context(), func(st store.Store) error {
		ls, ok := st.(store.LinkStore)
		if !ok {
			return fmt.Errorf("namespace links are not supported by this backend")
		}
		var links []store.NamespaceLink
		var err error
		if nsNamespace != "" {
			links, err = ls.ListNamespaceLinks(cmd.Context(), nsNamespace)
		} else {
			links, err = ls.ListAllNamespaceLinks(cmd.Context())
		}
		if err != nil {
			return err
		}
		out := cmd.OutOrStdout()
		if len(links) == 0 {
			fmt.Fprintln(out, "no namespace links") //nolint:errcheck
			return nil
		}
		for _, l := range links {
			if nsNamespace != "" {
				fmt.Fprintf(out, "%-30s tiers=%-8s created=%s\n", l.Target, l.Tiers, //nolint:errcheck
					l.CreatedAt.Format("2006-01-02T15:04:05Z"))
			} else {
				fmt.Fprintf(out, "%-30s -> %-30s tiers=%-8s created=%s\n", l.Namespace, l.Target, l.Tiers, //nolint:errcheck
					l.CreatedAt.Format("2006-01-02T15:04:05Z"))
			}
		}
		return nil
	})
}

func printRenamespace(cmd *cobra.Command, verb string, rep maintenance.RenamespaceReport) {
	out := cmd.OutOrStdout()
	if rep.DryRun {
		verb += " (dry-run)"
	}
	fmt.Fprintf(out, "%s: %d moved, %d left in place\n", verb, rep.Moved, rep.Skipped) //nolint:errcheck
	targets := make([]string, 0, len(rep.Targets))
	for ns := range rep.Targets {
		targets = append(targets, ns)
	}
	sort.Strings(targets)
	for _, ns := range targets {
		fmt.Fprintf(out, "  -> %q: %d\n", ns, rep.Targets[ns]) //nolint:errcheck
	}
}
