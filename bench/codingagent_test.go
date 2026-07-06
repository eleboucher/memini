//go:build bench

package bench_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/eleboucher/memini/bench"
	"github.com/eleboucher/memini/internal/embed"
	"github.com/eleboucher/memini/internal/llm"
	"github.com/eleboucher/memini/internal/search"
	"github.com/eleboucher/memini/internal/service"
	"github.com/eleboucher/memini/internal/store/sqlitevec"
)

// codingAgentData is the committed pilot dataset (relative to the bench package
// dir, which is the cwd when `go test` runs the package).
const codingAgentData = "data/codingagent_pilot.json"

// caCell identifies one 2x2 discrimination cell: an ingest path crossed with an
// answer strategy.
type caCell struct{ ingest, level string }

// TestCodingAgentGoldAudit is the offline referential-integrity tripwire for the
// pilot dataset: it needs no endpoints and runs in milliseconds, so it guards
// every edit to codingagent_pilot.json. A dataset with dangling gold, a bad
// category, a non-monotonic supersession chain, or a distractor leaked into a
// gold set is worse than no benchmark, so these are hard failures.
func TestCodingAgentGoldAudit(t *testing.T) {
	ds, meta, err := bench.LoadCodingAgent(codingAgentData)
	if err != nil {
		t.Fatalf("load %s: %v", codingAgentData, err)
	}

	itemIDs := make(map[string]bool, len(ds.Items))
	itemGroups := map[string]bool{}
	itemTime := map[string]time.Time{}
	for _, it := range ds.Items {
		if itemIDs[it.ID] {
			t.Errorf("duplicate item id %q", it.ID)
		}
		itemIDs[it.ID] = true
		itemGroups[it.Group] = true
		itemTime[it.ID] = it.Time
		if it.Time.IsZero() {
			t.Errorf("item %q: zero time", it.ID)
		}
		if it.Session == "" {
			t.Errorf("item %q: empty session", it.ID)
		}
		if it.Source == "" {
			t.Errorf("item %q: empty source (provenance is required for gold verification)", it.ID)
		}
	}

	// Distractors are fictional debris; they must never be a gold answer.
	for id, kind := range meta.Kind {
		if kind == "distractor" && !strings.HasPrefix(id, "d-") {
			t.Errorf("distractor %q should use the d- id prefix", id)
		}
	}

	// Supersession chains: the target must exist and be strictly newer, so
	// "what superseded what" has an unambiguous latest version.
	for id, target := range meta.SupersededBy {
		if !itemIDs[target] {
			t.Errorf("item %q superseded_by %q which does not exist", id, target)
			continue
		}
		if !itemTime[id].Before(itemTime[target]) {
			t.Errorf("item %q (%s) not strictly older than its successor %q (%s)",
				id, itemTime[id].Format("2006-01-02"), target, itemTime[target].Format("2006-01-02"))
		}
	}

	perCat := map[string]int{}
	for qi, q := range ds.Questions {
		if !bench.CodingAgentCategories[q.Category] {
			t.Errorf("question %d (%q): unknown category %q", qi, q.Query, q.Category)
		}
		perCat[q.Category]++
		if !itemGroups[q.Group] {
			t.Errorf("question %d (%q): group %q matches no item", qi, q.Query, q.Group)
		}
		for _, g := range q.Gold {
			if !itemIDs[g] {
				t.Errorf("question %d (%q): gold %q missing from items", qi, q.Query, g)
			}
			if meta.Kind[g] == "distractor" {
				t.Errorf("question %d (%q): gold %q is a distractor", qi, q.Query, g)
			}
		}
		for _, g := range q.GoldAll {
			if !itemIDs[g] {
				t.Errorf("question %d (%q): gold_all %q missing from items", qi, q.Query, g)
			}
			if meta.Kind[g] == "distractor" {
				t.Errorf("question %d (%q): gold_all %q is a distractor", qi, q.Query, g)
			}
		}
		switch q.Category {
		case "abstention":
			if len(q.Gold) != 0 || q.Answer != "" {
				t.Errorf("question %d (%q): abstention must have empty gold and empty answer", qi, q.Query)
			}
		case "synthesis":
			if len(q.GoldAll) < 2 {
				t.Errorf("question %d (%q): synthesis needs gold_all with >=2 evidence ids, got %d", qi, q.Query, len(q.GoldAll))
			}
			if q.Answer == "" {
				t.Errorf("question %d (%q): synthesis needs a reference answer", qi, q.Query)
			}
		default:
			if len(q.Gold) == 0 {
				t.Errorf("question %d (%q): non-abstention needs at least one gold id", qi, q.Query)
			}
			if q.Answer == "" {
				t.Errorf("question %d (%q): non-abstention needs a reference answer", qi, q.Query)
			}
		}
	}

	// Loose quota bands: a pilot with real headroom needs a distractor-rich
	// corpus and enough questions per category for a paired signal. These are
	// sanity floors, not the full v1 quotas.
	if len(ds.Items) < 100 {
		t.Errorf("corpus too small for headroom: %d items (want >=100)", len(ds.Items))
	}
	if len(ds.Questions) < 30 {
		t.Errorf("too few questions for a paired signal: %d (want >=30)", len(ds.Questions))
	}
	t.Logf("dataset %q: %d items, %d questions", ds.Name, len(ds.Items), len(ds.Questions))
	cats := make([]string, 0, len(perCat))
	for c := range perCat {
		cats = append(cats, c)
	}
	sort.Strings(cats)
	for _, c := range cats {
		t.Logf("  category %-16s %d", c, perCat[c])
	}
}

