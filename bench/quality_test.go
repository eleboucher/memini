package bench_test

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/service"
	"github.com/eleboucher/memini/internal/store/sqlitevec"
)

// TestRecallQualityScoreboard is the recall-quality baseline scoreboard: one
// tier-mixed labeled corpus, the production ranking configuration, and both
// sides of the quality ledger — recall (durable facts AND episodic details)
// and injection precision (how much of the returned window is on-topic). The
// tier-mix and reserve evals measure recall mechanics; the failures we have
// actually shipped (spray) were precision failures, so ranking changes are
// judged against this scoreboard on both axes.
//
// The corpus extends buildHardReserveCorpus (near-duplicate durable/episodic
// pairs, same-template distractor durables, episodic-only topics) with:
//   - confusable topics: episodic-only subjects that are lexical neighbours of
//     fact topics ("the database backup schedule" vs the "database engine"
//     fact), so an injected durable is nearby in embedding space, not random;
//   - episodic-gold details: one chatter line per queried topic carrying a
//     unique detail, plus a question only that line answers — episodic recall
//     under tier mix, where the reserve's evictions have a measurable cost.
//
// Query classes and their metrics (all at Limit=5):
//   - fact:       "catch me up" on a topic with a buried durable → durable
//     R@5 / MRR@5 (the recall side);
//   - episodic / confusable: "catch me up" on a topic with NO durable → any
//     durable in the window is an injection (spray, the precision side);
//   - detail:     the detail question → episodic-gold R@5 / MRR@5.
//
// P@3/P@5 is macro-averaged on-topic precision over every query (topical
// relevance is decidable by ID prefix). Rows: reserve=0 (pure relevance
// reference) and the production config (reserve=2, promote ratio 0.6).
// Embedders resolve via MEMINI_SWEEP_EMBEDDERS like the gate sweep; setting
// MEMINI_RERANK_URL additionally scores the embedder+reranker deployment.
func TestRecallQualityScoreboard(t *testing.T) {
	ctx := context.Background()

	embedders := sweepEmbedders(t, ctx)
	if len(embedders) == 0 {
		t.Skip("no sweep embedder reachable")
	}

	rerankModes := []struct {
		name string
		opts []service.Option
	}{{name: "composite"}}
	if ro := maybeReranker(t); ro != nil {
		rerankModes = append(rerankModes, struct {
			name string
			opts []service.Option
		}{name: "reranked", opts: ro})
	}

	corpus := buildQualityCorpus()
	clk := func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }

	// Production is reserve=2 with the default promote ratio; reserve=0 is the
	// pure-relevance reference that prices what the reserve buys and costs.
	configs := []struct {
		label   string
		reserve int
		opts    []service.Option
	}{
		{label: "reserve=0", reserve: 0},
		{label: "production", reserve: 2},
	}

	for _, emb := range embedders {
		t.Run(emb.name, func(t *testing.T) {
			vecs, err := emb.e.Embed(ctx, corpus.texts)
			if err != nil {
				t.Fatalf("embed corpus (%s): %v", emb.name, err)
			}

			for _, mode := range rerankModes {
				t.Logf("=== %s / %s — %d memories, %d fact / %d no-durable / %d detail queries (%d near-dup, %d distractor durables)",
					emb.name, mode.name, len(corpus.texts), corpus.nFact, corpus.nSpray, corpus.nDetail, corpus.nearDups, corpus.distractors)
				t.Logf("%-11s | dur R@5 / MRR  | epi R@5 / MRR  | P@3   | P@5   | spray q/n | inj q/n | fact/det n", "config")
				t.Logf("%s", "------------+----------------+----------------+-------+-------+-----------+---------+-----------")

				var baselineDurR5 float64
				for _, cfg := range configs {
					st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "quality.db"), emb.dims)
					if err != nil {
						t.Fatalf("open store: %v", err)
					}
					for i := range corpus.texts {
						if err := st.Upsert(ctx, &memory.Memory{
							ID: corpus.ids[i], Namespace: reserveNS, Tier: corpus.tiers[i], Content: corpus.texts[i],
							CreatedAt: clk(), UpdatedAt: clk(), LastAccessedAt: clk(), Embedding: vecs[i],
						}); err != nil {
							t.Fatalf("upsert: %v", err)
						}
					}
					opts := append([]service.Option{
						service.WithClock(clk), service.WithSyncReinforce(), service.WithScoreFusion(0.5),
					}, cfg.opts...)
					opts = append(opts, mode.opts...)
					svc := service.New(st, emb.e, opts...)

					m := runQualityQueries(ctx, t, svc, corpus, cfg.reserve)
					_ = st.Close()

					t.Logf("%-11s | %6.1f%% / %.3f | %6.1f%% / %.3f | %.3f | %.3f | %4d/%-4d | %3d/%-3d | %3d/%-3d",
						cfg.label, m.durR5, m.durMRR, m.epiR5, m.epiMRR, m.p3, m.p5, m.sprayQ, m.sprayN, m.injQ, m.injN, m.injFactN, m.injDetailN)

					if cfg.label == "reserve=0" {
						baselineDurR5 = m.durR5
					} else if m.durR5 < baselineDurR5 {
						// Structural sanity: the reserve only adds durables to
						// the window, so it cannot reduce durable recall.
						t.Errorf("%s/%s %s: durable R@5 %.1f%% below reserve=0 baseline %.1f%%",
							emb.name, mode.name, cfg.label, m.durR5, baselineDurR5)
					}
				}
			}
		})
	}
}

