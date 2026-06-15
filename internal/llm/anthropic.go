package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// AnthropicClient is a Client backed by an Anthropic Messages endpoint. It
// works against the real Anthropic API and against Anthropic-compatible
// providers (e.g. MiniMax) via a BaseURL override. The static system prompt is
// marked for prompt caching, so repeated consolidation calls reuse it at the
// cache-read rate.
type AnthropicClient struct {
	client    anthropic.Client
	model     string
	maxTokens int64
}

// NewAnthropic builds an Anthropic Messages client. Model is required; BaseURL
// is optional (defaults to the Anthropic API, override it for compatible
// providers).
func NewAnthropic(cfg Config) (*AnthropicClient, error) {
	if cfg.Model == "" {
		return nil, errors.New("llm: Model is required")
	}
	opts := []option.RequestOption{
		option.WithAPIKey(apiKeyOr(cfg.APIKey)),
		option.WithMaxRetries(maxRetries),
		option.WithHTTPClient(httpClientOr(cfg.HTTPClient)),
	}
	if cfg.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(strings.TrimRight(cfg.BaseURL, "/")))
	}
	return &AnthropicClient{
		client:    anthropic.NewClient(opts...),
		model:     cfg.Model,
		maxTokens: maxTokensOr(cfg.MaxTokens),
	}, nil
}

// Consolidate asks the model how the new memory relates to the candidates.
func (c *AnthropicClient) Consolidate(ctx context.Context, in Input) (Decision, error) {
	payload, err := json.Marshal(in)
	if err != nil {
		return Decision{}, err
	}
	content, err := c.chat(ctx, systemPrompt, string(payload))
	if err != nil {
		return Decision{}, err
	}
	return decodeDecision(content)
}

// Distill compresses episodic memories into durable semantic facts.
func (c *AnthropicClient) Distill(ctx context.Context, in DistillInput) ([]Fact, error) {
	payload, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	content, err := c.chat(ctx, distillPrompt, string(payload))
	if err != nil {
		return nil, err
	}
	return decodeFacts(content)
}

// Complete is a single-turn message returning the concatenated text blocks.
func (c *AnthropicClient) Complete(ctx context.Context, system, user string) (string, error) {
	return c.chat(ctx, system, user)
}

// chat sends a Messages request with the system prompt marked for caching, and
// concatenates the text blocks of the reply (skipping reasoning blocks, which
// some models — e.g. MiniMax M2 — always emit).
func (c *AnthropicClient) chat(ctx context.Context, system, user string) (string, error) {
	msg, err := c.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:       c.model,
		MaxTokens:   c.maxTokens,
		Temperature: anthropic.Float(0),
		System: []anthropic.TextBlockParam{{
			Text:         system,
			CacheControl: anthropic.NewCacheControlEphemeralParam(),
		}},
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(user)),
		},
	})
	if err != nil {
		return "", fmt.Errorf("llm: anthropic: %w", err)
	}
	var b strings.Builder
	for _, block := range msg.Content {
		if block.Type == "text" {
			b.WriteString(block.Text)
		}
	}
	out := strings.TrimSpace(b.String())
	if out == "" {
		// A reasoning-only reply (e.g. MiniMax M2 emitting only a thinking
		// block) has no text. Surface that distinctly so callers log
		// "no text" rather than a confusing downstream JSON decode error.
		return "", fmt.Errorf("llm: anthropic: %w (stop_reason %q)", errEmptyResponse, msg.StopReason)
	}
	return out, nil
}
