//go:build bench

package bench_test

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/eleboucher/memini/bench"
	"github.com/eleboucher/memini/internal/embed"
	"github.com/eleboucher/memini/internal/extract"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/search"
	"github.com/eleboucher/memini/internal/service"
	"github.com/eleboucher/memini/internal/store"
	"github.com/eleboucher/memini/internal/store/sqlitevec"
)

// TestEntityBoostRerank measures an entity-boost rerank: an LLM-free,
// IDF-weighted overlap between the query's entities and each candidate's,
// added to the fused relevance score. It reorders the existing pool rather than
// expanding it, unlike the entity-bridge in TestEntityBridgeDiagnostic.
//
// Verdict: harmful, do not use in recall. On this synthetic set it is redundant
// (fusion already reaches 100% R@1, since a rare entity token is what BM25
// rewards); on real data (TestEntityBoostRealData, LoCoMo) it degrades recall in
// every category as the weight rises (overall R@1 35.6% -> 10.5% at w=1.0). The
// keyword leg already captures useful entity overlap; adding an entity term to a
// calibrated fusion score injects noise. Kept as a documented negative.
//
// Needs a live embedder; skips when unreachable.
func TestEntityBoostRerank(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	t.Cleanup(cancel)

	baseURL := envOr("MEMINI_EMBED_BASE_URL", "http://127.0.0.1:8001/v1")
	model := envOr("MEMINI_EMBED_MODEL", "text-embedding-qwen3-embedding-0.6b")
	dims := envIntOr("MEMINI_EMBED_DIMS", 1024)

	e, err := embed.NewOpenAI(embed.OpenAIConfig{BaseURL: baseURL, Model: model, Dims: dims})
	if err != nil {
		t.Skipf("embedder config: %v", err)
	}
	probeEmbedder(ctx, t, baseURL, model)

	st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "entityboost.db"), dims)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	topics := buildEntityCorpus()

	// Ingest every memory (episodic, fresh; ranking here is fused relevance +
	// entity boost, not the Quality term).
	var texts, ids []string
	for _, tp := range topics {
		for i, txt := range tp.texts {
			texts = append(texts, txt)
			ids = append(ids, tp.ids[i])
		}
	}
	now := time.Unix(1_700_000_000, 0).UTC()
	for start := 0; start < len(texts); start += 64 {
		end := min(start+64, len(texts))
		vecs, err := e.Embed(ctx, texts[start:end])
		if err != nil {
			t.Fatalf("embed: %v", err)
		}
		for i, v := range vecs {
			if err := st.Upsert(ctx, &memory.Memory{
				ID: ids[start+i], Namespace: reserveNS, Tier: memory.TierEpisodic, Content: texts[start+i],
				CreatedAt: now, UpdatedAt: now, LastAccessedAt: now, Embedding: v,
			}); err != nil {
				t.Fatalf("upsert: %v", err)
			}
		}
	}

	weights := []float64{0, 0.3, 0.6, 1.0}
	// Two backbones: pure vector, and vector+keyword fusion (the production
	// default). Comparing them shows whether the boost only recovers what the
	// keyword leg already provides or adds lift on top of it.
	backbones := []string{"vector-only", "vector+keyword"}
	type acc struct{ r1, r5, rr, n float64 }
	accs := make([][]acc, len(backbones)) // accs[backbone][weight]
	for b := range accs {
		accs[b] = make([]acc, len(weights))
	}

	fetch := 60
	for _, tp := range topics {
		qvec, err := embed.EmbedOne(ctx, e, tp.query)
		if err != nil {
			t.Fatalf("embed query: %v", err)
		}
		vres, err := st.VectorSearch(ctx, reserveNS, qvec, store.Filter{}, fetch)
		if err != nil {
			t.Fatalf("vector search: %v", err)
		}
		kres, err := st.KeywordSearch(ctx, reserveNS, tp.query, store.Filter{}, fetch)
		if err != nil {
			t.Fatalf("keyword search: %v", err)
		}
		pools := [][]store.Scored{
			search.FuseScores([][]store.Scored{vres}, []float64{1}, 0),              // vector-only (normalized)
			search.FuseScores([][]store.Scored{vres, kres}, []float64{0.5, 0.5}, 0), // fusion
		}
		qEnts := entitySet(tp.query)
		for b, pool := range pools {
			idf := poolIDF(pool)
			for wi, w := range weights {
				ranked := entityBoostRerank(pool, qEnts, idf, w)
				rank := 1 + slices.IndexFunc(ranked, func(s store.Scored) bool { return s.Memory.ID == tp.goldID })
				a := &accs[b][wi]
				a.n++
				if rank == 1 {
					a.r1++
				}
				if rank >= 1 && rank <= 5 {
					a.r5++
				}
				if rank >= 1 {
					a.rr += 1.0 / float64(rank)
				}
			}
		}
	}

	t.Logf("entity-boost re-rank - %d entity-scoped topics, embedder=%s (LLM-free)", len(topics), model)
	t.Logf("%-14s | %-8s | %-8s | %-8s | %-8s", "backbone", "boost w", "R@1", "R@5", "MRR")
	t.Logf("%s", "---------------+----------+----------+----------+---------")
	for b, name := range backbones {
		for wi, w := range weights {
			a := accs[b][wi]
			t.Logf("%-14s | %-8.1f | %6.1f%% | %6.1f%% | %6.3f", name, w, pct(int(a.r1), a.n), pct(int(a.r5), a.n), a.rr/a.n)
		}
	}
}

