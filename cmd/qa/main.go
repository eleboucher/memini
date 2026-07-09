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
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/eleboucher/memini/bench"
	"github.com/eleboucher/memini/internal/embed"
	"github.com/eleboucher/memini/internal/llm"
	"github.com/eleboucher/memini/internal/search"
	"github.com/eleboucher/memini/internal/service"
	"github.com/eleboucher/memini/internal/store/sqlitevec"
)

// qaHTTPWithHeaders returns an *http.Client that injects headers from
// the MEMINI_HTTP_HEADERS env var (format: "key:value;key:value"). Returns
// nil when unset, so callers fall back to the SDK default.
func qaHTTPWithHeaders() *http.Client {
	raw := os.Getenv("MEMINI_HTTP_HEADERS")
	if raw == "" {
		return nil
	}
	headers := make(map[string]string)
	for _, pair := range strings.Split(raw, ";") {
		kv := strings.SplitN(pair, ":", 2)
		if len(kv) == 2 {
			headers[strings.TrimSpace(kv[0])] = strings.TrimSpace(kv[1])
		}
	}
	if len(headers) == 0 {
		return nil
	}
	return &http.Client{
		Timeout:   120 * time.Second,
		Transport: &headerTransport{base: http.DefaultTransport, headers: headers},
	}
}

type headerTransport struct {
	base    http.RoundTripper
	headers map[string]string
}

func (t *headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	for k, v := range t.headers {
		req.Header.Set(k, v)
	}
	return t.base.RoundTrip(req)
}

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
	suite := flag.String("suite", "locomo", "locomo | longmemeval | codingagent")
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
	reasoning := flag.String("reasoning", "",
		"answer reasoning level: empty/minimal (single-shot) | expand (multi-query rewrite) | low | medium | high (agentic tool loop)")
	distill := flag.Bool("distill", false,
		"with -ingest=write: distill each episodic capture into durable facts at write time "+
			"via the MEMINI_LLM_* chat backend (supersedes the heuristic extractor, as in production)")
	flag.Parse()
	debug = *dbg
	if *distill && *ingestMode != "write" { //nolint:goconst // bench CLI flag string
		return fmt.Errorf("-distill requires -ingest=write")
	}
	if *ckptPath == "" {
		*ckptPath = defaultCheckpoint(*suite, *ingestMode, *reasoning, *distill)
	}

	dims := envInt("MEMINI_EMBED_DIMS", 4096)
	client, err := embed.NewOpenAI(embed.OpenAIConfig{
		BaseURL: os.Getenv("MEMINI_EMBED_BASE_URL"), APIKey: os.Getenv("MEMINI_EMBED_API_KEY"),
		Model: os.Getenv("MEMINI_EMBED_MODEL"), Dims: dims,
		HTTPClient: qaHTTPWithHeaders(),
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
		Model:      os.Getenv("MEMINI_LLM_MODEL"),
		HTTPClient: qaHTTPWithHeaders(),
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
	var distiller llm.Distiller
	var consolidator llm.Consolidator
	if *distill {
		distiller = chat
	}
	// Wire consolidation independently of distill when LLM is available and
	// the write path is active — it's async and doesn't block ingestion.
	if *ingestMode == "write" {
		consolidator = chat
	}
	switch *ingestMode {
	case "upsert":
		err = bench.IngestQAUpsert(ctx, st, embedder, ds.Items)
	case "write":
		err = bench.IngestQAWrite(ctx, st, embedder, ds.Items, distiller, consolidator)
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
				correct, ans, err := bench.AnswerAndJudge(ctx, svcFor(q.Group), chat, q, *k, service.ReasoningLevel(*reasoning))
				if err != nil {
					fmt.Fprintf(os.Stderr, "q%d error: %v\n", i, err)
					continue
				}
				if debug {
					fmt.Fprintf(os.Stderr, "\n[Q] %s\n[group=%s cat=%s]\n[gold] %s\n[answer] %s\n[correct] %v\n",
						q.Query, q.Group, q.Category, q.Answer, ans, correct)
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

// defaultCheckpoint names the resume checkpoint after everything that shapes
// the answers, so differently-configured runs never share one.
func defaultCheckpoint(suite, ingestMode, reasoning string, distill bool) string {
	suffix := ""
	if distill {
		suffix = "_distill"
	}
	if reasoning != "" && reasoning != "minimal" {
		suffix += "_" + reasoning
	}
	return fmt.Sprintf("bench/results/qa_%s_%s%s.jsonl", suite, ingestMode, suffix)
}

// loadSuite loads and shapes the dataset. Abstention questions (LongMemEval
// "_abs" ids) get their own category bucket so the report and the judge treat
// them as decline-to-answer checks.
func loadSuite(suite, data, sessionDoc, holdout string) (*bench.Dataset, error) {
	if data == "" {
		return nil, fmt.Errorf("-data is required")
	}
	switch suite {
	case "codingagent":
		ds, _, err := bench.LoadCodingAgent(data)
		return ds, err
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
		return nil, fmt.Errorf("unknown suite %q (want locomo|longmemeval|codingagent)", suite)
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

var debug bool

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
