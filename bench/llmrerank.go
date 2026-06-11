package bench

import (
	"context"

	"github.com/eleboucher/memini/internal/embed"
	"github.com/eleboucher/memini/internal/llm"
	"github.com/eleboucher/memini/internal/search"
	"github.com/eleboucher/memini/internal/service"
	"github.com/eleboucher/memini/internal/store"
)

// LLMRerankCompare measures the with-LLM read-side rerank lift on pure
// retrieval. For each question it builds the production candidate order
// (hybrid score fusion -> composite re-rank), then re-orders the top `fetch`
// with an LLM reranker, and scores recall@1/@k and MRR for both. The LLM tier
// is slow (one chat call per question), so drive it over a subset with
// cmd/bench -limit.
func LLMRerankCompare(
	ctx context.Context, st store.Store, e embed.Embedder, rr llm.Reranker,
	ds *Dataset, k, fetch int, queryPrefix string,
) ([]RerankResult, error) {
	if err := ingestTimed(ctx, st, e, ds.Items); err != nil {
		return nil, err
	}
	pool := service.RecallPoolSize(k)
	baseAcc := map[string]*rerankAcc{}
	llmAcc := map[string]*rerankAcc{}
	bump := func(m map[string]*rerankAcc, cat string) *rerankAcc {
		if m[cat] == nil {
			m[cat] = &rerankAcc{}
		}
		return m[cat]
	}

	for _, q := range ds.Questions {
		qvec, err := embed.EmbedOne(ctx, e, queryPrefix+q.Query)
		if err != nil {
			return nil, err
		}
		vres, err := st.VectorSearch(ctx, q.Group, qvec, store.Filter{}, pool)
		if err != nil {
			return nil, err
		}
		kres, err := st.KeywordSearch(ctx, q.Group, q.Query, store.Filter{}, pool)
		if err != nil {
			return nil, err
		}
		// Production pre-LLM order: score fusion (alpha 0.5) + composite re-rank.
		fused := search.FuseScores([][]store.Scored{vres, kres}, []float64{0.5, 0.5}, 0)
		base := search.Rerank(fused, benchClock())
		for _, cat := range []string{q.Category, catAll} {
			score(bump(baseAcc, cat), base, q.Gold, k)
		}

		// LLM re-ranks the top `fetch` of the production order.
		head := base
		if len(head) > fetch {
			head = head[:fetch]
		}
		cands := make([]llm.RerankCandidate, len(head))
		for i, r := range head {
			cands[i] = llm.RerankCandidate{ID: r.Memory.ID, Content: r.Memory.Content}
		}
		orderedIDs, err := rr.Rerank(ctx, q.Query, cands)
		if err != nil {
			return nil, err
		}
		llmRanked := reorderByID(head, orderedIDs)
		for _, cat := range []string{q.Category, catAll} {
			score(bump(llmAcc, cat), llmRanked, q.Gold, k)
		}
	}

	var out []RerankResult
	for _, cat := range sortedKeys(baseAcc) {
		out = append(out, finalize("baseline", cat, baseAcc[cat]))
		out = append(out, finalize("llm-rerank", cat, llmAcc[cat]))
	}
	return out, nil
}

// reorderByID returns results ordered by orderedIDs (IDs not present keep their
// original relative position at the end). orderedIDs is the reranker's full
// permutation of the input IDs, so this is a stable reorder.
func reorderByID(results []store.Scored, orderedIDs []string) []store.Scored {
	byID := make(map[string]store.Scored, len(results))
	for _, r := range results {
		byID[r.Memory.ID] = r
	}
	out := make([]store.Scored, 0, len(results))
	placed := make(map[string]bool, len(results))
	for _, id := range orderedIDs {
		if r, ok := byID[id]; ok && !placed[id] {
			out = append(out, r)
			placed[id] = true
		}
	}
	for _, r := range results {
		if !placed[r.Memory.ID] {
			out = append(out, r)
		}
	}
	return out
}
