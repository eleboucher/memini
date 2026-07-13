package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
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
	"github.com/eleboucher/memini/internal/nsresolve"
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
	fmt.Fprintf(out, "  home namespace:  %s\n", orUnset(cfg.Home))               //nolint:errcheck
	if serverNS != pluginNS {
		warnings++
		warnf(out, "the plugin reads/writes %q but the server's header-less default is %q.", pluginNS, serverNS)
		note(out, "Plugins send an explicit namespace header, but bare MCP clients use the server")
		note(out, fmt.Sprintf("default, so they disagree. Pin one with MEMINI_DEFAULT_NAMESPACE=%q.", pluginNS))
	}
	warnings += warnGlobalNamespacePin(out, cwd, envNS)
	warnings += warnHomeUnset(out, cfg.Home)
	warnings += warnLingeringDeadFiles(out)
	fmt.Fprintln(out) //nolint:errcheck

	if baseURL := serverBaseURL(); baseURL != "" {
		warnings += runHandshakeProbe(cmd.Context(), out, baseURL, serverAPIKey(), cwd, pluginNS, pluginSrc)
		fmt.Fprintln(out) //nolint:errcheck
	}

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
	warnings += warnEnvSlashMigration(out, cfg, stats)

	entries, rsSource, rsErr := resolveReadSet(cmd.Context(), st, pluginNS, cfg.Home)
	if rsErr != nil {
		warnings++
		warnf(out, "could not resolve the read set for %q: %v", pluginNS, rsErr)
	} else {
		printRetrievalScope(out, entries, rsSource, pluginNS)
	}
	noteDanglingLinks(cmd.Context(), out, st, stats)
	noteDanglingKeyBindings(cmd.Context(), out, st, stats)

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

// nsStat is the per-namespace summary doctor reports.

// Tier-access label and the unset-value placeholder doctor's retrieval-scope
// output uses (constants to keep goconst quiet, not exported semantics).
const (
	tiersAll   = "all"
	labelUnset = "(unset)"
)

// localStoreLabel is the generic "no remote, opened the configured backend
// directly" source label, shared with import.go's local-store description
// (both name the same concept) so golangci-lint's goconst doesn't flag two
// independent literals for it.
const localStoreLabel = "local store"

// Origin labels for a read-set entry, mirroring internal/service/readset.go's
// Origin* constants (OriginPrimary/OriginAncestor/OriginHome/OriginLink) —
// duplicated as literal strings rather than importing internal/service for
// them: they're also exactly the "origin" values the REST read-set endpoint
// (GET /v1/namespaces/readset) puts on the wire, so a local literal here
// doubles as the JSON vocabulary fetchServerReadSet parses.
const (
	originPrimary  = "primary"
	originAncestor = "ancestor"
	originHome     = "home"
	originLink     = "link"
)

// doctorReadEntry is one namespace in doctor's reconstruction (or the
// server's own answer, via fetchServerReadSet) of a plain recall/briefing's
// default read set for a namespace: mirrors internal/service's
// ReadSetEntry/ReadSetEntryItem.
type doctorReadEntry struct {
	ns     string
	origin string // "primary", "ancestor", "home", or "link" (see the origin* constants)
	tiers  string // "all" (the request's own tier filter) or a comma-joined tier list
}

// durableTierNames is the tier list every ancestor/home cascade leg carries:
// doctor always reconstructs the *default* read set (no per-call tier
// filter, matching internal/service's ResolveReadSetInfo, the introspection
// endpoint's own resolution), so the durable-tier restriction on those legs
// is always the full set — semantic and procedural, the only tiers that ever
// cross a namespace boundary. Only a link's own tier override can narrow it
// further (see intersectLinkTiers).
var durableTierNames = []string{string(memory.TierSemantic), string(memory.TierProcedural)}

