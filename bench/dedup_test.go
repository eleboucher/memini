package bench_test

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/eleboucher/memini/bench"
	"github.com/eleboucher/memini/internal/embed"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/store"
	"github.com/eleboucher/memini/internal/store/sqlitevec"
)

// dedupTriple is one base fact plus a reworded restatement (near-duplicate,
// SHOULD be hinted/superseded) and a distinct fact on the same topic (must NOT
// be treated as a duplicate).
type dedupTriple struct{ base, paraphrase, distinct string }

// dedupTriples is a broad, diverse corpus so the score separation between
// near-duplicates and distinct-same-topic facts is measured on a large enough
// sample to be more than directional. Spans infra, code conventions, data,
// security, process, and preferences.
var dedupTriples = []dedupTriple{
	{"The API uses bearer-token authentication on all public routes.",
		"Every public API endpoint is protected by a bearer token.",
		"The API is versioned under a /v1 prefix with no breaking v2 yet."},
	{"The project stores vectors in Postgres with the pgvector extension.",
		"We keep embeddings in Postgres using pgvector.",
		"The project runs database migrations with golang-migrate."},
	{"Cache entries expire after a 10 minute TTL.",
		"Cached items live for ten minutes before expiring.",
		"The cache is sharded across four nodes by key hash."},
	{"Retries use exponential backoff capped at five attempts.",
		"We retry with exponential backoff, up to five tries.",
		"Retries are only enabled for idempotent GET requests."},
	{"Logs are emitted as structured JSON via slog.",
		"We log in structured JSON using slog.",
		"Logs are shipped to Loki and retained for thirty days."},
	{"The service deploys to Kubernetes via a Helm chart.",
		"Deployment is done through a Helm chart on Kubernetes.",
		"The service exposes a readiness probe on /healthz."},
	{"The rate limiter is a token bucket at 100 requests per second.",
		"We rate-limit with a token bucket of 100 req/s.",
		"The rate limiter keys buckets by API token, not by IP."},
	{"Background jobs run on NATS JetStream.",
		"We use NATS JetStream for the background job queue.",
		"Background jobs are retried at most three times before a dead-letter."},
	{"Metrics are scraped by Prometheus and shown in Grafana.",
		"We expose Prometheus metrics and dashboard them in Grafana.",
		"Metrics use a 15 second scrape interval."},
	{"Feature flags live in a Postgres-backed flag table.",
		"Feature flags are stored in a Postgres table.",
		"Feature flags are evaluated per-request, not cached."},
	{"The reranker is Qwen3-Reranker-0.6B served on port 8002.",
		"We run Qwen3-Reranker-0.6B as the cross-encoder on :8002.",
		"The reranker is only applied to the top 10 fused candidates."},
	{"Secrets are injected as environment variables at deploy time.",
		"We pass secrets in via env vars when deploying.",
		"Secrets are rotated every ninety days."},
	{"The frontend is built with React and Vite.",
		"We use React with Vite on the frontend.",
		"The frontend talks to the backend over a typed REST client."},
	{"Tests run on every pull request via GitHub Actions.",
		"CI runs the test suite on each PR through GitHub Actions.",
		"Tests must keep coverage above eighty percent to merge."},
	{"User passwords are hashed with Argon2id.",
		"We hash passwords using Argon2id.",
		"User sessions expire after two weeks of inactivity."},
	{"The default database isolation level is read committed.",
		"We run transactions at read-committed isolation by default.",
		"The database connection pool is capped at twenty connections."},
	{"Errors are wrapped with %w and inspected via errors.Is.",
		"We wrap errors using %w and check them with errors.Is.",
		"Errors return a 4xx for client faults and 5xx for server faults."},
	{"Configuration is loaded from environment variables with a MEMINI_ prefix.",
		"Config comes from MEMINI_-prefixed environment variables.",
		"Configuration is validated at startup and the process exits on a bad value."},
	{"The build produces a single static Go binary.",
		"We ship one statically linked Go binary.",
		"The build embeds the version via ldflags at compile time."},
	{"gRPC services authenticate with mutual TLS.",
		"Our gRPC endpoints use mTLS for authentication.",
		"gRPC requests carry a deadline propagated through the context."},
	{"The team does code review on every change before merge.",
		"Every change is peer-reviewed before it is merged.",
		"The team squashes commits when merging a pull request."},
	{"Image uploads are limited to 10 megabytes.",
		"We cap image uploads at 10 MB.",
		"Uploaded images are converted to WebP on ingest."},
	{"The search index is rebuilt nightly at 02:00 UTC.",
		"We rebuild the search index every night at 2am UTC.",
		"The search index is stored separately from the primary database."},
	{"Email is sent through Postmark.",
		"We send transactional email via Postmark.",
		"Email templates are stored as MJML and compiled at build time."},
	{"The mobile app caches data offline with SQLite.",
		"Offline data on mobile is cached in SQLite.",
		"The mobile app syncs in the background every fifteen minutes."},
	{"Pagination uses opaque cursors, not page numbers.",
		"We paginate with opaque cursors rather than page offsets.",
		"Pagination defaults to a page size of fifty items."},
	{"The webhook signature is verified with an HMAC-SHA256 header.",
		"We verify webhooks via an HMAC-SHA256 signature header.",
		"Webhook deliveries are retried for up to 24 hours."},
	{"Time is stored in UTC and converted to local on display.",
		"We persist timestamps in UTC and localize them in the UI.",
		"Time ranges in queries are inclusive of the start and exclusive of the end."},
	{"The CDN in front of the API is Cloudflare.",
		"We front the API with Cloudflare as the CDN.",
		"The CDN caches static assets for one year with immutable headers."},
	{"Database backups run hourly and are kept for seven days.",
		"We back up the database every hour, retained a week.",
		"Database backups are encrypted at rest with a KMS key."},
	{"The primary language for services is Go.",
		"We write our services in Go.",
		"The primary datastore for analytics is ClickHouse."},
	{"Dependency updates are automated with Renovate.",
		"We automate dependency bumps using Renovate.",
		"Dependencies are pinned to exact versions in the lockfile."},
	{"The admin UI is restricted to the internal network only.",
		"Admin UI access is limited to the internal LAN.",
		"The admin UI shows an audit log of every privileged action."},
	{"Long-running requests stream results over server-sent events.",
		"We stream slow responses using SSE.",
		"Long-running requests have a hard 30 second timeout."},
	{"The queue worker pool size is eight.",
		"We run eight queue workers.",
		"The queue worker uses at-least-once delivery semantics."},
	{"Feature branches are named with a type/scope prefix.",
		"We prefix feature branches with a type and scope.",
		"Feature branches are deleted automatically after merge."},
	{"The service emits OpenTelemetry traces to a collector.",
		"We send OTel traces to a collector.",
		"Trace sampling is set to ten percent in production."},
	{"User data deletion requests are honored within 30 days.",
		"We process data-deletion requests within a month.",
		"User data is partitioned by tenant for isolation."},
	{"The Helm chart is based on the bjw-s common library.",
		"Our Helm chart builds on the bjw-s common library.",
		"The Helm chart exposes the service through a Gateway API route."},
	{"Embeddings are produced by an external OpenAI-compatible endpoint.",
		"We generate embeddings via an external OpenAI-compatible API.",
		"Embeddings are 1024-dimensional and L2-normalized before storage."},
}

