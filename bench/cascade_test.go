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
	"github.com/eleboucher/memini/internal/store"
	"github.com/eleboucher/memini/internal/store/sqlitevec"
)

// TestCascadeRetrievalQuality measures what the ancestor/home/link read
// cascade actually does to recall — something the LongMemEval/LoCoMo suites
// cannot, because they ingest every item into ONE namespace (bench/system.go),
// so the cascade legs are a guaranteed no-op there and "benchmark byte-
// identical before/after" says nothing about the feature.
//
// It seeds a realistic hierarchy — a project namespace under a team under an
// org, plus the caller's home namespace and a lateral link — with durable
// facts at each level, some of them topically adjacent to the project's own
// facts (so ancestor facts genuinely compete in fusion, not just sit on
// unrelated topics). Then, for a fixed query set, it reports two numbers that
// pull in opposite directions:
//
//   - COVERAGE: for queries whose gold answer lives in an ancestor/home/link
//     namespace, does the cascade surface it? With the cascade off
//     (scope="project") these are structurally unreachable — recall must be 0;
//     the cascade's whole reason to exist is to lift that.
//   - DILUTION: for queries whose gold answer is a project-specific fact, does
//     adding the cascade legs push the project fact out of the top-k? The
//     primary leg is always in the pool, so any drop is ancestor/home/link
//     facts out-ranking the project's own — the cost side of the ledger.
//
// Needs a live embedder (the dev endpoint). Skips when unreachable, so CI
// without one is unaffected. Point it with MEMINI_EMBED_BASE_URL / _MODEL /
// _DIMS; MEMINI_RERANK_URL optionally wires the cross-encoder.
func TestCascadeRetrievalQuality(t *testing.T) {
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

	st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "cascade.db"), dims)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	clk := func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }
	opts := append([]service.Option{
		service.WithClock(clk), service.WithSyncReinforce(), service.WithScoreFusion(0.5),
	}, maybeReranker(t)...)
	svc := service.New(st, e, opts...) // cascade on by default

	// primary links to the shared Go namespace (one-hop lateral read).
	if err := st.PutLink(ctx, store.NamespaceLink{
		Src: cascadePrimary, Dst: cascadeLink, CreatedAt: clk(),
	}); err != nil {
		t.Fatalf("put link: %v", err)
	}

	if err := ingestCascadeCorpus(ctx, st, e, clk()); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	// Recall from the project namespace, with the caller's home set, under two
	// scopes: "project" (cascade off, primary only) and "full" (cascade on).
	type agg struct{ offHit5, onHit5, offHit10, onHit10, n int }
	byKind := map[string]*agg{}
	kindOrder := []string{"primary", "ancestor", "home", "link"}
	for _, k := range kindOrder {
		byKind[k] = &agg{}
	}

	for _, q := range cascadeQueries() {
		a := byKind[q.kind]
		a.n++
		off5 := cascadeRecall(ctx, t, svc, cascadeHome, "project", q.query, 5)
		on5 := cascadeRecall(ctx, t, svc, cascadeHome, "full", q.query, 5)
		off10 := cascadeRecall(ctx, t, svc, cascadeHome, "project", q.query, 10)
		on10 := cascadeRecall(ctx, t, svc, cascadeHome, "full", q.query, 10)
		if slices.Contains(off5, q.goldID) {
			a.offHit5++
		}
		if slices.Contains(on5, q.goldID) {
			a.onHit5++
		}
		if slices.Contains(off10, q.goldID) {
			a.offHit10++
		}
		if slices.Contains(on10, q.goldID) {
			a.onHit10++
		}
	}

	t.Logf("cascade retrieval eval — tree acme > acme/phoenix > acme/phoenix/api, home=%s, link=%s, embedder=%s",
		cascadeHome, cascadeLink, model)
	t.Logf("%-9s | %-5s | %-18s | %-18s", "gold in", "n", "cascade OFF R@5/10", "cascade ON R@5/10")
	t.Logf("%s", "----------+-------+--------------------+--------------------")
	for _, k := range kindOrder {
		a := byKind[k]
		nf := float64(a.n)
		t.Logf("%-9s | %-5d | %5.1f%% / %5.1f%%    | %5.1f%% / %5.1f%%",
			k, a.n, pct(a.offHit5, nf), pct(a.offHit10, nf), pct(a.onHit5, nf), pct(a.onHit10, nf))
	}

	// --- Coverage: the cascade's reason to exist -----------------------------
	// Ancestor/home/link golds are unreachable from a project-only read, and
	// reachable once the cascade is on.
	for _, k := range []string{"ancestor", "home", "link"} {
		a := byKind[k]
		if a.offHit10 != 0 {
			t.Errorf("%s golds: cascade OFF recall@10 = %d, want 0 (should be unreachable without the cascade)", k, a.offHit10)
		}
		if a.onHit10 == 0 {
			t.Errorf("%s golds: cascade ON recall@10 = 0, want > 0 (the cascade should reach them)", k)
		}
	}

	// --- Dilution: the cost side --------------------------------------------
	// Project-specific golds live in the always-present primary leg, so the
	// cascade can only displace them by out-ranking with ancestor/home/link
	// facts. Guard against a collapse: at most one project gold may fall out of
	// the top-5 when the cascade is on. The logged table carries the finer
	// signal; this is the tripwire.
	p := byKind["primary"]
	if p.onHit5 < p.offHit5-1 {
		t.Errorf("project golds diluted: cascade OFF R@5 hit %d/%d, ON R@5 hit %d/%d (dropped %d, tolerate 1)",
			p.offHit5, p.n, p.onHit5, p.n, p.offHit5-p.onHit5)
	}
}