// TestCodingAgentHeadroom measures retrieval headroom: recall@{1,5,10} for the
// upsert and write-path ingests, per category, plus coverage@k over the full
// synthesis evidence sets. It needs a live embedder (skips without one). The
// pre-registered gate (see bench/CODINGAGENT.md) is hybrid-upsert recall@5 in
// [50%, 88%]: above the band the corpus is too easy (add distractors), below it
// the gold is too narrow (re-mine).
func TestCodingAgentHeadroom(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	t.Cleanup(cancel)

	ds, _, err := bench.LoadCodingAgent(codingAgentData)
	if err != nil {
		t.Fatalf("load %s: %v", codingAgentData, err)
	}
	e := codingAgentEmbedder(ctx, t)
	dims := envIntOr("MEMINI_EMBED_DIMS", 1024)
	queryPrefix := os.Getenv("MEMINI_EMBED_QUERY_PREFIX")
	recallNow := latestNow(ds)
	ks := []int{1, 5, 10}

	for _, mode := range []bench.IngestMode{bench.IngestUpsert, bench.IngestWrite} {
		st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "headroom-"+string(mode)+".db"), dims)
		if err != nil {
			t.Fatalf("open store: %v", err)
		}
		sys := bench.MeminiSystemsOpts(st, e, bench.SystemOpts{
			Concurrency: 4, QueryPrefix: queryPrefix, FusionAlpha: 0.5,
			Mode: mode, Dated: true, RecallNow: recallNow,
		})
		hybrid := sys[0]
		results, err := bench.Run(ctx, hybrid, ds, ks)
		if err != nil {
			t.Fatalf("run %s: %v", mode, err)
		}
		// Vector/keyword legs share the ingested backend; report all three at k=5.
		var all []bench.Result
		for _, s := range sys {
			r, err := bench.Run(ctx, s, ds, ks)
			if err != nil {
				t.Fatalf("run %s/%s: %v", mode, s.Name(), err)
			}
			for _, res := range r {
				if res.K == 5 {
					all = append(all, res)
				}
			}
		}
		fmt.Printf("\n=== ingest=%s ===\n%s\n", mode, bench.Markdown(all))
		fmt.Printf("per-category recall (hybrid):\n%s\n", perCatTable(results))
		printCoverage(ctx, t, hybrid, ds, ks)

		if mode == bench.IngestUpsert {
			r5 := recallAtK(results, 5)
			verdict := "IN BAND"
			switch {
			case r5 >= 0.90:
				verdict = "TOO EASY (>=90%): add synthetic distractors and re-run"
			case r5 > 0.88:
				verdict = "high edge of band"
			case r5 < 0.40:
				verdict = "TOO HARD (<40%): gold too narrow, re-mine"
			case r5 < 0.50:
				verdict = "low edge of band"
			}
			fmt.Printf("\nHEADROOM GATE: hybrid-upsert recall@5 = %.1f%% -> %s\n", r5*100, verdict)
		}
		_ = st.Close()
	}
}

