package bench

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/eleboucher/memini/internal/embed"
	"github.com/eleboucher/memini/internal/llm"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/service"
	"github.com/eleboucher/memini/internal/store"
)

// Judge rubrics grade an answer's fact-equivalence to the reference. The base
// rubric accepts paraphrase (distill-on-write rewrites content, so an answer
// grounded on a distilled fact restates it in the model's words, not the
// corpus's); knowledge-update and temporal questions get the leniency the
// official LongMemEval evaluation applies; abstention grades the decline itself.
const judgeBase = "You grade answers. Given a question, the reference answer, and a candidate answer, " +
	"reply with exactly CORRECT or INCORRECT. The candidate is CORRECT if it conveys the same key fact(s) " +
	"as the reference, even if phrased differently or with extra words. The candidate may be a distilled or " +
	"summarized paraphrase of the reference; grade on the key fact(s), not the wording."

const judgeKnowledgeUpdate = judgeBase +
	" The reference is the UPDATED value of a fact that changed over time: the candidate is CORRECT if it " +
	"states the updated value (even if it also mentions the earlier value as outdated), and INCORRECT if it " +
	"gives only the earlier, superseded value."

const judgeTemporal = judgeBase +
	" Dates within one day of the reference are CORRECT (timezone and relative-date arithmetic slack)."

const judgeAbstention = "You grade answers to questions that are NOT answerable from the memories the " +
	"candidate saw. Reply with exactly CORRECT or INCORRECT. The candidate is CORRECT only if it declines to " +
	"answer — says it doesn't know, the information wasn't mentioned, or the question can't be answered. Any " +
	"substantive invented answer is INCORRECT."

// JudgeSystemFor returns the per-category judge rubric. The coding-agent suite's
// "temporal-update" reuses the knowledge-update rubric (the answer must reflect
// the latest value) and "abstention" the decline rubric; LongMemEval's
// knowledge-update / temporal-reasoning / *_abs categories keep their existing
// mappings.
func JudgeSystemFor(category string) string {
	switch {
	case category == "abstention" || strings.HasSuffix(category, "_abs"):
		return judgeAbstention
	case category == "knowledge-update" || category == "temporal-update":
		return judgeKnowledgeUpdate
	case category == "temporal-reasoning":
		return judgeTemporal
	default:
		return judgeBase
	}
}

// AnswerAndJudge runs the production answer path (recall + service.Answer's
// reader prompt; the agentic tool loop when a reasoning level is set) and grades
// the reply against the reference. It returns the verdict and the raw answer
// text so a paired comparison can list the discordant cases for inspection.
func AnswerAndJudge(
	ctx context.Context, svc *service.Service, judge llm.Completer, q Question, k int, level service.ReasoningLevel,
) (correct bool, answer string, err error) {
	res, err := svc.Answer(ctx, service.AnswerInput{Namespace: nsOf(q.Group), Query: q.Query, Limit: k, Reasoning: level})
	if err != nil {
		return false, "", err
	}
	ref := q.Answer
	if ref == "" {
		ref = "(no reference; unanswerable)"
	}
	grade, err := judge.Complete(ctx, JudgeSystemFor(q.Category),
		fmt.Sprintf("Question: %s\nReference: %s\nCandidate: %s\nGrade:", q.Query, ref, res.Answer))
	if err != nil {
		return false, res.Answer, err
	}
	g := strings.ToUpper(grade)
	correct = strings.Contains(g, "CORRECT") && !strings.Contains(g, "INCORRECT")
	return correct, res.Answer, nil
}

// qaWriteOpts mirrors the shipped server's no-LLM write wiring (cmd/memini):
// tier classification, gates, fingerprint/write dedup, corroboration,
// contradiction invalidation, the episodic low-signal gate, and heuristic
// extract-on-write. Shared by the cmd/qa command and the bench harness so both
// exercise the same production path.
func qaWriteOpts() []service.Option {
	return []service.Option{
		service.WithSyncReinforce(),
		service.WithWriteDedup(0.625, service.WriteDedupHint),
		service.WithCorroboration(0.70),
		service.WithContradictionDownrank(0.625),
		service.WithEpisodicMinChars(120),
		service.WithExtractOnWrite(true),
	}
}

// IngestQAWrite feeds items through service.Remember sequentially in dataset
// order (write-path corroborate/contradict is order-sensitive), clocked at each
// item's time so temporal targeting and valid_to invalidation see the real
// chronology. TTL is forced to never-expire (question dates can fall long after
// a session, and the bench measures answer quality, not retention). Item.Session
// rides along as session_id metadata so the session-echo guard and distill
// batching see realistic keys. distiller, when non-nil, wires LLM
// distill-on-write (superseding the heuristic extractor, as in production) — one
// completion per capture.
func IngestQAWrite(ctx context.Context, st store.Store, e embed.Embedder, items []Item, distiller llm.Distiller) error {
	// The clock advances per item while detached write-path work (extract,
	// distill, corroborate) reads it from background goroutines, so it must be
	// atomic.
	var clock atomic.Pointer[time.Time]
	start := benchClock()
	clock.Store(&start)
	opts := append(qaWriteOpts(), service.WithClock(func() time.Time { return *clock.Load() }))
	if distiller != nil {
		opts = append(opts, service.WithDistiller(distiller), service.WithDistillOnWrite(true))
	}
	svc := service.New(st, e, opts...)
	never := -time.Second
	var gated, merged int
	seen := map[string]bool{}
	for _, it := range items {
		if !it.Time.IsZero() {
			t := it.Time.UTC()
			clock.Store(&t)
		}
		var validFrom *time.Time
		if !it.Time.IsZero() {
			vf := it.Time.UTC()
			validFrom = &vf
		}
		var meta map[string]any
		if it.Session != "" {
			meta = map[string]any{"session_id": it.Session}
		}
		m, err := svc.Remember(ctx, service.RememberInput{
			Namespace: nsOf(it.Group), Content: it.Content, TTL: &never, ValidFrom: validFrom, Metadata: meta,
		})
		if err != nil {
			return fmt.Errorf("write-mode ingest %s: %w", it.ID, err)
		}
		if m == nil { // dropped by the episodic low-signal gate: accepted, not stored
			gated++
			continue
		}
		if seen[m.ID] {
			merged++
		}
		seen[m.ID] = true
	}
	svc.WaitBackground()
	if err := svc.FlushConsolidation(ctx); err != nil {
		return fmt.Errorf("write-mode ingest: flush consolidation: %w", err)
	}
	return nil
}

