//go:build bench

package bench_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/search"
	"github.com/eleboucher/memini/internal/service"
	"github.com/eleboucher/memini/internal/store"
	"github.com/eleboucher/memini/internal/store/sqlitevec"
)

// TestSessionEchoLocalization simulates a multi-turn coding session against
// the production no-LLM stack, reproducing the field report "my agent spat
// the same response back 3 messages in a row, based on my first message".
// Every other bench is one-shot recall over a fixed corpus; this one lets
// turn-capture write memories BETWEEN turns, the dynamic no existing harness
// exercises.
//
// Per turn (mirroring the real integration order): recall with the incoming
// user message, then Remember the turn capture exactly in the plugin shape
// (user[:1000] + "\n\n" + assistant[:3000], tier episodic, source
// turn_capture, session_id). Turn 4 shifts topic to measure pinning vs mere
// topical similarity.
//
// Hypotheses, stated before numbers:
//
//	(a) capture echo — the agent's own turn-1 response is stored and recalled
//	    back on later turns. Ruled IN if condition A surfaces the turn-1
//	    capture in top-k at turns 2+ while condition B (session exclusion)
//	    stays clean.
//	(b) first-message dominance — one memory pins top-1 across evolving
//	    queries via the usage feedback loop (every recall reinforces:
//	    usage = 1+ln(1+AccessCount), a +69% quality bump after the first
//	    hit). Ruled IN if the pin survives the turn-4 topic shift, in any
//	    condition.
//	(d) dedup coalescing — later turns collapse onto the turn-1 row. Ruled
//	    IN if a capture reuses an earlier ID (expected OUT: hint-mode write
//	    dedup skips episodic, fingerprint dedup is exact-only).
//	extract amplifier — with no LLM, extract-on-write mints durable facts
//	    from the capture text (the agent's own words) with no session_id, so
//	    session exclusion can never catch them. Ruled IN if extract rows
//	    derived from assistant text surface in later turns' top-k in ALL
//	    conditions.
//
// Condition A reproduces the echo by design (it models a client passing no
// session guard), so it carries no echo gates — only diagnostics. The live
// gates: condition B must never return a live-session row (ExcludeMetadata
// leak) nor any content derived from a live turn (the extract-on-write
// amplifier, fixed by stamping extract rows with the parent's session_id),
// and no guarded condition may pin top-1 across the turn-4 topic shift.
func TestSessionEchoLocalization(t *testing.T) {
	ctx := context.Background()
	embedders := sweepEmbedders(t, ctx)
	if len(embedders) == 0 {
		t.Skip("no sweep embedder reachable")
	}

	t0 := time.Unix(1_700_000_000, 0).UTC()
	conds := []sessionCondition{
		// A: model-driven memory_recall, a broken client guard, or a session-id
		// rollover — excluding a different id is the same as excluding nothing.
		{name: "mcp-no-exclude", capture: true},
		// B: a working integration guard (pre-tool-use.mjs and friends).
		{name: "plugin-exclude", exclude: map[string]string{"session_id": liveSessionID}, capture: true},
		// C: control — no captures at all, isolating dominance from echo.
		{name: "no-capture"},
	}
	for _, emb := range embedders {
		t.Run(emb.name, func(t *testing.T) {
			for _, cond := range conds {
				t.Run(cond.name, func(t *testing.T) {
					runSessionEcho(t, ctx, emb, cond, t0)
				})
			}
		})
	}
}

const liveSessionID = "sess-live"

type sessionCondition struct {
	name    string
	exclude map[string]string
	capture bool
}

// sessionTurn is one user→assistant exchange of the simulated session. The
// assistant text is what turn-capture stores verbatim — rich answers, well
// past the 120-char episodic gate, like real coding-agent output.
type sessionTurn struct {
	user      string
	assistant string
}