// entityBoostRerank re-ranks fused candidates by min-max-normalized fused
// relevance plus w times an IDF-weighted query/candidate entity overlap
// (normalized to [0,1] across the pool). Hub entities (high pool DF) contribute
// little; a rare shared entity contributes most. w=0 is exactly fused order.
func entityBoostRerank(fused []store.Scored, qEnts map[string]bool, idf map[string]float64, w float64) []store.Scored {
	if len(fused) == 0 {
		return fused
	}
	lo, hi := fused[len(fused)-1].Score, fused[0].Score
	for _, s := range fused { // fused is best-first but don't assume sorted bounds
		lo = math.Min(lo, s.Score)
		hi = math.Max(hi, s.Score)
	}
	span := hi - lo

	boosts := make([]float64, len(fused))
	maxBoost := 0.0
	for i, s := range fused {
		var b float64
		for ent := range entitySet(s.Memory.Content) {
			if qEnts[ent] {
				b += idf[ent]
			}
		}
		boosts[i] = b
		maxBoost = math.Max(maxBoost, b)
	}

	type scored struct {
		sc    store.Scored
		score float64
		pos   int
	}
	out := make([]scored, len(fused))
	for i, s := range fused {
		rel := 1.0
		if span > 0 {
			rel = (s.Score - lo) / span
		}
		boost := 0.0
		if maxBoost > 0 {
			boost = boosts[i] / maxBoost
		}
		out[i] = scored{sc: s, score: rel + w*boost, pos: i}
	}
	slices.SortStableFunc(out, func(a, b scored) int {
		if a.score != b.score {
			if a.score > b.score {
				return -1
			}
			return 1
		}
		return a.pos - b.pos // ties keep fused order
	})
	res := make([]store.Scored, len(out))
	for i, s := range out {
		res[i] = s.sc
	}
	return res
}

// entitySet returns the lowercased heuristic entities of a text as a set.
func entitySet(text string) map[string]bool {
	out := map[string]bool{}
	for _, ent := range extract.Entities(text) {
		out[strings.ToLower(ent)] = true
	}
	return out
}

// poolIDF computes inverse document frequency of each entity across the pool:
// log(1 + N/df). A hub entity in most candidates scores near log(2); a rare one
// scores high, so the boost rewards distinctive shared entities, not hubs.
func poolIDF(pool []store.Scored) map[string]float64 {
	df := map[string]int{}
	for _, s := range pool {
		for ent := range entitySet(s.Memory.Content) {
			df[ent]++
		}
	}
	n := float64(len(pool))
	idf := make(map[string]float64, len(df))
	for ent, d := range df {
		idf[ent] = math.Log(1 + n/float64(d))
	}
	return idf
}

