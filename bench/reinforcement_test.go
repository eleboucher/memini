//go:build bench

package bench_test

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/eleboucher/memini/internal/embed"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/service"
	"github.com/eleboucher/memini/internal/store/sqlitevec"
)

// TestReinforcementStabilityDecay measures memory.StabilityK — reinforcement
// stretching a memory's effective half-life. The other benches pin
// AccessCount=0, where StabilityK is a no-op; here access counts and ages vary,
// the only regime where it can move ranking.
//
// It sweeps StabilityK over {0, 0.5, 1, 2} (0 is current production) on two
// episodic populations (durable tiers skip the decay term):
//   - upside: the answer is reinforced but aged, buried under fresh same-topic
//     chatter. Does raising StabilityK recover it?
//   - guardrail: the answer is fresh but the distractors are the reinforced,
//     aged ones, so raising StabilityK lifts stale noise. Does it bury the fresh
//     answer? This is the downside the poisoning-fix defaults guard against.
//
// Needs a live embedder; skips when unreachable. Point it with
// MEMINI_EMBED_BASE_URL / _MODEL / _DIMS (+ optional MEMINI_RERANK_URL).
func TestReinforcementStabilityDecay(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)

	baseURL := envOr("MEMINI_EMBED_BASE_URL", "http://127.0.0.1:8001/v1")
	model := envOr("MEMINI_EMBED_MODEL", "text-embedding-qwen3-embedding-0.6b")
	dims := envIntOr("MEMINI_EMBED_DIMS", 1024)

	e, err := embed.NewOpenAI(embed.OpenAIConfig{BaseURL: baseURL, Model: model, Dims: dims})
	if err != nil {
		t.Skipf("embedder config: %v", err)
	}
	probeEmbedder(ctx, t, baseURL, model)

	// Restore the global knob so a leaked value can't corrupt other bench tests
	// in this package.
	origK := memory.StabilityK
	defer func() { memory.StabilityK = origK }()

	corpus := buildReinforceCorpus()

	// Embed every distinct memory once; reuse the vectors across the fresh store
	// we rebuild per StabilityK value (embeddings are content-deterministic, and
	// a fresh store per sweep point keeps reinforcement writes from leaking
	// between measurements).
	texts, ids, tiers, access, ages := flattenReinforceCorpus(corpus)
	const embedWindow = 64
	vecs := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += embedWindow {
		end := min(start+embedWindow, len(texts))
		batch, err := e.Embed(ctx, texts[start:end])
		if err != nil {
			t.Fatalf("embed corpus: %v", err)
		}
		vecs = append(vecs, batch...)
	}

	clk := func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }
	now := clk()

	kSweep := []float64{0, 0.5, 1.0, 2.0}
	type row struct {
		kS                                       float64
		upR5, upR10, upRank, grR5, grR10, grRank float64
	}
	rows := make([]row, 0, len(kSweep))

	for _, kS := range kSweep {
		memory.StabilityK = kS

		st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), fmt.Sprintf("reinforce-%.1f.db", kS)), dims)
		if err != nil {
			t.Fatalf("open store: %v", err)
		}
		for i := range texts {
			ts := now.Add(-ages[i])
			if err := st.Upsert(ctx, &memory.Memory{
				ID: ids[i], Namespace: reserveNS, Tier: tiers[i], Content: texts[i],
				AccessCount: access[i], CreatedAt: ts, UpdatedAt: ts, LastAccessedAt: ts,
				Embedding: vecs[i],
			}); err != nil {
				t.Fatalf("upsert: %v", err)
			}
		}

		opts := append([]service.Option{
			service.WithClock(clk), service.WithSyncReinforce(), service.WithScoreFusion(0.5),
		}, maybeReranker(t)...)
		svc := service.New(st, e, opts...)

		up := goldStats(ctx, t, svc, corpus, true)
		gr := goldStats(ctx, t, svc, corpus, false)
		rows = append(rows, row{kS, up.r5, up.r10, up.meanRank, gr.r5, gr.r10, gr.meanRank})
		_ = st.Close()
	}

	t.Logf("reinforcement stability sweep — %d topics/population, gold access=%d aged %dd, embedder=%s",
		len(corpus.upside), reinforceAccess, int(agedDuration.Hours()/24), model)
	t.Logf("%-6s | %-26s | %-26s", "StabK", "upside aged-reinforced gold", "guardrail fresh gold")
	t.Logf("%-6s | %-26s | %-26s", "", "R@5 / R@10 / mean-rank", "R@5 / R@10 / mean-rank")
	t.Logf("%s", "-------+----------------------------+----------------------------")
	for _, r := range rows {
		t.Logf("%-6.1f | %5.1f%% / %5.1f%% / %5.1f      | %5.1f%% / %5.1f%% / %5.1f",
			r.kS, r.upR5, r.upR10, r.upRank, r.grR5, r.grR10, r.grRank)
	}

	// Guardrail: stability-modulation must not tank recall of fresh relevant
	// answers by lifting stale-reinforced noise. Tolerance is generous (embedder
	// noise), so this only fires on a real collapse — the signal that shipping a
	// nonzero StabilityK would trade fresh-context recall for stale resurfacing.
	base, top := rows[0], rows[len(rows)-1]
	if top.grR10 < base.grR10-15.0 {
		t.Errorf("guardrail regressed: fresh-gold R@10 %.1f%% (StabilityK=0) -> %.1f%% (StabilityK=%.1f); stale-reinforced noise is burying fresh answers",
			base.grR10, top.grR10, top.kS)
	}
}

