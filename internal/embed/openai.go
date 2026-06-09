package embed

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

// maxRetries bounds SDK retries on rate-limit (429) and 5xx responses.
const maxRetries = 6

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
	client openai.Client
	model  string
	dims   int
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
	opts := []option.RequestOption{
		option.WithBaseURL(strings.TrimRight(cfg.BaseURL, "/")),
		option.WithAPIKey(apiKeyOr(cfg.APIKey)),
		option.WithMaxRetries(maxRetries),
	}
	if cfg.HTTPClient != nil {
		opts = append(opts, option.WithHTTPClient(cfg.HTTPClient))
	}
	return &OpenAIClient{client: openai.NewClient(opts...), model: cfg.Model, dims: cfg.Dims}, nil
}

// Dims returns the configured embedding dimensionality.
func (c *OpenAIClient) Dims() int { return c.dims }

// Embed returns one vector per text, preserving input order.
func (c *OpenAIClient) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	resp, err := c.client.Embeddings.New(ctx, openai.EmbeddingNewParams{
		Model:          c.model,
		Input:          openai.EmbeddingNewParamsInputUnion{OfArrayOfStrings: texts},
		EncodingFormat: openai.EmbeddingNewParamsEncodingFormatFloat,
	})
	if err != nil {
		return nil, fmt.Errorf("embed: %w", err)
	}
	if len(resp.Data) != len(texts) {
		return nil, fmt.Errorf("embed: expected %d vectors, got %d", len(texts), len(resp.Data))
	}

	// Place by Index so order matches input even if the server reorders.
	vecs := make([][]float32, len(resp.Data))
	for _, d := range resp.Data {
		if int(d.Index) >= len(vecs) {
			return nil, fmt.Errorf("embed: vector index %d out of range", d.Index)
		}
		if len(d.Embedding) != c.dims {
			return nil, fmt.Errorf("embed: vector %d has %d dims, configured %d", d.Index, len(d.Embedding), c.dims)
		}
		v := make([]float32, len(d.Embedding))
		for j, f := range d.Embedding {
			v[j] = float32(f)
		}
		vecs[d.Index] = v
	}
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
