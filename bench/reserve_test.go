package bench_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/eleboucher/memini/internal/embed"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/service"
	"github.com/eleboucher/memini/internal/store/sqlitevec"
)

// TestSemanticReserveTierMixed measures what MEMINI_RECALL_SEMANTIC_RESERVE
// actually does — something the LongMemEval/LoCoMo suites cannot, because they
// ingest every item as TierSemantic (bench/system.go), so reserveDurableTiers
// is a guaranteed no-op there.
//
// It builds a tier-MIXED corpus that mimics the post-turn-capture world: a terse
// durable fact per topic (TierSemantic) buried under verbose episodic chatter
// about the same topic (TierEpisodic) that out-ranks it. Then it reports, for
// reserve ∈ {0,1,2,3}:
//   - durable-gold recall@5/@10: does reserving slots recover the crowded-out fact?
//   - episodic-gold recall@5/@10: what does reserving COST when the answer is a turn?
//
// Needs a live embedder (the dev endpoint). Skips when unreachable, so CI without
// one is unaffected. Point it with MEMINI_EMBED_BASE_URL / _MODEL / _DIMS.
func TestSemanticReserveTierMixed(t *testing.T) {
	ctx := context.Background()

	baseURL := envOr("MEMINI_EMBED_BASE_URL", "http://127.0.0.1:8001/v1")
	model := envOr("MEMINI_EMBED_MODEL", "text-embedding-qwen3-embedding-0.6b")
	dims := envIntOr("MEMINI_EMBED_DIMS", 1024)

	e, err := embed.NewOpenAI(embed.OpenAIConfig{BaseURL: baseURL, Model: model, Dims: dims})
	if err != nil {
		t.Skipf("embedder config: %v", err)
	}
	probe, err := e.Embed(ctx, []string{"connectivity probe"})
	if err != nil {
		t.Skipf("live embedder unreachable at %s (%s): %v", baseURL, model, err)
	}
	// Guard against a dims mismatch silently producing meaningless recall numbers
	// (e.g. MEMINI_EMBED_DIMS=1024 but the model returns 768).
	if len(probe) != 1 || len(probe[0]) != dims {
		t.Skipf("embedder returned %d-dim vectors, configured for %d — set MEMINI_EMBED_DIMS", len(probe[0]), dims)
	}

	st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "reserve.db"), dims)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	// Fixed clock + sync reinforce → deterministic, no background writes racing
	// queries. Score fusion at the production default (alpha 0.5).
	clk := func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }
	svc := service.New(st, e, service.WithClock(clk), service.WithSyncReinforce(), service.WithScoreFusion(0.5))

	corpus := buildReserveCorpus()
	if err := ingestReserveCorpus(ctx, st, e, corpus, clk()); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	reserves := []int{0, 1, 2, 3}
	type row struct {
		reserve                      int
		durR5, durR10, epiR5, epiR10 float64
	}
	rows := make([]row, 0, len(reserves))
	for _, r := range reserves {
		durHits5, durHits10, epiHits5, epiHits10 := 0, 0, 0, 0
		for _, tp := range corpus {
			ids5 := recallIDs(ctx, t, svc, tp.factQuery, 5, r)
			ids10 := recallIDs(ctx, t, svc, tp.factQuery, 10, r)
			if slices.Contains(ids5, tp.factID) {
				durHits5++
			}
			if slices.Contains(ids10, tp.factID) {
				durHits10++
			}
			eids5 := recallIDs(ctx, t, svc, tp.detailQuery, 5, r)
			eids10 := recallIDs(ctx, t, svc, tp.detailQuery, 10, r)
			if slices.Contains(eids5, tp.detailID) {
				epiHits5++
			}
			if slices.Contains(eids10, tp.detailID) {
				epiHits10++
			}
		}
		n := float64(len(corpus))
		rows = append(rows, row{r, pct(durHits5, n), pct(durHits10, n), pct(epiHits5, n), pct(epiHits10, n)})
	}

	t.Logf("tier-mixed reserve eval — %d topics, 1 durable fact + %d episodic chatter each, embedder=%s",
		len(corpus), chatterPerTopic, model)
	t.Logf("%-9s | %-18s | %-18s", "reserve", "durable-gold R@5/10", "episodic-gold R@5/10")
	t.Logf("%s", "----------+--------------------+--------------------")
	for _, r := range rows {
		t.Logf("%-9d | %5.1f%% / %5.1f%%    | %5.1f%% / %5.1f%%",
			r.reserve, r.durR5, r.durR10, r.epiR5, r.epiR10)
	}

	// Sanity assertion (the mechanism must do *something* in the right direction):
	// reserving slots cannot reduce durable-gold R@5 below the no-reserve baseline.
	base, top := rows[0], rows[len(rows)-1]
	if top.durR5 < base.durR5 {
		t.Errorf("durable R@5 regressed with reserve: reserve=0 %.1f%% -> reserve=%d %.1f%%",
			base.durR5, top.reserve, top.durR5)
	}
}