// ancestorsOf returns every proper path prefix of ns, nearest first:
// "acme/phoenix/api" -> ["acme/phoenix", "acme"]. Duplicates
// internal/service/readset.go's unexported ancestorsOf — doctor
// reconstructs the resolver's cascade wholesale rather than importing
// internal/service's resolution machinery, the same precedent as the
// read-set/origin duplication above.
func ancestorsOf(ns string) []string {
	var out []string
	for i := strings.LastIndexByte(ns, '/'); i > 0; i = strings.LastIndexByte(ns[:i], '/') {
		out = append(out, ns[:i])
	}
	return out
}

// intersectLinkTiers restricts a link's own tier override to the durable
// set, mirroring internal/service/readset.go's intersectDurableTiers: an
// empty override means the full durable set; a non-empty one is intersected
// with it — the global tier rule (only semantic/procedural cross namespace
// boundaries) always wins over the link's own configuration. May return an
// empty slice when the link only lists non-durable tiers.
func intersectLinkTiers(linkTiers []memory.Tier) []string {
	if len(linkTiers) == 0 {
		return durableTierNames
	}
	var out []string
	for _, t := range []memory.Tier{memory.TierSemantic, memory.TierProcedural} {
		if slices.Contains(linkTiers, t) {
			out = append(out, string(t))
		}
	}
	return out
}

// localReadSet mirrors internal/service/readset.go's resolveDefaultReadSet
// for a store-only doctor run, with no server to ask: primary itself, then
// the cascade legs in order — ancestors (nearest first), home, then stored
// links — each contributing durable tiers only. Each leg is skipped when
// already present (widest-tiers-wins is moot here: ancestors/home always
// carry the full durable set already, the widest a leg can grant under
// doctor's fixed "no per-call tier filter" resolution). Degrades gracefully
// against a store predating LinkStore (no links leg).
func localReadSet(ctx context.Context, st store.Store, primary, home string) ([]doctorReadEntry, error) {
	entries := []doctorReadEntry{{ns: primary, origin: originPrimary, tiers: tiersAll}}
	seen := map[string]bool{primary: true}

	add := func(ns, origin string, tiers []string) {
		if ns == "" || seen[ns] || len(tiers) == 0 {
			return
		}
		seen[ns] = true
		entries = append(entries, doctorReadEntry{ns: ns, origin: origin, tiers: strings.Join(tiers, ",")})
	}

	for _, a := range ancestorsOf(primary) {
		add(a, originAncestor, durableTierNames)
	}

	if home != "" {
		add(home, originHome, durableTierNames)
	}

	ls, ok := st.(store.LinkStore)
	if !ok {
		return entries, nil
	}
	links, err := ls.ListLinks(ctx, primary)
	if err != nil {
		return nil, fmt.Errorf("read-set: list links: %w", err)
	}
	for _, l := range links {
		add(l.Dst, originLink, intersectLinkTiers(l.Tiers))
	}
	return entries, nil
}

// doctorReadSetTimeout bounds doctor's optional "prefer the server"
// read-set lookup, so an unreachable/hung server falls back to the local
// mirror promptly instead of hanging the whole command.
const doctorReadSetTimeout = 3 * time.Second

// remoteReadSetEntry/remoteReadSetResponse mirror the REST API's
// ReadSetEntryItem/ReadSetResponse (api/openapi.yaml, GET
// /v1/namespaces/readset): Tiers omitted means the request's own tier
// filter, unrestricted beyond that.
type remoteReadSetEntry struct {
	Namespace string   `json:"namespace"`
	Origin    string   `json:"origin"`
	Tiers     []string `json:"tiers,omitempty"`
}

type remoteReadSetResponse struct {
	Entries []remoteReadSetEntry `json:"entries"`
}

// serverBaseURL and serverAPIKey mirror the plugin hooks' env vars
// (plugin/scripts/_shared.mjs REST_URL/SECRET): MEMINI_BASE_URL (alias
// MEMINI_URL) and MEMINI_API_KEY (alias MEMINI_TOKEN). Doctor is a
// store-only CLI otherwise; these are opt-in so `doctor` can prefer a
// running server's own read-set resolution over reconstructing it locally.
func serverBaseURL() string { return firstNonEmptyEnv("MEMINI_BASE_URL", "MEMINI_URL") }
func serverAPIKey() string  { return firstNonEmptyEnv("MEMINI_API_KEY", "MEMINI_TOKEN") }