// Turns 1-3 evolve on one thread (worker-pool shutdown); turn 4 shifts topic
// entirely, so anything from the pool thread still in its top-3 is pinning.
var sessionTurns = []sessionTurn{
	{
		user: "Why does our worker pool deadlock when the context is cancelled during shutdown?",
		assistant: "The deadlock comes from the unbuffered results channel in internal/jobs/pool.go. " +
			"When the context is cancelled the consumer goroutine returns immediately, but the eight workers " +
			"are still trying to send their in-flight results on the unbuffered channel, so every send blocks " +
			"forever and Wait() never returns. Two fixes work together: give the results channel a buffer equal " +
			"to the worker count so in-flight sends can complete, and wrap every send in a select that also " +
			"watches ctx.Done() so a worker can bail out when nobody is listening anymore. The select is the " +
			"important half — the buffer alone just narrows the race window without closing it.",
	},
	{
		user: "OK I added the select on ctx.Done in the send path. Now TestPoolDrain hangs forever — what is wrong there?",
		assistant: "TestPoolDrain hangs because the WaitGroup still counts the workers that bailed out through " +
			"the ctx.Done branch. The test calls wg.Add(1) per worker but the early-return path skips wg.Done(), " +
			"so wg.Wait() blocks even though every goroutine has exited. Move the wg.Done() into a defer at the " +
			"top of the worker function so every exit path decrements it, and the drain test will finish. While " +
			"you are there, give the test a watchdog via context.WithTimeout so a future regression fails fast " +
			"instead of hanging CI for the full ten-minute timeout.",
	},
	{
		user: "Tests pass now. Should the pool expose an explicit Drain method, or is cancelling the context enough for callers?",
		assistant: "Expose a Drain(ctx) method and keep context cancellation as the hard stop. Drain should close " +
			"the intake channel, wait for in-flight jobs under a caller-supplied deadline, and return whatever " +
			"error the deadline produced; cancellation stays the emergency brake that abandons work. Callers get " +
			"graceful shutdown by default and can still bail out. Wire Drain into the service shutdown path " +
			"before the store closes, so no worker writes to a store that is already gone.",
	},
	{
		user: "Separate thing: how do we configure the sqlite busy_timeout for the store, and where does that live?",
		assistant: "Set it in the DSN when the store opens: internal/store/sqlitevec appends pragmas to the " +
			"connection string, so add _pragma=busy_timeout(5000) there. That makes writers wait up to five " +
			"seconds for the single-writer lock instead of failing immediately with SQLITE_BUSY. Keep the value " +
			"modest — the sweeper and the dedup job hold the write lock only briefly, and a long busy_timeout " +
			"just hides real contention instead of surfacing it.",
	},
}

// sessionSeed is one pre-existing memory: the project corpus the session
// recalls against. Durables are written 14 days back, prior-session captures
// 3 days back — a live project, not an empty store.
type sessionSeed struct {
	content string
	tier    memory.Tier
	meta    map[string]any
}

