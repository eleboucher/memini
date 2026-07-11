package main

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/eleboucher/memini/internal/httputil"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/store"
)

var (
	linkTiersCSV string
	linkNote     string
)

var linkCmd = &cobra.Command{
	Use:   "link",
	Short: "Manage cross-namespace read links (durable-tier recall from another namespace)",
}

var linkAddCmd = &cobra.Command{
	Use:   "add <src> <dst>",
	Short: "Create or replace a read link from src to dst",
	Long: "Create or replace a read link: recall scoped to src additionally reads durable " +
		"(semantic/procedural) memories from dst. Re-running with the same src/dst replaces " +
		"the tiers/note in place (upsert), never duplicates. dst need not exist yet — " +
		"namespaces exist implicitly.",
	Args: cobra.ExactArgs(2),
	RunE: runLinkAdd,
}

var linkRmCmd = &cobra.Command{
	Use:   "rm <src> <dst>",
	Short: "Remove a read link",
	Args:  cobra.ExactArgs(2),
	RunE:  runLinkRm,
}

var linkLsCmd = &cobra.Command{
	Use:   "ls [src]",
	Short: "List read links (every link in the store when src is omitted)",
	Args:  cobra.MaximumNArgs(1),
	RunE:  runLinkLs,
}

func init() {
	linkAddCmd.Flags().StringVar(&linkTiersCSV, "tiers", "",
		"comma-separated tiers this link admits, e.g. semantic,procedural (default: the service's durable default)")
	linkAddCmd.Flags().StringVar(&linkNote, "note", "", "free-text note explaining why the link exists")

	linkCmd.AddCommand(linkAddCmd, linkRmCmd, linkLsCmd)
	rootCmd.AddCommand(linkCmd)
}

// linkStoreOf type-asserts st to store.LinkStore, returning a clear error
// when the configured backend predates namespace links (store.LinkStore is
// an optional capability interface — see its doc comment).
func linkStoreOf(st store.Store) (store.LinkStore, error) {
	ls, ok := st.(store.LinkStore)
	if !ok {
		return nil, fmt.Errorf("this storage backend does not support namespace links")
	}
	return ls, nil
}

// normalizeLinkNamespace normalizes and validates a namespace argument,
// mirroring the REST handler's PutLink validation (rest.go).
func normalizeLinkNamespace(ns string) (string, error) {
	ns = httputil.NormalizeNamespace(ns)
	if err := httputil.ValidateNamespace(ns); err != nil {
		return "", fmt.Errorf("invalid namespace %q: %w", ns, err)
	}
	return ns, nil
}

// parseLinkTiers parses a comma-separated tier list, validating each tier
// name. An empty/blank string yields nil — the service layer's durable
// (semantic/procedural) default.
func parseLinkTiers(csv string) ([]memory.Tier, error) {
	csv = strings.TrimSpace(csv)
	if csv == "" {
		return nil, nil
	}
	var tiers []memory.Tier
	for part := range strings.SplitSeq(csv, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		t := memory.Tier(part)
		if !t.Valid() {
			return nil, fmt.Errorf("invalid tier %q", part)
		}
		tiers = append(tiers, t)
	}
	return tiers, nil
}

// validateLinkEndpoints normalizes and validates a link's src/dst pair,
// enforcing the same invariants as the REST PutLink handler (rest.go
// PutLink ~L591-620): both namespaces normalize+validate, "*" is rejected
// in dst (reserved for read-set patterns), and self-links are rejected —
// after normalization, so "acme/x/" vs "acme/x" is still a self-link.
// Shared by addLink (CLI writes) and fromExportLink (import restores,
// import.go), so no write path can bypass the invariants.
func validateLinkEndpoints(src, dst string) (string, string, error) {
	src, err := normalizeLinkNamespace(src)
	if err != nil {
		return "", "", err
	}
	dst, err = normalizeLinkNamespace(dst)
	if err != nil {
		return "", "", err
	}
	if strings.Contains(dst, "*") {
		return "", "", fmt.Errorf(`invalid dst namespace: "*" is reserved for read-set patterns`)
	}
	if dst == src {
		return "", "", fmt.Errorf("dst namespace equals src namespace (no self-links)")
	}
	return src, dst, nil
}

