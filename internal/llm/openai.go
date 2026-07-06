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
		option.WithHTTPClient(httpClientOr(cfg.HTTPClient)),
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

// ChatTools runs one round of a tool-calling conversation, translating the
// canonical tool/choice vocabulary to the /chat/completions encoding.
func (c *OpenAIClient) ChatTools(
	ctx context.Context, system string, turns []ChatTurn, tools []Tool, choice ToolChoice,
) (ChatResult, error) {
	messages := make([]openai.ChatCompletionMessageParamUnion, 0, len(turns)+1)
	messages = append(messages, openai.SystemMessage(system))
	for _, t := range turns {
		switch t.Role {
		case RoleAssistant:
			a := openai.ChatCompletionAssistantMessageParam{}
			if t.Text != "" {
				a.Content.OfString = openai.String(t.Text)
			}
			for _, call := range t.Calls {
				a.ToolCalls = append(a.ToolCalls, openai.ChatCompletionMessageToolCallUnionParam{
					OfFunction: &openai.ChatCompletionMessageFunctionToolCallParam{
						ID: call.ID,
						Function: openai.ChatCompletionMessageFunctionToolCallFunctionParam{
							Name: call.Name, Arguments: string(call.Args),
						},
					},
				})
			}
			messages = append(messages, openai.ChatCompletionMessageParamUnion{OfAssistant: &a})
		case RoleTool:
			messages = append(messages, openai.ToolMessage(t.Text, t.CallID))
		default:
			messages = append(messages, openai.UserMessage(t.Text))
		}
	}

	params := openai.ChatCompletionNewParams{
		Model:       c.model,
		Messages:    messages,
		Temperature: openai.Float(0),
		MaxTokens:   openai.Int(c.maxTokens),
	}
	if len(tools) > 0 && choice != ToolNone {
		for _, tool := range tools {
			params.Tools = append(params.Tools, openai.ChatCompletionFunctionTool(shared.FunctionDefinitionParam{
				Name:        tool.Name,
				Description: openai.String(tool.Description),
				Parameters:  shared.FunctionParameters(tool.Schema),
			}))
		}
		params.ToolChoice = openai.ChatCompletionToolChoiceOptionUnionParam{OfAuto: openai.String(string(choice))}
	}

	resp, err := c.client.Chat.Completions.New(ctx, params)
	if err != nil {
		return ChatResult{}, fmt.Errorf("llm: chat tools: %w", err)
	}
	if len(resp.Choices) == 0 {
		return ChatResult{}, errors.New("llm: empty response")
	}
	msg := resp.Choices[0].Message
	out := ChatResult{Text: strings.TrimSpace(msg.Content)}
	for _, call := range msg.ToolCalls {
		if call.Type != "" && call.Type != "function" {
			continue
		}
		out.Calls = append(out.Calls, ToolCall{
			ID: call.ID, Name: call.Function.Name, Args: json.RawMessage(call.Function.Arguments),
		})
	}
	if out.Text == "" && len(out.Calls) == 0 {
		return ChatResult{}, fmt.Errorf("llm: openai: %w (finish_reason %q)", errEmptyResponse, resp.Choices[0].FinishReason)
	}
	return out, nil
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
	out := strings.TrimSpace(resp.Choices[0].Message.Content)
	if out == "" {
		// Reasoning models can return a choice whose content is empty (the
		// budget was spent on hidden reasoning). Surface it distinctly so
		// callers log "no text" rather than a confusing JSON decode error.
		return "", fmt.Errorf("llm: openai: %w (finish_reason %q)", errEmptyResponse, resp.Choices[0].FinishReason)
	}
	return out, nil
}
