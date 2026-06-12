package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/eleboucher/memini/internal/config"
	"github.com/eleboucher/memini/internal/store"
)

// poolWarnThreshold is the memory count above which a namespace commonly used as
// a catch-all (default, openclaw) is flagged as a possible import collapse or
// shared-agent pool.
const poolWarnThreshold = 500

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Diagnose namespace mismatches and store health (read-only)",
	Long: "Report how the namespace resolves for writes vs recall, list per-namespace " +
		"memory counts, and flag the conditions behind 'agents stopped writing' and " +
		"'all my memories merged into one pool'. Read-only; changes nothing.",
	RunE: runDoctor,
}

func init() {
	rootCmd.AddCommand(doctorCmd)
}

func runDoctor(cmd *cobra.Command, _ []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	var warnings int

	cwd, _ := os.Getwd()
	serverNS := cfg.DefaultNamespace
	pluginNS, pluginSrc := config.ResolvePluginNamespace(cwd)
	envNS := firstNonEmptyEnv("MEMINI_DEFAULT_NAMESPACE", "MEMINI_NAMESPACE")

	fmt.Fprintf(out, "Namespace resolution (cwd: %s)\n", cwd)                    //nolint:errcheck
	fmt.Fprintf(out, "  env override:    %s\n", orUnset(envNS))                  //nolint:errcheck
	fmt.Fprintf(out, "  server default:  %q (%s)\n", serverNS, cfg.NamespaceSrc) //nolint:errcheck
	fmt.Fprintf(out, "  plugin resolves: %q (%s)\n", pluginNS, pluginSrc)        //nolint:errcheck
	if serverNS != pluginNS {
		warnings++
		warnf(out, "the plugin reads/writes %q but the server's header-less default is %q.", pluginNS, serverNS)
		note(out, "Plugins send an explicit namespace header, but bare MCP clients use the server")
		note(out, fmt.Sprintf("default, so they disagree. Pin one with MEMINI_DEFAULT_NAMESPACE=%q.", pluginNS))
	}
	fmt.Fprintln(out) //nolint:errcheck

	// Store section: read the configured store directly (no server required).
	st, err := buildStore(cmd.Context(), cfg)
	if err != nil {
		fmt.Fprintf(out, "Store (%s): WARN: cannot open store: %v\n", cfg.Backend, err) //nolint:errcheck
		doctorResult(out, warnings+1)
		return nil
	}
	defer func() { _ = st.Close() }()

	if err := st.Ping(cmd.Context()); err != nil {
		fmt.Fprintf(out, "Store (%s): WARN: not reachable: %v\n", cfg.Backend, err) //nolint:errcheck
		doctorResult(out, warnings+1)
		return nil
	}

	stats, err := namespaceStats(cmd.Context(), st)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Store (%s): reachable, %d namespace(s)\n", cfg.Backend, len(stats)) //nolint:errcheck
	warnings += printStoreStats(out, stats, pluginNS)

	doctorResult(out, warnings)
	return nil
}

// warnf prints an indented WARN line; note prints an indented continuation.
func warnf(out io.Writer, format string, args ...any) {
	fmt.Fprintf(out, "  WARN: "+format+"\n", args...) //nolint:errcheck
}

func note(out io.Writer, s string) {
	fmt.Fprintf(out, "        %s\n", s) //nolint:errcheck
}

// nsStat is the per-namespace summary doctor reports.
type nsStat struct {
	namespace  string
	total      int
	byTier     map[string]int
	superseded int
	lastWrite  time.Time
}

func namespaceStats(ctx context.Context, st store.Store) ([]nsStat, error) {
	names, err := st.ListNamespaces(ctx)
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	out := make([]nsStat, 0, len(names))
	for _, ns := range names {
		mems, err := st.List(ctx, ns, store.Filter{IncludeSuperseded: true, IncludeExpired: true}, 0)
		if err != nil {
			return nil, err
		}
		s := nsStat{namespace: ns, total: len(mems), byTier: map[string]int{}}
		for _, m := range mems {
			s.byTier[string(m.Tier)]++
			if m.SupersededBy != nil {
				s.superseded++
			}
			if m.UpdatedAt.After(s.lastWrite) {
				s.lastWrite = m.UpdatedAt
			}
		}
		out = append(out, s)
	}
	return out, nil
}

func printStoreStats(out io.Writer, stats []nsStat, pluginNS string) int {
	var warnings int
	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "  NAMESPACE\tTOTAL\tBY TIER\tLAST WRITE\tSUPERSEDED") //nolint:errcheck
	var biggest nsStat
	for _, s := range stats {
		if s.total > biggest.total {
			biggest = s
		}
		fmt.Fprintf(tw, "  %s\t%d\t%s\t%s\t%d\n", //nolint:errcheck
			s.namespace, s.total, tierBreakdown(s.byTier), lastWriteStr(s.lastWrite), s.superseded)
	}
	_ = tw.Flush()

	for _, s := range stats {
		if (s.namespace == "default" || s.namespace == "openclaw") && s.total > poolWarnThreshold {
			warnings++
			warnf(out, "namespace %q holds %d memories — possible import collapse or shared pool.", s.namespace, s.total)
			note(out, fmt.Sprintf("Recover isolation with: memini namespace split --from %s --dry-run", s.namespace))
		}
	}

	// Writes-where-recall-doesn't-look: the namespace the plugin uses is empty
	// while another namespace holds the bulk of memories.
	pluginTotal := 0
	for _, s := range stats {
		if s.namespace == pluginNS {
			pluginTotal = s.total
		}
	}
	if pluginTotal == 0 && biggest.total > 0 && biggest.namespace != pluginNS {
		warnings++
		warnf(out, "recall here uses namespace %q (empty), but %q holds %d memories.", pluginNS, biggest.namespace, biggest.total)
		note(out, "If agents seem to have lost memory, writes are landing in a different namespace.")
	}
	return warnings
}

func doctorResult(out io.Writer, warnings int) {
	fmt.Fprintln(out) //nolint:errcheck
	if warnings == 0 {
		fmt.Fprintln(out, "No problems detected.") //nolint:errcheck
		return
	}
	fmt.Fprintf(out, "%d warning(s) detected.\n", warnings) //nolint:errcheck
}

func tierBreakdown(byTier map[string]int) string {
	if len(byTier) == 0 {
		return "-"
	}
	tiers := make([]string, 0, len(byTier))
	for t := range byTier {
		tiers = append(tiers, t)
	}
	sort.Strings(tiers)
	parts := make([]string, 0, len(tiers))
	for _, t := range tiers {
		parts = append(parts, fmt.Sprintf("%s:%d", t, byTier[t]))
	}
	return strings.Join(parts, " ")
}

func lastWriteStr(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format(time.RFC3339)
}

func orUnset(s string) string {
	if s == "" {
		return "(unset)"
	}
	return fmt.Sprintf("%q", s)
}

func firstNonEmptyEnv(keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}
