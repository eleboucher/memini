//go:build bench

package bench_test

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/eleboucher/memini/internal/embed"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/service"
	"github.com/eleboucher/memini/internal/store/sqlitevec"
)

// importanceRerankPool is the rerank-pool depth this eval runs at. The
// importance reserve only changes which candidates a reranker SEES, so it is
// structurally inert unless the pool is deeper than the caller's limit — 25 sits
// comfortably above the k=5/k=10 pulls measured below while staying inside the
// recall pool (max(k*5, 50)) the buried facts must be promoted from.
const importanceRerankPool = 25

// TestImportancePoolReserve measures the rerank-pool importance reserve: does
// holding pool slots for high-assessed memories actually recover a fact the
// fused retrieval score buried below the pool cut, and what does it cost when
// the gold answer is the chatter instead?
//
// The corpus is the tier-mixed reserve corpus (one terse durable decision per
// topic under verbose episodic chatter on the same subject), with the durable
// facts carrying a high LLM-assessed importance — the signal the reserve reads.
// The chatter out-ranks the facts on lexical+semantic surface, so on a deep
// corpus the facts fall outside the reranker's pool and the cross-encoder never
// gets to judge them.
//
// It sweeps reserve ∈ {0,1,2,3} × minImp ∈ {0.6,0.75,0.9} and reports:
//   - fact-gold recall@5/@10: does reserving pool slots recover the buried fact?
//   - chatter-gold recall@5/@10: the crowding cost, on queries whose gold answer
//     is an episodic turn that a promoted fact could have displaced.
//
// Needs a live embedder AND a reranker (MEMINI_RERANK_URL): without a reranker
// the feature cannot fire at all, so the eval would measure nothing. Skips when
// either is missing, so CI without them is unaffected.
func TestImportancePoolReserve(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	t.Cleanup(cancel)

	baseURL := envOr("MEMINI_EMBED_BASE_URL", "http://127.0.0.1:8001/v1")
	model := envOr("MEMINI_EMBED_MODEL", "text-embedding-qwen3-embedding-0.6b")
	dims := envIntOr("MEMINI_EMBED_DIMS", 1024)

	e, err := embed.NewOpenAI(embed.OpenAIConfig{BaseURL: baseURL, Model: model, Dims: dims})
	if err != nil {
		t.Skipf("embedder config: %v", err)
	}
	probeEmbedder(ctx, t, baseURL, model)

	rerankOpts := maybeReranker(t)
	if len(rerankOpts) == 0 {
		t.Skip("importance pool reserve is inert without a reranker; set MEMINI_RERANK_URL")
	}

	st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "importance-reserve.db"), dims)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	clk := func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }
	corpus := buildReserveCorpus()
	if err := ingestAssessedCorpus(ctx, st, e, corpus, clk()); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	// newSvc builds a service at one (reserve, minImp) point. The knobs are
	// service options rather than per-call input, so each sweep point gets its
	// own service over the same already-ingested store.
	newSvc := func(reserve int, minImp float64) *service.Service {
		opts := append([]service.Option{
			service.WithClock(clk), service.WithSyncReinforce(), service.WithScoreFusion(0.5),
			service.WithRerankPool(importanceRerankPool),
			service.WithImportancePoolReserve(reserve),
			service.WithImportancePoolMin(minImp),
			// The durable-tier reserve is a separate mechanism operating on the
			// pre-rerank window; hold it at 0 so this eval attributes movement to
			// the importance reserve alone.
			service.WithRecallSemanticReserve(0),
		}, rerankOpts...)
		return service.New(st, e, opts...)
	}

	reserves := []int{0, 1, 2, 3}
	minImps := []float64{0.6, 0.75, 0.9}

	t.Logf("importance pool reserve eval — %d topics, 1 assessed-important fact + %d chatter each, rerankPool=%d, embedder=%s",
		len(corpus), chatterPerTopic, importanceRerankPool, model)
	t.Logf("%-8s | %-7s | %-20s | %-20s", "reserve", "minImp", "fact-gold R@5/10", "chatter-gold R@5/10")
	t.Logf("%s", "---------+---------+----------------------+---------------------")

	var baseFact5, bestFact5 float64
	for _, minImp := range minImps {
		for _, r := range reserves {
			svc := newSvc(r, minImp)
			factHits5, factHits10, chatHits5, chatHits10 := 0, 0, 0, 0
			for _, tp := range corpus {
				if slices.Contains(recallTop(ctx, t, svc, tp.factQuery, 5), tp.factID) {
					factHits5++
				}
				if slices.Contains(recallTop(ctx, t, svc, tp.factQuery, 10), tp.factID) {
					factHits10++
				}
				if slices.Contains(recallTop(ctx, t, svc, tp.detailQuery, 5), tp.detailID) {
					chatHits5++
				}
				if slices.Contains(recallTop(ctx, t, svc, tp.detailQuery, 10), tp.detailID) {
					chatHits10++
				}
			}
			n := float64(len(corpus))
			f5 := pct(factHits5, n)
			t.Logf("%-8d | %-7.2f | %6.1f%% / %6.1f%%    | %6.1f%% / %6.1f%%",
				r, minImp, f5, pct(factHits10, n), pct(chatHits5, n), pct(chatHits10, n))

			// reserve=0 is the feature-off baseline and must be identical at
			// every minImp — the threshold is unreachable with no slots to claim.
			if r == 0 && minImp == minImps[0] {
				baseFact5 = f5
			}
			if r == 0 && f5 != baseFact5 {
				t.Errorf("reserve=0 is not inert: fact R@5 %.1f%% at minImp=%.2f vs %.1f%% baseline", f5, minImp, baseFact5)
			}
			if f5 > bestFact5 {
				bestFact5 = f5
			}
		}
	}

	// Directional sanity: the mechanism exists to make buried important facts
	// reachable, so no sweep point may sit below the feature-off baseline.
	if bestFact5 < baseFact5 {
		t.Errorf("fact R@5 never reached the reserve=0 baseline: best %.1f%% vs %.1f%%", bestFact5, baseFact5)
	}
}

