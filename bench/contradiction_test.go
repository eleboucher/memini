//go:build bench

package bench_test

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/eleboucher/memini/bench"
	"github.com/eleboucher/memini/internal/contradict"
	"github.com/eleboucher/memini/internal/embed"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/service"
	"github.com/eleboucher/memini/internal/store"
	"github.com/eleboucher/memini/internal/store/sqlitevec"
)

// contradictionQuad is one topic with the three probe shapes the detector must
// tell apart against the base fact: a restatement (must NOT read as an update
// — the costly error), a genuine update (changed value or flipped polarity —
// must fire), and a distinct same-topic fact (must not fire).
type contradictionQuad struct {
	base        string
	restatement string
	update      string
	distinct    string
	kind        string // expected update trigger: "value" | "polarity"
}

// contradictionQuads extends the dedup corpus's domains (infra, conventions,
// data, security, process) with an update leg. Restatements deliberately
// include the misfire traps: number-as-word, unit aliases, am/pm times,
// reordered lists, added detail, entity alias spellings. Distincts include
// numbers and negations on the same topic — the false-positive traps.
var contradictionQuads = []contradictionQuad{
	// --- value swaps ---
	{"Cache entries expire after a 10 minute TTL.",
		"Cached items live for ten minutes before expiring.",
		"Cache entries expire after a 30 minute TTL.",
		"The cache is sharded across four nodes by key hash.", "value"},
	{"The rate limiter is a token bucket at 100 requests per second.",
		"We rate-limit with a token bucket of 100 req/s.",
		"The rate limiter is a token bucket at 250 requests per second.",
		"The rate limiter keys buckets by API token, not by IP.", "value"},
	{"The reranker is Qwen3-Reranker-0.6B served on port 8002.",
		"We run Qwen3-Reranker-0.6B as the cross-encoder on :8002.",
		"The reranker is Qwen3-Reranker-0.6B served on port 9002.",
		"The reranker is only applied to the top 10 fused candidates.", "value"},
	{"Image uploads are limited to 10 megabytes.",
		"We cap image uploads at 10 MB.",
		"Image uploads are limited to 25 megabytes.",
		"Uploaded images are converted to WebP on ingest.", "value"},
	{"The search index is rebuilt nightly at 02:00 UTC.",
		"We rebuild the search index every night at 2am UTC.",
		"The search index is rebuilt nightly at 04:00 UTC.",
		"The search index is stored separately from the primary database.", "value"},
	{"Database backups are kept for seven days.",
		"We retain database backups for a week — seven days.",
		"Database backups are kept for thirty days.",
		"Database backups are encrypted at rest with a KMS key.", "value"},
	{"The queue worker pool size is eight.",
		"We run eight queue workers.",
		"The queue worker pool size is sixteen.",
		"The queue worker uses at-least-once delivery semantics.", "value"},
	{"Tests must keep coverage above eighty percent to merge.",
		"Merging requires test coverage above 80 percent.",
		"Tests must keep coverage above ninety percent to merge.",
		"Tests run on every pull request via GitHub Actions.", "value"},
	{"Trace sampling is set to ten percent in production.",
		"We sample 10 percent of traces in production.",
		"Trace sampling is set to twenty five percent in production.",
		"The service emits OpenTelemetry traces to a collector.", "value"},
	{"Metrics use a 15 second scrape interval.",
		"The scrape interval for metrics is fifteen seconds.",
		"Metrics use a 60 second scrape interval.",
		"Metrics are scraped by Prometheus and shown in Grafana.", "value"},
	{"Pagination defaults to a page size of fifty items.",
		"The default page size for pagination is 50 items.",
		"Pagination defaults to a page size of twenty five items.",
		"Pagination uses opaque cursors, not page numbers.", "value"},
	{"User sessions expire after two weeks of inactivity.",
		"Sessions time out after 14 days — two weeks — of inactivity.",
		"User sessions expire after six weeks of inactivity.",
		"User passwords are hashed with Argon2id.", "value"},
	{"Email is sent through Postmark.",
		"We send transactional email via Postmark.",
		"Email is sent through SES.",
		"Email templates are stored as MJML and compiled at build time.", "value"},
	{"The CDN in front of the API is Cloudflare.",
		"We front the API with Cloudflare as the CDN.",
		"The CDN in front of the API is Fastly.",
		"The CDN caches static assets for one year with immutable headers.", "value"},
	{"The frontend is built with React and Vite.",
		"We use React with Vite on the frontend.",
		"The frontend is built with Svelte and Vite.",
		"The frontend talks to the backend over a typed REST client.", "value"},
	{"Background jobs run on NATS JetStream.",
		"We use NATS JetStream for the background job queue.",
		"Background jobs run on Kafka.",
		"Background jobs are retried at most three times before a dead-letter.", "value"},
	{"User passwords are hashed with Argon2id.",
		"We hash passwords using Argon2id.",
		"User passwords are hashed with PBKDF2.",
		"User sessions expire after two weeks of inactivity.", "value"},
	{"The service deploys to Kubernetes via a Helm chart.",
		"Deployment is done through a Helm chart on Kubernetes.",
		"The service deploys to Nomad via a job spec.",
		"The service exposes a readiness probe on /healthz.", "value"},
	{"The mobile app caches data offline with SQLite.",
		"Offline data on mobile is cached in SQLite.",
		"The mobile app caches data offline with Realm.",
		"The mobile app syncs in the background every fifteen minutes.", "value"},
	{"Dependency updates are automated with Renovate.",
		"We automate dependency bumps using Renovate.",
		"Dependency updates are automated with Dependabot.",
		"Dependencies are pinned to exact versions in the lockfile.", "value"},
	{"Services build with Go 1.22.",
		"We compile all services on Go 1.22.",
		"Services build with Go 1.23.",
		"Services build inside a Docker multi-stage image.", "value"},
	{"Long-running requests have a hard 30 second timeout.",
		"Slow requests are cut off after thirty seconds.",
		"Long-running requests have a hard 120 second timeout.",
		"Long-running requests stream results over server-sent events.", "value"},

	// --- polarity flips / retro-cue retirements ---
	{"The API uses bearer-token authentication on all public routes.",
		"Every public API endpoint is protected by a bearer token.",
		"The API no longer uses bearer-token authentication on public routes.",
		"The API is versioned under a /v1 prefix with no breaking v2 yet.", "polarity"},
	{"The project stores vectors in Postgres with the pgvector extension.",
		"We keep embeddings in Postgres using pgvector.",
		"The project switched from Postgres to Qdrant for vector storage.",
		"The project runs database migrations with golang-migrate.", "polarity"},
	{"Background jobs run on NATS JetStream.",
		"The background job queue runs on NATS JetStream.",
		"We no longer run background jobs on NATS JetStream.",
		"Background jobs are retried at most three times before a dead-letter.", "polarity"},
	{"The team does code review on every change before merge.",
		"Every change is peer-reviewed before it is merged.",
		"The team stopped doing code review on docs-only changes.",
		"The team squashes commits when merging a pull request.", "polarity"},
	{"The admin UI is restricted to the internal network only.",
		"Admin UI access is limited to the internal LAN.",
		"The admin UI is no longer restricted to the internal network.",
		"The admin UI shows an audit log of every privileged action.", "polarity"},
	{"gRPC services authenticate with mutual TLS.",
		"Our gRPC endpoints use mTLS for authentication.",
		"gRPC services no longer require mutual TLS.",
		"gRPC requests carry a deadline propagated through the context.", "polarity"},
	{"Logs are emitted as structured JSON via slog.",
		"We log in structured JSON using slog.",
		"We stopped using slog for structured JSON logs.",
		"Logs are shipped to Loki and retained for thirty days.", "polarity"},
	{"The webhook signature is verified with an HMAC-SHA256 header.",
		"We verify webhooks via an HMAC-SHA256 signature header.",
		"Webhook signatures are not verified with an HMAC-SHA256 header anymore.",
		"Webhook deliveries are retried for up to 24 hours.", "polarity"},
	{"Secrets are injected as environment variables at deploy time.",
		"We pass secrets in via env vars when deploying.",
		"Secrets are no longer injected as environment variables at deploy time.",
		"Secrets are rotated every ninety days.", "polarity"},
	{"Feature flags live in a Postgres-backed flag table.",
		"Feature flags are stored in a Postgres table.",
		"Feature flags moved off the Postgres-backed flag table.",
		"Feature flags are evaluated per-request, not cached.", "polarity"},
	{"Long-running requests stream results over server-sent events.",
		"We stream slow responses using SSE — server-sent events.",
		"Long-running requests no longer stream results over server-sent events.",
		"Long-running requests have a hard 30 second timeout.", "polarity"},
	{"Dependency updates are automated with Renovate.",
		"Renovate automates our dependency updates.",
		"We stopped using Renovate for dependency updates.",
		"Dependencies are pinned to exact versions in the lockfile.", "polarity"},
	{"Email is sent through Postmark.",
		"Transactional email goes out through Postmark.",
		"Postmark is deprecated for outbound email.",
		"Email templates are stored as MJML and compiled at build time.", "polarity"},
	{"The mobile app caches data offline with SQLite.",
		"The mobile app keeps an offline SQLite cache.",
		"The mobile app stopped caching data offline.",
		"The mobile app syncs in the background every fifteen minutes.", "polarity"},
}

