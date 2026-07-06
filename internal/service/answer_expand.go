package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/eleboucher/memini/internal/store"
)

// answerExpandSystem asks for lexically diverse rewrites of the question — the
// single-call form of the agentic loop's multi-round searching. One rewrite
// pass plus one synthesis pass captures the "search several phrasings" behavior
// at a fixed two-completion cost, with no tool loop.
const answerExpandSystem = "You rewrite a question into memory-search queries. Reply with 2 or 3 short " +
	"search queries, one per line, with no numbering and no commentary. Make them lexically diverse: use " +
	"synonyms and different angles (the entity, the value, the event), keep an exact keyword or name from " +
	"the question in at least one, and when the question concerns something that changes over time add a " +
	"query for later updates (e.g. append 'changed', 'default', 'now')."

// answerExpand is the cheap query-expansion strategy: one completion rewrites
// the question into 2-3 differently-phrased search queries, the recalls for the
// original and rewritten queries are unioned (first occurrence wins), and one
// synthesis completion answers over the merged context. Needs only a Completer.
func (s *Service) answerExpand(ctx context.Context, in AnswerInput) (AnswerResult, error) {
	rewrites, err := s.answerer.Complete(ctx, answerExpandSystem, "Question: "+in.Query)
	if err != nil {
		s.metrics.AnswerResult("error")
		return AnswerResult{}, fmt.Errorf("answer: expand: %w", err)
	}

	queries := []string{in.Query}
	for line := range strings.SplitSeq(rewrites, "\n") {
		q := strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "-*0123456789. "))
		if q != "" && len(queries) < 4 {
			queries = append(queries, q)
		}
	}

	seen := map[string]bool{}
	var sources []store.Scored
	for _, q := range queries {
		res, err := s.Recall(ctx, RecallInput{
			Namespace: in.Namespace, Query: q, Limit: in.Limit, Tiers: in.Tiers,
			Levels: in.Levels, Tags: in.Tags, Metadata: in.Metadata,
		})
		if err != nil {
			s.metrics.AnswerResult("error")
			return AnswerResult{}, err
		}
		for _, r := range res {
			if !seen[r.Memory.ID] {
				seen[r.Memory.ID] = true
				sources = append(sources, r)
			}
		}
	}

	ans, err := s.answerer.Complete(ctx, answerSystem,
		"Memories:\n"+formatAnswerContext(sources)+"\nQuestion: "+in.Query+"\nAnswer:")
	if err != nil {
		s.metrics.AnswerResult("error")
		return AnswerResult{}, fmt.Errorf("answer: expand synthesis: %w", err)
	}
	s.metrics.AnswerResult("ok")
	return AnswerResult{Answer: strings.TrimSpace(ans), Sources: sources}, nil
}
