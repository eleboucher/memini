package rerank

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

const defaultTimeout = 60 * time.Second

// maxRerankBodyBytes caps the response body read from the reranker so a
// misbehaving or hostile endpoint cannot exhaust memory by streaming an
// arbitrarily large body. The decoded result is bounded by len(candidates),
// so a legitimate response is far smaller than this.
const maxRerankBodyBytes = 8 << 20 // 8 MiB

// defaultTransport lifts the idle-conn cap above the stdlib default of 2/host
// so concurrent recalls reuse warm TCP. maxInFlight, when > 0, also aligns
// MaxIdleConnsPerHost to the rerank.Limited cap.
func defaultTransport(maxInFlight int) *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConns = 100
	t.MaxIdleConnsPerHost = 100
	if maxInFlight > 0 && maxInFlight < t.MaxIdleConnsPerHost {
		t.MaxIdleConnsPerHost = maxInFlight
	}
	return t
}

// Config configures the cross-encoder reranker client.
type Config struct {
	// BaseURL is the API root (e.g. http://host:8002/v1); "/rerank" is appended.
	BaseURL string
	Model   string
	APIKey  string
	// MaxDocChars truncates each document before sending so an oversized
	// candidate can't blow the server's physical batch and fail the whole
	// request. 0 disables truncation.
	MaxDocChars int
	HTTPClient  *http.Client
}

// CrossEncoder reranks candidates with a dedicated ranking model (bge-reranker,
// Qwen3-Reranker, mxbai-rerank, …) served over the Cohere-style /rerank API
// that Infinity, vLLM, TEI, and llama-server --rerank expose — a cheaper
// alternative to the LLM reranker.
type CrossEncoder struct {
	url         string
	model       string
	apiKey      string
	maxDocChars int
	client      *http.Client
}

// New builds a cross-encoder reranker client. BaseURL is required.
func New(cfg Config) (*CrossEncoder, error) {
	if cfg.BaseURL == "" {
		return nil, fmt.Errorf("rerank: base url is required")
	}
	c := cfg.HTTPClient
	if c == nil {
		c = &http.Client{Timeout: defaultTimeout, Transport: defaultTransport(0)}
	}
	return &CrossEncoder{
		url:         strings.TrimRight(cfg.BaseURL, "/") + "/rerank",
		model:       cfg.Model,
		apiKey:      cfg.APIKey,
		maxDocChars: cfg.MaxDocChars,
		client:      c,
	}, nil
}

type rerankRequest struct {
	Model     string   `json:"model,omitempty"`
	Query     string   `json:"query"`
	Documents []string `json:"documents"`
}

type rerankResponse struct {
	Results []struct {
		Index          int     `json:"index"`
		RelevanceScore float64 `json:"relevance_score"`
	} `json:"results"`
}

// Rerank scores every candidate against the query with the ranking model and
// returns the candidate IDs most-relevant-first. Candidates the server omits are
// appended in their original order, satisfying the reorder-only contract of
// Reranker.
func (c *CrossEncoder) Rerank(ctx context.Context, query string, candidates []Candidate) ([]string, error) {
	if len(candidates) <= 1 {
		return idsOf(candidates), nil
	}
	docs := make([]string, len(candidates))
	for i, cand := range candidates {
		docs[i] = truncateRunes(cand.Content, c.maxDocChars)
	}
	body, err := json.Marshal(rerankRequest{Model: c.model, Query: query, Documents: docs})
	if err != nil {
		return nil, fmt.Errorf("rerank: marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("rerank: request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("rerank: status %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	var rr rerankResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxRerankBodyBytes)).Decode(&rr); err != nil {
		return nil, fmt.Errorf("rerank: decode response: %w", err)
	}
	if len(rr.Results) == 0 {
		return nil, fmt.Errorf("rerank: empty results for %d documents", len(docs))
	}
	// Some servers return results unsorted; order by relevance descending.
	sort.SliceStable(rr.Results, func(i, j int) bool {
		return rr.Results[i].RelevanceScore > rr.Results[j].RelevanceScore
	})
	ordered := make([]string, 0, len(candidates))
	seen := make([]bool, len(candidates))
	for _, res := range rr.Results {
		if res.Index < 0 || res.Index >= len(candidates) || seen[res.Index] {
			continue
		}
		seen[res.Index] = true
		ordered = append(ordered, candidates[res.Index].ID)
	}
	for i, cand := range candidates {
		if !seen[i] {
			ordered = append(ordered, cand.ID)
		}
	}
	return ordered, nil
}

// truncateRunes caps s at max runes (not bytes, to avoid splitting UTF-8).
// max <= 0 means no limit.
func truncateRunes(s string, max int) string {
	if max <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}
