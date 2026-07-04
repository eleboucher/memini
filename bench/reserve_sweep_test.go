package bench_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/eleboucher/memini/internal/embed"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/service"
	"github.com/eleboucher/memini/internal/store/sqlitevec"
)

// TestReserveGateSweep hardens the reserve relevance-gate evidence beyond the
// single-embedder tier-mix evals: it sweeps the gate configuration (fixed
// promote ratios, the adaptive percentile gate, plus ungated and reserve-off
// references) over a corpus built to break the gate both ways, and reports the
// recall/precision tradeoff per embedder.
//
// The corpus is harder than buildReserveCorpus on purpose:
//   - near-duplicate durable/episodic pairs: an episodic turn that restates the
//     durable fact almost verbatim, so the fact must win a same-topic fight;
//   - distractor durables: fresh facts on never-queried topics, phrased with
//     the same template as real facts — the injection ammunition;
//   - mixed queries: some topics have a relevant durable (recall side), some
//     are episodic-only (precision side, where any durable in the window is an
//     injection).
//
// Metrics per gate config, over all queries at Limit=5 / SemanticReserve=2:
//   - durable R@5 and MRR@5 on the fact queries (did the gate let the buried
//     fact in, and how high?);
//   - precision@3/@5: fraction of returned memories relevant to the query's
//     topic (the injection-rate complement);
//   - inj q / inj n: queries with at least one off-topic durable in the top-5,
//     and the total count of such results.
//
// Embedders come from MEMINI_SWEEP_EMBEDDERS ("url|model|dims" entries,
// comma-separated) or a built-in dev list; unreachable ones are skipped so the
// suite degrades to whatever is live. Setting MEMINI_RERANK_URL additionally
// runs the whole sweep through the cross-encoder path — the reranker reorders
// the top-k window (membership is decided by the gate upstream), so it moves
// MRR and P@3, not R@5.
func TestReserveGateSweep(t *testing.T) {
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

	corpus := buildHardReserveCorpus()
	clk := func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }

	type gateConfig struct {
		label   string
		reserve int
		opts    []service.Option
	}
	configs := make([]gateConfig, 0, 11)
	configs = append(configs,
		gateConfig{label: "reserve=0", reserve: 0},
		gateConfig{label: "ungated", reserve: 2, opts: []service.Option{
			service.WithReservePromoteRatio(0), service.WithReserveTopAnchor(0),
		}},
	)
	// Ratio rows isolate the evictee leg (anchor off); anchor rows isolate the
	// absolute leg (ratio off) at the production reserve.
	for _, r := range []float64{0.4, 0.5, 0.6, 0.7, 0.8, 0.9} {
		configs = append(configs, gateConfig{
			label: fmt.Sprintf("ratio %.1f", r), reserve: 2,
			opts: []service.Option{service.WithReservePromoteRatio(r), service.WithReserveTopAnchor(0)},
		})
	}
	for _, a := range []float64{0.3, 0.4, 0.5, 0.6} {
		configs = append(configs, gateConfig{
			label: fmt.Sprintf("anchor %.1f", a), reserve: 2,
			opts: []service.Option{service.WithReservePromoteRatio(0), service.WithReserveTopAnchor(a)},
		})
	}
	for _, p := range []float64{5, 10, 25} {
		configs = append(configs, gateConfig{
			label: fmt.Sprintf("adaptive p%02.0f", p), reserve: 2,
			opts: []service.Option{service.WithReserveGatePercentile(p)},
		})
	}

	for _, emb := range embedders {
		t.Run(emb.name, func(t *testing.T) {
			vecs, err := emb.e.Embed(ctx, corpus.texts)
			if err != nil {
				t.Fatalf("embed corpus (%s): %v", emb.name, err)
			}

			for _, mode := range rerankModes {
				var baselineDurR5 float64
				t.Logf("=== %s / %s — %d fact topics (%d near-dup), %d episodic-only, %d distractor durables",
					emb.name, mode.name, len(corpus.factKeys), corpus.nearDups, len(corpus.episodicKeys), corpus.distractors)
				t.Logf("%-13s | dur R@5 | dur MRR | P@3   | P@5   | inj q/%d | inj n", "gate", len(corpus.queries))
				t.Logf("%s", "--------------+---------+---------+-------+-------+----------+------")

				for _, cfg := range configs {
					st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "sweep.db"), emb.dims)
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

					m := runSweepQueries(ctx, t, svc, corpus, cfg.reserve)
					_ = st.Close()

					t.Logf("%-13s | %6.1f%% | %7.3f | %5.3f | %5.3f | %5d    | %4d",
						cfg.label, m.durR5, m.durMRR, m.p3, m.p5, m.injQueries, m.injTotal)

					if cfg.label == "reserve=0" {
						baselineDurR5 = m.durR5
					} else if m.durR5 < baselineDurR5 {
						// Structural sanity, not tuning: the reserve only ever adds
						// durables to the window, so no gate config can push durable
						// recall below the reserve-off baseline.
						t.Errorf("%s/%s %s: durable R@5 %.1f%% below reserve=0 baseline %.1f%%",
							emb.name, mode.name, cfg.label, m.durR5, baselineDurR5)
					}
				}
			}
		})
	}
}