const chatterPerTopic = 12

type reserveTopic struct {
	factID, factQuery string
	detailID          string
	detailQuery       string
	factContent       string
	chatter           []string // chatter[i] has ID <factID-prefix>ep-i
	chatterIDs        []string
}

// buildReserveCorpus generates tier-mixable topics. Each topic is a distinct
// subject with one terse durable decision and several verbose episodic turns on
// the same subject (so they share lexical+semantic surface and tend to out-rank
// the fact). One chatter line carries a unique detail, used as episodic gold.
func buildReserveCorpus() []reserveTopic {
	subjects := []string{
		"the auth scheme", "the database engine", "the cache TTL", "the retry policy",
		"the log format", "the deploy target", "the rate limiter", "the queue backend",
		"the metrics stack", "the feature flag store", "the migration tool", "the API versioning",
	}
	decisions := []string{
		"bearer tokens on every public route", "Postgres with pgvector", "a 10 minute TTL",
		"exponential backoff capped at 5 tries", "structured JSON with slog", "a Kubernetes Helm chart",
		"a token-bucket at 100 req/s", "NATS JetStream", "Prometheus plus Grafana",
		"a Postgres-backed flag table", "golang-migrate with up/down files", "a /v1 prefix with no breaking v2 yet",
	}
	// Unique details, one per topic, that live in exactly one chatter line.
	details := []string{
		"the on-call rotation was three people that week",
		"the staging box had only 8GB of RAM",
		"Maria was out sick during that thread",
		"we measured a 250ms p99 on the slow path",
		"the incident ticket number was 4417",
		"the demo was scheduled for the Friday after",
		"the intern wrote the first draft of it",
		"there was a typo in the original RFC title",
		"the budget review pushed it two sprints",
		"the vendor quote came in at 12k a year",
		"the rollback took roughly 40 minutes",
		"the design doc was 14 pages long",
	}

	topics := make([]reserveTopic, 0, len(subjects))
	for i, subj := range subjects {
		factID := fmt.Sprintf("fact-%02d", i)
		tp := reserveTopic{
			factID:      factID,
			factContent: fmt.Sprintf("Decision: for %s, the team standardized on %s.", subj, decisions[i]),
			// Conversational query that does NOT echo "decision/standardized" — so
			// the terse fact has to win on topical relevance against the chatter,
			// not on a keyword gift. This is the regime where reserve can matter.
			factQuery: fmt.Sprintf("Catch me up on %s — what's the current state of it?", subj),
		}
		for j := range chatterPerTopic {
			id := fmt.Sprintf("ep-%02d-%d", i, j)
			tp.chatterIDs = append(tp.chatterIDs, id)
			line := fmt.Sprintf(
				"In standup #%d we kept going back and forth about %s. Several people had strong opinions on %s and nobody fully agreed on %s yet.",
				j, subj, subj, subj)
			if j == 2 {
				// The episodic-gold line: same subject surface, plus a unique detail.
				line = fmt.Sprintf(
					"While we were arguing about %s, someone mentioned that %s, which slowed the whole thread down.",
					subj, details[i])
				tp.detailID = id
				tp.detailQuery = details[i] + "?"
			}
			tp.chatter = append(tp.chatter, line)
		}
		topics = append(topics, tp)
	}
	return topics
}

// reserveNS is the namespace the eval ingests into and queries.
const reserveNS = "reserve-eval"

func ingestReserveCorpus(ctx context.Context, st *sqlitevec.Store, e embed.Embedder, topics []reserveTopic, now time.Time) error {
	var texts []string
	var ids []string
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
		if err := st.Upsert(ctx, &memory.Memory{
			ID: ids[i], Namespace: reserveNS, Tier: tiers[i], Content: texts[i],
			CreatedAt: now, UpdatedAt: now, LastAccessedAt: now, Embedding: vecs[i],
		}); err != nil {
			return err
		}
	}
	return nil
}

func recallIDs(ctx context.Context, t *testing.T, svc *service.Service, query string, k, reserve int) []string {
	t.Helper()
	res, err := svc.Recall(ctx, service.RecallInput{
		Namespace: reserveNS, Query: query, Limit: k, SemanticReserve: reserve,
	})
	if err != nil {
		t.Fatalf("recall %q (k=%d reserve=%d): %v", query, k, reserve, err)
	}
	ids := make([]string, 0, len(res))
	for _, s := range res {
		ids = append(ids, s.Memory.ID)
	}
	return ids
}

func pct(hits int, n float64) float64 {
	if n == 0 {
		return 0
	}
	return float64(hits) / n * 100
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envIntOr(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}