// tiersLabelFromStrings renders a wire-format tier list the same way
// doctorReadEntry.tiers does locally: an empty/omitted list is the request's
// own filter, unrestricted ("all"); otherwise the tiers joined verbatim.
func tiersLabelFromStrings(tiers []string) string {
	if len(tiers) == 0 {
		return tiersAll
	}
	return strings.Join(tiers, ",")
}

// fetchServerReadSet calls GET /v1/namespaces/readset on baseURL,
// header-scoped to primary (X-Memini-Namespace) and home (X-Memini-Home,
// when set) — config.DefaultNamespaceHeader/DefaultHomeHeader. Returns an
// error on any failure (unreachable, non-2xx, malformed body) so the caller
// can fall back to the local mirror: doctor must never hard-fail because a
// server it merely prefers happens to be down.
func fetchServerReadSet(ctx context.Context, baseURL, apiKey, primary, home string) ([]doctorReadEntry, error) {
	ctx, cancel := context.WithTimeout(ctx, doctorReadSetTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(baseURL, "/")+"/v1/namespaces/readset", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set(config.DefaultNamespaceHeader, primary)
	if home != "" {
		req.Header.Set(config.DefaultHomeHeader, home)
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("server returned %s", resp.Status)
	}
	var parsed remoteReadSetResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, err
	}
	entries := make([]doctorReadEntry, len(parsed.Entries))
	for i, e := range parsed.Entries {
		entries[i] = doctorReadEntry{ns: e.Namespace, origin: e.Origin, tiers: tiersLabelFromStrings(e.Tiers)}
	}
	return entries, nil
}

// resolveReadSet returns primary's effective read set: the server's own
// resolution when MEMINI_BASE_URL/MEMINI_URL is configured and reachable
// (preferred — it reflects the exact resolver a live recall/briefing uses,
// including any server-side link/home data doctor's local store might not
// have), else localReadSet's store-only mirror. The second return value
// names which source produced it, for display.
func resolveReadSet(ctx context.Context, st store.Store, primary, home string) ([]doctorReadEntry, string, error) {
	if baseURL := serverBaseURL(); baseURL != "" {
		if entries, err := fetchServerReadSet(ctx, baseURL, serverAPIKey(), primary, home); err == nil {
			return entries, "server (" + baseURL + ")", nil
		}
		// Configured but unreachable/erroring: fall through to the local
		// mirror rather than failing doctor over a server it only prefers.
	}
	entries, err := localReadSet(ctx, st, primary, home)
	return entries, localStoreLabel, err
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

// warnEnvSlashMigration flags a migration hazard fixed alongside read sets:
// MEMINI_DEFAULT_NAMESPACE / MEMINI_NAMESPACE values containing "/" (e.g.
// "team/project") used to be flattened to their basename ("project"); they
// are now preserved as-is (see config.sanitizeNamespacePath). A deployment
// upgrading across that fix now reads/writes the full path, "team/project",
// while pre-upgrade data may still sit under the old basename, "project";
// two namespaces silently diverging unless the operator notices. Only fires
// when the basename actually holds memories; a fresh deployment (or one that
// already migrated) has nothing there and gets no warning.
func warnEnvSlashMigration(out io.Writer, cfg *config.Config, stats []nsStat) int {
	if cfg.NamespaceSrc != config.NamespaceFromEnv || !strings.Contains(cfg.DefaultNamespace, "/") {
		return 0
	}
	basename := filepath.Base(cfg.DefaultNamespace)
	if basename == cfg.DefaultNamespace || basename == "." || basename == string(filepath.Separator) {
		return 0
	}
	total, ok := statsTotal(stats, basename)
	if !ok || total == 0 {
		return 0
	}
	warnf(out, "MEMINI_DEFAULT_NAMESPACE %q contains \"/\"; older memini versions flattened this to %q, "+
		"which still holds %d memories, while reads/writes now go to %q.", cfg.DefaultNamespace, basename, total, cfg.DefaultNamespace)
	note(out, fmt.Sprintf("Merge the old data forward with: memini namespace move --from %s --to %s", basename, cfg.DefaultNamespace))
	return 1
}

// printRetrievalScope renders primary's effective read set — resolved by
// resolveReadSet, either the server's own answer or localReadSet's mirror —
// as a NAMESPACE/ORIGIN/TIERS table, so "why does recall see/miss X" is
// answerable without reading the resolver's source. It no longer prints
// MEMINI_GLOBAL_NAMESPACE or a tenant-shared namespace: both are dead knobs
// under the ancestor/home/link cascade doctor now reflects (config still
// carries the fields until T12 deletes them; doctor just stops reading them
// here).
func printRetrievalScope(out io.Writer, entries []doctorReadEntry, source, primary string) {
	fmt.Fprintf(out, "Effective read set for %q (source: %s)\n", primary, source) //nolint:errcheck
	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "  NAMESPACE\tORIGIN\tTIERS") //nolint:errcheck
	for _, e := range entries {
		fmt.Fprintf(tw, "  %s\t%s\t%s\n", e.ns, e.origin, e.tiers) //nolint:errcheck
	}
	_ = tw.Flush()
	fmt.Fprintln(out) //nolint:errcheck
}

