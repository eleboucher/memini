package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/eleboucher/memini/internal/config"
	"github.com/eleboucher/memini/internal/importer"
	"github.com/eleboucher/memini/internal/logging"
	"github.com/eleboucher/memini/internal/maintenance"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/store"
)

var (
	importSource    string
	importNamespace string
	importMergeInto string
	importYes       bool
	importImp       float64
	importConf      float64
	importDryRun    bool
	importNoDedup   bool
	importDedupSim  float64
	importBatch     int
	importRemote    string
	importToken     string
	importMinLen    int
	importMinImp    float64
	importExtract   bool
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
		"namespace for records whose source carried none (fallback only; does not override source namespaces)")
	importCmd.Flags().StringVar(&importMergeInto, "merge-into", "",
		"force every record into this one namespace, discarding source namespaces (the original is kept in metadata for `namespace split`)")
	importCmd.Flags().BoolVar(&importYes, "yes", false,
		"skip the confirmation prompt when --merge-into collapses multiple source namespaces")
	importCmd.Flags().Float64Var(&importImp, "importance", 0.25,
		"importance assigned to records whose source carried none, so bulk imports rank below curated memories (0 = leave at 0)")
	importCmd.Flags().Float64Var(&importConf, "confidence", -1,
		"seed confidence for durable imported facts (0..1); <0 uses the default low import seed so they earn trust on recall")
	importCmd.Flags().BoolVar(&importDryRun, "dry-run", false,
		"parse and report where records would land without writing anything")
	importCmd.Flags().BoolVar(&importNoDedup, "no-dedup", false,
		"skip dedup of imported records: both the write-time exact-content skip and the post-import vector-cluster pass")
	importCmd.Flags().Float64Var(&importDedupSim, "dedup-similarity", 0.85,
		"similarity threshold for the post-import dedup pass")
	importCmd.Flags().IntVar(&importBatch, "batch", 0,
		"records per batch (0 = backend default)")
	importCmd.Flags().StringVar(&importRemote, "remote", "",
		"target a running memini server (e.g. https://memini.example.com) instead of the local store")
	importCmd.Flags().StringVar(&importToken, "token", "",
		"bearer token for the remote server (defaults to MEMINI_API_KEY)")
	importCmd.Flags().IntVar(&importMinLen, "min-length", 20,
		"skip records whose trimmed content is shorter than this many bytes (0 = off)")
	importCmd.Flags().Float64Var(&importMinImp, "min-importance", 0,
		"skip records below this importance (0 = off; note: sources without importance report 0)")
	importCmd.Flags().BoolVar(&importExtract, "extract", false,
		"also distil decisions/preferences/problems from conversations into durable semantic memories (no LLM)")

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
	if path == "" && importer.Source(importSource) == importer.SourceClaudeCode {
		home, err := os.UserHomeDir()
		if err == nil {
			path = home + "/.claude/projects"
		}
	}

	w := cmd.ErrOrStderr()
	isTerm := isTerminal(w)

	var loadProgress func(done, total int)
	if isTerm {
		loadProgress = func(done, total int) {
			if done == 0 {
				fmt.Fprintf(w, "\r\033[Kscanning... found %d files", total) //nolint:errcheck
			} else {
				fmt.Fprintf(w, "\r\033[Kloading records... %d/%d files", done, total) //nolint:errcheck
			}
		}
	}
	recs, rawData, err := loadRecords(importer.Source(importSource), path, w, loadProgress)
	if err != nil {
		return err
	}
	if isTerm {
		fmt.Fprintf(w, "\r\033[Kloaded %d records\n", len(recs)) //nolint:errcheck
	}

	// Distil durable semantic facts from the conversation records, alongside the
	// transient episodic exchanges (which age out on their TTL).
	if importExtract {
		typed := importer.ExtractTyped(recs)
		recs = append(recs, typed...)
		if isTerm {
			fmt.Fprintf(w, "extracted %d typed semantic memories\n", len(typed)) //nolint:errcheck
		}
	}

	// --merge-into collapses every source namespace into one. Confirm before
	// discarding multi-namespace scoping, since that is exactly the failure mode
	// that poisons a store (the original is preserved in metadata for recovery).
	if importMergeInto != "" {
		srcNS := distinctNamespaces(recs)
		if len(srcNS) > 1 && !importYes {
			if !confirmMerge(cmd, srcNS, importMergeInto) {
				return fmt.Errorf("aborted: %d source namespaces would merge into %q (re-run with --yes to confirm)",
					len(srcNS), importMergeInto)
			}
		}
	}

	im, target, dedup, linkStore, closeFn, err := buildImporter(cmd.Context(), cfg, log, importRemote, importToken)
	if err != nil {
		return err
	}
	defer closeFn()

	var importConfidence *float64
	if importConf >= 0 {
		importConfidence = &importConf
	}
	rep, err := im.Import(cmd.Context(), recs, importer.Options{
		DefaultNamespace:  importNamespace,
		ForceNamespace:    importMergeInto,
		Source:            importer.Source(importSource),
		DefaultImportance: importImp,
		Confidence:        importConfidence,
		SkipExisting:      true,
		DedupContent:      !importNoDedup,
		DryRun:            importDryRun,
		BatchSize:         importBatch,
		OnProgress:        newProgressWriter(w),
		MinContentLen:     importMinLen,
		MinImportance:     importMinImp,
	})

	out := cmd.OutOrStdout()
	verb := "import"
	if importDryRun {
		verb = "dry-run"
	}
	fmt.Fprintf(out, "%s %s -> %s: %d imported, %d duplicates, %d skipped, %d total\n", //nolint:errcheck
		verb, importSource, target, rep.Imported, rep.Duplicates, rep.Skipped, rep.Total)
	printNamespaceHistogram(out, rep.Namespaces)
	for _, e := range rep.Errors {
		fmt.Fprintln(cmd.ErrOrStderr(), "  error:", e) //nolint:errcheck
	}
	if err != nil {
		return err
	}
	if len(rep.Errors) > 0 {
		return fmt.Errorf("import completed with %d errors", len(rep.Errors))
	}

	if err := restoreImportedLinks(cmd.Context(), out, linkStore, importer.Source(importSource), rawData, importDryRun); err != nil {
		return err
	}

	// Collapse near-duplicates created or exposed by the import, scoped to the
	// namespaces it touched so others are untouched.
	if !importDryRun && !importNoDedup && dedup != nil && rep.Imported > 0 {
		if err := dedup(cmd.Context(), namespacesOf(rep.Namespaces), importDedupSim, out); err != nil {
			fmt.Fprintln(cmd.ErrOrStderr(), "  dedup warning:", err) //nolint:errcheck
		}
	}
	return nil
}