// TestCodingAgentDiscrimination runs the 2x2 that LongMemEval could not separate:
// {extract, distill} write-path ingest x {single-shot, agentic} answering. It
// needs a live embedder and MEMINI_LLM_* (skips otherwise). Each of the four
// cells ingests its own fresh store (re-ingest, not file-copy: copying a live
// WAL-mode SQLite DB with vec0/fts5 shadow tables corrupts it, and a shared
// store would let the first answer pass's recall reinforcement contaminate the
// second). Results checkpoint to bench/results so a long run resumes.
func TestCodingAgentDiscrimination(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
	t.Cleanup(cancel)

	ds, _, err := bench.LoadCodingAgent(codingAgentData)
	if err != nil {
		t.Fatalf("load %s: %v", codingAgentData, err)
	}
	e := codingAgentEmbedder(ctx, t)
	dims := envIntOr("MEMINI_EMBED_DIMS", 1024)
	chat := codingAgentChat(t, "MEMINI_LLM")
	judge := chat
	if os.Getenv("MEMINI_JUDGE_MODEL") != "" || os.Getenv("MEMINI_JUDGE_BASE_URL") != "" {
		judge = codingAgentChat(t, "MEMINI_JUDGE")
	}

	if err := os.MkdirAll("results", 0o755); err != nil {
		t.Fatalf("mkdir results: %v", err)
	}

	verdicts := map[caCell][]bool{} // per-question correctness, question order
	catAcc := map[caCell]map[string][2]int{}
	distillStats := map[string]bench.DistillStats{}
	chatStats := map[caCell]bench.ChatStats{}

	levels := []struct {
		name  string
		level service.ReasoningLevel
	}{
		{"single", ""},
		{"agentic", "medium"},
	}

	// Each of the four cells ingests its own fresh store. Copying a live WAL-mode
	// SQLite DB with vec0/fts5 shadow tables corrupts it, and sharing one store
	// across the two answer levels would let recall reinforcement from the first
	// pass (AccessCount/usage bumps) contaminate the second. Re-ingesting is the
	// robust choice: the extract ingest is deterministic (no LLM, fixed order) so
	// its two cells get identical stores and C1 is clean; the two distill ingests
	// are independent LLM draws, so within-distill contrasts carry ingest variance
	// (noted in CODINGAGENT.md).
	for _, ingest := range []string{"extract", "distill"} {
		for _, lv := range levels {
			key := caCell{ingest, lv.name}
			ckpt := filepath.Join("results", fmt.Sprintf("codingagent_%s_%s.jsonl", ingest, lv.name))
			cellPath := filepath.Join(t.TempDir(), fmt.Sprintf("ca-%s-%s.db", ingest, lv.name))
			cellSt, err := sqlitevec.Open(ctx, cellPath, dims)
			if err != nil {
				t.Fatalf("open cell store %v: %v", key, err)
			}
			var distiller llm.Distiller
			var counting *bench.CountingDistiller
			if ingest == "distill" {
				counting = bench.NewCountingDistiller(chat)
				distiller = counting
			}
			t.Logf("cell %v: ingesting %d items...", key, len(ds.Items))
			if err := bench.IngestQAWrite(ctx, cellSt, e, ds.Items, distiller); err != nil {
				t.Fatalf("ingest %v: %v", key, err)
			}
			if counting != nil {
				distillStats[ingest] = counting.Stats()
			}
			cc := bench.NewCountingChat(chat)
			v, ca := runAnswerCell(ctx, t, cellSt, e, chat, cc, judge, ds, lv.level, ckpt)
			verdicts[key] = v
			catAcc[key] = ca
			chatStats[key] = cc.Stats()
			_ = cellSt.Close()
		}
	}

	printDiscrimination(t, ds, verdicts, catAcc, distillStats, chatStats)
}

