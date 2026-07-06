package bench

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
)

// Result is one system's score on a dataset at a given K.
type Result struct {
	System    string  `json:"system"`
	Dataset   string  `json:"dataset"`
	K         int     `json:"k"`
	Questions int     `json:"questions"`
	RecallAtK float64 `json:"recall_at_k"`
	MRR       float64 `json:"mrr"`
	P50Millis float64 `json:"p50_ms"`
	P95Millis float64 `json:"p95_ms"`
	IngestMs  float64 `json:"ingest_ms"`
	// TokensInjectedMean is the mean estimated token count of the top-K
	// retrieved contents per question — what recall would inject into a
	// consumer's context. TokenEfficiency divides it by the corpus's total
	// estimated tokens: the cost axis of answering from memory vs full context.
	TokensInjectedMean float64            `json:"tokens_injected_mean"`
	TokenEfficiency    float64            `json:"token_efficiency"`
	PerCategory        map[string]float64 `json:"per_category,omitempty"`
}

// Run ingests the dataset into a system once, then scores recall_any@K and MRR
// for every K in ks from a single retrieval pass (retrieving max(ks) per
// question). Returns one Result per K.
func Run(ctx context.Context, sys System, ds *Dataset, ks []int) ([]Result, error) {
	start := time.Now()
	if err := sys.Ingest(ctx, ds.Items); err != nil {
		return nil, fmt.Errorf("bench: ingest %s: %w", sys.Name(), err)
	}
	ingestMs := float64(time.Since(start).Microseconds()) / 1000

	maxK := slices.Max(ks)
	hit := map[int]float64{}
	injTok := map[int]float64{}
	catHit := map[int]map[string]float64{}
	for _, k := range ks {
		catHit[k] = map[string]float64{}
	}
	catTotal := map[string]float64{}
	var rrSum float64
	latencies := make([]float64, 0, len(ds.Questions))
	var corpusTokens float64
	for _, it := range ds.Items {
		corpusTokens += float64(estimateTokens(it.Content))
	}

	for _, q := range ds.Questions {
		t0 := time.Now()
		got, err := sys.Recall(ctx, q.Group, q.Query, maxK)
		if err != nil {
			return nil, fmt.Errorf("bench: recall %s: %w", sys.Name(), err)
		}
		latencies = append(latencies, float64(time.Since(t0).Microseconds())/1000)

		catTotal[q.Category]++
		rank := firstGoldRankHits(got, q.Gold)
		if rank >= 0 {
			rrSum += 1.0 / float64(rank+1)
		}
		for _, k := range ks {
			if rank >= 0 && rank < k {
				hit[k]++
				catHit[k][q.Category]++
			}
			for _, h := range got[:min(k, len(got))] {
				injTok[k] += float64(estimateTokens(h.Content))
			}
		}
	}

	n := float64(len(ds.Questions))
	p50, p95 := percentile(latencies, 50), percentile(latencies, 95)
	results := make([]Result, 0, len(ks))
	for _, k := range ks {
		perCat := map[string]float64{}
		for cat, tot := range catTotal {
			if cat != "" && tot > 0 {
				perCat[cat] = catHit[k][cat] / tot
			}
		}
		if len(perCat) == 0 {
			perCat = nil
		}
		injMean := injTok[k] / n
		var tokEff float64
		if corpusTokens > 0 {
			tokEff = injMean / corpusTokens
		}
		results = append(results, Result{
			System: sys.Name(), Dataset: ds.Name, K: k, Questions: len(ds.Questions),
			RecallAtK: hit[k] / n, MRR: rrSum / n,
			P50Millis: p50, P95Millis: p95, IngestMs: ingestMs,
			TokensInjectedMean: injMean, TokenEfficiency: tokEff, PerCategory: perCat,
		})
	}
	return results, nil
}

// firstGoldRank returns the 0-based rank of the first retrieved id that is in
// gold, or -1 if none are.
func firstGoldRank(got, gold []string) int {
	for i, id := range got {
		if slices.Contains(gold, id) {
			return i
		}
	}
	return -1
}

// firstGoldRankHits is firstGoldRank over RecallHits: a hit matches when any
// of its item IDs is gold (write-mode dedup can land several items on one
// stored memory).
func firstGoldRankHits(hits []RecallHit, gold []string) int {
	for i, h := range hits {
		for _, id := range h.IDs {
			if slices.Contains(gold, id) {
				return i
			}
		}
	}
	return -1
}

// estimateTokens approximates the token count of s (~4 bytes/token, the usual
// English-text heuristic); exact tokenizer choice washes out of the
// injected/corpus ratio.
func estimateTokens(s string) int { return (len(s) + 3) / 4 }

func percentile(xs []float64, p int) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := slices.Clone(xs)
	sort.Float64s(s)
	idx := (p * (len(s) - 1)) / 100
	return s[idx]
}

// Markdown renders results that share a K as a comparison table, best first.
func Markdown(results []Result) string {
	sorted := slices.Clone(results)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].RecallAtK > sorted[j].RecallAtK })

	var b strings.Builder
	if len(sorted) > 0 {
		fmt.Fprintf(&b, "## %s — %d questions, recall_any@%d\n\n", sorted[0].Dataset, sorted[0].Questions, sorted[0].K)
	}
	b.WriteString("| System | Recall@K | MRR | inj tok | tok-eff | p50 (ms) | p95 (ms) | ingest (ms) |\n")
	b.WriteString("|--------|---------:|----:|--------:|--------:|---------:|---------:|------------:|\n")
	for _, r := range sorted {
		fmt.Fprintf(&b, "| %s | %.1f%% | %.1f%% | %.0f | %.3f%% | %.2f | %.2f | %.1f |\n",
			r.System, r.RecallAtK*100, r.MRR*100, r.TokensInjectedMean, r.TokenEfficiency*100,
			r.P50Millis, r.P95Millis, r.IngestMs)
	}
	return b.String()
}