// quadOutcome tallies detector verdicts for one probe class.
type quadOutcome struct{ restatement, update, distinct int }

func (o quadOutcome) add(c contradict.Class) quadOutcome {
	switch c {
	case contradict.Restatement:
		o.restatement++
	case contradict.Update:
		o.update++
	default:
		o.distinct++
	}
	return o
}

// classifyQuads runs the pure detector over the authored corpus and returns
// the per-class verdict tallies plus update recall split by expected trigger.
func classifyQuads(cfg contradict.Config) (rest, upd, dist quadOutcome, valueHit, polarityHit, valueN, polarityN int) {
	for _, q := range contradictionQuads {
		rest = rest.add(contradict.Classify(q.restatement, q.base, cfg).Class)
		dist = dist.add(contradict.Classify(q.distinct, q.base, cfg).Class)
		u := contradict.Classify(q.update, q.base, cfg)
		upd = upd.add(u.Class)
		if q.kind == "value" {
			valueN++
			if u.Class == contradict.Update {
				valueHit++
			}
		} else {
			polarityN++
			if u.Class == contradict.Update {
				polarityHit++
			}
		}
	}
	return rest, upd, dist, valueHit, polarityHit, valueN, polarityN
}

// TestContradictionDetectorPricing prices contradict.Classify on the authored
// quad corpus: the confusion matrix at the shipped Default config, plus
// one-dimension-at-a-time threshold sweeps. Pure text — no embedder needed.
//
// GO/NO-GO gates (defined before the numbers were seen):
//   - restatement→Update ≤ 1/len(quads): the costly error (downranks a live
//     fact AND loses its corroboration);
//   - distinct→Update ≤ 5%;
//   - update recall ≥ 60%.
func TestContradictionDetectorPricing(t *testing.T) {
	n := len(contradictionQuads)
	row := func(label string, cfg contradict.Config) (restMis, distMis int, recall float64) {
		rest, upd, dist, vHit, pHit, vN, pN := classifyQuads(cfg)
		recall = float64(upd.update) / float64(n)
		t.Logf("%-34s | rest→U %2d  rest→D %2d | upd→U %2d (val %d/%d, pol %d/%d) | dist→U %2d",
			label, rest.update, rest.distinct, upd.update, vHit, vN, pHit, pN, dist.update)
		return rest.update, dist.update, recall
	}

	t.Logf("detector pricing — %d quads (pure text, no embedder)", n)
	t.Logf("%-34s | restatement       | update recall            | distinct", "config")
	restMis, distMis, recall := row("Default", contradict.Default)

	for _, v := range []float64{0.4, 0.5, 0.6} {
		cfg := contradict.Default
		cfg.OverlapFloor = v
		row(fmt.Sprintf("OverlapFloor=%.1f", v), cfg)
	}
	for _, v := range []int{1, 2, 3} {
		cfg := contradict.Default
		cfg.ResidueMax = v
		row(fmt.Sprintf("ResidueMax=%d", v), cfg)
	}
	for _, v := range []float64{0.5, 0.6, 0.75} {
		cfg := contradict.Default
		cfg.NegOverlapFloor = v
		row(fmt.Sprintf("NegOverlapFloor=%.2f", v), cfg)
	}
	for _, v := range []int{3, 4, 5} {
		cfg := contradict.Default
		cfg.AliasPrefixMin = v
		row(fmt.Sprintf("AliasPrefixMin=%d", v), cfg)
	}

	if restMis > 1 {
		t.Errorf("GATE: %d restatements misread as updates (max 1)", restMis)
	}
	if float64(distMis) > 0.05*float64(n) {
		t.Errorf("GATE: %d distinct facts misread as updates (max 5%%)", distMis)
	}
	if recall < 0.6 {
		t.Errorf("GATE: update recall %.0f%% below 60%%", recall*100)
	}
}

