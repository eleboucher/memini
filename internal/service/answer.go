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

// answerSystem is memini's default reader prompt for Answer. It follows the
// conventions the LoCoMo/LongMemEval leaders converged on (mem0, mnemory):
// resolve relative dates against the memory's own dates, prefer the most recent
// on conflict, allow inference across memories, answer in a short phrase — plus
// two hardening rules: abstain instead of fabricating a specific the memories
// don't state (finding related context is not finding the answer), and never
// silently pick a side of a tagged conflict that dates cannot order.
const answerSystem = "You answer the question using ONLY the provided memories. " +
	"A memory may carry a date in [brackets]; use those dates to resolve relative time " +
	"references ('last year', 'two months ago') to a specific date, month, or year, and answer " +
	"with the absolute value — never repeat the relative phrase. If memories conflict, prefer the " +
	"most recent; memories tagged [may conflict with #N] disagree on the same claim — when their " +
	"dates cannot order them, reply \"conflicting memories\" rather than silently picking one. " +
	"Connect facts across memories and infer the answer when it is implied rather than stated " +
	"outright, but never invent a specific (name, date, number, place) that no memory states or " +
	"implies: finding related context is not the same as finding the answer, and a wrong guess is " +
	"worse than admitting absence. Reply with the fact only, in 6 words or fewer (a name, date, " +
	"number, or short phrase); do not explain or restate the question. If no memory is relevant, " +
	"or the specific fact asked for is missing, reply \"I don't know\"."

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
	})
	if err != nil {
		s.metrics.AnswerResult("error")
		return AnswerResult{}, err
	}
	ans, err := s.answerer.Complete(ctx, answerSystem,
		"Memories:\n"+formatAnswerContext(res)+"\nQuestion: "+in.Query+"\nAnswer:")
	if err != nil {
		s.metrics.AnswerResult("error")
		return AnswerResult{}, fmt.Errorf("answer: generate: %w", err)
	}
	s.metrics.AnswerResult("ok")
	return AnswerResult{Answer: strings.TrimSpace(ans), Sources: res}, nil
}

// formatAnswerContext renders recalled memories for a reader prompt: numbered,
// date-bracketed (ValidFrom over CreatedAt so backdated facts resolve relative
// time correctly), with deterministic conflict tags — the write-path detector
// only compares at write time, so recall can surface live pairs it never saw
// side by side. The lexical classifier is precision-first: a tag means a real
// same-claim disagreement, not embedding-space proximity.
func formatAnswerContext(res []store.Scored) string {
	conflicts := conflictTags(res)
	var b strings.Builder
	for i, r := range res {
		date := r.Memory.CreatedAt
		if r.Memory.ValidFrom != nil {
			date = *r.Memory.ValidFrom
		}
		fmt.Fprintf(&b, "%d. [%s] %s", i+1, date.Format("2006-01-02"), r.Memory.Content)
		if tagged := conflicts[i]; len(tagged) > 0 {
			fmt.Fprintf(&b, " [may conflict with %s]", refList(tagged))
		}
		b.WriteByte('\n')
	}
	return b.String()
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
