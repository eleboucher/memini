package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/eleboucher/memini/bench"
	"github.com/eleboucher/memini/internal/embed"
	"github.com/eleboucher/memini/internal/embed/embedtest"
	"github.com/eleboucher/memini/internal/store/sqlitevec"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "bench:", err)
		os.Exit(1)
	}
}

func run() error {
	suite := flag.String("suite", "sample", "sample | file | longmemeval | locomo")
	data := flag.String("data", "", "dataset path (for file/longmemeval/locomo)")
	k := flag.String("k", "5", "retrieval cutoff K (comma-separated for multiple, e.g. 5,10)")
	dims := flag.Int("dims", 256, "embedding dimensions (local embedder)")
	limit := flag.Int("limit", 0, "cap the number of questions (0 = all)")
	concurrency := flag.Int("concurrency", 8, "parallel embedding workers during ingest")
	outDir := flag.String("out", "bench/results", "directory for JSON results")
	rerank := flag.Bool("rerank", false, "compare pure-RRF vs recency-aware composite ranking (needs a timestamped dataset, e.g. longmemeval)")
	rerankCats := flag.String("rerank-cats", "knowledge-update,temporal-reasoning",
		"comma-separated question categories for -rerank (empty = all)")
	flag.Parse()

	ds, err := loadDataset(*suite, *data)
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
	defer func() { _ = saveCache() }()

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
		for _, c := range strings.Split(*rerankCats, ",") {
			if c = strings.TrimSpace(c); c != "" {
				cats = append(cats, c)
			}
		}
		rr, err := bench.RerankCompare(ctx, st, embedder, ds, cats, ks[0])
		if err != nil {
			return err
		}
		fmt.Println(bench.RerankMarkdown(rr, ks[0]))
		return nil
	}

	var results []bench.Result
	for _, sys := range bench.MeminiSystems(st, embedder, *concurrency) {
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

	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		return err
	}
	outPath := filepath.Join(*outDir, ds.Name+".json")
	buf, _ := json.MarshalIndent(results, "", "  ")
	if err := os.WriteFile(outPath, append(buf, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "wrote %s\n", outPath)
	return nil
}

func parseKs(csv string) []int {
	var ks []int
	for _, p := range strings.Split(csv, ",") {
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
	for i := 0; i < limit; i++ {
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

func loadDataset(suite, path string) (*bench.Dataset, error) {
	switch suite {
	case "sample":
		return bench.Sample()
	case "file":
		return bench.LoadFile(requirePath(path))
	case "longmemeval":
		return bench.LoadLongMemEval(requirePath(path))
	case "locomo":
		return bench.LoadLoCoMo(requirePath(path))
	default:
		return nil, fmt.Errorf("unknown suite %q", suite)
	}
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
