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
	"unicode/utf8"
)

const defaultTimeout = 60 * time.Second
const maxRerankBodyBytes = 8 << 20

// defaultTransport lifts MaxIdleConns above the stdlib default of 2/host
// so concurrent recalls reuse warm TCP. maxInFlight > 0 also aligns
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
	// MaxDocChars truncates each document before sending. 0 disables.
	MaxDocChars int
	// MaxBatchChars caps the total characters across query and documents in
	// a single /rerank request. Set just below the model's effective context
	// in characters (≈ n_ctx × chars-per-token × (1 − template reserve);
	// ~4000 for a 1024-token model). 0 disables proactive batching.
	MaxBatchChars int
	HTTPClient    *http.Client
}

// CrossEncoder reranks candidates with a dedicated ranking model served
// over the Cohere-style /rerank API (Infinity, vLLM, TEI, llama-server
// --rerank).
type CrossEncoder struct {
	url           string
	model         string
	apiKey        string
	maxDocChars   int
	maxBatchChars int
	client        *http.Client
}

func New(cfg Config) (*CrossEncoder, error) {
	if cfg.BaseURL == "" {
		return nil, fmt.Errorf("rerank: base url is required")
	}
	c := cfg.HTTPClient
	if c == nil {
		c = &http.Client{Timeout: defaultTimeout, Transport: defaultTransport(0)}
	}
	return &CrossEncoder{
		url:           strings.TrimRight(cfg.BaseURL, "/") + "/rerank",
		model:         cfg.Model,
		apiKey:        cfg.APIKey,
		maxDocChars:   cfg.MaxDocChars,
		maxBatchChars: cfg.MaxBatchChars,
		client:        c,
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

// Rerank scores every candidate against the query and returns candidate IDs
// most-relevant-first. Candidates the server omits are dropped. When the
// payload exceeds MaxBatchChars, candidates are split across multiple
// requests and merged by score.
func (c *CrossEncoder) Rerank(ctx context.Context, query string, candidates []Candidate) ([]string, error) {
	if len(candidates) <= 1 {
		return idsOf(candidates), nil
	}
	maxPerDoc := c.maxDocChars
	if c.maxBatchChars > 0 && (maxPerDoc == 0 || maxPerDoc > c.maxBatchChars) {
		maxPerDoc = c.maxBatchChars
	}
	docs := make([]string, len(candidates))
	for i, cand := range candidates {
		docs[i] = truncateRunes(cand.Content, maxPerDoc)
	}
	splits := c.splitBatches(docs, query, c.maxBatchChars)
	type scored struct {
		id    string
		score float64
	}
	var all []scored
	for _, span := range splits {
		batchCands := candidates[span.start:span.end]
		batchDocs := docs[span.start:span.end]
		scores, err := c.scoreBatch(ctx, query, batchCands, batchDocs)
		if err != nil {
			return nil, err
		}
		for id, sc := range scores {
			all = append(all, scored{id: id, score: sc})
		}
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].score != all[j].score {
			return all[i].score > all[j].score
		}
		return all[i].id < all[j].id
	})
	out := make([]string, 0, len(all))
	seen := make(map[string]struct{}, len(all))
	for _, s := range all {
		if _, dup := seen[s.id]; dup {
			continue
		}
		seen[s.id] = struct{}{}
		out = append(out, s.id)
	}
	return out, nil
}

type batchSpan struct{ start, end int }

func (c *CrossEncoder) splitBatches(docs []string, query string, charCap int) []batchSpan {
	if charCap <= 0 {
		return []batchSpan{{start: 0, end: len(docs)}}
	}
	envelopeOverhead := 38 + len(c.model)
	budget := charCap - utf8.RuneCountInString(query) - envelopeOverhead
	if budget <= 0 {
		maxQuery := charCap - envelopeOverhead
		if maxQuery > 0 {
			query = truncateRunes(query, maxQuery)
			budget = charCap - utf8.RuneCountInString(query) - envelopeOverhead
		}
		if budget <= 0 {
			return []batchSpan{{start: 0, end: len(docs)}}
		}
	}
	var out []batchSpan
	start := 0
	cur := 0
	for i, d := range docs {
		dc := utf8.RuneCountInString(d)
		if cur+dc > budget {
			out = append(out, batchSpan{start: start, end: i})
			start = i
			cur = 0
		}
		cur += dc
	}
	out = append(out, batchSpan{start: start, end: len(docs)})
	return out
}

func (c *CrossEncoder) scoreBatch(ctx context.Context, query string, candidates []Candidate, docs []string) (map[string]float64, error) {
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
	out := make(map[string]float64, len(rr.Results))
	for _, res := range rr.Results {
		if res.Index < 0 || res.Index >= len(candidates) {
			continue
		}
		out[candidates[res.Index].ID] = res.RelevanceScore
	}
	return out, nil
}

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