// qualityMetrics aggregates one config's run over the full query plan.
type qualityMetrics struct {
	durR5, durMRR        float64 // fact queries: gold durable in top-5, reciprocal rank
	epiR5, epiMRR        float64 // detail queries: gold episodic line in top-5
	p3, p5               float64 // macro on-topic precision over all queries
	sprayQ, sprayN       int     // no-durable topic queries with ≥1 injected durable / total injected
	injQ, injN           int     // same over every query
	injFactN, injDetailN int     // injected-durable counts split by query class
}

func runQualityQueries(ctx context.Context, t *testing.T, svc *service.Service, c qualityCorpus, reserve int) qualityMetrics {
	t.Helper()
	var m qualityMetrics
	var p3Sum, p5Sum float64
	for _, q := range c.queries {
		res, err := svc.Recall(ctx, service.RecallInput{
			Namespace: reserveNS, Query: q.text, Limit: 5, SemanticReserve: reserve,
		})
		if err != nil {
			t.Fatalf("recall %q: %v", q.text, err)
		}
		rel3, rel5, injected := 0, 0, 0
		goldRank := 0
		for i, r := range res {
			if r.Memory.ID == q.goldID {
				goldRank = i + 1
			}
			if onTopic(r.Memory.ID, q.key) {
				rel5++
				if i < 3 {
					rel3++
				}
			} else if r.Memory.Tier == memory.TierSemantic || r.Memory.Tier == memory.TierProcedural {
				injected++
			}
		}
		if len(res) > 0 {
			p5Sum += float64(rel5) / float64(len(res))
			p3Sum += float64(rel3) / float64(min(3, len(res)))
		}
		if injected > 0 {
			m.injQ++
		}
		m.injN += injected
		switch q.class {
		case "episodic", "confusable":
			if injected > 0 {
				m.sprayQ++
			}
			m.sprayN += injected
		case "fact":
			m.injFactN += injected
		case "detail":
			m.injDetailN += injected
		}
		switch q.class {
		case "fact":
			if goldRank > 0 {
				m.durR5++
				m.durMRR += 1 / float64(goldRank)
			}
		case "detail":
			if goldRank > 0 {
				m.epiR5++
				m.epiMRR += 1 / float64(goldRank)
			}
		}
	}
	m.durR5 = pct(int(m.durR5), float64(c.nFact))
	m.durMRR /= float64(c.nFact)
	m.epiR5 = pct(int(m.epiR5), float64(c.nDetail))
	m.epiMRR /= float64(c.nDetail)
	n := float64(len(c.queries))
	m.p3 = p3Sum / n
	m.p5 = p5Sum / n
	return m
}

// onTopic reports whether a memory ID belongs to the query's topic. IDs are
// "<key>-..." with distinct keys, so a prefix check is exact.
func onTopic(id, key string) bool {
	return len(id) > len(key) && id[:len(key)] == key && id[len(key)] == '-'
}

// qualityQuery is one scored recall: class decides which metric it feeds.
type qualityQuery struct {
	key    string // topic key — on-topic relevance is decidable by ID prefix
	text   string
	goldID string // durable fact (class fact) or episodic detail line (class detail)
	class  string // "fact" | "episodic" | "confusable" | "detail"
}

// qualityCorpus is the flattened ingest set plus the query plan.
type qualityCorpus struct {
	texts []string
	ids   []string
	tiers []memory.Tier

	nearDups, distractors  int
	nFact, nDetail, nSpray int
	queries                []qualityQuery
}

