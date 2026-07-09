package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/eleboucher/memini/internal/contradict"
	"github.com/eleboucher/memini/internal/llm"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/store"
)

// answerSystem is memini's reader prompt for Answer. It follows the
// conventions the LongMemEval/LoCoMo leaders converged on (mem0 benchmark
// prompt, Hindsight CARA): chronological context, chain-of-thought reasoning,
// explicit "most recent wins" for knowledge updates, temporal grounding
// against the question date, and a second-pass scan instruction.
const answerSystem = `You answer the question using ONLY the provided memories.

IMPORTANT RULES:
1. Scan ALL provided memories before answering. Do a SECOND full scan after your
   initial read — items at later positions are commonly missed.
2. For conflicting values of the same fact, use the most recent memory. The latest
   value REPLACES all earlier ones — do not sum or average. Memories about different
   people/contexts are NOT conflicting.
3. Use the dates in [brackets] to resolve relative time references ('last year',
   'two months ago') to a specific date, month, or year. Answer with the absolute
   value — never repeat the relative phrase.
4. Memories tagged [may conflict with #N] disagree on the same claim. When their
   dates cannot order them, reply "conflicting memories" rather than silently
   picking one.
5. Connect facts across memories and infer the answer when it is implied rather
   than stated outright. Facts needed for an answer are often in unrelated
   conversations — search ALL memories for each fact independently.
6. For counting or listing questions, enumerate each item with its date before
   giving the total. Do not skip items found during reasoning.
7. Never invent a specific (name, date, number, place) that no memory states or
   implies. Finding related context is not the same as finding the answer.
8. If no memory is relevant, or the specific fact asked for is missing, reply
   "I don't know".

Before answering, reason step-by-step inside <mem_thinking> tags:
- List every relevant memory and its date.
- For temporal questions: identify all dates, compute intervals from the question date.
- For counting: enumerate each item with its date.
- For preference questions: scan ALL memories for what the user likes, dislikes,
  avoids, or prefers — even indirectly stated. Infer implicit preferences from
  their actions and choices, not just explicit "I prefer" statements.
- CONTEXT CHECK: Before using a memory's value, verify it applies to the SAME
  context as the question (same person, same topic, same time period).
After your reasoning, give the final answer on a new line after </mem_thinking>.
Reply with the fact(s) only, as briefly as the question allows: a name, date, number,
or short phrase for a single fact, or one short sentence when the question asks you
to combine several facts.`

// AnswerInput is a retrieve-then-generate request.
type AnswerInput struct {
	Namespace string
	Query     string
	// Limit caps how many recalled memories are given to the reader (default 10).
	Limit int
	Tiers []memory.Tier
	// Levels restricts grounding to memories whose derivation level matches one of
	// the listed values; empty means no level constraint.
	Levels   []memory.Level
	Tags     []string
	Metadata map[string]string
	// Reasoning selects the answer strategy: empty/minimal is single-shot;
	// low/medium/high run the bounded tool loop (see ReasoningLevel). Falls
	// back to single-shot when the configured LLM client can't do tool calls.
	Reasoning ReasoningLevel
}

// AnswerResult is the generated answer and the memories it was grounded on.
type AnswerResult struct {
	Answer  string
	Sources []store.Scored
}

// Answer recalls memories for the query and asks the configured LLM to answer
// from them, grounding the response and returning the supporting memories. It
// requires an answerer (see WithAnswerer); recall reuses the full hybrid +
// rerank path, so a configured reranker applies here too.
func (s *Service) Answer(ctx context.Context, in AnswerInput) (AnswerResult, error) {
	start := time.Now()
	defer func() { s.metrics.OpDuration("answer", time.Since(start)) }()
	if s.answerer == nil {
		s.metrics.AnswerResult("error")
		return AnswerResult{}, fmt.Errorf("answer: no LLM configured (use WithAnswerer)")
	}
	if in.Reasoning == ReasoningExpand {
		return s.answerExpand(ctx, in)
	}
	if iters := in.Reasoning.iterations(); iters > 0 {
		if tc, ok := s.answerer.(llm.ToolChat); ok {
			return s.answerAgentic(ctx, in, tc, iters)
		}
		// The configured client can't do tool calls: single-shot is the honest
		// fallback rather than an error — the caller asked for more effort, not
		// a different capability.
	}
	res, err := s.Recall(ctx, RecallInput{
		Namespace: in.Namespace, Query: in.Query, Limit: in.Limit, Tiers: in.Tiers,
		Levels: in.Levels, Tags: in.Tags, Metadata: in.Metadata,
		IncludeLinked: true, // C6: expand 1-hop linked memories for multi-hop answers
	})
	if err != nil {
		s.metrics.AnswerResult("error")
		return AnswerResult{}, err
	}
	ans, err := s.answerer.Complete(ctx, answerSystem,
		"Today's date: "+s.now().Format("2006-01-02")+
			"\nMemories:\n"+formatAnswerContext(res)+"\nQuestion: "+in.Query+"\nAnswer:")
	if err != nil {
		s.metrics.AnswerResult("error")
		return AnswerResult{}, fmt.Errorf("answer: generate: %w", err)
	}
	s.metrics.AnswerResult("ok")
	return AnswerResult{Answer: stripMemThinking(strings.TrimSpace(ans)), Sources: res}, nil
}