// TestContradictionSimilarityGate measures, per embedder, whether the vector
// entry gate the write path already applies (dedupCheck's top-1 same-tier
// score) routes each probe class to the detector: genuine updates must clear
// the floor against their own base, restatements clearing it must still not
// read as updates, and distincts that sneak over must not fire. The detector
// runs on the RETRIEVED neighbor, not the authored pair, so a wrong top-1 is
// counted as the miss it would be in production.
func TestContradictionSimilarityGate(t *testing.T) {
	ctx := context.Background()
	embedders := sweepEmbedders(t, ctx)
	if len(embedders) == 0 {
		t.Skip("no sweep embedder reachable")
	}
	now := time.Unix(1_700_000_000, 0).UTC()

	for _, emb := range embedders {
		t.Run(emb.name, func(t *testing.T) {
			texts := make([]string, 0, len(contradictionQuads)*4)
			for _, q := range contradictionQuads {
				texts = append(texts, q.base, q.restatement, q.update, q.distinct)
			}
			vecs := make([][]float32, 0, len(texts))
			for start := 0; start < len(texts); start += 32 {
				end := min(start+32, len(texts))
				bv, err := emb.e.Embed(ctx, texts[start:end])
				if err != nil {
					t.Fatalf("embed batch: %v", err)
				}
				vecs = append(vecs, bv...)
			}

			st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "cq.db"), emb.dims)
			if err != nil {
				t.Fatalf("open store: %v", err)
			}
			t.Cleanup(func() { _ = st.Close() })
			const ns = "contradict-eval"
			for i, q := range contradictionQuads {
				if err := st.Upsert(ctx, &memory.Memory{
					ID: "cq-" + strconv.Itoa(i), Namespace: ns, Tier: memory.TierSemantic, Content: q.base,
					CreatedAt: now, UpdatedAt: now, LastAccessedAt: now, Embedding: vecs[i*4],
				}); err != nil {
					t.Fatalf("upsert base %d: %v", i, err)
				}
			}

			// probe returns the top-1 base score and content — what dedupCheck
			// would hand the detector.
			probe := func(vec []float32) (float64, string) {
				res, serr := st.VectorSearch(ctx, ns, vec,
					store.Filter{Tiers: []memory.Tier{memory.TierSemantic}, Now: now}, 1)
				if serr != nil {
					t.Fatalf("vector search: %v", serr)
				}
				if len(res) == 0 {
					return 0, ""
				}
				return res[0].Score, res[0].Memory.Content
			}

			type probed struct {
				score    float64
				neighbor string
				ownBase  bool
			}
			classes := map[string][]probed{}
			for i, q := range contradictionQuads {
				for off, text := range map[int]string{1: "restatement", 2: "update", 3: "distinct"} {
					score, neighbor := probe(vecs[i*4+off])
					classes[text] = append(classes[text], probed{score, neighbor, neighbor == q.base})
				}
			}

			for _, cls := range []string{"restatement", "update", "distinct"} {
				scores := make([]float64, 0, len(classes[cls]))
				own := 0
				for _, p := range classes[cls] {
					scores = append(scores, p.score)
					if p.ownBase {
						own++
					}
				}
				sort.Float64s(scores)
				t.Logf("%-11s top-1 score: min=%.3f median=%.3f p90=%.3f max=%.3f (own base %d/%d)",
					cls, scores[0], median(scores), pctile(scores, 0.90), scores[len(scores)-1], own, len(scores))
			}

			t.Logf("floor | upd routed | upd fired (recall) | rest fired | dist fired")
			t.Logf("------+------------+--------------------+------------+-----------")
			for floor := 0.50; floor <= 0.701; floor += 0.025 {
				fired := func(cls string, probeTexts func(q contradictionQuad) string) (routed, fire int) {
					for i, p := range classes[cls] {
						if p.score < floor {
							continue
						}
						routed++
						if contradict.Classify(probeTexts(contradictionQuads[i]), p.neighbor, contradict.Default).Class == contradict.Update {
							fire++
						}
					}
					return routed, fire
				}
				updRouted, updFire := fired("update", func(q contradictionQuad) string { return q.update })
				_, restFire := fired("restatement", func(q contradictionQuad) string { return q.restatement })
				_, distFire := fired("distinct", func(q contradictionQuad) string { return q.distinct })
				t.Logf("%.3f |   %2d/%-2d    |   %2d/%-2d (%4.0f%%)     |     %2d     |    %2d",
					floor, updRouted, len(classes["update"]), updFire, len(contradictionQuads),
					100*float64(updFire)/float64(len(contradictionQuads)), restFire, distFire)
			}
		})
	}
}

