package bench

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/eleboucher/memini/internal/embed"
	"github.com/eleboucher/memini/internal/rerank"
	"github.com/eleboucher/memini/internal/search"
	"github.com/eleboucher/memini/internal/store"
)

// RerankGateResult is one cross-encoder relevance-score threshold's effect under
// a per-query gate: if a query's best rerank score (over its recall pool) is
// below the threshold, nothing relevant exists and recall returns empty.
// Positive = own namespace (recall must survive); negative = a foreign namespace
// (injection must collapse). Cross-encoders emit calibrated absolute relevance,
// unlike bi-encoder cosine — this measures whether that separation is real.
type RerankGateResult struct {
	Threshold        float64 `json:"threshold"`
	PosRecallAtK     float64 `json:"pos_recall_at_k"`
	NegInjectionRate float64 `json:"neg_injection_rate"`
}

type rerankGateProbe struct {
	posTop  float64 // best rerank score over the own-namespace pool
	negTop  float64 // best rerank score over a foreign pool
	posHitK bool    // gold in the rerank-ordered top-k of the own pool
}

// RerankGateSweep ingests once, then for every question reranks its recall pool
// (hybrid fusion + composite, top `pool`) against the query in its own namespace
// (positive) and in a foreign namespace (negative), recording the top rerank
// score and whether the gold lands in the reranked top-k. It reports the top
// rerank-score distribution and, per threshold, positive recall@k vs negative
// injection. Negatives pair each question with the next question's namespace.
func RerankGateSweep(
	ctx context.Context, st store.Store, e embed.Embedder, ce *rerank.CrossEncoder,
	ds *Dataset, k, pool int, thresholds []float64, queryPrefix string,
) ([]RerankGateResult, error) {
	if err := ingestTimed(ctx, st, e, ds.Items); err != nil {
		return nil, err
	}
	qs := ds.Questions
	n := len(qs)
	if n < 2 {
		return nil, fmt.Errorf("bench: rerank-gate needs >=2 questions (got %d)", n)
	}

	// topRerank fuses the two legs, composite-ranks, reranks the top `pool`, and
	// returns the best rerank score plus the rerank-ordered results.
	topRerank := func(ns, query string, qvec []float32) (float64, []store.Scored, error) {
		vres, err := st.VectorSearch(ctx, ns, qvec, store.Filter{}, pool)
		if err != nil {
			return 0, nil, err
		}
		kres, err := st.KeywordSearch(ctx, ns, query, store.Filter{}, pool)
		if err != nil {
			return 0, nil, err
		}
		head := search.Rerank(search.FuseScores([][]store.Scored{vres, kres}, []float64{0.5, 0.5}, 0), benchClock())
		if len(head) > pool {
			head = head[:pool]
		}
		if len(head) == 0 {
			return 0, nil, nil
		}
		cands := make([]rerank.Candidate, len(head))
		for i, r := range head {
			cands[i] = rerank.Candidate{ID: r.Memory.ID, Content: r.Memory.Content}
		}
		scores, err := ce.Scores(ctx, query, cands)
		if err != nil {
			return 0, nil, err
		}
		ordered := slices.Clone(head)
		slices.SortStableFunc(ordered, func(a, b store.Scored) int {
			switch sa, sb := scores[a.Memory.ID], scores[b.Memory.ID]; {
			case sa > sb:
				return -1
			case sa < sb:
				return 1
			default:
				return 0
			}
		})
		var top float64
		first := true
		for _, sc := range scores {
			if first || sc > top {
				top, first = sc, false
			}
		}
		return top, ordered, nil
	}

	probes := make([]rerankGateProbe, 0, n)
	var posTops, negTops []float64
	for i, q := range qs {
		qvec, err := embed.EmbedOne(ctx, e, queryPrefix+q.Query)
		if err != nil {
			return nil, err
		}
		posTop, ordered, err := topRerank(nsOf(q.Group), q.Query, qvec)
		if err != nil {
			return nil, err
		}
		negTop, _, err := topRerank(nsOf(qs[(i+1)%n].Group), q.Query, qvec)
		if err != nil {
			return nil, err
		}
		rank := firstGoldRank(scoredIDs(ordered), q.Gold)
		probes = append(probes, rerankGateProbe{posTop: posTop, negTop: negTop, posHitK: rank >= 0 && rank < k})
		posTops = append(posTops, posTop)
		negTops = append(negTops, negTop)
	}

	fmt.Printf("top rerank-score distribution over %d queries (pool=%d):\n", n, pool)
	fmt.Printf("  positive (own ns):    p10=%.3f p25=%.3f p50=%.3f p75=%.3f p90=%.3f\n",
		pctile(posTops, 10), pctile(posTops, 25), pctile(posTops, 50), pctile(posTops, 75), pctile(posTops, 90))
	fmt.Printf("  negative (foreign):   p10=%.3f p25=%.3f p50=%.3f p75=%.3f p90=%.3f\n\n",
		pctile(negTops, 10), pctile(negTops, 25), pctile(negTops, 50), pctile(negTops, 75), pctile(negTops, 90))

	fn := float64(n)
	out := make([]RerankGateResult, 0, len(thresholds))
	for _, t := range thresholds {
		var posHit, negInject float64
		for _, p := range probes {
			if p.posHitK && p.posTop >= t {
				posHit++
			}
			if p.negTop >= t {
				negInject++
			}
		}
		out = append(out, RerankGateResult{Threshold: t, PosRecallAtK: posHit / fn, NegInjectionRate: negInject / fn})
	}
	return out, nil
}

// RerankGateMarkdown renders the sweep, lowest threshold first.
func RerankGateMarkdown(rows []RerankGateResult, k int) string {
	sorted := slices.Clone(rows)
	slices.SortFunc(sorted, func(a, b RerankGateResult) int {
		switch {
		case a.Threshold < b.Threshold:
			return -1
		case a.Threshold > b.Threshold:
			return 1
		default:
			return 0
		}
	})
	var b strings.Builder
	fmt.Fprintf(&b, "## cross-encoder rerank-gate sweep — recall_any@%d (positive) vs injection (negative)\n\n", k)
	b.WriteString("| min_rerank_score | pos R@K | neg inject % |\n")
	b.WriteString("|-----------------:|--------:|-------------:|\n")
	for _, r := range sorted {
		fmt.Fprintf(&b, "| %.3f | %.1f%% | %.1f%% |\n", r.Threshold, r.PosRecallAtK*100, r.NegInjectionRate*100)
	}
	return b.String()
}
