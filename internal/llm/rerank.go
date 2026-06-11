package llm

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// RerankCandidate is one memory offered to the LLM reranker.
type RerankCandidate struct {
	ID      string
	Content string
}

// Reranker reorders retrieved candidates by how well each answers a query — the
// optional with-LLM read stage, reading query and candidates together as a
// cross-encoder substitute embeddings can't be.
type Reranker interface {
	// Rerank returns candidate IDs ordered most-relevant-first. Candidates the
	// model omits are appended in their original order, so reranking can only
	// reorder, never drop, the input.
	Rerank(ctx context.Context, query string, candidates []RerankCandidate) ([]string, error)
}

// llmReranker ranks via a single chat completion over a numbered candidate list.
type llmReranker struct {
	c        Completer
	maxChars int // per-candidate content cap, to bound prompt size
}

// NewReranker builds an LLM reranker over a chat backend. The per-candidate
// content cap keeps the prompt small — a deep candidate pool of long memories
// can otherwise blow a RAM-limited local server's context/activation budget.
func NewReranker(c Completer) Reranker { return &llmReranker{c: c, maxChars: 300} }

// rerankTimeout bounds a single rerank call so a stalled or restarting backend
// fails the call instead of hanging the run indefinitely.
const rerankTimeout = 120 * time.Second

const rerankSystem = "You re-rank candidate memories by how well each one helps answer the user's " +
	"question. Output ONLY candidate numbers, most relevant first, comma-separated " +
	"(e.g. \"3, 1, 7\"). Include only candidates that are genuinely relevant; omit the rest. " +
	"If none are relevant, output the single word none."

var rerankNumRe = regexp.MustCompile(`\d+`)

func (r *llmReranker) Rerank(ctx context.Context, query string, candidates []RerankCandidate) ([]string, error) {
	if len(candidates) <= 1 {
		return idsOf(candidates), nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Question: %s\n\nCandidates:\n", query)
	for i, c := range candidates {
		fmt.Fprintf(&b, "[%d] %s\n", i+1, truncate(c.Content, r.maxChars))
	}
	b.WriteString("\nMost relevant candidate numbers (comma-separated, most relevant first):")

	ctx, cancel := context.WithTimeout(ctx, rerankTimeout)
	defer cancel()
	out, err := r.c.Complete(ctx, rerankSystem, b.String())
	if err != nil {
		return nil, fmt.Errorf("llm: rerank: %w", err)
	}
	return applyRerankOrder(out, candidates), nil
}

// applyRerankOrder parses the model's number list into an ID ordering: ranked
// candidates first (in the model's order, de-duplicated, 1-based indices into
// candidates), then any unranked candidates in their original order. A reply of
// "none" or no parseable numbers leaves the original order unchanged.
func applyRerankOrder(out string, candidates []RerankCandidate) []string {
	ordered := make([]string, 0, len(candidates))
	seen := make([]bool, len(candidates))
	for _, tok := range rerankNumRe.FindAllString(out, -1) {
		n, err := strconv.Atoi(tok)
		if err != nil || n < 1 || n > len(candidates) || seen[n-1] {
			continue
		}
		seen[n-1] = true
		ordered = append(ordered, candidates[n-1].ID)
	}
	for i, c := range candidates {
		if !seen[i] {
			ordered = append(ordered, c.ID)
		}
	}
	return ordered
}

func idsOf(candidates []RerankCandidate) []string {
	out := make([]string, len(candidates))
	for i, c := range candidates {
		out[i] = c.ID
	}
	return out
}

// truncate caps s to max bytes on a rune boundary.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && s[cut]&0xC0 == 0x80 {
		cut--
	}
	return s[:cut] + "…"
}
