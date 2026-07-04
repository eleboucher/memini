//go:build bench

package bench_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/eleboucher/memini/internal/extract"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/service"
	"github.com/eleboucher/memini/internal/store/sqlitevec"
)

// tierCase is one hand-labeled agent-write utterance. want is the tier the write
// SHOULD land in when the client omits a tier: "durable" for a fact worth
// asserting confidently on recall months later, "episodic" for chatter,
// questions, transient status, and hedged musing. kind is the expected marker
// class for durables (unused for episodic). trap annotates why the case is here.
type tierCase struct {
	text string
	want string // "durable" | "episodic"
	kind extract.Kind
	trap string
}

// tierFixture is a realistic agent-write corpus deliberately loaded with the
// classifier's trap shapes: terse single-marker durables (the false-episodic
// risk), two-marker transient status (the false-durable / poisoning risk),
// hedged musing, questions, and generic chatter. Labeled before running
// Classify. The label that matters most for poisoning is want=="episodic":
// any of those that Classify promotes is a false-durable — garbage the
// relevance-gated reserve will dutifully surface.
var tierFixture = []tierCase{
	// --- genuine durables (want promotion) ---
	{"We decided to use PostgreSQL instead of MySQL for the primary datastore.", "durable", extract.KindDecision, "decision, 2 markers"},
	{"Let's go with Redis for caching rather than Memcached.", "durable", extract.KindDecision, "decision, 2 markers"},
	{"We should switch to gRPC instead of REST for internal service calls.", "durable", extract.KindDecision, "decision, 2 markers"},
	{"We chose Kafka for the event bus; the tradeoff is operational complexity.", "durable", extract.KindDecision, "decision, 2 markers"},
	{"We settled on OpenTelemetry for tracing after weighing the pros and cons.", "durable", extract.KindDecision, "decision, 2 markers"},
	{"I'm going with the functional options pattern for the config API.", "durable", extract.KindDecision, "terse single-marker durable (false-episodic risk)"},
	{"The team agreed that every API response must be paginated, and the reason is that unbounded result sets caused real memory pressure in production during last quarter's incident.", "durable", extract.KindDecision, "verbose single-marker durable (<200 chars, no length bonus)"},
	{"I prefer tabs over spaces and always use gofmt before committing.", "durable", extract.KindPreference, "preference, 2 markers"},
	{"We always use conventional commits and never use force-push on main.", "durable", extract.KindPreference, "preference, 2 markers"},
	{"Please always use structured logging and never use fmt.Println in prod.", "durable", extract.KindPreference, "preference, 3 markers"},
	{"I hate when tests depend on wall-clock time; always use an injected clock.", "durable", extract.KindPreference, "preference, 2 markers"},
	{"My convention is to keep functions under fifty lines.", "durable", extract.KindPreference, "terse single-marker durable (false-episodic risk)"},
	{"The root cause of the crash was a nil pointer dereference in the parser.", "durable", extract.KindProblem, "problem, 2 markers"},
	{"The bug was a race condition and the fix was to add a mutex around the map.", "durable", extract.KindProblem, "problem, 3 markers"},
	{"The deadlock is resolved; we fixed it by always acquiring locks in ID order.", "durable", extract.KindProblem, "problem, 2 markers"},
	{"We fixed the deadlock by ordering lock acquisition consistently.", "durable", extract.KindProblem, "terse single-marker durable (false-episodic risk)"},

	// --- questions (want episodic) ---
	{"Should we use Redis or Memcached for the session cache?", "episodic", "", "question"},
	{"What's the best way to structure the config package?", "episodic", "", "question"},
	{"Do you think we should refactor the auth module first?", "episodic", "", "question w/ decision marker (gate must save)"},
	{"Which database are we using for analytics again?", "episodic", "", "question"},

	// --- transient status (want episodic) ---
	{"Running the tests now, will report back in a minute.", "episodic", "", "status"},
	{"Let me check the logs to see what happened.", "episodic", "", "status"},
	{"Okay, deploying to staging now.", "episodic", "", "status"},
	{"Let me run the migration and see if it works.", "episodic", "", "status"},
	{"Pulling the latest changes and rebuilding.", "episodic", "", "status"},
	{"Debugging the flaky test, it fails intermittently.", "episodic", "", "status"},
	{"The API call failed with a 500 error, retrying now.", "episodic", "", "status w/ problem marker (gate must save)"},
	{"I'm going to try the new endpoint and see how it does.", "episodic", "", "status w/ decision marker (gate must save)"},

	// --- two-marker transient status (the false-durable / poisoning traps) ---
	{"The build keeps failing and the tests are broken right now.", "episodic", "", "TRAP: transient status, 2 problem markers"},
	{"That whole approach is broken and honestly it's a mess of a problem.", "episodic", "", "TRAP: venting, 2 problem markers"},

	// --- hedged musing (want episodic; hedge veto must fire) ---
	{"I think maybe we should use GraphQL for the public API.", "episodic", "", "hedged (i think, maybe)"},
	{"We could possibly switch to Bun instead of Node down the line.", "episodic", "", "hedged (possibly)"},
	{"Probably better to cache this, but I'm not sure yet.", "episodic", "", "hedged (probably, not sure)"},
	{"It might be worth using a connection pool instead of opening new connections each time.", "episodic", "", "hedged (might)"},
	{"I guess we could go with Tailwind for the styling.", "episodic", "", "hedged (i guess)"},

	// --- generic chatter (want episodic; no markers) ---
	{"Thanks, that makes sense. Let me update the PR description.", "episodic", "", "chatter"},
	{"Good morning, ready to pair on the API today.", "episodic", "", "chatter"},
	{"The standup is at 3pm in the main conference room.", "episodic", "", "chatter"},
	{"I'll ping you once the deploy finishes.", "episodic", "", "chatter"},
	{"Sounds good, go ahead and merge whenever you're ready.", "episodic", "", "chatter"},
	{"Can you take a look at the failing CI job when you get a chance?", "episodic", "", "chatter"},
}