// TestContradictionWildFalsePositives runs the detector over nearest-neighbor
// pairs of REAL LongMemEval session docs — all distinct by construction, and
// exactly the neighbor population the write-path hook would see — and reports
// the Update flag rate. This is the wild false-positive ceiling.
//
// Skips without the data file (MEMINI_LME_DATA) or a live embedder.
func TestContradictionWildFalsePositives(t *testing.T) {
	ctx := context.Background()

	dataPath := envOr("MEMINI_LME_DATA", "data/longmemeval_s_cleaned.json")
	if _, err := os.Stat(dataPath); err != nil {
		t.Skipf("LongMemEval data not found at %s (set MEMINI_LME_DATA): %v", dataPath, err)
	}
	baseURL := envOr("MEMINI_EMBED_BASE_URL", "http://127.0.0.1:8001/v1")
	model := envOr("MEMINI_EMBED_MODEL", "text-embedding-qwen3-embedding-0.6b")
	dims := envIntOr("MEMINI_EMBED_DIMS", 1024)
	sampleN := envIntOr("MEMINI_DEDUP_SAMPLE", 300)

	e, err := embed.NewOpenAI(embed.OpenAIConfig{BaseURL: baseURL, Model: model, Dims: dims})
	if err != nil {
		t.Skipf("embedder config: %v", err)
	}
	probe, err := e.Embed(ctx, []string{"connectivity probe"})
	if err != nil {
		t.Skipf("live embedder unreachable at %s (%s): %v", baseURL, model, err)
	}
	if len(probe) != 1 || len(probe[0]) != dims {
		t.Skipf("embedder returned %d-dim vectors, configured for %d", len(probe[0]), dims)
	}

	ds, err := bench.LoadLongMemEval(dataPath, bench.DocFull)
	if err != nil {
		t.Fatalf("load LongMemEval: %v", err)
	}
	texts := make([]string, 0, sampleN)
	seen := make(map[string]struct{})
	for _, it := range ds.Items {
		if len(it.Content) < 40 {
			continue
		}
		if _, dup := seen[it.Content]; dup {
			continue
		}
		seen[it.Content] = struct{}{}
		texts = append(texts, it.Content)
		if len(texts) >= sampleN {
			break
		}
	}
	if len(texts) < 20 {
		t.Skipf("only %d usable docs in %s — too few", len(texts), dataPath)
	}

	st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "cq_real.db"), dims)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	const ns = "contradict-real"
	now := time.Unix(1_700_000_000, 0).UTC()
	vecs := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += 32 {
		end := min(start+32, len(texts))
		bv, berr := e.Embed(ctx, texts[start:end])
		if berr != nil {
			t.Fatalf("embed batch [%d:%d]: %v", start, end, berr)
		}
		vecs = append(vecs, bv...)
	}
	for i := range texts {
		if err := st.Upsert(ctx, &memory.Memory{
			ID: "doc-" + strconv.Itoa(i), Namespace: ns, Tier: memory.TierSemantic, Content: texts[i],
			CreatedAt: now, UpdatedAt: now, LastAccessedAt: now, Embedding: vecs[i],
		}); err != nil {
			t.Fatalf("upsert doc %d: %v", i, err)
		}
	}

	type nnPair struct {
		score float64
		flag  bool
	}
	pairs := make([]nnPair, 0, len(texts))
	for i := range texts {
		res, serr := st.VectorSearch(ctx, ns, vecs[i],
			store.Filter{Tiers: []memory.Tier{memory.TierSemantic}, Now: now}, 2)
		if serr != nil {
			t.Fatalf("vector search: %v", serr)
		}
		if len(res) < 2 {
			continue
		}
		verdict := contradict.Classify(texts[i], res[1].Memory.Content, contradict.Default)
		pairs = append(pairs, nnPair{res[1].Score, verdict.Class == contradict.Update})
	}

	t.Logf("wild false-positive eval — %d real LongMemEval NN pairs (all distinct), embedder=%s", len(pairs), model)
	for _, floor := range []float64{0.575, 0.60, 0.625, 0.65} {
		routed, flagged := 0, 0
		for _, p := range pairs {
			if p.score < floor {
				continue
			}
			routed++
			if p.flag {
				flagged++
			}
		}
		rate := 0.0
		if routed > 0 {
			rate = float64(flagged) / float64(routed)
		}
		t.Logf("  floor %.3f: %3d routed, %2d flagged as update (%.2f%%)", floor, routed, flagged, rate*100)
		if floor == 0.625 && float64(flagged) > 0.01*float64(max(routed, 1)) && flagged > 1 {
			t.Errorf("GATE: wild FP rate %.2f%% at floor 0.625 exceeds 1%%", rate*100)
		}
	}
}