// runAnswerCell answers every question over cellSt at the given reasoning level,
// resuming from ckpt. Returns per-question correctness (question order) and a
// per-category [correct,total] tally.
func runAnswerCell(
	ctx context.Context, t *testing.T, cellSt *sqlitevec.Store, e embed.Embedder,
	answerer llm.Client, cc *bench.CountingChat, judge llm.Completer,
	ds *bench.Dataset, level service.ReasoningLevel, ckpt string,
) ([]bool, map[string][2]int) {
	t.Helper()

	// One answering service per distinct question date, clocked at that date so
	// temporal targeting and as-of recall resolve against the query's "now".
	answerOpts := []service.Option{
		service.WithSyncReinforce(),
		service.WithAnswerer(cc),
		service.WithTemporalTargeting(0.40, search.RegexAnchorExtractor{}),
		service.WithRecallMinScore(0.1),
		service.WithRecallSemanticReserve(2),
	}
	svcByNow := map[int64]*service.Service{}
	svcFor := func(now time.Time) *service.Service {
		key := now.UnixNano()
		if s, ok := svcByNow[key]; ok {
			return s
		}
		n := now
		s := service.New(cellSt, e, append([]service.Option{
			service.WithClock(func() time.Time { return n }),
		}, answerOpts...)...)
		svcByNow[key] = s
		return s
	}

	done := loadResultCkpt(ckpt)
	f, err := os.OpenFile(ckpt, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open checkpoint %s: %v", ckpt, err)
	}
	defer func() { _ = f.Close() }()

	correct := make([]bool, len(ds.Questions))
	for i, q := range ds.Questions {
		if v, ok := done[i]; ok {
			correct[i] = v
			continue
		}
		// Local JIT model servers (LM Studio) transiently unload the chat model
		// when the embedder is hit on the same endpoint, returning a 400
		// "Model unloaded"; the next request reloads it. Retry so one eviction
		// doesn't abort a multi-hour run.
		var ok bool
		var err error
		for attempt := 0; attempt < 6; attempt++ {
			ok, _, err = bench.AnswerAndJudge(ctx, svcFor(q.Now), judge, q, 10, level)
			if err == nil {
				break
			}
			t.Logf("q%d (%s/%v) attempt %d: %v", i, q.Category, level, attempt, err)
			time.Sleep(time.Duration(attempt+1) * 2 * time.Second)
		}
		if err != nil {
			t.Fatalf("answer q%d (%s/%v) after retries: %v", i, q.Category, level, err)
		}
		correct[i] = ok
		if err := json.NewEncoder(f).Encode(ckptRow{Index: i, Category: q.Category, Correct: ok}); err != nil {
			t.Fatalf("checkpoint q%d: %v", i, err)
		}
	}

	catAcc := map[string][2]int{}
	for i, q := range ds.Questions {
		c := catAcc[q.Category]
		c[1]++
		if correct[i] {
			c[0]++
		}
		catAcc[q.Category] = c
	}
	return correct, catAcc
}