// TestMergeHintThreshold measures the write-time dedup score (store.VectorSearch
// top-1 — exactly what service.dedupCheck compares against MEMINI_MERGE_HINT_MIN_SCORE
// / MEMINI_AUTO_SUPERSEDE_MIN_SCORE) for near-duplicate vs distinct-same-topic
// writes, over a large corpus, and sweeps candidate thresholds to report
// near-dup recall vs distinct false-hit rate. A clean gap means a merge-hint /
// auto-supersede default is safe; an overlap means it isn't (re-check per embedder).
//
// Needs a live embedder; skips when unreachable. Point it with MEMINI_EMBED_*.
func TestMergeHintThreshold(t *testing.T) {
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
	if len(probe) != 1 || len(probe[0]) != dims {
		t.Skipf("embedder returned %d-dim vectors, configured for %d — set MEMINI_EMBED_DIMS", len(probe[0]), dims)
	}

	st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "dedup.db"), dims)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	const ns = "dedup-eval"
	now := time.Unix(1_700_000_000, 0).UTC()

	// Ingest every base fact (semantic tier).
	baseTexts := make([]string, len(dedupTriples))
	for i, tr := range dedupTriples {
		baseTexts[i] = tr.base
	}
	baseVecs, err := e.Embed(ctx, baseTexts)
	if err != nil {
		t.Fatalf("embed bases: %v", err)
	}
	for i := range dedupTriples {
		if err := st.Upsert(ctx, &memory.Memory{
			ID: baseID(i), Namespace: ns, Tier: memory.TierSemantic, Content: baseTexts[i],
			CreatedAt: now, UpdatedAt: now, LastAccessedAt: now, Embedding: baseVecs[i],
		}); err != nil {
			t.Fatalf("upsert base %d: %v", i, err)
		}
	}

	// Embed all probes in one call, then run the same top-1 same-tier vector
	// search dedupCheck runs.
	probes := make([]string, 0, len(dedupTriples)*2)
	for _, tr := range dedupTriples {
		probes = append(probes, tr.paraphrase, tr.distinct)
	}
	pv, err := e.Embed(ctx, probes)
	if err != nil {
		t.Fatalf("embed probes: %v", err)
	}
	topScore := func(vec []float32) float64 {
		res, serr := st.VectorSearch(ctx, ns, vec, store.Filter{Tiers: []memory.Tier{memory.TierSemantic}, Now: now}, 1)
		if serr != nil {
			t.Fatalf("vector search: %v", serr)
		}
		if len(res) == 0 {
			return 0
		}
		return res[0].Score
	}

	dup := make([]float64, 0, len(dedupTriples))
	dist := make([]float64, 0, len(dedupTriples))
	for i := range dedupTriples {
		dup = append(dup, topScore(pv[i*2]))
		dist = append(dist, topScore(pv[i*2+1]))
	}
	sort.Float64s(dup)
	sort.Float64s(dist)
	n := float64(len(dedupTriples))

	t.Logf("merge-hint dedup eval — %d triples, embedder=%s, score = store.VectorSearch top-1 (what dedupCheck gates on)", len(dedupTriples), model)
	t.Logf("near-duplicate (SHOULD hint):   min=%.3f  p10=%.3f  median=%.3f  max=%.3f", dup[0], pctile(dup, 0.10), median(dup), dup[len(dup)-1])
	t.Logf("distinct-same-topic (NO hint):  min=%.3f  median=%.3f  p90=%.3f  max=%.3f", dist[0], median(dist), pctile(dist, 0.90), dist[len(dist)-1])

	// Threshold sweep: near-dup recall (caught) vs distinct false-hit rate.
	t.Logf("threshold | near-dup recall | distinct false-hit")
	t.Logf("----------+-----------------+-------------------")
	var bestT, bestYouden float64
	for thr := 0.50; thr <= 0.751; thr += 0.025 {
		recall := float64(countGE(dup, thr)) / n
		falseHit := float64(countGE(dist, thr)) / n
		t.Logf("  %.3f   |     %5.1f%%     |     %5.1f%%", thr, recall*100, falseHit*100)
		if youden := recall - falseHit; youden > bestYouden {
			bestYouden, bestT = youden, thr
		}
	}
	t.Logf("best separating threshold ≈ %.3f (recall−falseHit = %.2f); distinct ceiling = %.3f", bestT, bestYouden, dist[len(dist)-1])
	if gap := dup[0] - dist[len(dist)-1]; gap > 0 {
		t.Logf("CLEAN SEPARATION: every near-dup outscores every distinct (gap=%.3f)", gap)
	} else {
		t.Logf("OVERLAP at the tails (gap=%.3f) — a default needs a threshold that trades a little recall for ~0 false hits", gap)
	}

	// Sanity: paraphrases must, on the whole, score clearly higher than distinct facts.
	if median(dup) <= median(dist) {
		t.Errorf("near-dup median (%.3f) should exceed distinct median (%.3f)", median(dup), median(dist))
	}
}

