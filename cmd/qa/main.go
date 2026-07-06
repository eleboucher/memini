// Command qa measures end-to-end QA accuracy (LLM-judged) over a benchmark
// conversation corpus: ingest into memini (direct upserts or the production
// write path), answer each question with the shipped service.Answer, and grade
// against the reference with per-category judge rubrics. This is the
// answer-quality companion to cmd/bench's retrieval scores.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/eleboucher/memini/bench"
	"github.com/eleboucher/memini/internal/embed"
	"github.com/eleboucher/memini/internal/llm"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/search"
	"github.com/eleboucher/memini/internal/service"
	"github.com/eleboucher/memini/internal/store/sqlitevec"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "qa:", err)
		os.Exit(1)
	}
}

type result struct {
	Index    int    `json:"i"`
	Category string `json:"category"`
	Correct  bool   `json:"correct"`
}

func run() error {
	suite := flag.String("suite", "locomo", "locomo | longmemeval")
	data := flag.String("data", "", "dataset path")
	holdout := flag.String("holdout", "all", "longmemeval question split: tune (450) | held (50) | all")
	sessionDoc := flag.String("session-doc", "full", "longmemeval doc construction: full | user-only | dated")
	ingestMode := flag.String("ingest", "upsert",
		"corpus ingestion: upsert (direct store writes) | write (production Remember path: classify, gates, dedup, corroborate/contradict)")
	k := flag.Int("k", 10, "recalled memories per answer")
	limit := flag.Int("limit", 0, "cap questions (0 = all)")
	workers := flag.Int("workers", 6, "concurrent question workers")
	ckptPath := flag.String("checkpoint", "", "resume checkpoint (JSONL; default bench/results/qa_<suite>_<ingest>.jsonl)")
	dbg := flag.Bool("debug", false, "print per-question retrieval/answer/grade to stderr")
	temporalBoost := flag.Float64("temporal-boost", 0.40, "temporal targeting boost (0 disables)")
	flag.Parse()
	debug = *dbg
	if *ckptPath == "" {
		*ckptPath = fmt.Sprintf("bench/results/qa_%s_%s.jsonl", *suite, *ingestMode)
	}

	dims := envInt("MEMINI_EMBED_DIMS", 4096)
	client, err := embed.NewOpenAI(embed.OpenAIConfig{
		BaseURL: os.Getenv("MEMINI_EMBED_BASE_URL"), APIKey: os.Getenv("MEMINI_EMBED_API_KEY"),
		Model: os.Getenv("MEMINI_EMBED_MODEL"), Dims: dims,
	})
	if err != nil {
		return err
	}
	embedder, err := embed.NewCached(embed.NewBatched(client, 20, 24000, 8000), 16384)
	if err != nil {
		return err
	}
	chat, err := llm.New(llm.API(os.Getenv("MEMINI_LLM_API")), llm.Config{
		BaseURL: os.Getenv("MEMINI_LLM_BASE_URL"), APIKey: os.Getenv("MEMINI_LLM_API_KEY"),
		Model: os.Getenv("MEMINI_LLM_MODEL"),
	})
	if err != nil {
		return err
	}

	ds, err := loadSuite(*suite, *data, *sessionDoc, *holdout)
	if err != nil {
		return err
	}
	if *limit > 0 && *limit < len(ds.Questions) {
		subsetQuestions(ds, *limit)
	}
	fmt.Fprintf(os.Stderr, "suite %s (%s ingest): %d items, %d questions, model %s\n",
		ds.Name, *ingestMode, len(ds.Items), len(ds.Questions), os.Getenv("MEMINI_LLM_MODEL"))

	ctx := context.Background()
	dbPath := filepath.Join(os.TempDir(), fmt.Sprintf("memini-qa-%s-%s.db", *suite, *ingestMode))
	_ = os.Remove(dbPath)
	st, err := sqlitevec.Open(ctx, dbPath, dims)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	// One answering service per conversation, clocked at that conversation's
	// question date: "last year" in a 2023 conversation must resolve against
	// 2023, not the machine clock. Falls back to a shared unclocked service.
	// The recall stack mirrors the shipped server (temporal targeting, fused
	// min-score, durable reserve).
	answerOpts := []service.Option{
		service.WithSyncReinforce(),
		service.WithAnswerer(chat),
		service.WithTemporalTargeting(*temporalBoost, search.RegexAnchorExtractor{}),
		service.WithRecallMinScore(0.1),
		service.WithRecallSemanticReserve(2),
	}
	base := service.New(st, embedder, answerOpts...)
	svcByGroup := map[string]*service.Service{}
	for _, q := range ds.Questions {
		if q.Now.IsZero() || svcByGroup[q.Group] != nil {
			continue
		}
		qNow := q.Now
		svcByGroup[q.Group] = service.New(st, embedder,
			append([]service.Option{service.WithClock(func() time.Time { return qNow })}, answerOpts...)...)
	}
	svcFor := func(group string) *service.Service {
		if s, ok := svcByGroup[group]; ok {
			return s
		}
		return base
	}

	fmt.Fprintf(os.Stderr, "ingesting %d items (%s)...\n", len(ds.Items), *ingestMode)
	switch *ingestMode {
	case "upsert":
		err = ingestUpsert(ctx, st, embedder, ds.Items)
	case "write":
		err = ingestWrite(ctx, st, embedder, ds.Items)
	default:
		err = fmt.Errorf("unknown -ingest %q (want upsert|write)", *ingestMode)
	}
	if err != nil {
		return err
	}

	done, err := loadCheckpoint(*ckptPath)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "resuming: %d/%d already done\n", len(done), len(ds.Questions))

	ckpt, err := os.OpenFile(*ckptPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer func() { _ = ckpt.Close() }()
	var mu sync.Mutex

	var wg sync.WaitGroup
	jobs := make(chan int)
	var processed int
	for w := 0; w < *workers; w++ {
		wg.Go(func() {
			for i := range jobs {
				q := ds.Questions[i]
				correct, err := answerAndJudge(ctx, svcFor(q.Group), chat, q, *k)
				if err != nil {
					fmt.Fprintf(os.Stderr, "q%d error: %v\n", i, err)
					continue
				}
				mu.Lock()
				if err := json.NewEncoder(ckpt).Encode(result{Index: i, Category: q.Category, Correct: correct}); err != nil {
					// A checkpoint that can't be trusted is worse than none:
					// a torn line breaks resume and silently re-bills questions.
					fmt.Fprintf(os.Stderr, "q%d: checkpoint write failed: %v\n", i, err)
				}
				processed++
				if processed%25 == 0 {
					fmt.Fprintf(os.Stderr, "  ...%d processed\n", processed)
				}
				mu.Unlock()
			}
		})
	}
	for i := range ds.Questions {
		if !done[i] {
			jobs <- i
		}
	}
	close(jobs)
	wg.Wait()

	return report(*ckptPath, len(ds.Questions))
}

