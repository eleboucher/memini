package rerank

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Completer is the single-turn chat completion an LLM reranker needs. The
// chat clients in internal/llm satisfy it; declaring it here keeps this
// package free of a dependency on the chat backends.
type Completer interface {
	Complete(ctx context.Context, system, user string) (string, error)
}

// llmReranker ranks via a single chat completion over a numbered candidate list.
type llmReranker struct {
	c        Completer
	maxChars int // per-candidate content cap, to bound prompt size
}

// NewLLM builds a reranker over a chat backend. The per-candidate content cap
// keeps the prompt small — a deep candidate pool of long memories can
// otherwise blow a RAM-limited local server's context/activation budget.
func NewLLM(c Completer) Reranker { return &llmReranker{c: c, maxChars: 300} }

const llmSystem = "You re-rank candidate memories by how well each one helps answer the user's " +
	"question. Output ONLY candidate numbers, most relevant first, comma-separated " +
	"(e.g. \"3, 1, 7\"). Include only candidates that are genuinely relevant; omit the rest. " +
	"If none are relevant, output the single word none."

var llmNumRe = regexp.MustCompile(`\d+`)

func (r *llmReranker) Rerank(ctx context.Context, query string, candidates []Candidate) ([]string, error) {
	if len(candidates) <= 1 {
		return idsOf(candidates), nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Question: %s\n\nCandidates:\n", query)
	for i, c := range candidates {
		fmt.Fprintf(&b, "[%d] %s\n", i+1, truncate(c.Content, r.maxChars))
	}
	b.WriteString("\nMost relevant candidate numbers (comma-separated, most relevant first):")

	// The deadline is owned by the caller (the service wraps Rerank in
	// MEMINI_RERANK_TIMEOUT, default 10s) and backstopped by the chat client's
	// per-attempt HTTP timeout; this reranker does not impose its own.
	out, err := r.c.Complete(ctx, llmSystem, b.String())
	if err != nil {
		return nil, fmt.Errorf("rerank: llm: %w", err)
	}
	return applyOrder(out, candidates), nil
}

// applyOrder parses the model's number list into an ID ordering: ranked
// candidates first (in the model's order, de-duplicated, 1-based indices into
// candidates), then any unranked candidates in their original order. A reply of
// "none" or no parseable numbers leaves the original order unchanged.
func applyOrder(out string, candidates []Candidate) []string {
	ordered := make([]string, 0, len(candidates))
	seen := make([]bool, len(candidates))
	for _, tok := range llmNumRe.FindAllString(out, -1) {
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
