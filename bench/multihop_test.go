package bench_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/eleboucher/memini/bench"
	"github.com/eleboucher/memini/internal/embed"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/service"
	"github.com/eleboucher/memini/internal/store/sqlitevec"
)

// TestMultiHopRetrievalCeiling is a retrieval-only diagnostic (no LLM) for
// LoCoMo multi-hop (category 1, gold set >= 2): does a single recall surface
// ALL of a question's gold memories, and how much would one follow-up recall
// recover when it doesn't? It answers whether cat-1's weakness is a retrieval
// miss (fixable by iteration / a second hop) or a single-shot answering
// artifact (retrieval already holds the evidence, the answerer just has to
// reason over it).
//
// "full-gold coverage" = every gold memory retrieved — what multi-hop
// answering needs; a partial set usually can't be answered. Three regimes:
//   - single: one recall (k)
//   - realistic 2nd hop: union with a recall augmented by the TOP-1 retrieved
//     memory's content (what a real term-anchored follow-up would achieve)
//   - oracle 2nd hop: union with a recall augmented by a FOUND-GOLD memory's
//     content (upper bound: if we knew which hit was the bridge)
//
// The union spans two k-sized result sets, modelling an agent that issues two
// recalls and sees both — not a single k window.
//
// Needs a live embedder; skips when unreachable. MEMINI_MULTIHOP_LIMIT caps
// the sampled cat-1 questions (0 = all), ingesting only their conversations.
func TestMultiHopRetrievalCeiling(t *testing.T) {
	ctx := context.Background()
	baseURL := envOr("MEMINI_EMBED_BASE_URL", "http://127.0.0.1:8001/v1")
	model := envOr("MEMINI_EMBED_MODEL", "text-embedding-qwen3-embedding-0.6b")
	dims := envIntOr("MEMINI_EMBED_DIMS", 1024)
	data := envOr("MEMINI_LOCOMO_DATA", "data/locomo10.json")

	e, err := embed.NewOpenAI(embed.OpenAIConfig{BaseURL: baseURL, Model: model, Dims: dims})
	if err != nil {
		t.Skipf("embedder config: %v", err)
	}
	if _, err := e.Embed(ctx, []string{"probe"}); err != nil {
		t.Skipf("live embedder unreachable at %s (%s): %v", baseURL, model, err)
	}

	ds, err := bench.LoadLoCoMo(data)
	if err != nil {
		t.Fatalf("load locomo: %v", err)
	}

	var qs []bench.Question
	for _, q := range ds.Questions {
		if q.Category == "1" && len(q.Gold) >= 2 {
			qs = append(qs, q)
		}
	}
	if limit := envIntOr("MEMINI_MULTIHOP_LIMIT", 0); limit > 0 && limit < len(qs) {
		step := float64(len(qs)) / float64(limit)
		sampled := make([]bench.Question, 0, limit)
		for i := range limit {
			sampled = append(sampled, qs[int(float64(i)*step)])
		}
		qs = sampled
	}
	if len(qs) == 0 {
		t.Skip("no multi-hop cat-1 questions in dataset")
	}

	// Ingest only the conversations the sampled questions reference.
	groups := map[string]bool{}
	for _, q := range qs {
		groups[q.Group] = true
	}
	byID := map[string]bench.Item{}
	var items []bench.Item
	for _, it := range ds.Items {
		if groups[it.Group] {
			items = append(items, it)
			byID[it.ID] = it
		}
	}

	st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "multihop.db"), dims)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := ingestItems(ctx, st, e, items); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	svc := service.New(st, e)

	const k = 10
	var singleFull, realisticFull, oracleFull int
	var singleRecall, realisticRecall, oracleRecall float64
	for _, q := range qs {
		got := recallSet(ctx, t, svc, q.Group, q.Query, k)
		found, total := coverage(got, q.Gold)
		singleRecall += float64(found) / float64(total)
		if found == total {
			singleFull, realisticFull, oracleFull = singleFull+1, realisticFull+1, oracleFull+1
			realisticRecall++
			oracleRecall++
			continue
		}

		// Realistic 2nd hop: augment with the top-1 retrieved memory.
		realGot := got
		if len(got) > 0 {
			aug := q.Query + " " + byID[got[0]].Content
			realGot = union(got, recallSet(ctx, t, svc, q.Group, aug, k))
		}
		rf, rt := coverage(realGot, q.Gold)
		realisticRecall += float64(rf) / float64(rt)
		if rf == rt {
			realisticFull++
		}

		// Oracle 2nd hop: augment with a found-gold memory (known bridge).
		oracleGot := got
		if bridge := firstFoundGold(got, q.Gold); bridge != "" {
			aug := q.Query + " " + byID[bridge].Content
			oracleGot = union(got, recallSet(ctx, t, svc, q.Group, aug, k))
		}
		of, ot := coverage(oracleGot, q.Gold)
		oracleRecall += float64(of) / float64(ot)
		if of == ot {
			oracleFull++
		}
	}

	n := float64(len(qs))
	t.Logf("cat-1 multi-hop retrieval diagnostic — %d questions, k=%d, embedder=%s", len(qs), k, model)
	t.Logf("%-28s | %-18s | %s", "regime", "full-gold coverage", "mean gold recall")
	t.Logf("%s", "-----------------------------+--------------------+-----------------")
	t.Logf("%-28s | %16.1f%% | %.1f%%", "single recall", pct(singleFull, n), singleRecall/n*100)
	t.Logf("%-28s | %16.1f%% | %.1f%%", "+ realistic 2nd hop (top-1)", pct(realisticFull, n), realisticRecall/n*100)
	t.Logf("%-28s | %16.1f%% | %.1f%%", "+ oracle 2nd hop (gold)", pct(oracleFull, n), oracleRecall/n*100)
}

func ingestItems(ctx context.Context, st *sqlitevec.Store, e embed.Embedder, items []bench.Item) error {
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

func recallSet(ctx context.Context, t *testing.T, svc *service.Service, ns, query string, k int) []string {
	t.Helper()
	res, err := svc.Recall(ctx, service.RecallInput{Namespace: ns, Query: query, Limit: k})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	ids := make([]string, 0, len(res))
	for _, s := range res {
		ids = append(ids, s.Memory.ID)
	}
	return ids
}

func coverage(got, gold []string) (found, total int) {
	set := make(map[string]bool, len(got))
	for _, g := range got {
		set[g] = true
	}
	for _, g := range gold {
		if set[g] {
			found++
		}
	}
	return found, len(gold)
}

func union(a, b []string) []string {
	seen := make(map[string]bool, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, x := range a {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	for _, x := range b {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

func firstFoundGold(got, gold []string) string {
	set := make(map[string]bool, len(got))
	for _, g := range got {
		set[g] = true
	}
	for _, g := range gold {
		if set[g] {
			return g
		}
	}
	return ""
}