// TestEntityBoostRealData runs the entity boost over LoCoMo across query
// categories, comparing fusion against fusion+boost at two weights. A rerank
// signal is measurable on the QA suites, unlike write-path features. Skips
// without a live embedder or the gitignored dataset; set MEMINI_LOCOMO_DATA.
func TestEntityBoostRealData(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	t.Cleanup(cancel)

	baseURL := envOr("MEMINI_EMBED_BASE_URL", "http://127.0.0.1:8001/v1")
	model := envOr("MEMINI_EMBED_MODEL", "text-embedding-qwen3-embedding-0.6b")
	dims := envIntOr("MEMINI_EMBED_DIMS", 1024)

	e, err := embed.NewOpenAI(embed.OpenAIConfig{BaseURL: baseURL, Model: model, Dims: dims})
	if err != nil {
		t.Skipf("embedder config: %v", err)
	}
	probeEmbedder(ctx, t, baseURL, model)

	dataPath := envOr("MEMINI_LOCOMO_DATA", "data/locomo10.json")
	if _, statErr := os.Stat(dataPath); statErr != nil {
		t.Skipf("dataset not found (gitignored): %v", statErr)
	}
	ds, err := bench.LoadLoCoMo(dataPath)
	if err != nil {
		t.Fatalf("load locomo: %v", err)
	}

	st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "entityboost-real.db"), dims)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := bench.IngestQAUpsert(ctx, st, e, ds.Items); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	k := 10
	fetch := service.RecallPoolSize(k)
	qp := os.Getenv("MEMINI_EMBED_QUERY_PREFIX")
	systems := []struct {
		name string
		w    float64
	}{{"fusion", 0}, {"fusion+ent0.5", 0.5}, {"fusion+ent1.0", 1.0}}

	const catAll = "all"
	accs := make([]map[string]*ebAcc, len(systems))
	for i := range accs {
		accs[i] = map[string]*ebAcc{}
	}
	bump := func(m map[string]*ebAcc, cat string) *ebAcc {
		if m[cat] == nil {
			m[cat] = &ebAcc{}
		}
		return m[cat]
	}

	for _, q := range ds.Questions {
		qvec, err := embed.EmbedOne(ctx, e, qp+q.Query)
		if err != nil {
			t.Fatalf("embed query: %v", err)
		}
		ns := bench.NamespaceOf(q.Group)
		vres, err := st.VectorSearch(ctx, ns, qvec, store.Filter{}, fetch)
		if err != nil {
			t.Fatalf("vector search: %v", err)
		}
		kres, err := st.KeywordSearch(ctx, ns, q.Query, store.Filter{}, fetch)
		if err != nil {
			t.Fatalf("keyword search: %v", err)
		}
		fused := search.FuseScores([][]store.Scored{vres, kres}, []float64{0.5, 0.5}, 0)
		qEnts := entitySet(q.Query)
		idf := poolIDF(fused)
		for si, s := range systems {
			ranked := entityBoostRerank(fused, qEnts, idf, s.w)
			cat := q.Category
			if cat == "" {
				cat = "uncategorized"
			}
			for _, c := range []string{cat, catAll} {
				ebScoreOne(bump(accs[si], c), ranked, q.Gold, k)
			}
		}
	}

	cats := make([]string, 0, len(accs[0]))
	for c := range accs[0] {
		cats = append(cats, c)
	}
	slices.Sort(cats)

	t.Logf("entity-boost on real data (%s, %d questions) - recall@1 / recall@%d / MRR", ds.Name, len(ds.Questions), k)
	t.Logf("%-16s | %-14s | %5s | %6s | %6s | %6s", "category", "system", "Q", "R@1", "R@K", "MRR")
	t.Logf("%s", strings.Repeat("-", 72))
	for _, cat := range cats {
		for si, s := range systems {
			a := accs[si][cat]
			t.Logf("%-16s | %-14s | %5d | %5.1f%% | %5.1f%% | %5.1f%%",
				cat, s.name, int(a.n), pct(int(a.r1), a.n), pct(int(a.rk), a.n), a.rr/a.n*100)
		}
	}
}

