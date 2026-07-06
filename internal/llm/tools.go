package llm

import (
	"context"
	"encoding/json"
)

// Tool describes one callable tool exposed to the model. Schema is the JSON
// Schema of the tool's arguments (type object, properties, required) — each
// backend translates it to its provider's tool encoding.
type Tool struct {
	Name        string
	Description string
	Schema      map[string]any
}

// ToolChoice is the canonical cross-provider tool-selection vocabulary,
// translated per backend (OpenAI none/auto/required; Anthropic none/auto/any).
type ToolChoice string

const (
	// ToolAuto lets the model choose between calling tools and answering.
	ToolAuto ToolChoice = "auto"
	// ToolNone forbids tool calls — the forced final-synthesis turn.
	ToolNone ToolChoice = "none"
	// ToolRequired forces at least one tool call.
	ToolRequired ToolChoice = "required"
)

// ToolCall is one tool invocation requested by the model. Args is the raw
// JSON arguments string; validate before use (models hallucinate fields).
type ToolCall struct {
	ID   string
	Name string
	Args json.RawMessage
}

// Chat turn roles.
const (
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

// ChatTurn is one entry of a tool-loop transcript.
type ChatTurn struct {
	// Role is one of RoleUser, RoleAssistant, RoleTool.
	Role string
	// Text is the user/assistant text, or the tool result content.
	Text string
	// Calls are the tool calls an assistant turn requested.
	Calls []ToolCall
	// CallID names the call a tool turn answers.
	CallID string
	// Name is the tool turn's tool name.
	Name string
}

// ChatResult is one round of a tool loop: final text, or tool calls to run.
type ChatResult struct {
	Text  string
	Calls []ToolCall
}

// ToolChat runs one round of a tool-calling conversation. Implemented by both
// backends; a caller holding a Completer can type-assert for loop support.
type ToolChat interface {
	ChatTools(ctx context.Context, system string, turns []ChatTurn, tools []Tool, choice ToolChoice) (ChatResult, error)
}

// schemaProps extracts the properties/required split some providers want from
// a full JSON-Schema object.
func schemaProps(schema map[string]any) (props any, required []string) {
	props = schema["properties"]
	if r, ok := schema["required"].([]string); ok {
		return props, r
	}
	if r, ok := schema["required"].([]any); ok {
		for _, v := range r {
			if s, ok := v.(string); ok {
				required = append(required, s)
			}
		}
	}
	return props, required
}
