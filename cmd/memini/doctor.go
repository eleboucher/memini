package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"slices"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/eleboucher/memini/internal/config"
	"github.com/eleboucher/memini/internal/embed"
	"github.com/eleboucher/memini/internal/logging"
	"github.com/eleboucher/memini/internal/maintenance"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/store"
)

// poolWarnThreshold is the memory count above which a namespace commonly used as
// a catch-all (default, openclaw) is flagged as a possible import collapse or
// shared-agent pool.
const poolWarnThreshold = 500

// nsDefault is the literal fallback namespace records land in when none is
// resolved — the prime catch-all an import collapse fills.
const nsDefault = "default"

// catchAllNamespaces are the names a bulk import or shared-agent setup tends to
// collapse everything into, so an oversized one is a poisoning signal.
var catchAllNamespaces = []string{nsDefault, "openclaw"}

func isCatchAllNamespace(ns string) bool {
	return slices.Contains(catchAllNamespaces, ns)
}

var (
	doctorFixFlag bool
	doctorScrub   bool
	doctorYes     bool
)

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Diagnose namespace mismatches and store health",
	Long: "Report how the namespace resolves for writes vs recall, list per-namespace " +
		"memory counts, and flag the conditions behind 'agents stopped writing' and " +
		"'all my memories merged into one pool'. Read-only unless --fix is given.",
	RunE: runDoctor,
}

func init() {
	doctorCmd.Flags().BoolVar(&doctorFixFlag, "fix", false,
		"after diagnosing, remediate: split attributable pools, backfill legacy confidence, "+
			"purge expired, scrub content junk, demote stale durable debris, dedup")
	doctorCmd.Flags().BoolVar(&doctorScrub, "scrub", false,
		"remove content-level junk only: session-lifecycle markers and exact-duplicate memories (preview unless --yes)")
	doctorCmd.Flags().BoolVar(&doctorYes, "yes", false,
		"apply --fix/--scrub changes (without it, they only preview)")
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
	warnings += printRetrievalScope(cmd.Context(), out, cfg, st, stats, serverNS, pluginNS)

	fmt.Fprintf(out, "Store (%s): reachable, %d namespace(s)\n", cfg.Backend, len(stats)) //nolint:errcheck
	warnings += printStoreStats(out, stats, pluginNS)

	if doctorFixFlag {
		return runDoctorFix(cmd, cfg, st, stats)
	}
	if doctorScrub {
		return runDoctorScrub(cmd.Context(), out, st, doctorYes)
	}
	doctorResult(out, warnings)
	return nil
}

// runDoctorScrub previews (or, with --yes, applies) the content-quality scrub:
// session-lifecycle markers and exact-duplicate memories the namespace fix and
// embedding dedup don't catch.
func runDoctorScrub(ctx context.Context, out io.Writer, st store.Store, apply bool) error {
	if apply {
		fmt.Fprintln(out, "\nScrub (applying):") //nolint:errcheck
	} else {
		fmt.Fprintln(out, "\nScrub (preview — re-run with --yes to apply):") //nolint:errcheck
	}
	rep, err := maintenance.Scrub(ctx, st, apply)
	if err != nil {
		return err
	}
	printScrub(out, rep)
	return nil
}

func printScrub(out io.Writer, rep maintenance.ScrubReport) {
	fmt.Fprintf(out, "  lifecycle markers:  %d\n", rep.LifecycleNoise)  //nolint:errcheck
	fmt.Fprintf(out, "  exact duplicates:   %d\n", rep.ExactDuplicates) //nolint:errcheck
	fmt.Fprintf(out, "  total removed:      %d\n", rep.Total())         //nolint:errcheck
}

// fixDeps carries the dependencies doctorFix needs, so the remediation logic is
// testable without a cobra command or config. embedder may be nil (dedup is then
// skipped).
type fixDeps struct {
	store       store.Store
	embedder    embed.Embedder
	dedupSim    float64
	demoteAfter time.Duration
	now         time.Time
	apply       bool
}