const (
	cascadePrimary = "acme/phoenix/api" // project (request namespace)
	cascadeTeam    = "acme/phoenix"     // team (ancestor, depth 1)
	cascadeOrg     = "acme"             // org (ancestor, depth 2)
	cascadeHome    = "personal/kit"     // caller's home
	cascadeLink    = "shared/golang"    // lateral link target
)

type cascadeFact struct {
	id, ns, content string
}

type cascadeQuery struct {
	query, goldID, kind string
}

// cascadeCorpus is the seeded fact set. Project facts include several that are
// topically adjacent to ancestor facts (auth, logging, deploys) so the
// ancestor facts genuinely compete in fusion rather than sitting on unrelated
// topics — that is what makes the dilution number meaningful.
func cascadeCorpus() []cascadeFact {
	return []cascadeFact{
		// Project-specific (primary) — the gold for "primary" queries.
		{"p-auth", cascadePrimary, "The api service authenticates every route with a bearer token minted by the phoenix gateway sidecar."},
		{"p-log", cascadePrimary, "The api service ships its request logs to the phoenix-logs S3 bucket with a 30 day retention."},
		{"p-health", cascadePrimary, "The api service exposes its liveness probe at /internal/healthz, not the default /health."},
		{"p-db", cascadePrimary, "The api service reads and writes the orders table in the phoenix-primary Postgres cluster."},
		{"p-rate", cascadePrimary, "The api service rate-limits anonymous callers to 20 requests per second per IP."},

		// Team level (ancestor depth 1) — some adjacent to project topics.
		{"t-deploy", cascadeTeam, "Team Phoenix deploys exclusively through the shared Argo CD pipeline and never runs kubectl apply by hand."},
		{"t-authpolicy", cascadeTeam, "Team Phoenix requires every service to reject requests whose bearer token is older than fifteen minutes."},
		{"t-retro", cascadeTeam, "Team Phoenix runs its incident retrospective every Thursday at 15:00 UTC in the phoenix-oncall channel."},

		// Org level (ancestor depth 2) — company-wide policy.
		{"o-commits", cascadeOrg, "The company standardizes on Conventional Commits for every repository, enforced in CI."},
		{"o-otel", cascadeOrg, "All company services must emit structured JSON logs and OpenTelemetry traces to the central collector."},
		{"o-secrets", cascadeOrg, "Company policy forbids plaintext secrets in any repository; everything goes through the Vault sidecar."},

		// Home (caller's personal namespace) — durable preference.
		{"h-style", cascadeHome, "I prefer terse commit messages, tabs over spaces, and dark mode in every tool I use."},

		// Link target (shared/golang) — lateral shared knowledge.
		{"l-gofmt", cascadeLink, "Always run gofmt and golangci-lint before committing any Go code in a shared repository."},
	}
}

// cascadeQueries pairs a natural-language query with its gold fact and which
// namespace layer that gold lives in. Primary queries measure dilution;
// ancestor/home/link queries measure coverage.
func cascadeQueries() []cascadeQuery {
	return []cascadeQuery{
		// Primary golds — answerable from the project namespace alone.
		{"how does the api service authenticate incoming requests?", "p-auth", "primary"},
		{"where do the api request logs end up and for how long?", "p-log", "primary"},
		{"what path is the api health check on?", "p-health", "primary"},
		{"which database does the api service use for orders?", "p-db", "primary"},
		{"what is the rate limit for anonymous api callers?", "p-rate", "primary"},

		// Ancestor golds — only reachable via the cascade.
		{"how does our team ship changes to production?", "t-deploy", "ancestor"},
		{"how fresh does a bearer token have to be for team services?", "t-authpolicy", "ancestor"},
		{"when is the phoenix incident retro?", "t-retro", "ancestor"},
		{"what commit message convention should I follow?", "o-commits", "ancestor"},
		{"how are logs and traces supposed to be emitted company-wide?", "o-otel", "ancestor"},
		{"how should services handle secrets?", "o-secrets", "ancestor"},

		// Home gold — the caller's own durable preference.
		{"what are my personal formatting and editor preferences?", "h-style", "home"},

		// Link gold — lateral shared knowledge.
		{"what should I run before committing Go code?", "l-gofmt", "link"},
	}
}

func ingestCascadeCorpus(ctx context.Context, st *sqlitevec.Store, e embed.Embedder, now time.Time) error {
	corpus := cascadeCorpus()
	texts := make([]string, len(corpus))
	for i, f := range corpus {
		texts[i] = f.content
	}
	vecs, err := e.Embed(ctx, texts)
	if err != nil {
		return err
	}
	for i, f := range corpus {
		if err := st.Upsert(ctx, &memory.Memory{
			ID: f.id, Namespace: f.ns, Tier: memory.TierSemantic, Content: f.content,
			CreatedAt: now, UpdatedAt: now, LastAccessedAt: now, Embedding: vecs[i],
		}); err != nil {
			return err
		}
	}
	return nil
}

// cascadeRecall recalls from ns with the given home and scope, returning the
// ordered result IDs.
func cascadeRecall(ctx context.Context, t *testing.T, svc *service.Service, home, scope, query string, k int) []string {
	t.Helper()
	res, err := svc.Recall(ctx, service.RecallInput{
		Namespace: cascadePrimary, Home: home, Scope: scope, Query: query, Limit: k,
	})
	if err != nil {
		t.Fatalf("recall %q (scope=%s k=%d): %v", query, scope, k, err)
	}
	ids := make([]string, 0, len(res))
	for _, s := range res {
		ids = append(ids, s.Memory.ID)
	}
	return ids
}