// noteDanglingLinks flags every stored link whose dst namespace holds no
// memories yet, across the whole store (ListAllLinks), not just primary's
// own outgoing links — a stale or forward-looking link anywhere surfaces
// here. This is a note, not a warning: linking ahead of a namespace's first
// write is legal (e.g. provisioning a link before a team's first commit
// lands there), so it doesn't count toward doctor's warning tally.
// Degrades gracefully against a store predating LinkStore.
func noteDanglingLinks(ctx context.Context, out io.Writer, st store.Store, stats []nsStat) {
	ls, ok := st.(store.LinkStore)
	if !ok {
		return
	}
	links, err := ls.ListAllLinks(ctx)
	if err != nil {
		notef(out, "could not list links to check for dangling destinations: %v", err)
		return
	}
	sort.Slice(links, func(i, j int) bool {
		if links[i].Src != links[j].Src {
			return links[i].Src < links[j].Src
		}
		return links[i].Dst < links[j].Dst
	})
	for _, l := range links {
		if total, ok := statsTotal(stats, l.Dst); ok && total > 0 {
			continue
		}
		notef(out, "link %s -> %s: destination has no memories yet (links to future namespaces are legal).", l.Src, l.Dst)
	}
}

// noteDanglingKeyBindings flags every API key in this store's api_keys table
// (store.APIKeyStore — not MEMINI_API_KEYS_FILE, which doctor never loads;
// see cmd/memini/key.go's own "this store's api_keys table only" scoping)
// whose HomeNS or DefaultNS names a namespace with zero memories yet. Like
// noteDanglingLinks, this is a note, not a warning: namespaces exist
// implicitly the moment something is written to them, so binding a key ahead
// of its own first write (e.g. provisioning a new hire's key before their
// first session) is a legal, expected pattern rather than a
// misconfiguration, and it never counts toward doctor's warning tally.
// Degrades gracefully against a store predating APIKeyStore.
func noteDanglingKeyBindings(ctx context.Context, out io.Writer, st store.Store, stats []nsStat) {
	ks, ok := st.(store.APIKeyStore)
	if !ok {
		return
	}
	keys, err := ks.ListAPIKeys(ctx)
	if err != nil {
		notef(out, "could not list api keys to check for dangling namespace bindings: %v", err)
		return
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].Name < keys[j].Name })
	for _, k := range keys {
		if k.HomeNS != "" {
			if total, ok := statsTotal(stats, k.HomeNS); !ok || total == 0 {
				notef(out, "key %q: home namespace %q has no memories yet (dangling bindings are legal by design).", k.Name, k.HomeNS)
			}
		}
		if k.DefaultNS != "" {
			if total, ok := statsTotal(stats, k.DefaultNS); !ok || total == 0 {
				notef(out, "key %q: default namespace %q has no memories yet (dangling bindings are legal by design).", k.Name, k.DefaultNS)
			}
		}
	}
}