// loadSuite loads and shapes the dataset. Abstention questions (LongMemEval
// "_abs" ids) get their own category bucket so the report and the judge treat
// them as decline-to-answer checks.
func loadSuite(suite, data, sessionDoc, holdout string) (*bench.Dataset, error) {
	if data == "" {
		return nil, fmt.Errorf("-data is required")
	}
	switch suite {
	case "locomo":
		return bench.LoadLoCoMo(data)
	case "longmemeval":
		ds, err := bench.LoadLongMemEval(data, bench.DocMode(sessionDoc))
		if err != nil {
			return nil, err
		}
		ds, err = bench.SplitHoldout(ds, holdout)
		if err != nil {
			return nil, err
		}
		for i, q := range ds.Questions {
			if strings.HasSuffix(q.Group, "_abs") {
				ds.Questions[i].Category = q.Category + "_abs"
			}
		}
		return ds, nil
	default:
		return nil, fmt.Errorf("unknown suite %q (want locomo|longmemeval)", suite)
	}
}

// subsetQuestions samples evenly across the dataset so a small limit still
// spans all conversations instead of truncating to the first one, and prunes
// items to the surviving groups.
func subsetQuestions(ds *bench.Dataset, limit int) {
	step := float64(len(ds.Questions)) / float64(limit)
	sampled := make([]bench.Question, 0, limit)
	for i := 0; i < limit; i++ {
		sampled = append(sampled, ds.Questions[int(float64(i)*step)])
	}
	ds.Questions = sampled
	groups := map[string]bool{}
	for _, q := range ds.Questions {
		groups[q.Group] = true
	}
	kept := ds.Items[:0]
	for _, it := range ds.Items {
		if groups[it.Group] {
			kept = append(kept, it)
		}
	}
	ds.Items = kept
}