// addLink validates src/dst/tiers and upserts the link, mirroring PutLink's
// REST validation (see validateLinkEndpoints). dst is not required to
// already hold memories — namespaces exist implicitly.
func addLink(ctx context.Context, ls store.LinkStore, src, dst, tiersCSV, note string) (store.NamespaceLink, error) {
	src, dst, err := validateLinkEndpoints(src, dst)
	if err != nil {
		return store.NamespaceLink{}, err
	}
	tiers, err := parseLinkTiers(tiersCSV)
	if err != nil {
		return store.NamespaceLink{}, err
	}
	link := store.NamespaceLink{Src: src, Dst: dst, Tiers: tiers, Note: note, CreatedAt: time.Now().UTC()}
	if err := ls.PutLink(ctx, link); err != nil {
		return store.NamespaceLink{}, err
	}
	return link, nil
}

func runLinkAdd(cmd *cobra.Command, args []string) error {
	return withLocalStore(cmd.Context(), func(st store.Store) error {
		ls, err := linkStoreOf(st)
		if err != nil {
			return err
		}
		link, err := addLink(cmd.Context(), ls, args[0], args[1], linkTiersCSV, linkNote)
		if err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "linked %s -> %s\n", link.Src, link.Dst) //nolint:errcheck
		return nil
	})
}

func runLinkRm(cmd *cobra.Command, args []string) error {
	return withLocalStore(cmd.Context(), func(st store.Store) error {
		ls, err := linkStoreOf(st)
		if err != nil {
			return err
		}
		src, err := normalizeLinkNamespace(args[0])
		if err != nil {
			return err
		}
		dst, err := normalizeLinkNamespace(args[1])
		if err != nil {
			return err
		}
		found, err := ls.DeleteLink(cmd.Context(), src, dst)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("no link from %q to %q", src, dst)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "removed %s -> %s\n", src, dst) //nolint:errcheck
		return nil
	})
}

func runLinkLs(cmd *cobra.Command, args []string) error {
	return withLocalStore(cmd.Context(), func(st store.Store) error {
		ls, err := linkStoreOf(st)
		if err != nil {
			return err
		}
		var links []store.NamespaceLink
		if len(args) > 0 {
			src, nerr := normalizeLinkNamespace(args[0])
			if nerr != nil {
				return nerr
			}
			links, err = ls.ListLinks(cmd.Context(), src)
		} else {
			links, err = ls.ListAllLinks(cmd.Context())
		}
		if err != nil {
			return err
		}
		printLinks(cmd.OutOrStdout(), links)
		return nil
	})
}

// printLinks renders links as a tabwriter table (doctor.go's table style).
func printLinks(out io.Writer, links []store.NamespaceLink) {
	if len(links) == 0 {
		fmt.Fprintln(out, "no links") //nolint:errcheck
		return
	}
	sorted := make([]store.NamespaceLink, len(links))
	copy(sorted, links)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Src != sorted[j].Src {
			return sorted[i].Src < sorted[j].Src
		}
		return sorted[i].Dst < sorted[j].Dst
	})
	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "SRC\tDST\tTIERS\tNOTE") //nolint:errcheck
	for _, l := range sorted {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", l.Src, l.Dst, tiersLabel(l.Tiers), noteOr(l.Note)) //nolint:errcheck
	}
	_ = tw.Flush()
}

// tiersLabel renders a link's tier restriction for display: nil means the
// service-layer durable default applies.
func tiersLabel(tiers []memory.Tier) string {
	if len(tiers) == 0 {
		return "(default: durable)"
	}
	parts := make([]string, len(tiers))
	for i, t := range tiers {
		parts[i] = string(t)
	}
	return strings.Join(parts, ",")
}

func noteOr(note string) string {
	if note == "" {
		return "-"
	}
	return note
}