var sessionSeeds = []sessionSeed{
	{tier: memory.TierSemantic, content: "The worker pool in internal/jobs uses a fixed fan-out of eight goroutines reading from a single intake channel."},
	{tier: memory.TierSemantic, content: "The project targets Go 1.24 and CI runs the full test suite with the race detector enabled."},
	{tier: memory.TierSemantic, content: "The store backend is sqlite with the sqlite-vec extension; postgres is the alternative backend behind the same store interface."},
	{tier: memory.TierSemantic, content: "Releases are cut with goreleaser from main; version tags follow semver and deliberately stay in 0.x."},
	{tier: memory.TierSemantic, content: "Configuration comes from environment variables parsed once at startup; recall tuning knobs are baked as constants in cmd/memini."},
	{tier: memory.TierSemantic, content: "Graceful shutdown order is: stop intake, drain the workers, flush background jobs, then close the store."},
	{tier: memory.TierProcedural, content: "To run the integration tests: go test -tags integration ./... with the dev database container running."},
	{tier: memory.TierProcedural, content: "To cut a release: tag vX.Y.Z on main, push the tag, and CI runs goreleaser with the changelog generated by git-cliff."},
	{tier: memory.TierProcedural, content: "To add a store migration: create a numbered SQL file under internal/store/migrations and register it in the migration list."},
	// Prior-session turn captures, in the exact plugin shape. Adjacent topics
	// on purpose: session exclusion does NOT cover these (by design), so they
	// are the material for cross-session dominance in condition B.
	{tier: memory.TierEpisodic, meta: turnMeta("sess-prior-1"), content: "Should the intake channel for the job queue be buffered or unbuffered?\n\nA small buffer smooths submission bursts but hides backpressure from callers. Keep the intake unbuffered so a full pool pushes back immediately, and let the caller decide whether to queue; the results side is where buffering actually helps."},
	{tier: memory.TierEpisodic, meta: turnMeta("sess-prior-1"), content: "Why did the linter start flagging our error wrapping after the last upgrade?\n\ngolangci-lint tightened err113 defaults: dynamic errors built with fmt.Errorf without %w are now reported. Wrap the underlying error with %w where callers inspect it, and define sentinel errors for the cases tests match on."},
	{tier: memory.TierEpisodic, meta: turnMeta("sess-prior-1"), content: "What causes the CI flake in TestSweeperRace?\n\nThe sweeper's ticker fires during store close in roughly one run in fifty. The fix is to stop the ticker before closing the store and wait for the sweep goroutine to exit, mirroring what the service shutdown path already does."},
	{tier: memory.TierEpisodic, meta: turnMeta("sess-prior-2"), content: "How should structured logging be set up for the new jobs package?\n\nUse the shared slog handler from internal/log with a component attribute set to jobs. Log job start and end at debug, failures at error with the job id, and never log payload contents — they can carry user data."},
	{tier: memory.TierEpisodic, meta: turnMeta("sess-prior-2"), content: "What batch size should the embedder client use for backfills?\n\nSixteen. Larger batches trip the 413 request cap on TEI-style endpoints, and the throughput win past sixteen is marginal. The batched wrapper already splits requests, so callers just pass the full slice."},
	{tier: memory.TierEpisodic, meta: turnMeta("sess-prior-2"), content: "Which Grafana panel shows the write-path routing decisions?\n\nThe Contradiction routing panel on the memini dashboard tracks the contradict metric by outcome label. Corroboration and dedup hints each have their own stat tiles on the same row, wired to the counters the service already exports."},
}

func turnMeta(session string) map[string]any {
	return map[string]any{"source": "turn_capture", "session_id": session, "format": "turn"}
}