// ingestAssessedCorpus writes the tier-mixed reserve corpus with the durable
// facts carrying a high LLM-assessed importance (the signal the pool reserve
// reads) and the chatter left at its tier-seeded baseline.
func ingestAssessedCorpus(ctx context.Context, st *sqlitevec.Store, e embed.Embedder, topics []reserveTopic, now time.Time) error {
	const factAssessed = 0.95

	var texts, ids []string
	var tiers []memory.Tier
	for _, tp := range topics {
		texts = append(texts, tp.factContent)
		ids = append(ids, tp.factID)
		tiers = append(tiers, memory.TierSemantic)
		for j, c := range tp.chatter {
			texts = append(texts, c)
			ids = append(ids, tp.chatterIDs[j])
			tiers = append(tiers, memory.TierEpisodic)
		}
	}
	vecs, err := e.Embed(ctx, texts)
	if err != nil {
		return err
	}
	for i := range texts {
		m := &memory.Memory{
			ID: ids[i], Namespace: reserveNS, Tier: tiers[i], Content: texts[i],
			CreatedAt: now, UpdatedAt: now, LastAccessedAt: now, Embedding: vecs[i],
		}
		if tiers[i] == memory.TierSemantic {
			assessed := factAssessed
			m.AssessedImportance = &assessed
		}
		if err := st.Upsert(ctx, m); err != nil {
			return err
		}
	}
	return nil
}

// recallTop pulls the top k IDs for a query. Unlike recallIDs it leaves the
// semantic reserve at the service default (0 here) so the importance reserve is
// the only mechanism under test.
func recallTop(ctx context.Context, t *testing.T, svc *service.Service, query string, k int) []string {
	t.Helper()
	res, err := svc.Recall(ctx, service.RecallInput{Namespace: reserveNS, Query: query, Limit: k})
	if err != nil {
		t.Fatalf("recall %q (k=%d): %v", query, k, err)
	}
	ids := make([]string, 0, len(res))
	for _, s := range res {
		ids = append(ids, s.Memory.ID)
	}
	return ids
}
