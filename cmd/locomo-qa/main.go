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
	"github.com/eleboucher/memini/internal/service"
	"github.com/eleboucher/memini/internal/store/sqlitevec"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "locomo-qa:", err)
		os.Exit(1)
	}
}

type result struct {
	Index    int    `json:"i"`
	Category string `json:"category"`
	Correct  bool   `json:"correct"`
}

func run() error {
	data := flag.String("data", "/tmp/locomo10.json", "LoCoMo dataset path")
	k := flag.Int("k", 10, "retrieved turns per question")
	limit := flag.Int("limit", 0, "cap questions (0 = all)")
	workers := flag.Int("workers", 6, "concurrent question workers")
	ckptPath := flag.String("checkpoint", "bench/results/locomo_qa.jsonl", "resume checkpoint (JSONL)")
	dbg := flag.Bool("debug", false, "print per-question retrieval/answer/grade to stderr")
	flag.Parse()
	debug = *dbg

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

	ds, err := bench.LoadLoCoMo(*data)
	if err != nil {
		return err
	}
	if *limit > 0 && *limit < len(ds.Questions) {
		// Sample evenly across the dataset so a small limit still spans all
		// conversations instead of truncating to the first one.
		step := float64(len(ds.Questions)) / float64(*limit)
		sampled := make([]bench.Question, 0, *limit)
		for i := 0; i < *limit; i++ {
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

	ctx := context.Background()
	dbPath := filepath.Join(os.TempDir(), "memini-locomo-qa.db")
	_ = os.Remove(dbPath)
	st, err := sqlitevec.Open(ctx, dbPath, dims)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	svc := service.New(st, embedder)

	fmt.Fprintf(os.Stderr, "ingesting %d turns...\n", len(ds.Items))
	if err := ingest(ctx, st, embedder, ds.Items); err != nil {
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
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				q := ds.Questions[i]
				correct, err := answerAndJudge(ctx, svc, chat, q, *k)
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
				if processed%50 == 0 {
					fmt.Fprintf(os.Stderr, "  ...%d processed\n", processed)
				}
				mu.Unlock()
			}
		}()
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

func ingest(ctx context.Context, st *sqlitevec.Store, e embed.Embedder, items []bench.Item) error {
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
			ns := it.Group
			if ns == "" {
				ns = "default"
			}
			if err := st.Upsert(ctx, &memory.Memory{
				ID: it.ID, Namespace: ns, Tier: memory.TierSemantic, Content: it.Content,
				CreatedAt: now, UpdatedAt: now, LastAccessedAt: now, Embedding: vecs[i],
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

const genSystem = "You answer questions using ONLY the provided conversation excerpts. " +
	"Reply with just the answer (a short phrase, name, or date). " +
	"Each excerpt is prefixed with its date in [brackets]; use it to resolve relative time references " +
	"(e.g. 'yesterday', 'last year', 'two weeks ago') to an absolute date. " +
	"If the answer is not in the excerpts, reply 'Not mentioned'."

const judgeSystem = "You grade answers. Given a question, the reference answer, and a candidate answer, " +
	"reply with exactly CORRECT or INCORRECT. The candidate is CORRECT if it conveys the same key fact(s) " +
	"as the reference, even if phrased differently or with extra words."

var debug bool

func answerAndJudge(ctx context.Context, svc *service.Service, chat llm.Completer, q bench.Question, k int) (bool, error) {
	res, err := svc.Recall(ctx, service.RecallInput{Namespace: q.Group, Query: q.Query, Limit: k})
	if err != nil {
		return false, err
	}
	var ctxB strings.Builder
	for _, r := range res {
		ctxB.WriteString("- ")
		ctxB.WriteString(r.Memory.Content)
		ctxB.WriteString("\n")
	}
	answer, err := chat.Complete(ctx,
		genSystem, "Conversation excerpts:\n"+ctxB.String()+"\nQuestion: "+q.Query+"\nAnswer:")
	if err != nil {
		return false, err
	}
	grade, err := chat.Complete(ctx, judgeSystem,
		fmt.Sprintf("Question: %s\nReference: %s\nCandidate: %s\nGrade:", q.Query, q.Answer, answer))
	if err != nil {
		return false, err
	}
	g := strings.ToUpper(grade)
	correct := strings.Contains(g, "CORRECT") && !strings.Contains(g, "INCORRECT")
	if debug {
		fmt.Fprintf(os.Stderr, "\n[Q] %s\n[group=%s retrieved=%d]\n[gold] %s\n[answer] %s\n[grade] %s => %v\n",
			q.Query, q.Group, len(res), q.Answer, strings.TrimSpace(answer), strings.TrimSpace(grade), correct)
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
	fmt.Printf("\nLoCoMo QA accuracy (LLM-judge): %.1f%%  (%d/%d answered; %d total questions)\n",
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