// ebAcc accumulates recall@1, recall@k, reciprocal rank, and count for the
// entity-boost real-data eval (the package-bench rerankAcc is not exported).
type ebAcc struct{ r1, rk, rr, n float64 }

func ebScoreOne(a *ebAcc, ranked []store.Scored, gold []string, k int) {
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

type entityTopic struct {
	query  string
	goldID string
	texts  []string
	ids    []string
}

// buildEntityCorpus builds entity-scoped topics: each has a distinctive proper
// noun (a person) tied to a decision on a subject (the gold), plus same-subject
// distractors that are more topically dense but do not name the person. This is
// the regime where an entity signal could help over pure topicality.
func buildEntityCorpus() []entityTopic {
	people := []string{
		"Priya", "Dmitri", "Ngozi", "Salvatore", "Fatima", "Bjorn", "Aiko", "Rasmus",
		"Imani", "Thiago", "Yelena", "Kwame", "Sunita", "Marcin", "Oluwaseun", "Henrik",
	}
	subjects := []string{
		"the caching layer", "the auth flow", "the deploy pipeline", "the search index",
		"the queue backend", "the rate limiter", "the metrics stack", "the migration plan",
		"the backup policy", "the API schema", "the log pipeline", "the feature flags",
		"the load test", "the on-call setup", "the secret rotation", "the CDN config",
	}
	decisions := []string{
		"Redis", "short-lived JWTs", "a canary rollout", "an inverted index",
		"NATS JetStream", "a token bucket", "Prometheus", "an expand-contract approach",
		"nightly snapshots", "a versioned envelope", "structured JSON", "a database-backed store",
		"a soak test at 2x peak", "a follow-the-sun rotation", "weekly rotation", "a pull-through cache",
	}
	topics := make([]entityTopic, 0, len(people))
	for i, person := range people {
		subj, dec := subjects[i], decisions[i]
		goldID := fmt.Sprintf("ent-gold-%02d", i)
		tp := entityTopic{
			// Query names the person + subject but not the decision wording.
			query:  fmt.Sprintf("What did %s decide about %s?", person, subj),
			goldID: goldID,
		}
		// Gold: names the person, terse, mentions the subject once (lower subject
		// density than the distractors, so vector alone ranks it below them).
		tp.texts = append(tp.texts, fmt.Sprintf("%s decided on %s for %s.", person, dec, subj))
		tp.ids = append(tp.ids, goldID)
		// Distractor A: subject-saturated, no person.
		tp.texts = append(tp.texts, fmt.Sprintf("%s was the main topic all sprint: we discussed %s, reviewed %s, and revisited %s after the latency incident.", capitalize(subj), subj, subj, subj))
		tp.ids = append(tp.ids, fmt.Sprintf("ent-da-%02d", i))
		// Distractor B: subject-heavy plus a different person, so the entity has to
		// discriminate which person, not just the presence of a name.
		other := people[(i+1)%len(people)]
		tp.texts = append(tp.texts, fmt.Sprintf("%s kept raising %s and %s in every standup, pushing hard on %s.", other, subj, subj, subj))
		tp.ids = append(tp.ids, fmt.Sprintf("ent-db-%02d", i))
		// Distractor C: subject-heavy generic chatter.
		tp.texts = append(tp.texts, fmt.Sprintf("The thread about %s kept going; %s this, %s that, and still nobody settled %s.", subj, subj, subj, subj))
		tp.ids = append(tp.ids, fmt.Sprintf("ent-dc-%02d", i))
		topics = append(topics, tp)
	}
	return topics
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
