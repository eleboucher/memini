package embed

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

// maxRetries bounds SDK retries on rate-limit (429) and 5xx responses.
const maxRetries = 6

// defaultHTTPTimeout bounds a single embeddings HTTP attempt so a hung endpoint
// cannot park a recall/import goroutine indefinitely. Each SDK retry gets a
// fresh attempt under this bound; callers also pass a request context.
const defaultHTTPTimeout = 60 * time.Second

// OpenAIConfig configures the OpenAI-compatible embeddings client.
type OpenAIConfig struct {
	BaseURL string // e.g. http://localhost:8081/v1
	APIKey  string // optional bearer token
	Model   string
	Dims    int
	// HTTPClient is optional; the SDK default is used when nil.
	HTTPClient *http.Client
}

// OpenAIClient calls an OpenAI-compatible /embeddings endpoint.
type OpenAIClient struct {
	client  openai.Client
	model   string
	dims    int
	metrics Metrics
}

// NewOpenAI builds an embeddings client. BaseURL and Model are required.
func NewOpenAI(cfg OpenAIConfig) (*OpenAIClient, error) {
	if cfg.BaseURL == "" {
		return nil, errors.New("embed: BaseURL is required")
	}
	if cfg.Model == "" {
		return nil, errors.New("embed: Model is required")
	}
	if cfg.Dims <= 0 {
		return nil, errors.New("embed: Dims must be positive")
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultHTTPTimeout}
	}
	opts := []option.RequestOption{
		option.WithBaseURL(strings.TrimRight(cfg.BaseURL, "/")),
		option.WithAPIKey(apiKeyOr(cfg.APIKey)),
		option.WithMaxRetries(maxRetries),
		option.WithHTTPClient(httpClient),
	}
	return &OpenAIClient{client: openai.NewClient(opts...), model: cfg.Model, dims: cfg.Dims}, nil
}

// SetMetrics installs an observability sink on this client. Reports real
// token usage from the API's Usage block. A nil m disables instrumentation.
func (c *OpenAIClient) SetMetrics(m Metrics) {
	if m == nil {
		c.metrics = nopMetrics{}
		return
	}
	c.metrics = m
}

// Dims returns the configured embedding dimensionality.
func (c *OpenAIClient) Dims() int { return c.dims }

// Embed returns one vector per text, preserving input order.
func (c *OpenAIClient) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	m := c.metrics
	if m == nil {
		m = nopMetrics{}
	}
	start := time.Now()
	resp, err := c.client.Embeddings.New(ctx, openai.EmbeddingNewParams{
		Model:          c.model,
		Input:          openai.EmbeddingNewParamsInputUnion{OfArrayOfStrings: texts},
		EncodingFormat: openai.EmbeddingNewParamsEncodingFormatFloat,
	})
	if err != nil {
		m.Error("openai")
		return nil, fmt.Errorf("embed: %w", err)
	}
	if len(resp.Data) != len(texts) {
		m.Error("openai")
		return nil, fmt.Errorf("embed: expected %d vectors, got %d", len(texts), len(resp.Data))
	}

	// Place by Index so order matches input even if the server reorders.
	vecs := make([][]float32, len(resp.Data))
	for _, d := range resp.Data {
		if int(d.Index) >= len(vecs) {
			m.Error("openai")
			return nil, fmt.Errorf("embed: vector index %d out of range", d.Index)
		}
		if len(d.Embedding) != c.dims {
			m.Error("openai")
			return nil, fmt.Errorf("embed: vector %d has %d dims, configured %d", d.Index, len(d.Embedding), c.dims)
		}
		v := make([]float32, len(d.Embedding))
		for j, f := range d.Embedding {
			v[j] = float32(f)
		}
		vecs[d.Index] = v
	}
	tokens := 0
	if resp.Usage.TotalTokens > 0 {
		tokens = int(resp.Usage.TotalTokens)
	}
	m.Observe("openai", len(texts), tokens, time.Since(start))
	return vecs, nil
}

// apiKeyOr substitutes a placeholder so the SDK (which requires a key) works
// against local/compatible endpoints that ignore auth.
func apiKeyOr(k string) string {
	if k == "" {
		return "noauth"
	}
	return k
}