// sweepMetrics aggregates one gate config's run over the full query set.
type sweepMetrics struct {
	durR5, durMRR, p3, p5 float64
	injQueries, injTotal  int
}

func runSweepQueries(ctx context.Context, t *testing.T, svc *service.Service, c hardCorpus, reserve int) sweepMetrics {
	t.Helper()
	var m sweepMetrics
	var p3Sum, p5Sum float64
	factQueries := 0
	for _, q := range c.queries {
		res, err := svc.Recall(ctx, service.RecallInput{
			Namespace: reserveNS, Query: q.text, Limit: 5, SemanticReserve: reserve,
		})
		if err != nil {
			t.Fatalf("recall %q: %v", q.text, err)
		}
		rel3, rel5, injected := 0, 0, 0
		for i, r := range res {
			onTopic := strings.HasPrefix(r.Memory.ID, q.key+"-")
			if onTopic {
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
			m.injQueries++
			m.injTotal += injected
		}
		if q.factID != "" {
			factQueries++
			for i, r := range res {
				if r.Memory.ID == q.factID {
					m.durR5++
					m.durMRR += 1 / float64(i+1)
					break
				}
			}
		}
	}
	n := float64(len(c.queries))
	m.durR5 = pct(int(m.durR5), float64(factQueries))
	m.durMRR /= float64(factQueries)
	m.p3 = p3Sum / n
	m.p5 = p5Sum / n
	return m
}

// hardCorpus is the flattened ingest set plus the query plan. Every memory ID
// is "<topic key>-..." so topical relevance is decidable by prefix.
type hardCorpus struct {
	texts []string
	ids   []string
	tiers []memory.Tier

	factKeys     []string
	episodicKeys []string
	nearDups     int
	distractors  int

	queries []sweepQuery
}

type sweepQuery struct {
	key    string
	text   string
	factID string // "" for episodic-only topics
}

const sweepChatterPerTopic = 10

// buildHardReserveCorpus assembles the mixed corpus described on the test.
// Topic domains are deliberately disjoint so "off-topic" is unambiguous.
func buildHardReserveCorpus() hardCorpus {
	factTopics := []struct {
		key, subject, decision string
		nearDup                bool
	}{
		{"auth", "the auth scheme", "bearer tokens on every public route", true},
		{"db", "the database engine", "Postgres with pgvector", true},
		{"cache", "the cache TTL", "a 10 minute TTL", true},
		{"retry", "the retry policy", "exponential backoff capped at 5 tries", true},
		{"logs", "the log format", "structured JSON with slog", true},
		{"deploy", "the deploy target", "a Kubernetes Helm chart", false},
		{"rate", "the rate limiter", "a token-bucket at 100 req/s", false},
		{"queue", "the queue backend", "NATS JetStream", false},
		{"metrics", "the metrics stack", "Prometheus plus Grafana", false},
		{"flags", "the feature flag store", "a Postgres-backed flag table", false},
	}
	episodicTopics := []struct{ key, subject string }{
		{"coffee", "the office coffee machine"},
		{"offsite", "the summer team offsite"},
		{"parking", "the parking situation"},
		{"snacks", "the Friday demo snacks"},
		{"desks", "the new standing desks"},
		{"holiday", "the holiday rotation"},
	}
	// Never queried; same "Decision:" template as the real facts so they sit in
	// the same region of embedding space that the gate must keep out.
	distractorFacts := []struct{ key, subject, decision string }{
		{"dx-brand", "the company rebrand", "the new logo and a two-color palette"},
		{"dx-hire", "the hiring bar", "two technical interviews plus a take-home"},
		{"dx-perf", "the perf review cycle", "twice-yearly written reviews"},
		{"dx-travel", "the travel policy", "economy under six hours, premium beyond"},
		{"dx-oss", "the open-source policy", "MIT for tools, internal for services"},
		{"dx-sec", "the laptop security baseline", "full-disk encryption and a hardware key"},
		{"dx-meet", "the meeting policy", "no-meeting Wednesdays"},
		{"dx-exp", "the expense process", "receipts over 25 euros filed within a month"},
	}

	var c hardCorpus
	add := func(id, text string, tier memory.Tier) {
		c.ids = append(c.ids, id)
		c.texts = append(c.texts, text)
		c.tiers = append(c.tiers, tier)
	}
	chatter := func(key, subject string) {
		for j := range sweepChatterPerTopic {
			add(fmt.Sprintf("%s-ep-%d", key, j), fmt.Sprintf(
				"In standup #%d we kept going back and forth about %s. Several people had strong opinions on %s and nobody fully agreed on %s yet.",
				j, subject, subject, subject), memory.TierEpisodic)
		}
	}
	query := func(subject string) string {
		return fmt.Sprintf("Catch me up on %s — what's the current state of it?", subject)
	}

	for _, tp := range factTopics {
		factID := tp.key + "-fact"
		add(factID, fmt.Sprintf("Decision: for %s, the team standardized on %s.", tp.subject, tp.decision), memory.TierSemantic)
		if tp.nearDup {
			c.nearDups++
			add(tp.key+"-dup", fmt.Sprintf("Recap from the thread: for %s the team standardized on %s.", tp.subject, tp.decision), memory.TierEpisodic)
		}
		chatter(tp.key, tp.subject)
		c.factKeys = append(c.factKeys, tp.key)
		c.queries = append(c.queries, sweepQuery{key: tp.key, text: query(tp.subject), factID: factID})
	}
	for _, tp := range episodicTopics {
		chatter(tp.key, tp.subject)
		c.episodicKeys = append(c.episodicKeys, tp.key)
		c.queries = append(c.queries, sweepQuery{key: tp.key, text: query(tp.subject)})
	}
	for _, d := range distractorFacts {
		c.distractors++
		add(d.key+"-fact", fmt.Sprintf("Decision: for %s, the team standardized on %s.", d.subject, d.decision), memory.TierSemantic)
	}
	return c
}

// sweepEmbedder is one live embedding endpoint the sweep runs against.
type sweepEmbedder struct {
	name string
	e    embed.Embedder
	dims int
}

// sweepEmbedders resolves the embedder matrix: MEMINI_SWEEP_EMBEDDERS as
// comma-separated "baseURL|model|dims" entries, else a built-in dev list.
// Unreachable or dims-mismatched endpoints are skipped with a log line.
func sweepEmbedders(t *testing.T, ctx context.Context) []sweepEmbedder {
	t.Helper()
	specs := []string{
		"http://127.0.0.1:8001/v1|text-embedding-qwen3-embedding-0.6b|1024",
		"http://127.0.0.1:8001/v1|text-embedding-all-minilm-l6-v2-embedding|384",
		"http://127.0.0.1:8001/v1|text-embedding-nomic-embed-text-v1.5|768",
		"http://127.0.0.1:8081/v1|BAAI/bge-small-en-v1.5|384",
	}
	if env := os.Getenv("MEMINI_SWEEP_EMBEDDERS"); env != "" {
		specs = strings.Split(env, ",")
	}
	var out []sweepEmbedder
	for _, spec := range specs {
		parts := strings.Split(strings.TrimSpace(spec), "|")
		if len(parts) != 3 {
			t.Fatalf("bad MEMINI_SWEEP_EMBEDDERS entry %q (want baseURL|model|dims)", spec)
		}
		dims, err := strconv.Atoi(parts[2])
		if err != nil || dims <= 0 {
			t.Fatalf("bad dims in MEMINI_SWEEP_EMBEDDERS entry %q", spec)
		}
		raw, err := embed.NewOpenAI(embed.OpenAIConfig{BaseURL: parts[0], Model: parts[1], Dims: dims})
		if err != nil {
			t.Logf("skip embedder %s: %v", parts[1], err)
			continue
		}
		// Batch like the shipped pipeline does, and stay under strict endpoint
		// caps (TEI rejects large single requests with 413).
		e := embed.NewBatched(raw, 16, 0, 0)
		probe, err := e.Embed(ctx, []string{"connectivity probe"})
		if err != nil {
			t.Logf("skip embedder %s at %s: unreachable: %v", parts[1], parts[0], err)
			continue
		}
		if len(probe) != 1 || len(probe[0]) != dims {
			t.Logf("skip embedder %s: returned %d dims, spec says %d", parts[1], len(probe[0]), dims)
			continue
		}
		name := parts[1]
		if i := strings.LastIndex(name, "/"); i >= 0 {
			name = name[i+1:]
		}
		out = append(out, sweepEmbedder{name: name, e: e, dims: dims})
	}
	names := make([]string, 0, len(out))
	for _, e := range out {
		names = append(names, e.name)
	}
	slices.Sort(names)
	t.Logf("sweep embedders: %s", strings.Join(names, ", "))
	return out
}