// buildQualityCorpus extends the gate sweep's hard corpus with confusable
// episodic-only topics and per-topic episodic-gold detail lines. It leaves
// buildHardReserveCorpus itself untouched so the sweep's documented evidence
// stays reproducible.
func buildQualityCorpus() qualityCorpus {
	base := buildHardReserveCorpus()
	c := qualityCorpus{
		texts: base.texts, ids: base.ids, tiers: base.tiers,
		nearDups: base.nearDups, distractors: base.distractors,
	}
	for _, q := range base.queries {
		class := "episodic"
		if q.factID != "" {
			class = "fact"
			c.nFact++
		} else {
			c.nSpray++
		}
		c.queries = append(c.queries, qualityQuery{key: q.key, text: q.text, goldID: q.factID, class: class})
	}

	// Lexical neighbours of fact topics, episodic-only: the nearby durable
	// ("Postgres with pgvector") is the natural injection when asked about
	// "the database backup schedule" — the hardest precision case.
	confusables := []struct{ key, subject string }{
		{"dbbackup", "the database backup schedule"},
		{"authoncall", "the auth team on-call rotation"},
		{"cachehw", "the cache server hardware upgrade"},
		{"deployfreeze", "the deploy freeze calendar"},
	}
	for _, tp := range confusables {
		for j := range sweepChatterPerTopic {
			c.texts = append(c.texts, fmt.Sprintf(
				"In standup #%d we kept going back and forth about %s. Several people had strong opinions on %s and nobody fully agreed on %s yet.",
				j, tp.subject, tp.subject, tp.subject))
			c.ids = append(c.ids, fmt.Sprintf("%s-ep-%d", tp.key, j))
			c.tiers = append(c.tiers, memory.TierEpisodic)
		}
		c.queries = append(c.queries, qualityQuery{
			key:   tp.key,
			text:  fmt.Sprintf("Catch me up on %s — what's the current state of it?", tp.subject),
			class: "confusable",
		})
		c.nSpray++
	}

	// One unique detail per queried topic, living in exactly one chatter line;
	// the question is answerable only by that line (episodic gold).
	details := []struct{ key, subject, fact, question string }{
		{"auth", "the auth scheme", "the pen-test report flagged two endpoints", "How many endpoints did the pen-test report flag?"},
		{"db", "the database engine", "the staging box had only 8GB of RAM", "How much RAM did the staging box have?"},
		{"cache", "the cache TTL", "we measured a 250ms p99 on the slow path", "What p99 did we measure on the slow path?"},
		{"retry", "the retry policy", "the incident ticket number was 4417", "What was the incident ticket number?"},
		{"logs", "the log format", "the old parser choked on multiline entries", "What did the old log parser choke on?"},
		{"deploy", "the deploy target", "the rollback took roughly 40 minutes", "How long did the rollback take?"},
		{"rate", "the rate limiter", "the burst spike peaked at 900 requests per second", "What did the burst traffic spike peak at?"},
		{"queue", "the queue backend", "the vendor quote came in at 12k a year", "What did the vendor quote come in at?"},
		{"metrics", "the metrics stack", "the dashboard had 14 panels nobody looked at", "How many unused panels did the dashboard have?"},
		{"flags", "the feature flag store", "the intern wrote the first draft of it", "Who wrote the first draft of it?"},
		{"coffee", "the office coffee machine", "the descaling kit cost 30 euros", "How much did the descaling kit cost?"},
		{"offsite", "the summer team offsite", "the venue double-booked our main room", "What went wrong with the offsite venue booking?"},
		{"parking", "the parking situation", "the garage closes at 11pm on weekdays", "When does the garage close on weekdays?"},
		{"snacks", "the Friday demo snacks", "the bakery order was 40 croissants", "How many croissants were in the bakery order?"},
		{"desks", "the new standing desks", "three desks arrived with missing bolts", "How many desks arrived with missing bolts?"},
		{"holiday", "the holiday rotation", "Maria took the last two weeks of December", "Who took the last two weeks of December off?"},
		{"dbbackup", "the database backup schedule", "the nightly dump ran 90 minutes over", "How long did the nightly dump overrun?"},
		{"authoncall", "the auth team on-call rotation", "the pager escalation had a 15 minute gap", "What gap did the pager escalation have?"},
		{"cachehw", "the cache server hardware upgrade", "the new nodes came with 64GB each", "How much memory do the new cache nodes have?"},
		{"deployfreeze", "the deploy freeze calendar", "the freeze starts the week before Black Friday", "When does the deploy freeze start?"},
	}
	for _, d := range details {
		id := d.key + "-detail"
		c.texts = append(c.texts, fmt.Sprintf(
			"While we were arguing about %s, someone mentioned that %s, which derailed the thread for a while.",
			d.subject, d.fact))
		c.ids = append(c.ids, id)
		c.tiers = append(c.tiers, memory.TierEpisodic)
		c.queries = append(c.queries, qualityQuery{key: d.key, text: d.question, goldID: id, class: "detail"})
		c.nDetail++
	}
	return c
}