// warnGlobalNamespacePin flags MEMINI_NAMESPACE/MEMINI_DEFAULT_NAMESPACE
// (envNS) pinned while this cwd's git-derived namespace would resolve to
// something else: a common "catch-all trap" — a global export silently
// redirects every repo's writes/reads to one namespace regardless of cwd,
// which is exactly how an oversized shared pool accumulates. Silent when no
// env override is set, or it happens to agree with what git resolves here.
func warnGlobalNamespacePin(out io.Writer, cwd, envNS string) int {
	if envNS == "" {
		return 0
	}
	gitNS, _ := config.ResolveDirNamespace(cwd)
	if gitNS == "" || gitNS == envNS {
		return 0
	}
	warnf(out, "MEMINI_NAMESPACE/MEMINI_DEFAULT_NAMESPACE pins every namespace resolution to %q, "+
		"but this directory's git-derived namespace is %q.", envNS, gitNS)
	note(out, "A global pin silently overrides per-repo isolation everywhere it's exported — the same trap that")
	note(out, "collapses unrelated repos into one catch-all pool. Recover isolation with `memini namespace split`,")
	note(out, "or scope the pin to this repo instead of exporting it globally.")
	return 1
}

// warnHomeUnset flags a missing MEMINI_HOME (home, Config.Home): without it,
// visibility:"personal" writes error out and no personal-namespace leg
// merges into recall/briefing's default read set.
func warnHomeUnset(out io.Writer, home string) int {
	if home != "" {
		return 0
	}
	warnf(out, "MEMINI_HOME is unset: visibility:\"personal\" writes will error, and no personal-namespace leg merges into recall/briefing.")
	note(out, "Set MEMINI_HOME=<your-personal-namespace> (e.g. personal/kit) to enable it.")
	return 1
}

// notef prints an indented informational line that does not count toward
// doctor's warning tally — for a condition that's legal/expected but worth
// surfacing, distinct from warnf's WARN-prefixed, tallied lines.
func notef(out io.Writer, format string, args ...any) {
	fmt.Fprintf(out, "  note: "+format+"\n", args...) //nolint:errcheck
}

// --- lingering dead files: the retired client-side config/cache the Go side
// never reads (the handshake redesign moved that intent server-side) ---

// configDirFor/cacheDirFor return $XDG_CONFIG_HOME/memini and
// $XDG_CACHE_HOME/memini (or the ~/.config, ~/.cache fallback), mirroring
// packages/memini-client's own default paths for the files it used to write:
// overrides.json under CONFIG, project-map.json and namespace under CACHE.
// "" when even $HOME can't be resolved, in which case the check for that
// base is skipped rather than guessed at.
func configDirFor() string { return xdgDir("XDG_CONFIG_HOME", ".config") }
func cacheDirFor() string  { return xdgDir("XDG_CACHE_HOME", ".cache") }

func xdgDir(envVar, fallbackLeaf string) string {
	base := strings.TrimSpace(os.Getenv(envVar))
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		base = filepath.Join(home, fallbackLeaf)
	}
	return filepath.Join(base, "memini")
}

// warnLingeringDeadFiles flags on-disk files the config-handshake redesign
// retired: the Go CLI no longer reads any of them (this is the only doctor
// check that even looks at their path), but a file left over from before the
// redesign is easy to mistake for live config. Each present file gets its
// own WARN naming exactly what to do about it; a missing/unreadable XDG base
// degrades to silence, same as every other doctor check that touches the
// filesystem.
func warnLingeringDeadFiles(out io.Writer) int {
	var n int
	if dir := configDirFor(); dir != "" {
		n += warnIfFileExists(out, filepath.Join(dir, "overrides.json"), "no longer read; migrate to server pins")
	}
	if dir := cacheDirFor(); dir != "" {
		n += warnIfFileExists(out, filepath.Join(dir, "project-map.json"), "legacy cache; safe to delete")
		n += warnIfFileExists(out, filepath.Join(dir, "namespace"), "legacy cache; safe to delete")
	}
	return n
}

