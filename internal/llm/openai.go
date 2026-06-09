package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"
)

// OpenAIClient is a Client backed by an OpenAI-compatible /chat/completions
// endpoint.
type OpenAIClient struct {
	client    openai.Client
	model     string
	maxTokens int64
}

// NewOpenAI builds a chat client. BaseURL and Model are required.
func NewOpenAI(cfg Config) (*OpenAIClient, error) {
	if cfg.BaseURL == "" {
		return nil, errors.New("llm: BaseURL is required")
	}
	if cfg.Model == "" {
		return nil, errors.New("llm: Model is required")
	}
	opts := []option.RequestOption{
		option.WithBaseURL(strings.TrimRight(cfg.BaseURL, "/")),
		option.WithAPIKey(apiKeyOr(cfg.APIKey)),
		option.WithMaxRetries(maxRetries),
	}
	if cfg.HTTPClient != nil {
		opts = append(opts, option.WithHTTPClient(cfg.HTTPClient))
	}
	return &OpenAIClient{
		client:    openai.NewClient(opts...),
		model:     cfg.Model,
		maxTokens: maxTokensOr(cfg.MaxTokens),
	}, nil
}

// Consolidate asks the model how the new memory relates to the candidates.
func (c *OpenAIClient) Consolidate(ctx context.Context, in Input) (Decision, error) {
	payload, err := json.Marshal(in)
	if err != nil {
		return Decision{}, err
	}
	content, err := c.chat(ctx, systemPrompt, string(payload), true)
	if err != nil {
		return Decision{}, err
	}
	return decodeDecision(content)
}

// Distill compresses episodic memories into durable semantic facts.
func (c *OpenAIClient) Distill(ctx context.Context, in DistillInput) ([]Fact, error) {
	payload, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	content, err := c.chat(ctx, distillPrompt, string(payload), true)
	if err != nil {
		return nil, err
	}
	return decodeFacts(content)
}

// Complete is a single-turn chat completion returning the assistant message text.
func (c *OpenAIClient) Complete(ctx context.Context, system, user string) (string, error) {
	return c.chat(ctx, system, user, false)
}

func (c *OpenAIClient) chat(ctx context.Context, system, user string, jsonMode bool) (string, error) {
	params := openai.ChatCompletionNewParams{
		Model: c.model,
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.SystemMessage(system),
			openai.UserMessage(user),
		},
		Temperature: openai.Float(0),
		MaxTokens:   openai.Int(c.maxTokens),
	}
	if jsonMode {
		params.ResponseFormat = openai.ChatCompletionNewParamsResponseFormatUnion{
			OfJSONObject: &shared.ResponseFormatJSONObjectParam{},
		}
	}
	resp, err := c.client.Chat.Completions.New(ctx, params)
	if err != nil {
		return "", fmt.Errorf("llm: chat: %w", err)
	}
	if len(resp.Choices) == 0 {
		return "", errors.New("llm: empty response")
	}
	return resp.Choices[0].Message.Content, nil
}