// TestTierClassifyPrecisionRecall measures the no-LLM write-time tier classifier
// on the hand-labeled fixture — pure text, no embedder. It reports the
// confusion matrix and, above all, the false-durable rate (chatter promoted to
// a durable tier): the poisoning input the relevance-gated reserve amplifies.
func TestTierClassifyPrecisionRecall(t *testing.T) {
	var tp, fp, fn, tn int
	var kindWrong int
	var falseDurables, falseEpisodics []tierCase

	for _, c := range tierFixture {
		kind, ok := extract.Classify(c.text)
		gotDurable := ok
		wantDurable := c.want == "durable"
		switch {
		case wantDurable && gotDurable:
			tp++
			if kind != c.kind {
				kindWrong++
				t.Logf("kind mismatch: got %q want %q — %q", kind, c.kind, c.text)
			}
		case wantDurable && !gotDurable:
			fn++
			falseEpisodics = append(falseEpisodics, c)
		case !wantDurable && gotDurable:
			fp++
			falseDurables = append(falseDurables, c)
		default:
			tn++
		}
	}

	durTotal := tp + fn
	epiTotal := fp + tn
	precision := ratio(tp, tp+fp)
	recall := ratio(tp, tp+fn)
	falseDurableRate := ratio(fp, epiTotal)
	falseEpisodicRate := ratio(fn, durTotal)

	t.Logf("fixture: %d writes (%d durable, %d episodic)", len(tierFixture), durTotal, epiTotal)
	t.Logf("confusion: TP=%d FP=%d FN=%d TN=%d", tp, fp, fn, tn)
	t.Logf("precision (of promoted, share correct): %.3f", precision)
	t.Logf("recall    (of durables, share promoted): %.3f", recall)
	t.Logf("FALSE-DURABLE rate (chatter -> durable):  %.3f  (%d/%d) <- poisoning input to reserve", falseDurableRate, fp, epiTotal)
	t.Logf("false-episodic rate (durable -> lost):    %.3f  (%d/%d)", falseEpisodicRate, fn, durTotal)
	t.Logf("kind mislabels among true durables: %d/%d", kindWrong, tp)

	if len(falseDurables) > 0 {
		t.Logf("--- false-durables (episodic label promoted to durable) ---")
		for _, c := range falseDurables {
			kind, _ := extract.Classify(c.text)
			t.Logf("  [%s -> %s] %q  (%s)", "episodic", kind.Tier(), c.text, c.trap)
		}
	}
	if len(falseEpisodics) > 0 {
		t.Logf("--- false-episodics (durable label lost to episodic TTL) ---")
		for _, c := range falseEpisodics {
			t.Logf("  [durable -> episodic] %q  (%s)", c.text, c.trap)
		}
	}

	// Anti-poisoning tripwire: the false-durable rate is the input the
	// relevance-gated reserve amplifies, so it is what a marker/veto change must
	// not regress. Measured at 1/25 (a lone transient-problem status). Guard a
	// little above that; false-episodics are deliberately un-asserted — the
	// extractor is conservative by design ("a miss costs nothing"), so losing
	// terse durables is expected and is the opposite of poisoning.
	if fp > 2 {
		t.Errorf("false-durable count %d exceeds tripwire 2 (rate %.3f) — a marker/veto loosening is poisoning the durable tiers", fp, falseDurableRate)
	}
	if kindWrong > 0 {
		t.Errorf("%d promoted durables got the wrong marker kind", kindWrong)
	}
}