// ingestUpsert loads items directly into the store (retrieval-only baseline):
// semantic tier, dated at the session time so temporal targeting can aim.
func ingestUpsert(ctx context.Context, st *sqlitevec.Store, e embed.Embedder, items []bench.Item) error {
	const batch = 25
	now := time.Unix(1_700_000_000, 0).UTC()
	for start := 0; start < len(items); start += batch {
		end := min(start+batch, len(items))
		texts := make([]string, end-start)
		for i, it := range items[start:end] {
			texts[i] = it.Content
		}
		vecs, err := e.Embed(ctx, texts)
		if err != nil {
			return err
		}
		for i, it := range items[start:end] {
			ts := now
			var validFrom *time.Time
			if !it.Time.IsZero() {
				ts = it.Time
				validFrom = &it.Time
			}
			if err := st.Upsert(ctx, &memory.Memory{
				ID: it.ID, Namespace: nsOf(it.Group), Tier: memory.TierSemantic, Content: it.Content,
				CreatedAt: ts, UpdatedAt: ts, LastAccessedAt: ts, ValidFrom: validFrom,
				Embedding: vecs[i],
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

// ingestWrite feeds items through service.Remember sequentially in dataset
// order, mirroring the shipped server's no-LLM write wiring (tier
// classification, gates, fingerprint/write dedup, corroboration, contradiction
// invalidation, heuristic extract). The LLM is deliberately NOT wired into
// ingest — distill/consolidate over a full benchmark corpus would be thousands
// of completions; this run measures the write path's curation, not its LLM
// enrichment. Each write is clocked at its item's session time, and TTL is
// overridden to never-expire: classifier-routed episodic memories carry a 90d
// TTL, and benchmark question dates can fall months after the sessions — the
// bench measures answer quality, not retention policy.
func ingestWrite(ctx context.Context, st *sqlitevec.Store, e embed.Embedder, items []bench.Item) error {
	ingestNow := time.Unix(1_700_000_000, 0).UTC()
	svc := service.New(st, e,
		service.WithSyncReinforce(),
		service.WithClock(func() time.Time { return ingestNow }),
		service.WithWriteDedup(0.625, service.WriteDedupHint),
		service.WithCorroboration(0.70),
		service.WithContradictionDownrank(0.625),
		service.WithEpisodicMinChars(120),
		service.WithExtractOnWrite(true),
	)
	never := -time.Second
	var gated, merged int
	seen := map[string]bool{}
	for _, it := range items {
		if !it.Time.IsZero() {
			ingestNow = it.Time.UTC()
		}
		var validFrom *time.Time
		if !it.Time.IsZero() {
			vf := it.Time.UTC()
			validFrom = &vf
		}
		m, err := svc.Remember(ctx, service.RememberInput{
			Namespace: nsOf(it.Group), Content: it.Content, TTL: &never, ValidFrom: validFrom,
		})
		if err != nil {
			return fmt.Errorf("write-mode ingest %s: %w", it.ID, err)
		}
		if m == nil { // dropped by the episodic low-signal gate: accepted, not stored
			gated++
			continue
		}
		if seen[m.ID] {
			merged++
		}
		seen[m.ID] = true
	}
	svc.WaitBackground()
	fmt.Fprintf(os.Stderr, "write-mode ingest: %d items -> %d memories, %d gated, %d merged\n",
		len(items), len(seen), gated, merged)
	return nil
}

func nsOf(group string) string {
	if group == "" {
		return "default"
	}
	return group
}

// Judge rubrics. The base rubric grades fact equivalence; knowledge-update and
// temporal questions get the leniency the official LongMemEval evaluation
// applies, and abstention questions grade the decline itself.
const judgeBase = "You grade answers. Given a question, the reference answer, and a candidate answer, " +
	"reply with exactly CORRECT or INCORRECT. The candidate is CORRECT if it conveys the same key fact(s) " +
	"as the reference, even if phrased differently or with extra words."

const judgeKnowledgeUpdate = judgeBase +
	" The reference is the UPDATED value of a fact that changed over time: the candidate is CORRECT if it " +
	"states the updated value (even if it also mentions the earlier value as outdated), and INCORRECT if it " +
	"gives only the earlier, superseded value."

const judgeTemporal = judgeBase +
	" Dates within one day of the reference are CORRECT (timezone and relative-date arithmetic slack)."

const judgeAbstention = "You grade answers to questions that are NOT answerable from the conversation the " +
	"candidate saw. Reply with exactly CORRECT or INCORRECT. The candidate is CORRECT only if it declines to " +
	"answer — says it doesn't know, the information wasn't mentioned, or the question can't be answered. Any " +
	"substantive invented answer is INCORRECT."

func judgeSystemFor(category string) string {
	switch {
	case strings.HasSuffix(category, "_abs"):
		return judgeAbstention
	case category == "knowledge-update":
		return judgeKnowledgeUpdate
	case category == "temporal-reasoning":
		return judgeTemporal
	default:
		return judgeBase
	}
}

var debug bool

// answerAndJudge runs the production answer path (recall + service.Answer's
// reader prompt) and grades the reply against the reference.
func answerAndJudge(ctx context.Context, svc *service.Service, chat llm.Completer, q bench.Question, k int) (bool, error) {
	res, err := svc.Answer(ctx, service.AnswerInput{Namespace: q.Group, Query: q.Query, Limit: k})
	if err != nil {
		return false, err
	}
	ref := q.Answer
	if ref == "" {
		ref = "(no reference; unanswerable)"
	}
	grade, err := chat.Complete(ctx, judgeSystemFor(q.Category),
		fmt.Sprintf("Question: %s\nReference: %s\nCandidate: %s\nGrade:", q.Query, ref, res.Answer))
	if err != nil {
		return false, err
	}
	g := strings.ToUpper(grade)
	correct := strings.Contains(g, "CORRECT") && !strings.Contains(g, "INCORRECT")
	if debug {
		fmt.Fprintf(os.Stderr, "\n[Q] %s\n[group=%s cat=%s sources=%d]\n[gold] %s\n[answer] %s\n[grade] %s => %v\n",
			q.Query, q.Group, q.Category, len(res.Sources), q.Answer, res.Answer, strings.TrimSpace(grade), correct)
	}
	return correct, nil
}

func loadCheckpoint(path string) (map[int]bool, error) {
	done := map[int]bool{}
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return done, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var r result
		if json.Unmarshal(sc.Bytes(), &r) == nil {
			done[r.Index] = true
		}
	}
	return done, sc.Err()
}

func report(path string, total int) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	var correct, n int
	perCatCorrect := map[string]int{}
	perCatTotal := map[string]int{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var r result
		if json.Unmarshal(sc.Bytes(), &r) != nil {
			continue
		}
		n++
		perCatTotal[r.Category]++
		if r.Correct {
			correct++
			perCatCorrect[r.Category]++
		}
	}
	acc := 0.0
	if n > 0 {
		acc = float64(correct) / float64(n) * 100
	}
	fmt.Printf("\nQA accuracy (LLM-judge): %.1f%%  (%d/%d answered; %d total questions)\n",
		acc, correct, n, total)
	fmt.Println("by category:")
	for cat, tot := range perCatTotal {
		fmt.Printf("  category %s: %.1f%% (%d/%d)\n", cat, float64(perCatCorrect[cat])/float64(tot)*100, perCatCorrect[cat], tot)
	}
	return nil
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			return n
		}
	}
	return def
}