// dedupFunc runs a post-import dedup pass over the given namespaces and writes a
// one-line summary to out.
type dedupFunc func(ctx context.Context, namespaces []string, similarity float64, out io.Writer) error

// buildImporter also returns the target's store.LinkStore capability (nil
// for a remote target, or a local backend that predates namespace links) so
// runImport can restore links from a native export after the memory import.
func buildImporter(
	ctx context.Context, cfg *config.Config, log *slog.Logger, remote, token string,
) (*importer.Importer, string, dedupFunc, store.LinkStore, func(), error) {
	if remote != "" {
		client := importer.NewRemoteClient(remote, token, config.DefaultNamespaceHeader)
		dedup := func(ctx context.Context, namespaces []string, sim float64, out io.Writer) error {
			var clusters, tombstoned, done int
			var failed []string
			for _, ns := range namespaces {
				res, err := client.Dedup(ctx, ns, sim)
				if err != nil {
					// Don't let one namespace's failure skip the rest — report it
					// and keep deduping the others.
					failed = append(failed, fmt.Sprintf("%s: %v", ns, err))
					continue
				}
				done++
				clusters += res.ClustersFound
				tombstoned += res.Tombstoned
			}
			fmt.Fprintf(out, "dedup: %d clusters, %d tombstoned across %d namespaces\n", //nolint:errcheck
				clusters, tombstoned, done)
			if len(failed) > 0 {
				return fmt.Errorf("dedup failed for %d namespace(s): %s", len(failed), strings.Join(failed, "; "))
			}
			return nil
		}
		return importer.NewRemote(client), remote, dedup, nil, func() {}, nil
	}
	st, err := buildStore(ctx, cfg)
	if err != nil {
		return nil, "", nil, nil, nil, err
	}
	embedder, err := buildEmbedder(cfg, log, nil)
	if err != nil {
		_ = st.Close()
		return nil, "", nil, nil, nil, err
	}
	dedup := func(ctx context.Context, namespaces []string, sim float64, out io.Writer) error {
		rep, err := maintenance.Dedup(ctx, st, embedder, maintenance.DedupOptions{
			Namespaces: namespaces,
			Similarity: sim,
			Log:        log,
		})
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "dedup: %d clusters, %d tombstoned across %d namespaces\n", //nolint:errcheck
			rep.ClustersFound, rep.Tombstoned, rep.Namespaces)
		return nil
	}
	linkStore, _ := st.(store.LinkStore)
	return importer.NewLocal(st, embedder), localStoreLabel, dedup, linkStore, func() { _ = st.Close() }, nil
}