// printDiscrimination renders the per-cell accuracy tables, the pre-registered
// paired contrasts (McNemar exact p), and the cost table.
func printDiscrimination(
	t *testing.T, ds *bench.Dataset,
	verdicts map[caCell][]bool,
	catAcc map[caCell]map[string][2]int,
	distillStats map[string]bench.DistillStats,
	chatStats map[caCell]bench.ChatStats,
) {
	t.Helper()
	cells := []caCell{{"extract", "single"}, {"extract", "agentic"}, {"distill", "single"}, {"distill", "agentic"}}

	cats := map[string]bool{}
	for _, q := range ds.Questions {
		cats[q.Category] = true
	}
	catList := make([]string, 0, len(cats))
	for c := range cats {
		catList = append(catList, c)
	}
	sort.Strings(catList)

	var b strings.Builder
	fmt.Fprintf(&b, "\n## Discrimination — %d questions, 2x2 (ingest x answer)\n\n", len(ds.Questions))
	fmt.Fprintf(&b, "| category | ext/single | ext/agentic | dist/single | dist/agentic |\n")
	fmt.Fprintf(&b, "|----------|-----------:|------------:|------------:|-------------:|\n")
	for _, c := range catList {
		fmt.Fprintf(&b, "| %s |", c)
		for _, k := range cells {
			cell := catAcc[k][c]
			fmt.Fprintf(&b, " %.0f%% (%d/%d) |", pctOf(cell[0], cell[1]), cell[0], cell[1])
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "| **overall** |")
	for _, k := range cells {
		c, n := trueCount(verdicts[k]), len(verdicts[k])
		fmt.Fprintf(&b, " **%.0f%% (%d/%d)** |", pctOf(c, n), c, n)
	}
	b.WriteString("\n")

	fmt.Fprintf(&b, "\n### Pre-registered paired contrasts (McNemar exact)\n\n")
	contrast(&b, "C1 agentic vs single (extract)", ds, verdicts[caCell{"extract", "agentic"}], verdicts[caCell{"extract", "single"}])
	contrast(&b, "C2 distill vs extract (single)", ds, verdicts[caCell{"distill", "single"}], verdicts[caCell{"extract", "single"}])
	contrast(&b, "C3 distill+agentic vs extract+single", ds, verdicts[caCell{"distill", "agentic"}], verdicts[caCell{"extract", "single"}])

	fmt.Fprintf(&b, "\n### Cost\n\n")
	for ingest, s := range distillStats {
		fmt.Fprintf(&b, "distill(%s): %d calls, %d episodes-in, %d facts-out, ~%d in / ~%d out tokens\n",
			ingest, s.Calls, s.Episodes, s.Facts, s.InTokens, s.OutTokens)
	}
	for _, k := range cells {
		s := chatStats[k]
		fmt.Fprintf(&b, "answer(%s/%s): %d completes, %d tool-rounds, ~%d in / ~%d out tokens\n",
			k.ingest, k.level, s.Completes, s.ToolRounds, s.InTokens, s.OutTokens)
	}
	fmt.Print(b.String())
}

// contrast prints arm A vs arm B: per-question win/loss/tie, the discordant
// question indices (for inspection), and the two-sided McNemar exact p.
func contrast(b *strings.Builder, name string, ds *bench.Dataset, a, bb []bool) {
	if len(a) != len(bb) || len(a) != len(ds.Questions) {
		fmt.Fprintf(b, "- %s: incomplete (a=%d b=%d q=%d)\n", name, len(a), len(bb), len(ds.Questions))
		return
	}
	var aWin, bWin, tie int
	var aWinIdx, bWinIdx []int
	for i := range a {
		switch {
		case a[i] && !bb[i]:
			aWin++
			aWinIdx = append(aWinIdx, i)
		case !a[i] && bb[i]:
			bWin++
			bWinIdx = append(bWinIdx, i)
		default:
			tie++
		}
	}
	p := bench.McNemarExact(aWin, bWin)
	accA, accB := pctOf(trueCount(a), len(a)), pctOf(trueCount(bb), len(bb))
	fmt.Fprintf(b, "- **%s**: %.0f%% vs %.0f%% (Δ %+.0fpp); A-only=%d B-only=%d tie=%d; McNemar p=%.4f%s\n",
		name, accA, accB, accA-accB, aWin, bWin, tie, p, sig(p))
	fmt.Fprintf(b, "    A-only q: %v ; B-only q: %v\n", aWinIdx, bWinIdx)
}

func sig(p float64) string {
	if p < 0.05 {
		return " *"
	}
	return ""
}

// printCoverage reports coverage@k over the full synthesis evidence sets: the
// mean fraction of a question's gold_all present in the top-k. Recall_any (in
// Run) credits a single hit; synthesis needs the whole set, so coverage is the
// honest metric for it.
func printCoverage(ctx context.Context, t *testing.T, sys bench.System, ds *bench.Dataset, ks []int) {
	t.Helper()
	maxK := slices.Max(ks)
	sums := map[int]float64{}
	var n int
	for _, q := range ds.Questions {
		if q.Category != "synthesis" || len(q.GoldAll) == 0 {
			continue
		}
		n++
		hits, err := sys.Recall(ctx, q.Group, q.Query, maxK)
		if err != nil {
			t.Fatalf("coverage recall: %v", err)
		}
		for _, k := range ks {
			got := map[string]bool{}
			for _, h := range hits[:min(k, len(hits))] {
				for _, id := range h.IDs {
					got[id] = true
				}
			}
			var c int
			for _, g := range q.GoldAll {
				if got[g] {
					c++
				}
			}
			sums[k] += float64(c) / float64(len(q.GoldAll))
		}
	}
	if n == 0 {
		return
	}
	var parts []string
	for _, k := range ks {
		parts = append(parts, fmt.Sprintf("coverage@%d=%.0f%%", k, sums[k]/float64(n)*100))
	}
	fmt.Printf("synthesis (%d q): %s\n", n, strings.Join(parts, "  "))
}

// --- small helpers ---

type ckptRow struct {
	Index    int    `json:"i"`
	Category string `json:"category"`
	Correct  bool   `json:"correct"`
}

func loadResultCkpt(path string) map[int]bool {
	done := map[int]bool{}
	f, err := os.Open(path)
	if err != nil {
		return done
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var r ckptRow
		if json.Unmarshal(sc.Bytes(), &r) == nil {
			done[r.Index] = r.Correct
		}
	}
	return done
}

func codingAgentEmbedder(ctx context.Context, t *testing.T) embed.Embedder {
	t.Helper()
	baseURL := envOr("MEMINI_EMBED_BASE_URL", "http://127.0.0.1:8001/v1")
	model := envOr("MEMINI_EMBED_MODEL", "text-embedding-qwen3-embedding-0.6b")
	dims := envIntOr("MEMINI_EMBED_DIMS", 1024)
	client, err := embed.NewOpenAI(embed.OpenAIConfig{BaseURL: baseURL, Model: model, Dims: dims})
	if err != nil {
		t.Skipf("embedder config: %v", err)
	}
	probeEmbedder(ctx, t, baseURL, model)
	e, err := embed.NewCached(embed.NewBatched(client, 20, 24000, 8000), 16384)
	if err != nil {
		t.Fatalf("embedder cache: %v", err)
	}
	return e
}

func codingAgentChat(t *testing.T, prefix string) llm.Client {
	t.Helper()
	base := os.Getenv(prefix + "_BASE_URL")
	model := os.Getenv(prefix + "_MODEL")
	if base == "" && model == "" {
		t.Skipf("%s_* not set: skipping LLM answer/discrimination", prefix)
	}
	c, err := llm.New(llm.API(os.Getenv(prefix+"_API")), llm.Config{
		BaseURL: base, APIKey: os.Getenv(prefix + "_API_KEY"), Model: model,
	})
	if err != nil {
		t.Fatalf("%s client: %v", prefix, err)
	}
	return c
}

func latestNow(ds *bench.Dataset) time.Time {
	var latest time.Time
	for _, q := range ds.Questions {
		if q.Now.After(latest) {
			latest = q.Now
		}
	}
	return latest
}

func recallAtK(results []bench.Result, k int) float64 {
	for _, r := range results {
		if r.K == k {
			return r.RecallAtK
		}
	}
	return 0
}

func perCatTable(results []bench.Result) string {
	var r5 *bench.Result
	for i := range results {
		if results[i].K == 5 {
			r5 = &results[i]
		}
	}
	if r5 == nil || r5.PerCategory == nil {
		return "(none)"
	}
	cats := make([]string, 0, len(r5.PerCategory))
	for c := range r5.PerCategory {
		cats = append(cats, c)
	}
	sort.Strings(cats)
	var b strings.Builder
	for _, c := range cats {
		fmt.Fprintf(&b, "  %-16s %.0f%%\n", c, r5.PerCategory[c]*100)
	}
	return b.String()
}

func trueCount(bs []bool) int {
	n := 0
	for _, b := range bs {
		if b {
			n++
		}
	}
	return n
}

func pctOf(a, n int) float64 {
	if n == 0 {
		return 0
	}
	return float64(a) / float64(n) * 100
}
