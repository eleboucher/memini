package bench

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/eleboucher/memini/internal/embed"
	"github.com/eleboucher/memini/internal/service"
	"github.com/eleboucher/memini/internal/store"
)

// VecGateResult is one absolute-vector-score threshold's effect under a
// per-query semantic-relevance gate: if a query's best raw vector score
// (1/(1+L2)) is below the threshold, nothing relevant exists and recall returns
// empty. Positive = each query against its own namespace (recall must survive);
// negative = the same query against a foreign namespace (injection must
// collapse). The right default is the knee: highest threshold where PosRecallAtK
// is ~unchanged but NegInjectionRate has dropped.
type VecGateResult struct {
	Threshold        float64 `json:"threshold"`
	PosRecallAtK     float64 `json:"pos_recall_at_k"`
	NegInjectionRate float64 `json:"neg_injection_rate"`
}

// vecGateProbe is one question's measured signals, gathered once so the
// threshold sweep is analytic (no re-running recall per threshold).
type vecGateProbe struct {
	posTopVec float64 // best raw vector score in the own namespace
	negTopVec float64 // best raw vector score in a foreign namespace
	posHitK   bool    // gold retrieved within k by the real fused recall (gate off)
}

// VecGateSweep ingests once, then for every question measures the top raw vector
// score in its own namespace (positive) and in a foreign namespace (negative),
// plus whether the real fused recall already retrieves the gold. It reports, per
// threshold, the per-query gate's effect: positive recall@k (lost only when the
// own-namespace top vector score falls below the gate) and negative injection
// rate (a foreign query passes the gate when its top vector score clears it).
// Negatives pair each question with the next question's namespace; group ids are
// unique per question, so the paired namespace never holds the answer.
func VecGateSweep(
	ctx context.Context, st store.Store, e embed.Embedder, ds *Dataset,
	k int, thresholds []float64, concurrency int, queryPrefix string, fusionAlpha float64,
) ([]VecGateResult, error) {
	b := newMeminiBackend(st, e, concurrency, queryPrefix, fusionAlpha, 0, 0, IngestUpsert)
	if err := b.ingest(ctx, ds.Items); err != nil {
		return nil, fmt.Errorf("bench: vecgate ingest: %w", err)
	}

	qs := ds.Questions
	n := len(qs)
	if n < 2 {
		return nil, fmt.Errorf("bench: vecgate needs >=2 questions (got %d)", n)
	}

	probes := make([]vecGateProbe, n)
	var posTops, negTops []float64
	for i, q := range qs {
		qvec, err := embed.EmbedOne(ctx, b.embedder, queryPrefix+q.Query)
		if err != nil {
			return nil, fmt.Errorf("bench: vecgate embed: %w", err)
		}
		posTop, err := topVecScore(ctx, st, nsOf(q.Group), qvec, k)
		if err != nil {
			return nil, err
		}
		negTop, err := topVecScore(ctx, st, nsOf(qs[(i+1)%n].Group), qvec, k)
		if err != nil {
			return nil, err
		}

		res, err := b.svc.Recall(ctx, service.RecallInput{Namespace: nsOf(q.Group), Query: q.Query, Limit: k})
		if err != nil {
			return nil, fmt.Errorf("bench: vecgate recall: %w", err)
		}
		rank := firstGoldRank(scoredIDs(res), q.Gold)

		probes[i] = vecGateProbe{posTopVec: posTop, negTopVec: negTop, posHitK: rank >= 0 && rank < k}
		posTops = append(posTops, posTop)
		negTops = append(negTops, negTop)
	}

	fmt.Printf("top vector-score distribution (1/(1+L2)) over %d queries:\n", n)
	fmt.Printf("  positive (own ns):    p10=%.3f p25=%.3f p50=%.3f p75=%.3f p90=%.3f\n",
		pctile(posTops, 10), pctile(posTops, 25), pctile(posTops, 50), pctile(posTops, 75), pctile(posTops, 90))
	fmt.Printf("  negative (foreign):   p10=%.3f p25=%.3f p50=%.3f p75=%.3f p90=%.3f\n\n",
		pctile(negTops, 10), pctile(negTops, 25), pctile(negTops, 50), pctile(negTops, 75), pctile(negTops, 90))

	fn := float64(n)
	out := make([]VecGateResult, 0, len(thresholds))
	for _, t := range thresholds {
		var posHit, negInject float64
		for _, p := range probes {
			if p.posHitK && p.posTopVec >= t {
				posHit++
			}
			if p.negTopVec >= t {
				negInject++
			}
		}
		out = append(out, VecGateResult{Threshold: t, PosRecallAtK: posHit / fn, NegInjectionRate: negInject / fn})
	}
	return out, nil
}

// topVecScore returns the best raw vector score (store-native, 1/(1+L2)) for a
// query vector in a namespace, or 0 when the namespace has no candidates.
func topVecScore(ctx context.Context, st store.Store, ns string, qvec []float32, k int) (float64, error) {
	res, err := st.VectorSearch(ctx, ns, qvec, store.Filter{}, k)
	if err != nil {
		return 0, fmt.Errorf("bench: vecgate vector search: %w", err)
	}
	if len(res) == 0 {
		return 0, nil
	}
	return res[0].Score, nil
}

func pctile(xs []float64, p int) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := slices.Clone(xs)
	slices.Sort(s)
	idx := (p * (len(s) - 1)) / 100
	return s[idx]
}

// VecGateMarkdown renders the sweep, lowest threshold first.
func VecGateMarkdown(rows []VecGateResult, k int) string {
	sorted := slices.Clone(rows)
	slices.SortFunc(sorted, func(a, b VecGateResult) int {
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
	fmt.Fprintf(&b, "## per-query vector-relevance gate sweep — recall_any@%d (positive) vs injection (negative)\n\n", k)
	b.WriteString("| min_vec_score | pos R@K | neg inject % |\n")
	b.WriteString("|--------------:|--------:|-------------:|\n")
	for _, r := range sorted {
		fmt.Fprintf(&b, "| %.3f | %.1f%% | %.1f%% |\n", r.Threshold, r.PosRecallAtK*100, r.NegInjectionRate*100)
	}
	return b.String()
}