func runSessionEcho(t *testing.T, ctx context.Context, emb sweepEmbedder, cond sessionCondition, t0 time.Time) {
	day := 24 * time.Hour
	st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), cond.name+".db"), emb.dims)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	cur := t0.Add(-14 * day)
	clk := func() time.Time { return cur }
	const ns = "session-echo"
	// The production no-LLM stack (cmd/memini/root.go buildServiceStack), plus
	// bench determinism. Consolidator/distiller/reranker are nil in the main
	// deployment target, so their options are omitted rather than stubbed.
	svc := service.New(st, emb.e,
		service.WithClock(clk), service.WithSyncReinforce(),
		service.WithShortTermCap(1000),
		service.WithScoreFusion(search.DefaultFusionAlpha),
		service.WithWriteDedup(0.625, service.WriteDedupHint),
		service.WithCorroboration(0.70),
		service.WithContradictionDownrank(0.625),
		service.WithTemporalTargeting(0.40, search.RegexAnchorExtractor{}),
		service.WithRecallMinScore(0.1),
		service.WithRecallSemanticReserve(2),
		service.WithEpisodicMinChars(120),
		service.WithDistillOnWrite(true),
		service.WithExtractOnWrite(true),
	)

	// Seed the project corpus through the real write path: seeds need real
	// confidence seeding, extraction, and corroboration behavior, or the
	// session runs against a store that behaves nothing like production.
	for _, seed := range sessionSeeds {
		if seed.tier == memory.TierEpisodic {
			continue
		}
		if _, rerr := svc.Remember(ctx, service.RememberInput{Namespace: ns, Content: seed.content, Tier: seed.tier}); rerr != nil {
			t.Fatalf("seed durable: %v", rerr)
		}
	}
	svc.WaitBackground()
	cur = t0.Add(-3 * day)
	for _, seed := range sessionSeeds {
		if seed.tier != memory.TierEpisodic {
			continue
		}
		if _, rerr := svc.Remember(ctx, service.RememberInput{
			Namespace: ns, Content: seed.content, Tier: seed.tier,
			Tags: []string{"turn-capture", "memini"}, Metadata: seed.meta,
		}); rerr != nil {
			t.Fatalf("seed prior capture: %v", rerr)
		}
	}
	svc.WaitBackground()

	known := listIDs(t, ctx, st, ns)

	var (
		captureIDs   []string
		top1AtK3     []string // per turn, the k=3 top hit's ID
		t1Access     []int    // turn-1 capture AccessCount after each turn
		extractTotal int
		echoTop1     int // turns where a live capture ranked #1 at k=3
	)
	for turn := range sessionTurns {
		cur = t0.Add(time.Duration(turn) * 2 * time.Minute)
		q := sessionTurns[turn].user

		// k=3 is the injection-realistic window, k=5 the diagnostic wide angle.
		// Running both doubles reinforcement vs a single-recall integration —
		// acceptable in a diagnostic; it only makes dominance show up sooner.
		var resK3 []store.Scored
		for _, k := range []int{3, 5} {
			res, rerr := svc.Recall(ctx, service.RecallInput{
				Namespace: ns, Query: q, Limit: k, ExcludeMetadata: cond.exclude,
			})
			if rerr != nil {
				t.Fatalf("turn %d recall k=%d: %v", turn+1, k, rerr)
			}
			if k == 3 {
				resK3 = res
			}
			var row strings.Builder
			ownShare, priorShare := 0, 0
			for i, r := range res {
				sid, _ := r.Memory.Metadata["session_id"].(string)
				if sid == liveSessionID {
					ownShare++
					if cond.exclude != nil {
						t.Errorf("turn %d k=%d: ExcludeMetadata leak — live-session row %s returned despite exclusion", turn+1, k, r.Memory.ID)
					}
				}
				// With exclusion on, nothing derived from this session's turns may
				// come back — including extract-on-write facts minted from the
				// assistant's own answers (they inherit the parent's session_id).
				if cond.exclude != nil && fromLiveTurn(r.Memory.Content, turn) {
					t.Errorf("turn %d k=%d: live-session-derived content returned despite exclusion: %q", turn+1, k, preview(r.Memory.Content))
				}
				if strings.HasPrefix(sid, "sess-prior") {
					priorShare++
				}
				echo := ""
				for j, cid := range captureIDs {
					if r.Memory.ID == cid {
						echo = " ECHO:t" + string(rune('1'+j))
					}
				}
				row.WriteString(strings.ReplaceAll(
					strings.TrimSpace(
						"  "+string(rune('1'+i))+": "+string(r.Memory.Tier)+" "+preview(r.Memory.Content)+echo), "\n", " ") + "\n")
			}
			t.Logf("turn %d k=%d own=%d/%d prior=%d/%d\n%s", turn+1, k, ownShare, k, priorShare, k, row.String())
		}
		if len(resK3) > 0 {
			top1AtK3 = append(top1AtK3, resK3[0].Memory.ID)
			for _, cid := range captureIDs {
				if resK3[0].Memory.ID == cid {
					echoTop1++
				}
			}
		} else {
			top1AtK3 = append(top1AtK3, "")
		}

		if cond.capture {
			content := clip(sessionTurns[turn].user, 1000) + "\n\n" + clip(sessionTurns[turn].assistant, 3000)
			m, rerr := svc.Remember(ctx, service.RememberInput{
				Namespace: ns, Content: content, Tier: memory.TierEpisodic,
				Tags:     []string{"turn-capture", "memini"},
				Metadata: turnMeta(liveSessionID),
			})
			if rerr != nil {
				t.Fatalf("turn %d capture: %v", turn+1, rerr)
			}
			if m == nil {
				t.Fatalf("turn %d capture dropped by the episodic gate — fixture too short, run invalid", turn+1)
			}
			// Hypothesis (d): a reused ID means the write coalesced onto an
			// earlier row instead of inserting.
			if known[m.ID] {
				t.Logf("turn %d capture COALESCED onto existing row %s (hypothesis d)", turn+1, m.ID)
			}
			captureIDs = append(captureIDs, m.ID)
		}
		svc.WaitBackground()

		// Extraction accounting: any new row that is not the capture itself
		// was minted by extract-on-write — from text that includes the
		// assistant's own words, with no session_id to exclude on.
		rows := listIDs(t, ctx, st, ns)
		for id := range rows {
			if known[id] || (len(captureIDs) > 0 && id == captureIDs[len(captureIDs)-1]) {
				continue
			}
			m, gerr := st.Get(ctx, ns, id)
			if gerr != nil {
				t.Fatalf("get extract row: %v", gerr)
			}
			fromAssistant := strings.Contains(sessionTurns[turn].assistant, strings.TrimSpace(m.Content))
			extractTotal++
			t.Logf("turn %d extract row: tier=%s kind=%v fromAssistant=%v %q", turn+1, m.Tier, m.Metadata["memory_type"], fromAssistant, preview(m.Content))
		}
		known = rows

		if len(captureIDs) > 0 {
			g, gerr := st.Get(ctx, ns, captureIDs[0])
			if gerr != nil {
				t.Fatalf("get turn-1 capture: %v", gerr)
			}
			t1Access = append(t1Access, g.AccessCount)
		}
	}

	pinned := len(top1AtK3) == len(sessionTurns) && top1AtK3[1] != "" &&
		top1AtK3[1] == top1AtK3[2] && top1AtK3[2] == top1AtK3[3]
	t.Logf("summary %s: top1@k3 per turn %v | live-capture top-1 hits %d | t1 AccessCount %v | extract rows %d | turns2-4 pinned to one row: %v",
		cond.name, shortIDs(top1AtK3), echoTop1, t1Access, extractTotal, pinned)

	// Regression gates. Condition A (no exclusion) echoes by design — it models
	// a client that passes no session guard — so the gates apply to the guarded
	// condition and the no-capture control only.
	if cond.name != "mcp-no-exclude" {
		if echoTop1 > 0 {
			t.Errorf("gate: live-session capture ranked #1 at k=3 on %d turn(s); want 0", echoTop1)
		}
		if pinned {
			t.Errorf("gate: top-1 pinned to a single row across turns 2-4 despite the turn-4 topic shift")
		}
	}
}

// fromLiveTurn reports whether content is (or was extracted from) one of the
// live session's turns captured before the given turn index: the capture row
// itself contains the turn's assistant text, and an extract-on-write fact is a
// fragment of the turn's user or assistant text.
func fromLiveTurn(content string, turn int) bool {
	c := strings.TrimSpace(content)
	for j := 0; j < turn; j++ {
		if strings.Contains(c, sessionTurns[j].assistant) ||
			strings.Contains(sessionTurns[j].assistant, c) ||
			strings.Contains(sessionTurns[j].user, c) {
			return true
		}
	}
	return false
}

func listIDs(t *testing.T, ctx context.Context, st *sqlitevec.Store, ns string) map[string]bool {
	t.Helper()
	rows, err := st.List(ctx, ns, store.Filter{}, 0)
	if err != nil {
		t.Fatalf("list rows: %v", err)
	}
	ids := make(map[string]bool, len(rows))
	for _, m := range rows {
		ids[m.ID] = true
	}
	return ids
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func preview(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 72 {
		return s[:72] + "…"
	}
	return s
}

func shortIDs(ids []string) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		if len(id) > 8 {
			id = id[:8]
		}
		out[i] = id
	}
	return out
}