const (
	reinforceAccess = 12 // simulated prior recalls of the reinforced memory
	agedDuration    = 30 * 24 * 3600 * time.Second
)

type reinforceGold struct {
	query  string
	goldID string
	texts  []string
	ids    []string
	access []int
	ages   []time.Duration
}

type reinforceCorpus struct {
	upside, guardrail []reinforceGold
}

// buildReinforceCorpus builds two tier-mixed populations that differ only in
// WHERE the reinforcement+age sits: on the answer (upside) or on the noise
// (guardrail). Both query a generic topical prompt that does not echo the
// answer's wording, so the gold must win on ranking, not a keyword gift.
func buildReinforceCorpus() reinforceCorpus {
	upSubjects := []string{
		"the auth scheme", "the database engine", "the cache TTL", "the retry policy",
		"the log format", "the deploy target", "the rate limiter", "the queue backend",
		"the session store", "the password hashing", "the file upload limit", "the background job runner",
		"the email provider", "the search index", "the config format", "the health check",
		"the API pagination", "the realtime transport", "the image pipeline", "the audit trail",
	}
	upDecisions := []string{
		"bearer tokens on every public route", "Postgres with pgvector", "a 10 minute TTL",
		"exponential backoff capped at 5 tries", "structured JSON with slog", "a Kubernetes Helm chart",
		"a token-bucket at 100 req/s", "NATS JetStream",
		"Redis with a 24 hour expiry", "argon2id with a 64MB cost", "a hard 25MB cap per request", "asynq on top of Redis",
		"Postmark for transactional mail", "an inverted index in Bleve", "TOML loaded once at startup", "a shallow livez plus a deep readyz",
		"cursor-based with an opaque token", "server-sent events over plain HTTP", "on-the-fly resizing behind a CDN", "an append-only Postgres table",
	}
	grSubjects := []string{
		"the metrics stack", "the feature flag store", "the migration tool", "the API versioning",
		"the secret manager", "the CI runner", "the object store", "the tracing backend",
		"the load balancer", "the DNS provider", "the container registry", "the TLS setup",
		"the backup strategy", "the alerting route", "the linting setup", "the dependency updates",
		"the staging environment", "the API gateway", "the log retention", "the on-call schedule",
	}
	grDecisions := []string{
		"Prometheus plus Grafana", "a Postgres-backed flag table", "golang-migrate with up/down files",
		"a /v1 prefix with no breaking v2 yet", "Vault with short-lived tokens", "self-hosted Forgejo runners",
		"S3-compatible Garage buckets", "OpenTelemetry to Tempo",
		"Envoy Gateway on the edge", "Cloudflare with proxied records", "a self-hosted Harbor instance", "cert-manager with Let's Encrypt",
		"nightly restic snapshots to S3", "PagerDuty for sev1 and Slack for the rest", "golangci-lint in a pre-commit hook", "Renovate with grouped PRs",
		"an ephemeral namespace per PR", "Kong with a rate-limit plugin", "30 days hot then cold storage", "a weekly rotation across four engineers",
	}

	corpus := reinforceCorpus{}
	// upside: gold is reinforced+aged; chatter is fresh.
	for i, subj := range upSubjects {
		corpus.upside = append(corpus.upside, buildGold(
			"up", i, subj, upDecisions[i],
			reinforceAccess, agedDuration, // gold: reinforced + aged
			0, 0)) //                          chatter: fresh
	}
	// guardrail: gold is fresh; chatter is the reinforced+aged noise.
	for i, subj := range grSubjects {
		corpus.guardrail = append(corpus.guardrail, buildGold(
			"gr", i, subj, grDecisions[i],
			0, 0, //                            gold: fresh
			reinforceAccess, agedDuration)) //  chatter: reinforced + aged
	}
	return corpus
}

