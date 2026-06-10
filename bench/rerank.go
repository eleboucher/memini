package bench

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/eleboucher/memini/internal/embed"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/search"
	"github.com/eleboucher/memini/internal/store"
)

// rerankFallbackTime grounds items whose source carried no timestamp, so the
// recency factor stays well-defined.
var rerankFallbackTime = time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC)

// rerankAcc accumulates recall@1, recall@K, reciprocal rank, and count.
type rerankAcc struct{ r1, rk, rr, n float64 }

// RerankResult is one ranking strategy's score over a question set.
type RerankResult struct {
	System    string
	Category  string
	Questions int
	RecallAt1 float64
	RecallAtK float64
	MRR       float64
}

// RerankCompare isolates the effect of recency-aware re-ranking: it ingests ds
// (items carry source timestamps), then for each selected question scores the
// SAME fused candidate set two ways — pure RRF order vs the composite re-ranker
// using the question's reference time. Reports recall@1, recall@K, and MRR per
// category and overall, for both strategies. cats empty means all categories.
func RerankCompare(
	ctx context.Context, st store.Store, e embed.Embedder, ds *Dataset, cats []string, k int, queryPrefix string,
) ([]RerankResult, error) {
	if err := ingestTimed(ctx, st, e, ds.Items); err != nil {
		return nil, err
	}

	want := map[string]bool{}
	for _, c := range cats {
		want[c] = true
	}

	// Strategies to compare: pure RRF (relevance only) plus composite re-rankers
	// at increasing recency weight, to locate the weight at which recency stops
	// helping and starts burying correct-but-older memories.
	strategies := []struct {
		name string
		w    search.RerankWeights
	}{
		{"rrf", search.RerankWeights{Relevance: 1, Recency: 0, Importance: 0}},
		{"recency0.05", search.RerankWeights{Relevance: 0.80, Recency: 0.05, Importance: 0.15}},
		{"recency0.15", search.RerankWeights{Relevance: 0.75, Recency: 0.15, Importance: 0.10}},
		{"recency0.25", search.RerankWeights{Relevance: 0.60, Recency: 0.25, Importance: 0.15}},
	}
	// acc[strategy][category]
	accs := make([]map[string]*rerankAcc, len(strategies))
	for i := range accs {
		accs[i] = map[string]*rerankAcc{}
	}
	bump := func(m map[string]*rerankAcc, cat string) *rerankAcc {
		if m[cat] == nil {
			m[cat] = &rerankAcc{}
		}
		return m[cat]
	}

	// Mirror service.Recall's deep candidate pool (max(k*5, 50) per leg) so the
	// comparison reflects what the composite re-ranker sees in production.
	fetch := max(k*5, 50)
	for _, q := range ds.Questions {
		if len(want) > 0 && !want[q.Category] {
			continue
		}
		qvec, err := embed.EmbedOne(ctx, e, queryPrefix+q.Query)
		if err != nil {
			return nil, err
		}
		vres, err := st.VectorSearch(ctx, q.Group, qvec, store.Filter{}, fetch)
		if err != nil {
			return nil, err
		}
		kres, err := st.KeywordSearch(ctx, q.Group, q.Query, store.Filter{}, fetch)
		if err != nil {
			return nil, err
		}

		fused := search.Fuse([][]store.Scored{vres, kres}, 0, search.DefaultRRFK)
		now := q.Now
		if now.IsZero() {
			now = rerankFallbackTime
		}
		for si, s := range strategies {
			ranked := search.RerankWith(fused, now, s.w)
			for _, cat := range []string{q.Category, "all"} {
				score(bump(accs[si], cat), ranked, q.Gold, k)
			}
		}
	}

	var out []RerankResult
	for _, cat := range sortedKeys(accs[0]) {
		for si, s := range strategies {
			out = append(out, finalize(s.name, cat, accs[si][cat]))
		}
	}
	return out, nil
}

// ingestTimed upserts items using their source timestamp as created/last-accessed,
// embedding in windows. Single-writer upserts (sqlite).
func ingestTimed(ctx context.Context, st store.Store, e embed.Embedder, items []Item) error {
	for start := 0; start < len(items); start += ingestWindow {
		end := min(start+ingestWindow, len(items))
		window := items[start:end]
		texts := make([]string, len(window))
		for i, it := range window {
			texts[i] = it.Content
		}
		vecs, err := e.Embed(ctx, texts)
		if err != nil {
			return err
		}
		for i, it := range window {
			ts := it.Time
			if ts.IsZero() {
				ts = rerankFallbackTime
			}
			if err := st.Upsert(ctx, &memory.Memory{
				ID: it.ID, Namespace: nsOf(it.Group), Tier: memory.TierSemantic,
				Content: it.Content, CreatedAt: ts, UpdatedAt: ts, LastAccessedAt: ts,
				Embedding: vecs[i],
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

func score(a *rerankAcc, ranked []store.Scored, gold []string, k int) {
	a.n++
	for i, r := range ranked {
		if slices.Contains(gold, r.Memory.ID) {
			if i == 0 {
				a.r1++
			}
			if i < k {
				a.rk++
			}
			a.rr += 1.0 / float64(i+1)
			return
		}
	}
}

func finalize(system, cat string, a *rerankAcc) RerankResult {
	if a.n == 0 {
		return RerankResult{System: system, Category: cat}
	}
	return RerankResult{
		System: system, Category: cat, Questions: int(a.n),
		RecallAt1: a.r1 / a.n, RecallAtK: a.rk / a.n, MRR: a.rr / a.n,
	}
}

func sortedKeys(m map[string]*rerankAcc) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// RerankMarkdown renders the RRF-vs-composite comparison, grouped by category.
func RerankMarkdown(results []RerankResult, k int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Recency-aware re-rank vs pure RRF (recall@1 / recall@%d / MRR)\n\n", k)
	b.WriteString("| Category | System | Q | R@1 | R@K | MRR |\n")
	b.WriteString("|---|---|--:|--:|--:|--:|\n")
	for _, r := range results {
		fmt.Fprintf(&b, "| %s | %s | %d | %.1f%% | %.1f%% | %.1f%% |\n",
			r.Category, r.System, r.Questions, r.RecallAt1*100, r.RecallAtK*100, r.MRR*100)
	}
	return b.String()
}