// runDoctorFix builds the remediation dependencies from config and runs it.
func runDoctorFix(cmd *cobra.Command, cfg *config.Config, st store.Store, stats []nsStat) error {
	d := fixDeps{
		store: st, dedupSim: cfg.DedupSimilarity, demoteAfter: cfg.DemoteAfter,
		now: time.Now().UTC(), apply: doctorYes,
	}
	if cfg.EmbedBaseURL != "" {
		embedder, err := buildEmbedder(cfg, logging.New(cfg.LogLevel, cfg.LogFormat), nil)
		if err != nil {
			return err
		}
		d.embedder = embedder
	}
	return doctorFix(cmd.Context(), cmd.OutOrStdout(), stats, d)
}

// doctorFix remediates an already-poisoned store. It previews by default and
// mutates only when d.apply is set. It splits oversized catch-all pools when
// attribution is unambiguous (>=90% of records carry a grouping key), then (on
// apply) purges expired memories, demotes stale durable debris, and deduplicates.
func doctorFix(ctx context.Context, out io.Writer, stats []nsStat, d fixDeps) error {
	if d.apply {
		fmt.Fprintln(out, "\nRemediation (applying):") //nolint:errcheck
	} else {
		fmt.Fprintln(out, "\nRemediation (preview — re-run with --yes to apply):") //nolint:errcheck
	}

	// Split unambiguously-attributable catch-all pools.
	for _, s := range stats {
		if !isCatchAllNamespace(s.namespace) {
			continue
		}
		if s.total <= poolWarnThreshold {
			continue
		}
		preview, err := maintenance.Split(ctx, d.store, s.namespace, nil, true)
		if err != nil {
			return err
		}
		total := preview.Moved + preview.Skipped
		if total == 0 || float64(preview.Moved)/float64(total) < 0.9 {
			fmt.Fprintf(out, "  skip split of %q: only %d/%d records attributable (<90%%)\n", //nolint:errcheck
				s.namespace, preview.Moved, total)
			note(out, "run `memini namespace split` manually to inspect")
			continue
		}
		rep := preview
		if d.apply {
			if rep, err = maintenance.Split(ctx, d.store, s.namespace, nil, false); err != nil {
				return err
			}
		}
		fmt.Fprintf(out, "  split %q: %d memories into %d namespaces\n", s.namespace, rep.Moved, len(rep.Targets)) //nolint:errcheck
	}

	if !d.apply {
		fmt.Fprintln(out, "  (backfill/purge/demote/dedup run on --yes)") //nolint:errcheck
		return nil
	}

	// Backfill pre-0.0.11 durable memories so the demote sweep below sees them.
	back, err := maintenance.BackfillConfidence(ctx, d.store, d.now)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "  backfilled %d legacy durable memories (seeded %.2f)\n", //nolint:errcheck
		back.Seeded, memory.ConfidenceSeedImported)

	n, err := maintenance.PurgeExpired(ctx, d.store, d.now)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "  purged %d expired memories\n", n) //nolint:errcheck

	// Scrub content junk (lifecycle markers, exact duplicates) that the pool
	// split and embedding dedup don't catch.
	scrub, err := maintenance.Scrub(ctx, d.store, true)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "  scrubbed %d lifecycle markers, %d exact duplicates\n", //nolint:errcheck
		scrub.LifecycleNoise, scrub.ExactDuplicates)

	// Honor the configured demotion window; when periodic demotion is disabled
	// (0), still age out debris in this one-shot remediation with a 60d default.
	demoteAfter := d.demoteAfter
	if demoteAfter <= 0 {
		demoteAfter = 60 * 24 * time.Hour
	}
	n, err = maintenance.DemoteStale(ctx, d.store, d.now.Add(-demoteAfter), d.now)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "  demoted %d stale durable memories\n", n) //nolint:errcheck

	// Heal broken supersession chains (no embedder needed) before deduping, so
	// the pass below re-collapses any duplicates this resurrects.
	repaired, err := maintenance.RepairSupersession(ctx, d.store, nil, false, nil)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "  repaired %d stranded memories across %d namespaces\n", //nolint:errcheck
		repaired.Restored, repaired.Namespaces)

	if d.embedder == nil {
		fmt.Fprintln(out, "  skip dedup: no embedder configured (set MEMINI_EMBED_BASE_URL)") //nolint:errcheck
		return nil
	}
	rep, err := maintenance.Dedup(ctx, d.store, d.embedder, maintenance.DedupOptions{Similarity: d.dedupSim})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "  dedup: %d clusters, %d tombstoned\n", rep.ClustersFound, rep.Tombstoned) //nolint:errcheck
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
	namespace     string
	total         int
	byTier        map[string]int
	superseded    int
	lowConfidence int // durable memories whose corroboration is below the demote floor
	lastWrite     time.Time

	// Write-path signals: how the heuristic tier machinery is behaving.
	classified   int // durable writes tiered by the marker classifier (tier_classified=marker)
	promoted     int // durable facts produced by promotion (promoted_from set)
	corroborated int // durable memories whose confidence grew past the fresh seed
}

