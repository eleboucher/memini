package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/store"
)

// answerSystem is memini's default reader prompt for Answer. It follows the
// conventions the LoCoMo/LongMemEval leaders converged on (mem0, mnemory):
// resolve relative dates against the memory's own dates, prefer the most recent
// on conflict, allow inference across memories, answer in a short phrase, and
// only decline when nothing relevant was retrieved.
const answerSystem = "You answer the question using ONLY the provided memories. " +
	"A memory may carry a date in [brackets]; use those dates to resolve relative time " +
	"references ('last year', 'two months ago') to a specific date, month, or year, and answer " +
	"with the absolute value — never repeat the relative phrase. If memories conflict, prefer the " +
	"most recent. Connect facts across memories and infer the answer when it is implied rather than " +
	"stated outright. Reply with the fact only, in 6 words or fewer (a name, date, number, or short " +
	"phrase); do not explain or restate the question. Only if no memory is relevant, reply \"I don't know\"."

// AnswerInput is a retrieve-then-generate request.
type AnswerInput struct {
	Namespace string
	Query     string
	// Limit caps how many recalled memories are given to the reader (default 10).
	Limit int
	Tiers []memory.Tier
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
	res, err := s.Recall(ctx, RecallInput{
		Namespace: in.Namespace, Query: in.Query, Limit: in.Limit, Tiers: in.Tiers,
	})
	if err != nil {
		s.metrics.AnswerResult("error")
		return AnswerResult{}, err
	}
	var b strings.Builder
	for _, r := range res {
		// The date annotation the system prompt refers to: anchor each memory
		// on its creation date so relative time references can be resolved.
		fmt.Fprintf(&b, "- [%s] %s\n", r.Memory.CreatedAt.Format("2006-01-02"), r.Memory.Content)
	}
	ans, err := s.answerer.Complete(ctx, answerSystem,
		"Memories:\n"+b.String()+"\nQuestion: "+in.Query+"\nAnswer:")
	if err != nil {
		s.metrics.AnswerResult("error")
		return AnswerResult{}, fmt.Errorf("answer: generate: %w", err)
	}
	s.metrics.AnswerResult("ok")
	return AnswerResult{Answer: strings.TrimSpace(ans), Sources: res}, nil
}