// staleTopic is one entrenched-old-fact vs fresh-update scenario.
type staleTopic struct {
	key, subject string
	oldFact      string
	update       string
}

var staleTopics = []staleTopic{
	{"sched", "the report scheduler", "Decision: for the report scheduler, the team standardized on Temporal.", "Decision: for the report scheduler, the team standardized on Airflow."},
	{"gateway", "the API gateway", "Decision: for the API gateway, the team standardized on Kong.", "Decision: for the API gateway, the team standardized on Envoy."},
	{"ttlpolicy", "the session TTL policy", "Decision: sessions expire after a 30 minute TTL.", "Decision: sessions expire after a 90 minute TTL."},
	{"artifacts", "the artifact registry", "Decision: for the artifact registry, the team standardized on Harbor.", "Decision: for the artifact registry, the team standardized on Artifactory."},
	{"payments", "the payments provider", "Decision: for the payments provider, the team standardized on Stripe.", "Decision: for the payments provider, the team standardized on Adyen."},
	{"search", "the search backend", "Decision: for the search backend, the team standardized on Elasticsearch.", "Decision: for the search backend, the team standardized on Typesense."},
	{"alerting", "the alerting channel", "Decision: alerts page the on-call through PagerDuty.", "Decision: alerts switched from PagerDuty to Opsgenie for paging the on-call."},
	{"docs", "the docs site generator", "Decision: the docs site is built with Docusaurus.", "Decision: the docs site is no longer built with Docusaurus."},
	{"linter", "the lint toolchain", "Decision: the lint toolchain runs golangci-lint on every commit.", "Decision: the lint toolchain no longer runs golangci-lint on every commit."},
	{"regions", "the deployment regions", "Decision: the service deploys to three regions.", "Decision: the service deploys to five regions."},
	{"sso", "the SSO provider", "Decision: for the SSO provider, the team standardized on Okta.", "Decision: the team switched from Okta to Keycloak for the SSO provider."},
	{"billing", "the billing cycle job", "Decision: the billing cycle job runs at 03:00 UTC.", "Decision: the billing cycle job runs at 06:00 UTC."},
}