func namespaceStats(ctx context.Context, st store.Store) ([]nsStat, error) {
	names, err := st.ListNamespaces(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
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
			if m.Tier.Term() == memory.LongTerm && m.Confidence != nil &&
				m.EffectiveConfidence(now) < memory.ConfidenceDemoteFloor {
				s.lowConfidence++
			}
			if m.UpdatedAt.After(s.lastWrite) {
				s.lastWrite = m.UpdatedAt
			}
			if m.Metadata != nil {
				if v, _ := m.Metadata["tier_classified"].(string); v == "marker" {
					s.classified++
				}
				if _, ok := m.Metadata["promoted_from"]; ok {
					s.promoted++
				}
			}
			if m.Tier.Term() == memory.LongTerm && m.Confidence != nil &&
				*m.Confidence > memory.ConfidenceSeedFresh {
				s.corroborated++
			}
		}
		out = append(out, s)
	}
	return out, nil
}

func printStoreStats(out io.Writer, stats []nsStat, pluginNS string) int {
	var warnings int
	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "  NAMESPACE\tTOTAL\tBY TIER\tLAST WRITE\tSUPERSEDED\tLOW-CONF") //nolint:errcheck
	var biggest nsStat
	for _, s := range stats {
		if s.total > biggest.total {
			biggest = s
		}
		fmt.Fprintf(tw, "  %s\t%d\t%s\t%s\t%d\t%d\n", //nolint:errcheck
			s.namespace, s.total, tierBreakdown(s.byTier), lastWriteStr(s.lastWrite), s.superseded, s.lowConfidence)
	}
	_ = tw.Flush()

	for _, s := range stats {
		if isCatchAllNamespace(s.namespace) && s.total > poolWarnThreshold {
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
	printWritePathSignals(out, stats)
	return warnings
}

// printWritePathSignals aggregates how the heuristic tier machinery is
// behaving across namespaces: writes the classifier tiered durable, facts the
// promoter produced, and durable memories whose confidence has grown past the
// fresh seed (re-observed via corroboration or exact restatement).
func printWritePathSignals(out io.Writer, stats []nsStat) {
	var classified, promoted, corroborated int
	for _, s := range stats {
		classified += s.classified
		promoted += s.promoted
		corroborated += s.corroborated
	}
	if classified == 0 && promoted == 0 && corroborated == 0 {
		return
	}
	fmt.Fprintln(out, "Write-path signals:")                                                       //nolint:errcheck
	fmt.Fprintf(out, "  marker-classified durable writes:  %d\n", classified)                      //nolint:errcheck
	fmt.Fprintf(out, "  promotion-produced facts:          %d\n", promoted)                        //nolint:errcheck
	fmt.Fprintf(out, "  corroborated durable memories:     %d (confidence above the %.2f seed)\n", //nolint:errcheck
		corroborated, memory.ConfidenceSeedFresh)
}

// doctorReadSetClamp mirrors internal/service/service.go's unexported
// readSetMaxEntries: the entry count a live read-set expansion clamps to.
// Duplicated here (rather than exported) because doctor reconstructs the
// resolver's logic wholesale (see resolveDoctorReadSet), following the same
// precedent as the namespace-divergence check above, which mirrors
// config.ResolvePluginNamespace's resolution order instead of calling into a
// running server.
const doctorReadSetClamp = 64

// doctorReadEntry is one namespace in doctor's reconstruction of a plain
// recall/briefing's default read set for a namespace.
type doctorReadEntry struct {
	ns     string
	tiers  string // "all" (the request's own tier filter) or "durable" (semantic+procedural only)
	source string // "default", "subtree-pattern", "env", "link", "global"
}

// resolveDoctorReadSet reconstructs the default read set for primary,
// mirroring internal/service/readset.go's resolveDefaultReadSet: primary
// itself, then its persistent namespace links, then MEMINI_GLOBAL_NAMESPACE,
// then MEMINI_READ_NAMESPACES, the order that makes "the widest tier access
// wins, never narrowed" hold when two sources name the same namespace. It
// omits the parts only a live request carries: scope=subtree on primary (that
// would only add more namespaces to what's shown here) and a per-call tier
// filter (assumed absent, i.e. every tier admitted, the common case). Pure:
// allNamespaces (for "/*" pattern expansion) and links are pre-fetched by the
// caller, which already has both from namespaceStats and store.LinkStore.
//
// The second return value lists redundant-configuration notes: an env/link
// entry naming primary itself (a no-op, since primary is already included),
// or two different sources naming the same namespace (the later one has no
// effect; the earlier source's tier access wins).
func resolveDoctorReadSet(primary string, readNamespaces []string, globalNamespace string, links []store.NamespaceLink, allNamespaces []string) ([]doctorReadEntry, []string) {
	entries := []doctorReadEntry{{ns: primary, tiers: "all", source: "default"}}
	seen := map[string]bool{primary: true}
	claimedBy := map[string]string{primary: "the request namespace itself"}
	var notes []string

	add := func(ns, tiers, source, desc string) {
		if seen[ns] {
			if ns == primary {
				notes = append(notes, fmt.Sprintf("%s names %q, which is already the request namespace (no effect)", desc, ns))
			} else {
				notes = append(notes, fmt.Sprintf("%s names %q, already in the read set via %s (redundant)", desc, ns, claimedBy[ns]))
			}
			return
		}
		seen[ns] = true
		claimedBy[ns] = desc
		entries = append(entries, doctorReadEntry{ns: ns, tiers: tiers, source: source})
	}
	addSubtree := func(base, tiers, desc string) {
		prefix := base + "/"
		for _, n := range allNamespaces {
			if n != base && strings.HasPrefix(n, prefix) {
				add(n, tiers, "subtree-pattern", desc+" subtree")
			}
		}
	}

	for _, l := range links {
		if l.Namespace != primary {
			continue
		}
		tiers := "durable"
		if l.Tiers == "all" {
			tiers = "all"
		}
		base, isSubtree := strings.CutSuffix(l.Target, "/*")
		desc := fmt.Sprintf("link to %q", l.Target)
		add(base, tiers, "link", desc)
		if isSubtree {
			addSubtree(base, tiers, desc)
		}
	}

	if globalNamespace != "" {
		add(globalNamespace, "durable", "global", "MEMINI_GLOBAL_NAMESPACE")
	}

	for _, rn := range readNamespaces {
		base, isSubtree := strings.CutSuffix(rn, "/*")
		desc := fmt.Sprintf("MEMINI_READ_NAMESPACES entry %q", rn)
		add(base, "durable", "env", desc)
		if isSubtree {
			addSubtree(base, "durable", desc)
		}
	}

	return entries, notes
}

// statsTotal returns ns's memory count from stats, and whether ns appears in
// stats at all (a namespace with no memories, ever, never appears).
func statsTotal(stats []nsStat, ns string) (int, bool) {
	for _, s := range stats {
		if s.namespace == ns {
			return s.total, true
		}
	}
	return 0, false
}

// orUnsetList formats a string slice for display the way orUnset formats a
// single value: "(unset)" when empty, else comma-joined.
func orUnsetList(vs []string) string {
	if len(vs) == 0 {
		return "(unset)"
	}
	return strings.Join(vs, ", ")
}

// printNamespaceLinks prints ns's outgoing persistent links, or "none".
func printNamespaceLinks(out io.Writer, ns string, links []store.NamespaceLink) {
	if len(links) == 0 {
		fmt.Fprintf(out, "  links (%s): none\n", ns) //nolint:errcheck
		return
	}
	fmt.Fprintf(out, "  links (%s):\n", ns) //nolint:errcheck
	for _, l := range links {
		fmt.Fprintf(out, "    -> %s (tiers=%s)\n", l.Target, l.Tiers) //nolint:errcheck
	}
}

// printRetrievalScope reports the read-set inputs (MEMINI_GLOBAL_NAMESPACE,
// MEMINI_READ_NAMESPACES, persistent namespace links) and the resolved
// effective read set for the plugin-resolved namespace, so "why does recall
// see/miss X" is answerable without reading the resolver's source. Degrades
// gracefully when the backend doesn't implement store.LinkStore: the links
// lines note that instead of failing, and the effective read set is still
// shown (env + global only, since no links can be consulted).
func printRetrievalScope(ctx context.Context, out io.Writer, cfg *config.Config, st store.Store, stats []nsStat, serverNS, pluginNS string) int {
	var warnings int
	fmt.Fprintln(out, "Retrieval scope")                                                 //nolint:errcheck
	fmt.Fprintf(out, "  MEMINI_GLOBAL_NAMESPACE: %s\n", orUnset(cfg.GlobalNamespace))    //nolint:errcheck
	fmt.Fprintf(out, "  MEMINI_READ_NAMESPACES:  %s\n", orUnsetList(cfg.ReadNamespaces)) //nolint:errcheck

	var pluginLinks []store.NamespaceLink
	if ls, ok := st.(store.LinkStore); !ok {
		fmt.Fprintln(out, "  links: not supported by this backend") //nolint:errcheck
	} else {
		serverLinks, err := ls.ListNamespaceLinks(ctx, serverNS)
		if err != nil {
			warnings++
			warnf(out, "cannot list links for %q: %v", serverNS, err)
		} else {
			printNamespaceLinks(out, serverNS, serverLinks)
		}
		if pluginNS == serverNS {
			pluginLinks = serverLinks
		} else if pluginLinks, err = ls.ListNamespaceLinks(ctx, pluginNS); err != nil {
			warnings++
			warnf(out, "cannot list links for %q: %v", pluginNS, err)
		} else {
			printNamespaceLinks(out, pluginNS, pluginLinks)
		}
	}

	allNamespaces := make([]string, len(stats))
	for i, s := range stats {
		allNamespaces[i] = s.namespace
	}
	entries, notes := resolveDoctorReadSet(pluginNS, cfg.ReadNamespaces, cfg.GlobalNamespace, pluginLinks, allNamespaces)

	fmt.Fprintf(out, "  effective read set for %q (plain recall/briefing, no per-call namespaces list):\n", pluginNS) //nolint:errcheck
	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "  NAMESPACE\tTIERS\tSOURCE") //nolint:errcheck
	for _, e := range entries {
		fmt.Fprintf(tw, "  %s\t%s\t%s\n", e.ns, e.tiers, e.source) //nolint:errcheck
	}
	_ = tw.Flush()

	for _, n := range notes {
		warnings++
		warnf(out, "%s.", n)
	}
	for _, e := range entries {
		if e.source == "default" {
			continue
		}
		if total, _ := statsTotal(stats, e.ns); total == 0 {
			warnings++
			warnf(out, "read-set entry %q (via %s) currently holds 0 memories.", e.ns, e.source)
		}
	}
	if len(entries) > doctorReadSetClamp {
		warnings++
		warnf(out, "resolved read set has %d entries, above the %d-entry clamp; recall/briefing drops the tail.",
			len(entries), doctorReadSetClamp)
	}
	fmt.Fprintln(out) //nolint:errcheck
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