// IngestQAUpsert loads items directly into the store (retrieval-only baseline):
// semantic tier, dated at the item time so temporal targeting can aim.
func IngestQAUpsert(ctx context.Context, st store.Store, e embed.Embedder, items []Item) error {
	const batch = 25
	fallback := benchClock()
	for start := 0; start < len(items); start += batch {
		end := min(start+batch, len(items))
		texts := make([]string, end-start)
		for i, it := range items[start:end] {
			texts[i] = it.Content
		}
		vecs, err := e.Embed(ctx, texts)
		if err != nil {
			return err
		}
		for i, it := range items[start:end] {
			ts := fallback
			var validFrom *time.Time
			if !it.Time.IsZero() {
				ts = it.Time
				validFrom = &it.Time
			}
			if err := st.Upsert(ctx, &memory.Memory{
				ID: it.ID, Namespace: nsOf(it.Group), Tier: memory.TierSemantic, Content: it.Content,
				CreatedAt: ts, UpdatedAt: ts, LastAccessedAt: ts, ValidFrom: validFrom,
				Embedding: vecs[i],
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

// CountingChat wraps an llm.Client with per-direction token counters so an
// answer arm can report what the reader (and, in the agentic loop, each tool
// round) spent — the answer-path analogue of CountingDistiller. It implements
// both llm.Completer (single-shot reader + judge) and llm.ToolChat (the agentic
// loop): service.Answer type-asserts the answerer to ToolChat, so the wrapper
// must be passed as the WithAnswerer value or the loop silently degrades to
// single-shot. Token counts use the same ~4 bytes/token estimate as the
// retrieval metrics and cover the prompt/response text only.
type CountingChat struct {
	completer llm.Completer
	tools     llm.ToolChat

	completes  atomic.Int64
	toolRounds atomic.Int64
	inTokens   atomic.Int64
	outTokens  atomic.Int64
}

// NewCountingChat wraps c; if c implements llm.ToolChat the agentic loop is
// supported, otherwise ChatTools reports an error the loop treats as a fallback.
func NewCountingChat(c llm.Client) *CountingChat {
	cc := &CountingChat{completer: c}
	if tc, ok := any(c).(llm.ToolChat); ok {
		cc.tools = tc
	}
	return cc
}

// Complete forwards to the wrapped completer, counting the call and payload
// tokens in each direction.
func (c *CountingChat) Complete(ctx context.Context, system, user string) (string, error) {
	c.completes.Add(1)
	c.inTokens.Add(int64(estimateTokens(system) + estimateTokens(user)))
	out, err := c.completer.Complete(ctx, system, user)
	if err != nil {
		return "", err
	}
	c.outTokens.Add(int64(estimateTokens(out)))
	return out, nil
}

// ChatTools forwards one agentic round, counting the round and payload tokens.
func (c *CountingChat) ChatTools(
	ctx context.Context, system string, turns []llm.ChatTurn, tools []llm.Tool, choice llm.ToolChoice,
) (llm.ChatResult, error) {
	if c.tools == nil {
		return llm.ChatResult{}, fmt.Errorf("bench: wrapped client does not support tool calls")
	}
	c.toolRounds.Add(1)
	in := estimateTokens(system)
	for _, t := range turns {
		in += estimateTokens(t.Text)
	}
	c.inTokens.Add(int64(in))
	res, err := c.tools.ChatTools(ctx, system, turns, tools, choice)
	if err != nil {
		return res, err
	}
	out := estimateTokens(res.Text)
	for _, call := range res.Calls {
		out += estimateTokens(string(call.Args))
	}
	c.outTokens.Add(int64(out))
	return res, nil
}

// ChatStats is a snapshot of a CountingChat's counters.
type ChatStats struct {
	Completes  int64 `json:"completes"`
	ToolRounds int64 `json:"tool_rounds"`
	InTokens   int64 `json:"in_tokens_est"`
	OutTokens  int64 `json:"out_tokens_est"`
}

// Stats returns the current counter values.
func (c *CountingChat) Stats() ChatStats {
	return ChatStats{
		Completes:  c.completes.Load(),
		ToolRounds: c.toolRounds.Load(),
		InTokens:   c.inTokens.Load(),
		OutTokens:  c.outTokens.Load(),
	}
}