// TestTierClassifyDownstreamHarm prices the poisoning end-to-end on live
// embedders: it ingests the fixture through svc.Remember with the tier OMITTED
// (so Classify actually fires — the QA/scoreboard benches bypass this by
// upserting directly), confirms the realized durable-tier population, then
// probes whether any false-durable is injected into the recall window on
// episodic-topic queries that have no relevant durable — the exact "reserve
// surfaces garbage" failure. Skips when no sweep embedder is reachable.
func TestTierClassifyDownstreamHarm(t *testing.T) {
	ctx := context.Background()
	embedders := sweepEmbedders(t, ctx)
	if len(embedders) == 0 {
		t.Skip("no sweep embedder reachable")
	}

	for _, emb := range embedders {
		t.Run(emb.name, func(t *testing.T) {
			st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "tier.db"), emb.dims)
			if err != nil {
				t.Fatalf("open store: %v", err)
			}
			t.Cleanup(func() { _ = st.Close() })

			const ns = "tier-eval"
			// Production defaults: score fusion, no episodic value gate (the shipped
			// out-of-box default per the defaults policy), classification on.
			svc := service.New(st, emb.e, service.WithSyncReinforce(), service.WithScoreFusion(0.5))

			// Ingest every fixture write with the tier OMITTED so Classify decides.
			storedTier := map[string]memory.Tier{} // id -> tier
			labelByID := map[string]string{}       // id -> ground-truth want
			textByID := map[string]string{}
			var realizedFalseDurable int
			for _, c := range tierFixture {
				m, rerr := svc.Remember(ctx, service.RememberInput{Namespace: ns, Content: c.text})
				if rerr != nil {
					t.Fatalf("remember %q: %v", c.text, rerr)
				}
				if m == nil {
					continue // dropped by a gate (not enabled here, but be safe)
				}
				storedTier[m.ID] = m.Tier
				labelByID[m.ID] = c.want
				textByID[m.ID] = c.text
				if c.want == "episodic" && m.Tier.Term() == memory.LongTerm {
					realizedFalseDurable++
					t.Logf("realized false-durable: [%s] %q", m.Tier, c.text)
				}
			}
			svc.WaitBackground()

			// Episodic-topic queries with no relevant durable answer: if a
			// false-durable is injected here, the poisoning is real and harmful.
			epiQueries := []string{
				"what time is the standup meeting",
				"can you check the CI job",
				"are the tests still running",
				"is the deploy finished yet",
				"good morning let's pair",
			}
			var injections, spray int
			for _, q := range epiQueries {
				res, rerr := svc.Recall(ctx, service.RecallInput{
					Namespace: ns, Query: q, Limit: 3, SemanticReserve: 2,
				})
				if rerr != nil {
					t.Fatalf("recall %q: %v", q, rerr)
				}
				hitDurable := false
				for _, sc := range res {
					if sc.Memory.Tier.Term() == memory.LongTerm {
						injections++
						hitDurable = true
						t.Logf("injection on %q: [%s] %q", q, sc.Memory.Tier, sc.Memory.Content)
					}
				}
				if hitDurable {
					spray++
				}
			}

			t.Logf("%s: realized false-durables in store %d; durable injections over %d episodic queries: %d (spray queries %d)",
				emb.name, realizedFalseDurable, len(epiQueries), injections, spray)
		})
	}
}

func ratio(n, d int) float64 {
	if d == 0 {
		return 0
	}
	return float64(n) / float64(d)
}