// distinctNamespaces returns the sorted set of namespaces the records carry
// (empty source namespaces are ignored — they fall to the default/merge target).
func distinctNamespaces(recs []importer.Record) []string {
	seen := map[string]struct{}{}
	for _, r := range recs {
		if r.Namespace != "" {
			seen[r.Namespace] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for ns := range seen {
		out = append(out, ns)
	}
	sort.Strings(out)
	return out
}

// namespacesOf returns the sorted keys of a histogram.
func namespacesOf(hist map[string]int) []string {
	out := make([]string, 0, len(hist))
	for ns := range hist {
		out = append(out, ns)
	}
	sort.Strings(out)
	return out
}

// printNamespaceHistogram lists records-per-destination-namespace so a single-
// pool collapse is visible at a glance.
func printNamespaceHistogram(w io.Writer, hist map[string]int) {
	if len(hist) == 0 {
		return
	}
	for _, ns := range namespacesOf(hist) {
		fmt.Fprintf(w, "  namespace %q: %d\n", ns, hist[ns]) //nolint:errcheck
	}
}

// confirmMerge prompts on stderr before a --merge-into collapses multiple source
// namespaces into one. A non-y/yes answer (or non-interactive input) aborts.
func confirmMerge(cmd *cobra.Command, srcNS []string, target string) bool {
	w := cmd.ErrOrStderr()
	fmt.Fprintf(w, "warning: merging %d source namespaces (%s) into %q\n", //nolint:errcheck
		len(srcNS), strings.Join(srcNS, ", "), target)
	fmt.Fprint(w, "continue? [y/N] ") //nolint:errcheck
	sc := bufio.NewScanner(cmd.InOrStdin())
	if !sc.Scan() {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(sc.Text())) {
	case "y", "yes":
		return true
	default:
		return false
	}
}

func readInput(path string) ([]byte, error) {
	if path == "" || path == "-" {
		return io.ReadAll(os.Stdin)
	}
	return os.ReadFile(path)
}

// loadRecords parses path into portable Records. It also returns the raw
// input bytes (nil for the claude-code directory-scan branch, which has no
// single JSON payload) so callers can separately extract a native export's
// top-level "links" array (see loadLinks) without re-reading stdin, which
// would return EOF on a second read.
func loadRecords(
	src importer.Source, path string, w io.Writer, onProgress func(done, total int),
) ([]importer.Record, []byte, error) {
	if src == importer.SourceClaudeCode && path != "" && path != "-" {
		if info, err := os.Stat(path); err == nil && info.IsDir() {
			recs, warns, err := importer.LoadClaudeCodeWithProgress(path, onProgress)
			for _, warn := range warns {
				fmt.Fprintln(w, "  warning:", warn) //nolint:errcheck
			}
			return recs, nil, err
		}
	}
	data, err := readInput(path)
	if err != nil {
		return nil, nil, err
	}
	recs, err := importer.Parse(src, data)
	return recs, data, err
}

// loadLinks extracts a native memini export's top-level "links" array (gap
// G6). Only importer.SourceMemini carries links; every other source (and a
// nil data, e.g. the claude-code directory-scan path) yields nil, nil. An
// export from before links existed has no "links" key at all — unmarshaling
// into a struct with an omittable field simply leaves it empty, so an old
// export keeps importing without error.
func loadLinks(src importer.Source, data []byte) ([]store.NamespaceLink, error) {
	if src != importer.SourceMemini || len(data) == 0 {
		return nil, nil
	}
	var wrapper struct {
		Links []exportLink `json:"links"`
	}
	if err := json.Unmarshal(data, &wrapper); err != nil {
		// A bare-array native export (the other historical shape) isn't a
		// JSON object and has no links key either way.
		return nil, nil //nolint:nilerr
	}
	links := make([]store.NamespaceLink, 0, len(wrapper.Links))
	for _, el := range wrapper.Links {
		l, err := fromExportLink(el)
		if err != nil {
			return nil, err
		}
		links = append(links, l)
	}
	return links, nil
}

// fromExportLink converts an export document's link shape back into a
// store.NamespaceLink, enforcing the same invariants as addLink (link.go)
// via the shared validateLinkEndpoints — normalize src/dst, reject "*" in
// dst, reject self-links — plus tier-name validation, so a hand-edited
// export cannot restore a link the CLI/REST write path would refuse. Every
// failure names the offending link. An unparseable created_at fails loudly
// too (consistent with the bad-tier failure) rather than silently zeroing
// the timestamp.
func fromExportLink(el exportLink) (store.NamespaceLink, error) {
	src, dst, err := validateLinkEndpoints(el.Src, el.Dst)
	if err != nil {
		return store.NamespaceLink{}, fmt.Errorf("import: link %q -> %q: %w", el.Src, el.Dst, err)
	}
	l := store.NamespaceLink{Src: src, Dst: dst, Note: el.Note}
	if len(el.Tiers) > 0 {
		l.Tiers = make([]memory.Tier, 0, len(el.Tiers))
		for _, t := range el.Tiers {
			mt := memory.Tier(t)
			if !mt.Valid() {
				return store.NamespaceLink{}, fmt.Errorf("import: link %q -> %q: invalid tier %q", el.Src, el.Dst, t)
			}
			l.Tiers = append(l.Tiers, mt)
		}
	}
	if el.CreatedAt != "" {
		t, err := time.Parse(time.RFC3339Nano, el.CreatedAt)
		if err != nil {
			return store.NamespaceLink{}, fmt.Errorf("import: link %q -> %q: invalid created_at %q: %w", el.Src, el.Dst, el.CreatedAt, err)
		}
		l.CreatedAt = t
	}
	return l, nil
}

// restoreLinks upserts links into ls via PutLink, an insert-or-replace keyed
// by (Src, Dst) — so restoring the same export twice never duplicates rows.
// It returns the number successfully written before any error, so a partial
// failure's progress is visible.
func restoreLinks(ctx context.Context, ls store.LinkStore, links []store.NamespaceLink) (int, error) {
	for i, l := range links {
		if err := ls.PutLink(ctx, l); err != nil {
			return i, fmt.Errorf("link %s -> %s: %w", l.Src, l.Dst, err)
		}
	}
	return len(links), nil
}

// restoreImportedLinks parses and restores a native export's namespace links
// after the memory import succeeds (gap G6). It is a no-op for a dry run, a
// remote target, or a backend that predates namespace links (linkStore
// nil); loadLinks itself is a no-op for any source but memini, or an export
// carrying no links (old-format-compatible).
func restoreImportedLinks(
	ctx context.Context, out io.Writer, linkStore store.LinkStore, src importer.Source, rawData []byte, dryRun bool,
) error {
	if dryRun || linkStore == nil {
		return nil
	}
	links, err := loadLinks(src, rawData)
	if err != nil {
		return err
	}
	if len(links) == 0 {
		return nil
	}
	n, err := restoreLinks(ctx, linkStore, links)
	if err != nil {
		return fmt.Errorf("restore %d/%d namespace link(s): %w", n, len(links), err)
	}
	fmt.Fprintf(out, "restored %d namespace link(s)\n", n) //nolint:errcheck
	return nil
}