// warnIfFileExists is an os.Stat existence check, not a content read: a
// retired file's presence is the whole signal, and doctor has no reason to
// parse a format it no longer honors.
func warnIfFileExists(out io.Writer, path, reason string) int {
	if _, err := os.Stat(path); err != nil {
		return 0
	}
	warnf(out, "%s: %s", path, reason)
	return 1
}

// --- handshake probe: doctor's own client of POST /v1/handshake ---

// doctorHandshakeTimeout bounds the handshake probe the same way
// doctorReadSetTimeout bounds the read-set fetch: an unreachable server must
// not hang doctor.
const doctorHandshakeTimeout = 3 * time.Second

// handshakeClientName identifies doctor's own handshake probe in server-side
// logging/diagnostics (HandshakeRequest.client.name, api/openapi.yaml) — a
// live agent's handshake always sends its own name instead, so this value
// showing up in server logs unambiguously means "someone ran doctor here."
const handshakeClientName = "memini-doctor"

// handshakeProbeRequest/handshakeProbeProject/handshakeProbeClient mirror the
// REST API's HandshakeRequest (api/openapi.yaml, POST /v1/handshake) —
// doctor sends exactly the facts config.PluginFacts gathers for
// ResolvePluginNamespace, so the probe and the local derivation start from
// identical inputs and any difference in the answer is a genuine server-side
// rule (a pin, a key default), never a facts mismatch. Local mirror types
// rather than internal/api/rest's generated ones, matching
// remoteReadSetEntry/remoteReadSetResponse's precedent above: doctor only
// ever needs a handful of response fields, and a hand-rolled shape makes that
// explicit instead of decoding into (and mostly discarding) the full
// generated response.
type handshakeProbeRequest struct {
	Project handshakeProbeProject `json:"project"`
	Client  handshakeProbeClient  `json:"client"`
}

type handshakeProbeProject struct {
	RemoteURL        string `json:"remote_url,omitempty"`
	ToplevelPath     string `json:"toplevel_path,omitempty"`
	ToplevelBasename string `json:"toplevel_basename,omitempty"`
	CwdBasename      string `json:"cwd_basename"`
	Agent            string `json:"agent,omitempty"`
	EnvNamespace     string `json:"env_namespace,omitempty"`
}

type handshakeProbeClient struct {
	Name string `json:"name,omitempty"`
}

// handshakeProbeResponse mirrors only the HandshakeResponse fields
// (api/openapi.yaml) doctor reports on; settings/settings_sources/read_set
// are decoded and discarded (doctor already has its own read-set view via
// fetchServerReadSet, above).
type handshakeProbeResponse struct {
	Namespace       string                 `json:"namespace"`
	NamespaceSource string                 `json:"namespace_source"`
	Identity        handshakeProbeIdentity `json:"identity"`
}

type handshakeProbeIdentity struct {
	Authenticated    bool   `json:"authenticated"`
	KeyName          string `json:"key_name,omitempty"`
	Home             string `json:"home,omitempty"`
	DefaultNamespace string `json:"default_namespace,omitempty"`
}

// probeHandshake calls POST /v1/handshake on baseURL with facts, identifying
// itself as client.name handshakeClientName. Returns an error on any failure
// (unreachable, non-2xx, malformed body) — unlike fetchServerReadSet there is
// no local mirror to fall back to, so the caller reports the failure itself
// as the diagnostic.
func probeHandshake(ctx context.Context, baseURL, apiKey string, facts nsresolve.Facts) (handshakeProbeResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, doctorHandshakeTimeout)
	defer cancel()

	body, err := json.Marshal(handshakeProbeRequest{
		Project: handshakeProbeProject{
			RemoteURL:        facts.RemoteURL,
			ToplevelPath:     facts.ToplevelPath,
			ToplevelBasename: facts.ToplevelBasename,
			CwdBasename:      facts.CwdBasename,
			Agent:            facts.Agent,
			EnvNamespace:     facts.EnvNamespace,
		},
		Client: handshakeProbeClient{Name: handshakeClientName},
	})
	if err != nil {
		return handshakeProbeResponse{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(baseURL, "/")+"/v1/handshake", bytes.NewReader(body))
	if err != nil {
		return handshakeProbeResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return handshakeProbeResponse{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return handshakeProbeResponse{}, fmt.Errorf("server returned %s", resp.Status)
	}
	var parsed handshakeProbeResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return handshakeProbeResponse{}, err
	}
	return parsed, nil
}