// TestMergeHintRealCorpusFalseHit ingests a sample of REAL LongMemEval session
// docs and measures the nearest-neighbor (distinct) score distribution — the
// false-hit ceiling on real, diverse content at scale. If even the most-similar
// pair of distinct real memories stays below the threshold the authored eval
// suggested, that threshold is safe against false hits on real data.
//
// Skips without the LongMemEval data file (set MEMINI_LME_DATA) or a live
// embedder. Sample size via MEMINI_DEDUP_SAMPLE (default 300).
func TestMergeHintRealCorpusFalseHit(t *testing.T) {
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
	// Sample the first sampleN items with distinct content (one namespace, so the
	// nearest-neighbor search ranges over the whole sample).
	texts := make([]string, 0, sampleN)
	seen := make(map[string]struct{})
	for _, it := range ds.Items {
		c := it.Content
		if len(c) < 40 { // skip near-empty docs that can't be a meaningful near-dup
			continue
		}
		if _, dup := seen[c]; dup {
			continue
		}
		seen[c] = struct{}{}
		texts = append(texts, c)
		if len(texts) >= sampleN {
			break
		}
	}
	if len(texts) < 20 {
		t.Skipf("only %d usable docs in %s — too few", len(texts), dataPath)
	}

	st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "dedup_real.db"), dims)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	const ns = "dedup-real"
	now := time.Unix(1_700_000_000, 0).UTC()
	// Embed in batches so a single request body doesn't balloon, and ingest.
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

	// For each doc, the 2nd hit (top-1 is itself) is the nearest DISTINCT memory.
	nn := make([]float64, 0, len(texts))
	for i := range texts {
		res, serr := st.VectorSearch(ctx, ns, vecs[i], store.Filter{Tiers: []memory.Tier{memory.TierSemantic}, Now: now}, 2)
		if serr != nil {
			t.Fatalf("vector search: %v", serr)
		}
		if len(res) >= 2 {
			nn = append(nn, res[1].Score)
		}
	}
	sort.Float64s(nn)

	t.Logf("real-corpus false-hit eval — %d distinct LongMemEval docs, embedder=%s", len(texts), model)
	t.Logf("nearest-DISTINCT score:  min=%.3f  median=%.3f  p90=%.3f  p99=%.3f  max=%.3f",
		nn[0], median(nn), pctile(nn, 0.90), pctile(nn, 0.99), nn[len(nn)-1])
	for _, thr := range []float64{0.575, 0.60, 0.625, 0.65} {
		rate := float64(countGE(nn, thr)) / float64(len(nn))
		t.Logf("  false-hit rate @ %.3f = %5.2f%% (%d / %d distinct pairs)", thr, rate*100, countGE(nn, thr), len(nn))
	}
}

func baseID(i int) string { return "base-" + strconv.Itoa(i) }

func countGE(sorted []float64, thr float64) int {
	// sorted ascending → first index >= thr; count from there.
	i := sort.SearchFloat64s(sorted, thr)
	return len(sorted) - i
}

func median(sorted []float64) float64 { return pctile(sorted, 0.5) }

func pctile(sorted []float64, p float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	idx := max(0, min(int(p*float64(n-1)), n-1))
	return sorted[idx]
}
