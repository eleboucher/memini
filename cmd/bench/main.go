package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/eleboucher/memini/bench"
	"github.com/eleboucher/memini/internal/embed"
	"github.com/eleboucher/memini/internal/embed/embedtest"
	"github.com/eleboucher/memini/internal/llm"
	rerankpkg "github.com/eleboucher/memini/internal/rerank"
	"github.com/eleboucher/memini/internal/store"
	"github.com/eleboucher/memini/internal/store/sqlitevec"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "bench:", err)
		os.Exit(1)
	}
}

func run() error {
	suite := flag.String("suite", "sample", "sample | file | longmemeval | locomo | locomo-sessions")
	data := flag.String("data", "", "dataset path (for file/longmemeval/locomo)")
	k := flag.String("k", "5", "retrieval cutoff K (comma-separated for multiple, e.g. 5,10)")
	dims := flag.Int("dims", 256, "embedding dimensions (local embedder)")
	limit := flag.Int("limit", 0, "cap the number of questions (0 = all)")
	concurrency := flag.Int("concurrency", 8, "parallel embedding workers during ingest")
	outDir := flag.String("out", "bench/results", "directory for JSON results")
	rerank := flag.Bool("rerank", false, "compare pure-RRF vs recency-aware composite ranking (needs a timestamped dataset, e.g. longmemeval)")
	rerankCats := flag.String("rerank-cats", "knowledge-update,temporal-reasoning",
		"comma-separated question categories for -rerank (empty = all)")
	fusionAlpha := flag.Float64("fusion", 0.5,
		"hybrid fusion: >=0 = score fusion with this vector weight (0.5 = balanced, default); <0 = RRF")
	poolFactor := flag.Int("pool-factor", 0, "per-leg recall pool factor (max(k*factor, floor); 0 = default)")
	poolFloor := flag.Int("pool-floor", 0, "per-leg recall pool floor (0 = default)")
	holdout := flag.String("holdout", "all", "longmemeval question split: tune (450) | held (50) | all")
	sessionDoc := flag.String("session-doc", "full", "longmemeval doc construction: full | user-only | dated")
	llmRerank := flag.Bool("llm-rerank", false, "with-LLM tier: production order vs LLM rerank (needs MEMINI_LLM_*; use -limit)")
	llmRerankPool := flag.Int("llm-rerank-pool", 20, "candidates handed to the reranker per question")
	ceRerankURL := flag.String("rerank-url", "",
		"rerank tier with a cross-encoder /rerank endpoint at this base URL (e.g. http://localhost:8002/v1)")
	ceRerankModel := flag.String("rerank-model", "", "cross-encoder model name for -rerank-url")
	ceMaxDocChars := flag.Int("rerank-max-doc-chars", 2048,
		"cross-encoder: truncate each candidate to this many chars (production default 2048)")
	ceMaxBatchChars := flag.Int("rerank-max-batch-chars", 6000,
		"cross-encoder: cap total query+docs chars per request, splitting as needed (production default 6000)")
	vecGate := flag.String("vec-gate", "",
		"sweep the absolute vector-relevance gate over these thresholds "+
			"(comma-separated, e.g. 0,0.2,0.3,0.4); positive recall vs foreign-namespace injection")
	rerankGate := flag.String("rerank-gate", "",
		"sweep a cross-encoder rerank-score gate over these thresholds (needs -rerank-url); "+
			"positive recall vs foreign-namespace injection")
	rerankGatePool := flag.Int("rerank-gate-pool", 20, "candidates reranked per question for -rerank-gate")
	ingest := flag.String("ingest", "upsert",
		"corpus ingestion: upsert (direct store writes, historical default) | "+
			"write (production Remember path: classify, gates, dedup, corroborate/contradict)")
	flag.Parse()

	ingestMode, err := parseIngestMode(*ingest)
	if err != nil {
		return err
	}

	ds, err := loadDataset(*suite, *data, bench.DocMode(*sessionDoc))
	if err != nil {
		return err
	}
	if *suite == "longmemeval" && *sessionDoc != "" && *sessionDoc != string(bench.DocFull) {
		ds.Name += "-" + *sessionDoc
	}
	ds, err = splitHoldout(ds, *holdout)
	if err != nil {
		return err
	}
	if *limit > 0 {
		ds = subset(ds, *limit)
	}
	fmt.Fprintf(os.Stderr, "dataset %q: %d items, %d questions\n", ds.Name, len(ds.Items), len(ds.Questions))

	embedder, dim, saveCache, err := buildEmbedder(*dims)
	if err != nil {
		return err
	}
	defer func() {
		if err := saveCache(); err != nil {
			fmt.Fprintf(os.Stderr, "bench: saving embedding cache failed: %v\n", err)
		}
	}()

	// Same query-side embedding instruction the server honors in production.
	queryPrefix := os.Getenv("MEMINI_EMBED_QUERY_PREFIX")
	if queryPrefix != "" {
		fmt.Fprintf(os.Stderr, "query prefix: %q\n", queryPrefix)
	}

	ctx := context.Background()
	tmpDir, err := os.MkdirTemp("", "memini-bench-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()
	st, err := sqlitevec.Open(ctx, filepath.Join(tmpDir, "bench.db"), dim)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	ks := parseKs(*k)

	// Rerank comparison: isolate the recency-aware re-ranker against pure RRF on
	// the same retrieved candidates, using each question's reference time.
	if *rerank {
		var cats []string
		for c := range strings.SplitSeq(*rerankCats, ",") {
			if c = strings.TrimSpace(c); c != "" {
				cats = append(cats, c)
			}
		}
		rr, err := bench.RerankCompare(ctx, st, embedder, ds, cats, ks[0], queryPrefix)
		if err != nil {
			return err
		}
		fmt.Println(bench.RerankMarkdown(rr, ks[0]))
		return nil
	}

	// Rerank tier: production order vs reranked, on retrieval recall. One reranker
	// call per question — use -limit to subset. Cross-encoder (-rerank-url) is
	// fast; the LLM (-llm-rerank) is slow.
	if (*llmRerank || *ceRerankURL != "") && *rerankGate == "" {
		reranker, err := buildReranker(*ceRerankURL, *ceRerankModel, *ceMaxDocChars, *ceMaxBatchChars)
		if err != nil {
			return err
		}
		rr, err := bench.LLMRerankCompare(ctx, st, embedder, reranker, ds, ks[0], *llmRerankPool, queryPrefix)
		if err != nil {
			return err
		}
		fmt.Println(bench.RerankMarkdown(rr, ks[0]))
		return nil
	}

	// Relevance-gate sweeps: positive recall (own namespace) vs negative injection
	// (foreign namespace) across thresholds, on the vector score or the rerank score.
	if *vecGate != "" {
		return runVecGate(ctx, st, embedder, ds, ks[0], parseFloatList(*vecGate), *concurrency, queryPrefix, *fusionAlpha)
	}
	if *rerankGate != "" {
		return runRerankGate(ctx, st, embedder, ds, ks[0], parseFloatList(*rerankGate), *rerankGatePool,
			*ceRerankURL, *ceRerankModel, *ceMaxDocChars, *ceMaxBatchChars, queryPrefix)
	}

	var results []bench.Result
	for _, sys := range bench.MeminiSystems(st, embedder, *concurrency, queryPrefix, *fusionAlpha, *poolFactor, *poolFloor, ingestMode) {
		rs, err := bench.Run(ctx, sys, ds, ks)
		if err != nil {
			return err
		}
		results = append(results, rs...)
	}

	// One table per K, best system first.
	for _, kk := range ks {
		var forK []bench.Result
		for _, r := range results {
			if r.K == kk {
				forK = append(forK, r)
			}
		}
		fmt.Println(bench.Markdown(forK))
	}
	printPerCategory(results, ks[len(ks)-1])

	cfg := runConfig{
		Suite: *suite, Dataset: ds.Name, Holdout: *holdout, SessionDoc: *sessionDoc,
		IngestMode: string(ingestMode), Ks: ks, Limit: *limit, Concurrency: *concurrency,
		FusionAlpha: *fusionAlpha, PoolFactor: *poolFactor, PoolFloor: *poolFloor,
		EmbedBaseURL: os.Getenv("MEMINI_EMBED_BASE_URL"), EmbedModel: embedModel(), EmbedDims: dim,
		QueryPrefix: queryPrefix, DocPrefix: os.Getenv("MEMINI_EMBED_DOC_PREFIX"),
		GitCommit: gitCommit(),
	}
	return writeReport(*outDir, ds.Name, cfg, results)
}

// writeReport writes the run's config snapshot and results as one JSON file so
// past numbers stay self-describing and reproducible.
func writeReport(outDir, name string, cfg runConfig, results []bench.Result) error {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	outPath := filepath.Join(outDir, name+".json")
	report := struct {
		Config  runConfig      `json:"config"`
		Results []bench.Result `json:"results"`
	}{Config: cfg, Results: results}
	buf, _ := json.MarshalIndent(report, "", "  ")
	if err := os.WriteFile(outPath, append(buf, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "wrote %s\n", outPath)
	return nil
}

// runConfig snapshots everything that shaped a run, embedded in the result
// JSON so past numbers stay self-describing and reproducible.
type runConfig struct {
	Suite        string  `json:"suite"`
	Dataset      string  `json:"dataset"`
	Holdout      string  `json:"holdout,omitempty"`
	SessionDoc   string  `json:"session_doc,omitempty"`
	IngestMode   string  `json:"ingest_mode"`
	Ks           []int   `json:"ks"`
	Limit        int     `json:"limit,omitempty"`
	Concurrency  int     `json:"concurrency"`
	FusionAlpha  float64 `json:"fusion_alpha"`
	PoolFactor   int     `json:"pool_factor,omitempty"`
	PoolFloor    int     `json:"pool_floor,omitempty"`
	EmbedBaseURL string  `json:"embed_base_url,omitempty"`
	EmbedModel   string  `json:"embed_model"`
	EmbedDims    int     `json:"embed_dims"`
	QueryPrefix  string  `json:"query_prefix,omitempty"`
	DocPrefix    string  `json:"doc_prefix,omitempty"`
	GitCommit    string  `json:"git_commit,omitempty"`
}

// embedModel mirrors buildEmbedder's model resolution for the config snapshot.
func embedModel() string {
	if os.Getenv("MEMINI_EMBED_BASE_URL") == "" {
		return "embedtest"
	}
	if m := os.Getenv("MEMINI_EMBED_MODEL"); m != "" {
		return m
	}
	return "text-embedding-3-small"
}

// gitCommit returns the working tree's short commit hash, best-effort.
func gitCommit() string {
	out, err := exec.Command("git", "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func parseIngestMode(s string) (bench.IngestMode, error) {
	mode := bench.IngestMode(s)
	switch mode {
	case bench.IngestUpsert, bench.IngestWrite:
		return mode, nil
	default:
		return "", fmt.Errorf("unknown -ingest %q (want upsert|write)", s)
	}
}

// parseFloatList parses a comma-separated list of non-negative floats, skipping
// malformed or negative entries.
func parseFloatList(csv string) []float64 {
	var out []float64
	for p := range strings.SplitSeq(csv, ",") {
		if v, err := strconv.ParseFloat(strings.TrimSpace(p), 64); err == nil && v >= 0 {
			out = append(out, v)
		}
	}
	return out
}

func runVecGate(ctx context.Context, st store.Store, e embed.Embedder, ds *bench.Dataset,
	k int, ts []float64, concurrency int, queryPrefix string, fusionAlpha float64,
) error {
	rr, err := bench.VecGateSweep(ctx, st, e, ds, k, ts, concurrency, queryPrefix, fusionAlpha)
	if err != nil {
		return err
	}
	fmt.Println(bench.VecGateMarkdown(rr, k))
	return nil
}

func runRerankGate(ctx context.Context, st store.Store, e embed.Embedder, ds *bench.Dataset,
	k int, ts []float64, pool int, ceURL, ceModel string, maxDoc, maxBatch int, queryPrefix string,
) error {
	if ceURL == "" {
		return fmt.Errorf("bench: -rerank-gate needs -rerank-url")
	}
	ce, err := rerankpkg.New(rerankpkg.Config{
		BaseURL: ceURL, Model: ceModel, APIKey: os.Getenv("MEMINI_RERANK_API_KEY"),
		MaxDocChars: maxDoc, MaxBatchChars: maxBatch,
	})
	if err != nil {
		return err
	}
	rr, err := bench.RerankGateSweep(ctx, st, e, ce, ds, k, pool, ts, queryPrefix)
	if err != nil {
		return err
	}
	fmt.Println(bench.RerankGateMarkdown(rr, k))
	return nil
}

func parseKs(csv string) []int {
	var ks []int
	for p := range strings.SplitSeq(csv, ",") {
		if n, err := strconv.Atoi(strings.TrimSpace(p)); err == nil && n > 0 {
			ks = append(ks, n)
		}
	}
	if len(ks) == 0 {
		ks = []int{5}
	}
	sort.Ints(ks)
	return ks
}

// printPerCategory prints the hybrid system's per-category recall at the given K.
func printPerCategory(results []bench.Result, k int) {
	for _, r := range results {
		if r.System == "memini-hybrid" && r.K == k && len(r.PerCategory) > 0 {
			fmt.Printf("\n### memini-hybrid by category (recall_any@%d)\n\n| Category | Recall |\n|---|--:|\n", k)
			cats := make([]string, 0, len(r.PerCategory))
			for c := range r.PerCategory {
				cats = append(cats, c)
			}
			sort.Strings(cats)
			for _, c := range cats {
				fmt.Printf("| %s | %.1f%% |\n", c, r.PerCategory[c]*100)
			}
		}
	}
}

// subset evenly samples up to limit questions across the dataset and prunes
// items to the groups those questions reference.
func subset(ds *bench.Dataset, limit int) *bench.Dataset {
	if limit >= len(ds.Questions) {
		return ds
	}
	step := float64(len(ds.Questions)) / float64(limit)
	groups := map[string]bool{}
	qs := make([]bench.Question, 0, limit)
	for i := range limit {
		q := ds.Questions[int(float64(i)*step)]
		qs = append(qs, q)
		groups[q.Group] = true
	}
	var items []bench.Item
	for _, it := range ds.Items {
		if groups[it.Group] {
			items = append(items, it)
		}
	}
	return &bench.Dataset{Name: ds.Name, Items: items, Questions: qs}
}

func loadDataset(suite, path string, docMode bench.DocMode) (*bench.Dataset, error) {
	switch suite {
	case "sample":
		return bench.Sample()
	case "file":
		return bench.LoadFile(requirePath(path))
	case "longmemeval":
		return bench.LoadLongMemEval(requirePath(path), docMode)
	case "locomo":
		return bench.LoadLoCoMo(requirePath(path))
	case "locomo-sessions":
		return bench.LoadLoCoMoSessions(requirePath(path))
	default:
		return nil, fmt.Errorf("unknown suite %q", suite)
	}
}

// splitHoldout filters longmemeval questions into a deterministic tune/held
// split: every 10th question by load order is "held" (50 of 500), the rest are
// "tune" (450). "all" returns the dataset unchanged. Items are pruned to the
// surviving questions' groups, and ds.Name is suffixed so results files don't
// collide across splits.
func splitHoldout(ds *bench.Dataset, mode string) (*bench.Dataset, error) {
	switch mode {
	case "", "all":
		return ds, nil
	case "tune", "held":
	default:
		return nil, fmt.Errorf("unknown holdout %q (want tune|held|all)", mode)
	}
	wantHeld := mode == "held"
	groups := map[string]bool{}
	qs := make([]bench.Question, 0, len(ds.Questions))
	for i, q := range ds.Questions {
		if (i%10 == 9) == wantHeld {
			qs = append(qs, q)
			groups[q.Group] = true
		}
	}
	items := make([]bench.Item, 0, len(ds.Items))
	for _, it := range ds.Items {
		if groups[it.Group] {
			items = append(items, it)
		}
	}
	return &bench.Dataset{Name: ds.Name + "-" + mode, Items: items, Questions: qs}, nil
}

func requirePath(p string) string {
	if p == "" {
		fmt.Fprintln(os.Stderr, "bench: -data is required for this suite")
		os.Exit(2)
	}
	return p
}

// buildEmbedder uses a real endpoint when configured, else a deterministic
// local embedder. The returned hook persists the on-disk embedding cache.
func buildEmbedder(localDims int) (embed.Embedder, int, func() error, error) {
	noop := func() error { return nil }
	if base := os.Getenv("MEMINI_EMBED_BASE_URL"); base != "" {
		dims := localDims
		if v := os.Getenv("MEMINI_EMBED_DIMS"); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				dims = n
			}
		}
		model := os.Getenv("MEMINI_EMBED_MODEL")
		if model == "" {
			model = "text-embedding-3-small"
		}
		c, err := embed.NewOpenAI(embed.OpenAIConfig{
			BaseURL: base, APIKey: os.Getenv("MEMINI_EMBED_API_KEY"), Model: model, Dims: dims,
		})
		if err != nil {
			return nil, 0, noop, err
		}
		// Batch to keep payloads under endpoint limits; persist to disk to avoid
		// re-embedding across runs.
		cachePath := filepath.Join(os.TempDir(),
			fmt.Sprintf("memini-embcache-%s-%d.gob", strings.NewReplacer("/", "_", ":", "_").Replace(model), dims))
		dc, err := embed.NewDiskCache(embed.NewBatched(c, 20, 24000, 8000), cachePath)
		if err != nil {
			return nil, 0, noop, err
		}
		fmt.Fprintf(os.Stderr, "using embeddings endpoint %s (model=%s dims=%d); embedding cache %s (%d cached)\n",
			base, model, dims, cachePath, dc.Len())
		return dc, dims, dc.Save, nil
	}
	fmt.Fprintf(os.Stderr, "using deterministic local embedder (dims=%d) — set MEMINI_EMBED_BASE_URL for a real model\n", localDims)
	return embedtest.New(localDims), localDims, noop, nil
}

// buildReranker constructs the rerank tier's backend: a cross-encoder at ceURL
// when set, else an LLM reranker from the MEMINI_LLM_* environment (matching how
// cmd/memini and cmd/locomo-qa configure their chat backend).
func buildReranker(ceURL, ceModel string, maxDocChars, maxBatchChars int) (rerankpkg.Reranker, error) {
	if ceURL != "" {
		return rerankpkg.New(rerankpkg.Config{
			BaseURL: ceURL, Model: ceModel, APIKey: os.Getenv("MEMINI_RERANK_API_KEY"),
			MaxDocChars: maxDocChars, MaxBatchChars: maxBatchChars,
		})
	}
	client, err := llm.New(llm.API(os.Getenv("MEMINI_LLM_API")), llm.Config{
		BaseURL: os.Getenv("MEMINI_LLM_BASE_URL"),
		APIKey:  os.Getenv("MEMINI_LLM_API_KEY"),
		Model:   os.Getenv("MEMINI_LLM_MODEL"),
	})
	if err != nil {
		return nil, err
	}
	return rerankpkg.NewLLM(client), nil
}