// buildGold assembles one topic: a terse decision (the gold answer) plus
// chatterPerTopic verbose same-subject chatter lines. goldAccess/goldAge apply
// to the answer; chatAccess/chatAge apply to the noise.
func buildGold(pfx string, i int, subj, decision string, goldAccess int, goldAge time.Duration, chatAccess int, chatAge time.Duration) reinforceGold {
	goldID := fmt.Sprintf("%s-gold-%02d", pfx, i)
	g := reinforceGold{
		query:  fmt.Sprintf("Catch me up on %s — what's the current state of it?", subj),
		goldID: goldID,
	}
	g.texts = append(g.texts, fmt.Sprintf("Decision: for %s, the team standardized on %s.", subj, decision))
	g.ids = append(g.ids, goldID)
	g.access = append(g.access, goldAccess)
	g.ages = append(g.ages, goldAge)
	for j := range chatterPerTopic {
		g.texts = append(g.texts, fmt.Sprintf(
			"In standup #%d we kept going back and forth about %s. Several people had strong opinions on %s and nobody fully agreed on %s yet.",
			j, subj, subj, subj))
		g.ids = append(g.ids, fmt.Sprintf("%s-ep-%02d-%d", pfx, i, j))
		g.access = append(g.access, chatAccess)
		g.ages = append(g.ages, chatAge)
	}
	return g
}

func flattenReinforceCorpus(c reinforceCorpus) (texts, ids []string, tiers []memory.Tier, access []int, ages []time.Duration) {
	for _, pop := range [][]reinforceGold{c.upside, c.guardrail} {
		for _, g := range pop {
			for i := range g.texts {
				texts = append(texts, g.texts[i])
				ids = append(ids, g.ids[i])
				tiers = append(tiers, memory.TierEpisodic)
				access = append(access, g.access[i])
				ages = append(ages, g.ages[i])
			}
		}
	}
	return
}

type goldAgg struct{ r5, r10, meanRank float64 }

// goldStats queries each topic once (deep, k=50) and reports recall@5/@10 and
// the gold's mean rank. One query per topic means reinforcement (which runs
// after ranking) can't perturb the measurement.
func goldStats(ctx context.Context, t *testing.T, svc *service.Service, c reinforceCorpus, upside bool) goldAgg {
	pop := c.upside
	if !upside {
		pop = c.guardrail
	}
	hits5, hits10, rankSum := 0, 0, 0
	for _, g := range pop {
		ids := recallIDs(ctx, t, svc, g.query, 50, 0)
		idx := slices.Index(ids, g.goldID)
		if idx >= 0 && idx < 5 {
			hits5++
		}
		if idx >= 0 && idx < 10 {
			hits10++
		}
		if idx >= 0 {
			rankSum += idx + 1
		} else {
			rankSum += len(ids) + 1
		}
	}
	n := float64(len(pop))
	return goldAgg{pct(hits5, n), pct(hits10, n), float64(rankSum) / n}
}
