package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/eleboucher/memini/internal/config"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/store"
)

var (
	exportNamespace     string
	exportAllNamespaces bool
	exportTiers         []string
	exportTags          []string
	exportMeta          []string
	exportIncludeExp    bool
	exportIncludeSup    bool
	exportOutput        string
	exportPretty        bool
)

// exportRecord is the round-trippable memini export shape: it matches the
// fields `import --source memini` reads back (see internal/importer/memini.go),
// so an export can be re-imported without loss.
type exportRecord struct {
	ID         string         `json:"id"`
	Namespace  string         `json:"namespace"`
	Tier       string         `json:"tier"`
	Content    string         `json:"content"`
	Summary    string         `json:"summary,omitempty"`
	Tags       []string       `json:"tags,omitempty"`
	Metadata   map[string]any `json:"metadata,omitempty"`
	Importance float64        `json:"importance"`
	CreatedAt  string         `json:"created_at,omitempty"`
	UpdatedAt  string         `json:"updated_at,omitempty"`
	ExpiresAt  string         `json:"expires_at,omitempty"`
}

// exportLink is the round-trippable shape for a namespace link (see
// store.NamespaceLink) in the native export document. It matches the fields
// `import --source memini` reads back (see loadLinks in import.go).
type exportLink struct {
	Src       string   `json:"src"`
	Dst       string   `json:"dst"`
	Tiers     []string `json:"tiers,omitempty"`
	Note      string   `json:"note,omitempty"`
	CreatedAt string   `json:"created_at,omitempty"`
}

// exportFile is the native export document. Links is a top-level array
// (gap G6): a link crosses namespace boundaries by nature, so it doesn't
// belong to any single memory record. Older exports carry no "links" key at
// all — import.go's loadLinks treats that as an empty list, not an error.
type exportFile struct {
	Memories []exportRecord `json:"memories"`
	Links    []exportLink   `json:"links,omitempty"`
}

var exportCmd = &cobra.Command{
	Use:   "export [file]",
	Short: "Export memories to memini's portable JSON (re-importable for backup/migration)",
	Long: "Export a namespace's memories to a JSON document re-importable with " +
		"`memini import --source memini`. Writes stdout when no path or -o is given. " +
		"Filter with --tier/--tag/--meta to export a slice.",
	Args: cobra.MaximumNArgs(1),
	RunE: runExport,
}

func init() {
	exportCmd.Flags().StringVar(&exportNamespace, "namespace", "", "namespace to export (defaults to the resolved default)")
	exportCmd.Flags().BoolVar(&exportAllNamespaces, "all-namespaces", false, "export every namespace (each record keeps its own namespace)")
	exportCmd.Flags().StringArrayVar(&exportTiers, "tier", nil, "restrict to these tiers (repeatable; working|episodic|semantic|procedural)")
	exportCmd.Flags().StringArrayVar(&exportTags, "tag", nil, "restrict to memories carrying every listed tag (repeatable, AND)")
	exportCmd.Flags().StringArrayVar(&exportMeta, "meta", nil,
		"restrict to memories whose metadata contains each key=value pair (repeatable, AND)")
	exportCmd.Flags().BoolVar(&exportIncludeExp, "include-expired", false, "include memories past their TTL")
	exportCmd.Flags().BoolVar(&exportIncludeSup, "include-superseded", false, "include contradiction-tombstoned memories")
	exportCmd.Flags().StringVarP(&exportOutput, "output", "o", "", "write to this file instead of stdout")
	exportCmd.Flags().BoolVar(&exportPretty, "pretty", false, "indent the JSON output")
	rootCmd.AddCommand(exportCmd)
}

func runExport(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	tiers, err := parseExportTiers(exportTiers)
	if err != nil {
		return err
	}
	meta, err := parseMetaPairs(exportMeta)
	if err != nil {
		return err
	}
	f := store.Filter{
		Tiers:             tiers,
		Tags:              exportTags,
		Metadata:          meta,
		IncludeExpired:    exportIncludeExp,
		IncludeSuperseded: exportIncludeSup,
	}

	ns := exportNamespace
	if ns == "" {
		ns = cfg.DefaultNamespace
	}

	var ef exportFile
	err = withLocalStore(cmd.Context(), func(st store.Store) error {
		namespaces := []string{ns}
		if exportAllNamespaces {
			names, lerr := st.ListNamespaces(cmd.Context())
			if lerr != nil {
				return lerr
			}
			sort.Strings(names)
			namespaces = names
		}
		var gerr error
		ef, gerr = gatherExport(cmd.Context(), st, namespaces, exportAllNamespaces, f)
		return gerr
	})
	if err != nil {
		return err
	}

	out, closeOut, err := exportWriter(args)
	if err != nil {
		return err
	}
	defer closeOut()

	enc := json.NewEncoder(out)
	if exportPretty {
		enc.SetIndent("", "  ")
	}
	enc.SetEscapeHTML(false)
	if err := enc.Encode(ef); err != nil {
		return err
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "exported %d memories, %d links\n", len(ef.Memories), len(ef.Links)) //nolint:errcheck
	return nil
}

