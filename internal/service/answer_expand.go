package service

import (
	"context"
	"fmt"
	"regexp"
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

// expandQueries calls the LLM to rewrite a query into 2-3 lexically diverse
// variants, returning the original + rewrites. Returns just the original when
// no answerer is configured, the LLM call fails or times out, or the query is
// too short to benefit from expansion (< 3 words, quoted, or a UUID/error-code
// lookup).
//
// The call is bounded by recallRewriteTimeout so an enabled query_rewrite
// can't ride along the LLM client's much longer HTTP timeout (120s) and hold
// up recall; a timeout is just another Complete error and falls back to the
// original query alone.
func (s *Service) expandQueries(ctx context.Context, query string) []string {
	if s.answerer == nil || !shouldExpandQuery(query) {
		return []string{query}
	}
	cctx := ctx
	if s.recallRewriteTimeout > 0 {
		var cancel context.CancelFunc
		cctx, cancel = context.WithTimeout(ctx, s.recallRewriteTimeout)
		defer cancel()
	}
	rewrites, err := s.answerer.Complete(cctx, answerExpandSystem, "Question: "+query)
	if err != nil {
		return []string{query}
	}
	queries := []string{query}
	for line := range strings.SplitSeq(rewrites, "\n") {
		q := strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "-*0123456789. "))
		if q != "" && len(queries) < 4 {
			queries = append(queries, q)
		}
	}
	return queries
}

// shouldExpandQuery gates query expansion: skip queries too short to benefit,
// exact-match lookups (quoted strings), and UUID/error-code patterns.
func shouldExpandQuery(q string) bool {
	q = strings.TrimSpace(q)
	if q == "" {
		return false
	}
	words := strings.Fields(q)
	if len(words) < 3 {
		return false
	}
	if strings.Contains(q, "\"") || strings.Contains(q, "'") {
		return false
	}
	// UUIDs and error codes are precise lookups, not ambiguous queries.
	if isUUIDOrCode(q) {
		return false
	}
	return true
}

// isUUIDOrCode detects UUID-shaped strings and snake/kebab error codes.
func isUUIDOrCode(q string) bool {
	uuidPattern := regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	if uuidPattern.MatchString(q) {
		return true
	}
	// All-caps snake/kebab: ERR_AUTH_FAILED, ECONNREFUSED, panic-signal-12
	upper := strings.ToUpper(q)
	if upper == q && (strings.Contains(q, "_") || strings.Contains(q, "-")) && len(q) < 40 {
		return true
	}
	return false
}

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
			Namespace: in.Namespace, Home: in.Home, Query: q, Limit: in.Limit, Tiers: in.Tiers,
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
		"Today's date: "+s.now().Format("2006-01-02")+
			"\nMemories:\n"+formatAnswerContext(sources)+"\nQuestion: "+in.Query+"\nAnswer:")
	if err != nil {
		s.metrics.AnswerResult("error")
		return AnswerResult{}, fmt.Errorf("answer: expand synthesis: %w", err)
	}
	s.metrics.AnswerResult("ok")
	return AnswerResult{Answer: stripMemThinking(strings.TrimSpace(ans)), Sources: sources}, nil
}