// formatAnswerContext renders recalled memories for a reader prompt:
// chronologically sorted (oldest first so the LLM sees the timeline in
// order), grouped by date with human-readable headers, date-bracketed
// (ValidFrom over CreatedAt so backdated facts resolve relative time
// correctly), with deterministic conflict tags. Chronological order helps
// the LLM reconstruct event timelines and identify the most recent value
// for knowledge-update questions.
func formatAnswerContext(res []store.Scored) string {
	conflicts := conflictTags(res)

	// Sort chronologically (oldest first) for temporal reasoning.
	type indexed struct {
		scored store.Scored
		idx    int // original index for conflict tag lookup
	}
	sorted := make([]indexed, len(res))
	for i, r := range res {
		sorted[i] = indexed{scored: r, idx: i}
	}
	// Stable sort by date so ties keep recall relevance order.
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0; j-- {
			dj := dateOf(sorted[j].scored.Memory)
			djPrev := dateOf(sorted[j-1].scored.Memory)
			if dj.Before(djPrev) {
				sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
			} else {
				break
			}
		}
	}

	var b strings.Builder
	var prevDate string
	for newNum, s := range sorted {
		date := dateOf(s.scored.Memory)
		dateStr := date.Format("2006-01-02")
		// Date header when the date changes.
		if dateStr != prevDate {
			if prevDate != "" {
				b.WriteByte('\n')
			}
			fmt.Fprintf(&b, "--- %s ---\n", dateStr)
			prevDate = dateStr
		}
		fmt.Fprintf(&b, "%d. [%s] %s", newNum+1, dateStr, s.scored.Memory.Content)
		if tagged := conflicts[s.idx]; len(tagged) > 0 {
			fmt.Fprintf(&b, " [may conflict with %s]", refList(tagged))
		}
		b.WriteByte('\n')
	}
	return b.String()
}

func dateOf(m *memory.Memory) time.Time {
	if m.ValidFrom != nil {
		return *m.ValidFrom
	}
	return m.CreatedAt
}

// stripMemThinking removes <mem_thinking>...</mem_thinking> blocks from the
// LLM response, leaving only the final answer. The tags are used for
// chain-of-thought reasoning that should not appear in the answer.
func stripMemThinking(s string) string {
	for {
		start := strings.Index(s, "<mem_thinking>")
		if start < 0 {
			break
		}
		end := strings.Index(s, "</mem_thinking>")
		if end < 0 {
			// No closing tag — strip from start to end of string.
			return strings.TrimSpace(s[:start])
		}
		s = s[:start] + s[end+len("</mem_thinking>"):]
	}
	return strings.TrimSpace(s)
}

// conflictTags classifies every recalled pair with the lexical contradiction
// detector and returns, per memory index, the 1-based indices it disagrees
// with. Answer's recall is small (default 10), so the pairwise scan is
// negligible next to the LLM call.
func conflictTags(res []store.Scored) map[int][]int {
	tags := map[int][]int{}
	for i := range res {
		for j := i + 1; j < len(res); j++ {
			newer, older := res[i].Memory, res[j].Memory
			if older.CreatedAt.After(newer.CreatedAt) {
				newer, older = older, newer
			}
			if contradict.Classify(newer.Content, older.Content, contradict.Default).Class == contradict.Update {
				tags[i] = append(tags[i], j+1)
				tags[j] = append(tags[j], i+1)
			}
		}
	}
	return tags
}

// refList renders 1-based memory indices as "#2, #5".
func refList(ids []int) string {
	parts := make([]string, len(ids))
	for i, n := range ids {
		parts[i] = fmt.Sprintf("#%d", n)
	}
	return strings.Join(parts, ", ")
}