// gatherExport collects the exportable memories and namespace links for
// namespaces (each memory keeps its own namespace; namespaces is either one
// namespace or the full set of namespaces holding memories, for
// --all-namespaces). Links come from ListAllLinks.
//
// allNamespaces must be true whenever the caller resolved namespaces via
// ListNamespaces (i.e. --all-namespaces): that call only returns namespaces
// with at least one memory row, but a link may point at a namespace that
// holds none (namespaces exist implicitly, by design — see link.go). Filtering
// links against that memory-only set would silently drop such links even
// though --all-namespaces means "everything". So allNamespaces=true exports
// every link unconditionally; otherwise a link is kept only when its Src or
// Dst is in namespaces.
//
// LinkStore is an optional store capability: a backend that predates
// namespace links yields no Links, not an error (graceful degrade, the
// EmbedModelStore precedent).
func gatherExport(ctx context.Context, st store.Store, namespaces []string, allNamespaces bool, f store.Filter) (exportFile, error) {
	ef := exportFile{Memories: []exportRecord{}}
	nsSet := make(map[string]bool, len(namespaces))
	for _, n := range namespaces {
		nsSet[n] = true
	}
	for _, n := range namespaces {
		mems, err := st.List(ctx, n, f, 0)
		if err != nil {
			return exportFile{}, err
		}
		for _, m := range mems {
			ef.Memories = append(ef.Memories, toExportRecord(m))
		}
	}
	if ls, ok := st.(store.LinkStore); ok {
		all, err := ls.ListAllLinks(ctx)
		if err != nil {
			return exportFile{}, err
		}
		for _, l := range all {
			if allNamespaces || nsSet[l.Src] || nsSet[l.Dst] {
				ef.Links = append(ef.Links, toExportLink(l))
			}
		}
	}
	return ef, nil
}

func toExportRecord(m *memory.Memory) exportRecord {
	r := exportRecord{
		ID:         m.ID,
		Namespace:  m.Namespace,
		Tier:       string(m.Tier),
		Content:    m.Content,
		Summary:    m.Summary,
		Tags:       m.Tags,
		Metadata:   m.Metadata,
		Importance: m.Importance,
		CreatedAt:  m.CreatedAt.UTC().Format(time.RFC3339Nano),
		UpdatedAt:  m.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
	if m.ExpiresAt != nil {
		r.ExpiresAt = m.ExpiresAt.UTC().Format(time.RFC3339Nano)
	}
	return r
}

// toExportLink converts a stored link to its round-trippable export shape.
func toExportLink(l store.NamespaceLink) exportLink {
	el := exportLink{Src: l.Src, Dst: l.Dst, Note: l.Note}
	if len(l.Tiers) > 0 {
		el.Tiers = make([]string, len(l.Tiers))
		for i, t := range l.Tiers {
			el.Tiers[i] = string(t)
		}
	}
	if !l.CreatedAt.IsZero() {
		el.CreatedAt = l.CreatedAt.UTC().Format(time.RFC3339Nano)
	}
	return el
}

// exportWriter resolves the output sink: -o flag, then a positional path, then
// stdout. The returned close func is a no-op for stdout.
func exportWriter(args []string) (io.Writer, func(), error) {
	path := exportOutput
	if path == "" && len(args) > 0 {
		path = args[0]
	}
	if path == "" || path == "-" {
		return os.Stdout, func() {}, nil
	}
	file, err := os.Create(path)
	if err != nil {
		return nil, nil, err
	}
	return file, func() { _ = file.Close() }, nil
}

func parseExportTiers(in []string) ([]memory.Tier, error) {
	var tiers []memory.Tier
	for _, v := range in {
		for part := range strings.SplitSeq(v, ",") {
			t := memory.Tier(strings.TrimSpace(part))
			if t == "" {
				continue
			}
			if !t.Valid() {
				return nil, fmt.Errorf("invalid tier %q", t)
			}
			tiers = append(tiers, t)
		}
	}
	return tiers, nil
}

// parseMetaPairs parses key=value flags into a metadata filter map, splitting
// on the first '=' so values may contain '='.
func parseMetaPairs(in []string) (map[string]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(in))
	for _, v := range in {
		k, val, ok := strings.Cut(v, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" {
			return nil, fmt.Errorf("invalid --meta %q: want key=value", v)
		}
		out[k] = val
	}
	return out, nil
}
