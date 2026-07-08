//go:build bench

package bench_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/eleboucher/memini/bench"
	"github.com/eleboucher/memini/internal/store/sqlitevec"
)

// TestCodingAgentSynthesis is the write-time synthesis spike A/B. Both arms
// ingest the v1 corpus via the production write path and answer single-shot (the
// cheap default arm — the agentic loop was already settled opt-in/off). The
// treatment adds one offline, QUESTION-BLIND synthesis pass between ingest and
// answering, which precomputes combined durable facts. The pre-registered
// question: does moving synthesis to write time lift synthesis accuracy WITHOUT
// regressing the other six categories (the poisoning guardrail)? Needs a live
// embedder + MEMINI_LLM_* and MEMINI_CODINGAGENT_DATA=data/codingagent_v1.json.
func TestCodingAgentSynthesis(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
	t.Cleanup(cancel)

	ds, _, err := bench.LoadCodingAgent(codingAgentDataPath())
	if err != nil {
		t.Fatalf("load %s: %v", codingAgentDataPath(), err)
	}
	e := codingAgentEmbedder(ctx, t)
	dims := envIntOr("MEMINI_EMBED_DIMS", 1024)
	chat := codingAgentChat(t, "MEMINI_LLM")
	judge := chat
	if os.Getenv("MEMINI_JUDGE_MODEL") != "" || os.Getenv("MEMINI_JUDGE_BASE_URL") != "" {
		judge = codingAgentChat(t, "MEMINI_JUDGE")
	}
	if err := os.MkdirAll("results", 0o755); err != nil {
		t.Fatalf("mkdir results: %v", err)
	}
	synthNow := latestNow(ds)

	arms := []struct {
		name       string
		synthesize bool
	}{
		{"baseline", false},
		{"synth", true},
	}

	verdicts := map[string][]bool{}
	catAcc := map[string]map[string][2]int{}
	var synthStats bench.SynthStats
	var extraItems int

	for _, arm := range arms {
		st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "synth-"+arm.name+".db"), dims)
		if err != nil {
			t.Fatalf("open %s store: %v", arm.name, err)
		}
		t.Logf("arm %s: ingesting %d items...", arm.name, len(ds.Items))
		if err := bench.IngestQAWrite(ctx, st, e, ds.Items, nil, nil); err != nil {
			t.Fatalf("ingest %s: %v", arm.name, err)
		}
		if arm.synthesize {
			// One namespace in this corpus; synthesize over it. Question-blind:
			// Synthesize sees only the store, never ds.Questions.
			ns := bench.NamespaceOf(ds.Questions[0].Group)
			s, err := bench.Synthesize(ctx, st, e, chat, ns, synthNow)
			if err != nil {
				t.Fatalf("synthesize: %v", err)
			}
			synthStats = s
			extraItems = s.Facts
			t.Logf("synthesis: %d clusters -> %d facts (%d calls, ~%d/%d tok)",
				s.Clusters, s.Facts, s.Calls, s.InTokens, s.OutTokens)
		}

		cc := bench.NewCountingChat(chat)
		ckpt := filepath.Join("results", fmt.Sprintf("codingagent_synth_%s.jsonl", arm.name))
		v, ca, _ := runAnswerCell(ctx, t, st, e, cc, judge, ds, "", ckpt) // "" = single-shot
		verdicts[arm.name] = v
		catAcc[arm.name] = ca
		_ = st.Close()
	}

	printSynthAB(t, ds, verdicts, catAcc, synthStats, extraItems)
}

// printSynthAB renders per-category baseline vs synth accuracy, the per-category
// paired contrasts (synthesis is the primary endpoint; the other six are
// poisoning guardrails), and the synthesizer's cost.
func printSynthAB(
	t *testing.T, ds *bench.Dataset,
	verdicts map[string][]bool, catAcc map[string]map[string][2]int,
	synthStats bench.SynthStats, extraItems int,
) {
	t.Helper()
	cats := map[string]bool{}
	for _, q := range ds.Questions {
		cats[q.Category] = true
	}
	catList := make([]string, 0, len(cats))
	for c := range cats {
		catList = append(catList, c)
	}
	sort.Strings(catList)

	var b strings.Builder
	fmt.Fprintf(&b, "\n## Synthesis spike A/B — %d questions, single-shot answerer\n\n", len(ds.Questions))
	fmt.Fprintf(&b, "| category | baseline | synth | Δ |\n")
	fmt.Fprintf(&b, "|----------|---------:|------:|--:|\n")
	for _, c := range catList {
		bl, sy := catAcc["baseline"][c], catAcc["synth"][c]
		blp, syp := pctOf(bl[0], bl[1]), pctOf(sy[0], sy[1])
		fmt.Fprintf(&b, "| %s | %.0f%% (%d/%d) | %.0f%% (%d/%d) | %+.0fpp |\n",
			c, blp, bl[0], bl[1], syp, sy[0], sy[1], syp-blp)
	}
	blc, bln := trueCount(verdicts["baseline"]), len(verdicts["baseline"])
	syc, syn := trueCount(verdicts["synth"]), len(verdicts["synth"])
	fmt.Fprintf(&b, "| **overall** | **%.0f%% (%d/%d)** | **%.0f%% (%d/%d)** | **%+.0fpp** |\n",
		pctOf(blc, bln), blc, bln, pctOf(syc, syn), syc, syn, pctOf(syc, syn)-pctOf(blc, bln))

	fmt.Fprintf(&b, "\n### Paired contrasts (McNemar exact) — synth vs baseline\n\n")
	contrast(&b, "PRIMARY synthesis", catVerdicts(ds, verdicts["synth"], "synthesis"), catVerdicts(ds, verdicts["baseline"], "synthesis"))
	contrast(&b, "overall", verdicts["synth"], verdicts["baseline"])
	fmt.Fprintf(&b, "\n_guardrail (poisoning) — the six non-synthesis categories must not regress:_\n")
	for _, c := range catList {
		if c == "synthesis" {
			continue
		}
		contrast(&b, "guardrail "+c, catVerdicts(ds, verdicts["synth"], c), catVerdicts(ds, verdicts["baseline"], c))
	}

	fmt.Fprintf(&b, "\n### Cost\n\nsynthesizer: %d clusters, %d LLM calls, %d facts stored (+%d items), ~%d in / ~%d out tokens\n",
		synthStats.Clusters, synthStats.Calls, synthStats.Facts, extraItems, synthStats.InTokens, synthStats.OutTokens)
	fmt.Print(b.String())
}

// catVerdicts projects a per-question verdict slice down to one category, so a
// paired McNemar contrast can run on just that category's questions (both arms
// answer the same question set in the same order).
func catVerdicts(ds *bench.Dataset, v []bool, category string) []bool {
	var out []bool
	for i, q := range ds.Questions {
		if q.Category == category {
			out = append(out, v[i])
		}
	}
	return out
}