// staleRestatements are the short-term re-observations that entrench the old
// fact through the REAL corroboration path. Distinct phrasings per round so
// the fingerprint dedup doesn't swallow rounds 2 and 3.
func staleRestatements(tp staleTopic) [3]string {
	claim := tp.oldFact
	return [3]string{
		"Reminder from today's sync: " + claim,
		"Came up again in planning — still true: " + claim,
		"Confirmed once more while onboarding a teammate: " + claim,
	}
}

func staleQuery(subject string) string {
	return fmt.Sprintf("Catch me up on %s — what's the current state of it?", subject)
}

// staleMetrics is one measurement pass over all topics at one window size.
type staleMetrics struct {
	freshInWin, staleAbove, bothInWin int
	freshRankSum                      int
	n                                 int
}

// TestContradictionStaleVsFresh measures the harm the missing contradiction
// mirror causes on the no-LLM target, then prices the candidate downrank
// actions — all through store methods, no service changes.
//
// Per topic: the old durable fact is written 30 days back and entrenched via
// the REAL flows (three short-term restatements that fire
// corroborateNearestAsync, plus reinforcing recalls), the contradicting update
// is written fresh, chatter surrounds both. Baseline: where does the fresh
// update rank vs the entrenched stale fact at the production window? Then each
// candidate action is applied to the stale fact and the window re-measured.
//
// Measurement recalls reinforce delivered results (production behavior), so
// later rows face slightly MORE entrenched stale facts — the drift is
// conservative and the per-row confidence/access numbers are printed.
func TestContradictionStaleVsFresh(t *testing.T) {
	ctx := context.Background()
	embedders := sweepEmbedders(t, ctx)
	if len(embedders) == 0 {
		t.Skip("no sweep embedder reachable")
	}

	t0 := time.Unix(1_700_000_000, 0).UTC()
	day := 24 * time.Hour

	for _, emb := range embedders {
		t.Run(emb.name, func(t *testing.T) {
			st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "stale.db"), emb.dims)
			if err != nil {
				t.Fatalf("open store: %v", err)
			}
			t.Cleanup(func() { _ = st.Close() })

			cur := t0.Add(-30 * day)
			clk := func() time.Time { return cur }
			const ns = "stale-eval"
			svc := service.New(st, emb.e,
				service.WithClock(clk), service.WithSyncReinforce(),
				service.WithScoreFusion(0.5), service.WithCorroboration(0.70))

			// t0−30d: the old durable facts.
			oldID := map[string]string{}
			for _, tp := range staleTopics {
				m, rerr := svc.Remember(ctx, service.RememberInput{Namespace: ns, Content: tp.oldFact, Tier: memory.TierSemantic})
				if rerr != nil {
					t.Fatalf("remember old fact %s: %v", tp.key, rerr)
				}
				oldID[tp.key] = m.ID
			}

			// Three corroboration rounds through the real short-term write path.
			for round := range 3 {
				cur = t0.Add(time.Duration(-28+2*round) * day)
				for _, tp := range staleTopics {
					if _, rerr := svc.Remember(ctx, service.RememberInput{
						Namespace: ns, Content: staleRestatements(tp)[round], Tier: memory.TierEpisodic,
					}); rerr != nil {
						t.Fatalf("remember restatement %s/%d: %v", tp.key, round, rerr)
					}
				}
				svc.WaitBackground()
			}

			// Reinforcing recalls (usage growth on whatever ranks).
			for _, when := range []time.Duration{-20 * day, -15 * day} {
				cur = t0.Add(when)
				for _, tp := range staleTopics {
					if _, rerr := svc.Recall(ctx, service.RecallInput{
						Namespace: ns, Query: staleQuery(tp.subject), Limit: 5, SemanticReserve: 2,
					}); rerr != nil {
						t.Fatalf("entrench recall %s: %v", tp.key, rerr)
					}
				}
			}

			// t0: the contradicting updates, written fresh.
			cur = t0
			updateID := map[string]string{}
			for _, tp := range staleTopics {
				m, rerr := svc.Remember(ctx, service.RememberInput{Namespace: ns, Content: tp.update, Tier: memory.TierSemantic})
				if rerr != nil {
					t.Fatalf("remember update %s: %v", tp.key, rerr)
				}
				updateID[tp.key] = m.ID
			}
			svc.WaitBackground()

			// Chatter surrounds both (direct upsert: background filler, not a flow
			// under test — and Remember-side extraction must not spawn extras).
			chatterTexts := make([]string, 0, 10*len(staleTopics))
			chatterIDs := make([]string, 0, 10*len(staleTopics))
			for _, tp := range staleTopics {
				for j := range 10 {
					chatterTexts = append(chatterTexts, fmt.Sprintf(
						"In standup #%d we kept going back and forth about %s. Several people had strong opinions on %s and nobody fully agreed yet.",
						j, tp.subject, tp.subject))
					chatterIDs = append(chatterIDs, fmt.Sprintf("%s-ep-%d", tp.key, j))
				}
			}
			cvecs := make([][]float32, 0, len(chatterTexts))
			for start := 0; start < len(chatterTexts); start += 32 {
				end := min(start+32, len(chatterTexts))
				bv, berr := emb.e.Embed(ctx, chatterTexts[start:end])
				if berr != nil {
					t.Fatalf("embed chatter: %v", berr)
				}
				cvecs = append(cvecs, bv...)
			}
			for i := range chatterTexts {
				if err := st.Upsert(ctx, &memory.Memory{
					ID: chatterIDs[i], Namespace: ns, Tier: memory.TierEpisodic, Content: chatterTexts[i],
					CreatedAt: t0, UpdatedAt: t0, LastAccessedAt: t0, Embedding: cvecs[i],
				}); err != nil {
					t.Fatalf("upsert chatter: %v", err)
				}
			}

			// Print the achieved entrenchment so it is measured, not assumed.
			var confSum float64
			var accessSum int
			detected := 0
			for _, tp := range staleTopics {
				m, gerr := st.Get(ctx, ns, oldID[tp.key])
				if gerr != nil {
					t.Fatalf("get old fact %s: %v", tp.key, gerr)
				}
				confSum += m.EffectiveConfidence(t0)
				accessSum += m.AccessCount
				if contradict.Classify(tp.update, tp.oldFact, contradict.Default).Class == contradict.Update {
					detected++
				}
			}
			nTopics := len(staleTopics)
			t.Logf("entrenchment: mean effective confidence %.3f (seed %.2f), mean AccessCount %.1f; detector fires on %d/%d update pairs",
				confSum/float64(nTopics), memory.ConfidenceSeedFresh, float64(accessSum)/float64(nTopics), detected, nTopics)

			measure := func(sv *service.Service, k int) staleMetrics {
				var m staleMetrics
				m.n = nTopics
				for _, tp := range staleTopics {
					res, rerr := sv.Recall(ctx, service.RecallInput{
						Namespace: ns, Query: staleQuery(tp.subject), Limit: k, SemanticReserve: 2,
					})
					if rerr != nil {
						t.Fatalf("recall %s: %v", tp.key, rerr)
					}
					oldRank, freshRank := 0, 0
					for i, r := range res {
						switch r.Memory.ID {
						case oldID[tp.key]:
							oldRank = i + 1
						case updateID[tp.key]:
							freshRank = i + 1
						}
					}
					if freshRank > 0 {
						m.freshInWin++
						m.freshRankSum += freshRank
					}
					if oldRank > 0 && (freshRank == 0 || oldRank < freshRank) {
						m.staleAbove++
					}
					if oldRank > 0 && freshRank > 0 {
						m.bothInWin++
					}
				}
				return m
			}
			logRow := func(label string, k int, m staleMetrics) {
				meanRank := 0.0
				if m.freshInWin > 0 {
					meanRank = float64(m.freshRankSum) / float64(m.freshInWin)
				}
				t.Logf("%-22s k=%d | fresh in window %2d/%d (mean rank %.1f) | stale above fresh %2d/%d | both in window %2d/%d",
					label, k, m.freshInWin, m.n, meanRank, m.staleAbove, m.n, m.bothInWin, m.n)
			}

			for _, k := range []int{3, 5} {
				logRow("baseline (no action)", k, measure(svc, k))
			}

			// Candidate actions, applied via existing store methods and restored
			// after each measurement. The usage-aware set is the only candidate
			// whose stale-below-fresh invariant holds for any AccessCount.
			actions := []struct {
				name  string
				value func(eff float64, access int) float64
			}{
				{"shrink c*0.5", func(eff float64, _ int) float64 { return eff * 0.5 }},
				{"shrink c*0.25", func(eff float64, _ int) float64 { return eff * 0.25 }},
				{"cap at 0.2", func(eff float64, _ int) float64 { return math.Min(eff, 0.2) }},
				{"usage-aware set", func(eff float64, access int) float64 {
					return math.Min(eff, 0.9*memory.ConfidenceSeedFresh/(1+math.Log1p(float64(access))))
				}},
			}
			for _, act := range actions {
				restore := map[string]float64{}
				for _, tp := range staleTopics {
					m, gerr := st.Get(ctx, ns, oldID[tp.key])
					if gerr != nil {
						t.Fatalf("get: %v", gerr)
					}
					eff := m.EffectiveConfidence(t0)
					restore[tp.key] = eff
					if serr := st.SetConfidence(ctx, ns, oldID[tp.key], act.value(eff, m.AccessCount), t0); serr != nil {
						t.Fatalf("set confidence: %v", serr)
					}
				}
				for _, k := range []int{3, 5} {
					logRow(act.name, k, measure(svc, k))
				}
				for _, tp := range staleTopics {
					if serr := st.SetConfidence(ctx, ns, oldID[tp.key], restore[tp.key], t0); serr != nil {
						t.Fatalf("restore confidence: %v", serr)
					}
				}
			}

			// Destructive ceiling (NOT a candidate default): tombstone the stale
			// fact. Runs last — nothing to restore after it.
			for _, tp := range staleTopics {
				if serr := st.SetSuperseded(ctx, ns, oldID[tp.key], updateID[tp.key]); serr != nil {
					t.Fatalf("supersede: %v", serr)
				}
			}
			for _, k := range []int{3, 5} {
				logRow("supersede (ceiling)", k, measure(svc, k))
			}
		})
	}
}
