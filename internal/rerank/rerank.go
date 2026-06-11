// Package rerank holds the optional read-side rerank stage of recall: after
// hybrid retrieval and composite ranking, a reranker reads the query and the
// candidates together — something embeddings can't — and reorders them by how
// well each answers the query. Two implementations share the Reranker
// contract: a cross-encoder ranking model behind a /rerank endpoint
// (CrossEncoder) and a chat LLM prompted over a numbered list (NewLLM).
package rerank

import "context"

// Candidate is one memory offered to a reranker.
type Candidate struct {
	ID      string
	Content string
}

// Reranker reorders retrieved candidates by how well each answers a query.
type Reranker interface {
	// Rerank returns candidate IDs ordered most-relevant-first. Candidates the
	// backend omits are appended in their original order, so reranking can only
	// reorder, never drop, the input.
	Rerank(ctx context.Context, query string, candidates []Candidate) ([]string, error)
}

func idsOf(candidates []Candidate) []string {
	out := make([]string, len(candidates))
	for i, c := range candidates {
		out[i] = c.ID
	}
	return out
}