// runHandshakeProbe performs doctor's optional handshake probe (only reached
// when MEMINI_BASE_URL is configured, see runDoctor): POST /v1/handshake with
// the same facts config.PluginFacts gathers for ResolvePluginNamespace, then
// reports the server's answer against the local one (localNS/localSrc).
// Unlike resolveReadSet's silent fallback, a failed probe has no local mirror
// to fall back to — MEMINI_BASE_URL being configured but unreachable is
// itself worth a WARN, since there is nothing else to show for it.
func runHandshakeProbe(ctx context.Context, out io.Writer, baseURL, apiKey, cwd, localNS string, localSrc config.NamespaceSource) int {
	resp, err := probeHandshake(ctx, baseURL, apiKey, config.PluginFacts(cwd))
	if err != nil {
		warnf(out, "handshake probe to %s failed: %v", baseURL, err)
		return 1
	}
	reportHandshakeProbe(out, resp, localNS, localSrc)
	return 0
}

// reportHandshakeProbe prints the handshake probe's resolved namespace,
// identity, and how it relates to ResolvePluginNamespace's own answer
// (localNS/localSrc): they can legitimately differ — a server-side pin or a
// key's own default namespace applies only there — so a difference is
// explained via the probe's own namespace_source (handshakeMismatchReason)
// rather than just flagged. This never adds to doctor's warning tally: an
// explained divergence is not a problem, and an unexplained one still reads
// as "worth investigating" rather than an alarm.
func reportHandshakeProbe(out io.Writer, resp handshakeProbeResponse, localNS string, localSrc config.NamespaceSource) {
	fmt.Fprintf(out, "Handshake probe: namespace %q (%s)\n", resp.Namespace, resp.NamespaceSource) //nolint:errcheck
	id := resp.Identity
	fmt.Fprintf(out, "  identity: authenticated=%v", id.Authenticated) //nolint:errcheck
	if id.KeyName != "" {
		fmt.Fprintf(out, ", key=%q", id.KeyName) //nolint:errcheck
	}
	if id.Home != "" {
		fmt.Fprintf(out, ", home=%q", id.Home) //nolint:errcheck
	}
	if id.DefaultNamespace != "" {
		fmt.Fprintf(out, ", default_namespace=%q", id.DefaultNamespace) //nolint:errcheck
	}
	fmt.Fprintln(out) //nolint:errcheck

	if resp.Namespace == localNS {
		fmt.Fprintf(out, "  matches the local derivation (%s)\n", localSrc) //nolint:errcheck
		return
	}
	fmt.Fprintf(out, "  differs from the local derivation %q (%s):\n", localNS, localSrc) //nolint:errcheck
	if reason, ok := handshakeMismatchReason(resp.NamespaceSource); ok {
		note(out, reason)
		return
	}
	note(out, fmt.Sprintf("server resolved via %q; no known legitimate-divergence rule matches — worth investigating.", resp.NamespaceSource))
}

// handshakeMismatchReason names WHY the handshake probe's namespace can
// legitimately outrank the local derivation, keyed by the probe's own
// namespace_source (nsresolve's vocabulary, api/openapi.yaml
// HandshakeResponse.namespace_source): a pin or a key's own default are
// expected divergences server-side derivation alone can't see, not bugs — so
// doctor explains them by name instead of just alarming. ok is false for any
// other source, which reportHandshakeProbe treats as unexplained.
func handshakeMismatchReason(source string) (reason string, ok bool) {
	switch source {
	case nsresolve.SourcePin:
		return "the server has an operator-created pin for this project, which outranks local derivation.", true
	case nsresolve.SourceKeyDefault:
		return "this API key carries its own default namespace, used because nothing else resolved server-side.", true
	default:
		return "", false
	}
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
		return labelUnset
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
