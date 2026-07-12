package llm_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eleboucher/memini/internal/llm"
)

// anthropicReply is the minimal Messages response the SDK needs to parse.
const anthropicReply = `{"id":"msg_1","type":"message","role":"assistant","model":"m",
	"content":[{"type":"thinking","thinking":"reasoning..."},{"type":"text","text":%s}],
	"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`

func TestAnthropicCompleteSkipsThinkingAndCachesSystem(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/v1/messages") {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(strings.Replace(anthropicReply, "%s", `"the answer"`, 1)))
	}))
	defer srv.Close()

	c, err := llm.NewAnthropic(llm.Config{BaseURL: srv.URL, Model: "m"})
	if err != nil {
		t.Fatalf("NewAnthropic: %v", err)
	}
	got, err := c.Complete(context.Background(), "sys", "user")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got != "the answer" {
		t.Errorf("Complete = %q, want %q (thinking block must be skipped)", got, "the answer")
	}

	// The system prompt must be sent as a cached block.
	sys, ok := gotBody["system"].([]any)
	if !ok || len(sys) == 0 {
		t.Fatalf("system not sent as block array: %v", gotBody["system"])
	}
	block, _ := sys[0].(map[string]any)
	if _, ok := block["cache_control"]; !ok {
		t.Errorf("system block missing cache_control: %v", block)
	}
}

func TestAnthropicRejectsReasoningOnlyReply(t *testing.T) {
	// A reply with only a thinking block (no text) must surface a clear error,
	// not an empty string that fails downstream JSON decoding.
	const reasoningOnly = `{"id":"msg_1","type":"message","role":"assistant","model":"m",
		"content":[{"type":"thinking","thinking":"just reasoning, no answer"}],
		"stop_reason":"max_tokens","usage":{"input_tokens":1,"output_tokens":1}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(reasoningOnly))
	}))
	defer srv.Close()

	c, _ := llm.NewAnthropic(llm.Config{BaseURL: srv.URL, Model: "m"})
	_, err := c.Complete(context.Background(), "sys", "user")
	if err == nil {
		t.Fatal("expected an error for a reasoning-only reply, got nil")
	}
	if !strings.Contains(err.Error(), "no text") {
		t.Errorf("error should mention the empty response, got %v", err)
	}
}

func TestAnthropicConsolidateParsesFencedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Wrap the decision in a markdown fence to exercise fence-stripping.
		fenced, _ := json.Marshal("```json\n{\"action\":\"supersede\",\"target\":\"m2\",\"content\":\"new fact\"}\n```")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(strings.Replace(anthropicReply, "%s", string(fenced), 1)))
	}))
	defer srv.Close()

	c, _ := llm.NewAnthropic(llm.Config{BaseURL: srv.URL, Model: "m"})
	dec, err := c.Consolidate(context.Background(), llm.Input{New: "x"})
	if err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	if dec.Action != llm.ActionSupersede || dec.Target != "m2" || dec.Content != "new fact" {
		t.Errorf("decision = %+v", dec)
	}
}

// anthropicToolReply is a minimal Messages response ending in a tool_use block.
const anthropicToolReply = `{"id":"msg_1","type":"message","role":"assistant","model":"m",
	"content":[{"type":"tool_use","id":"tu_1","name":"search_memory","input":{"q":"x"}}],
	"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}`

func TestAnthropicChatToolsEncodesRequest(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(anthropicToolReply))
	}))
	defer srv.Close()

	// Args must survive as a RawMessage: key order and formatting untouched.
	rawArgs := `{"b":2,"a":"x"}`
	turns := []llm.ChatTurn{
		{Role: llm.RoleUser, Text: "find it"},
		{Role: llm.RoleAssistant, Text: "on it", Calls: []llm.ToolCall{
			{ID: "tu_1", Name: "search_memory", Args: json.RawMessage(rawArgs)},
		}},
		{Role: llm.RoleTool, CallID: "tu_1", Name: "search_memory", Text: "found 2"},
	}

	c, _ := llm.NewAnthropic(llm.Config{BaseURL: srv.URL, Model: "m"})
	if _, err := c.ChatTools(context.Background(), "sys", turns, []llm.Tool{searchTool}, llm.ToolRequired); err != nil {
		t.Fatalf("ChatTools: %v", err)
	}

	msgs, _ := gotBody["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages = %d, want 3 (user,assistant,user): %v", len(msgs), gotBody["messages"])
	}
	for i, role := range []string{"user", "assistant", "user"} {
		if m, _ := msgs[i].(map[string]any); m["role"] != role {
			t.Errorf("messages[%d].role = %v, want %q", i, m["role"], role)
		}
	}

	// Assistant turn: a text block followed by the tool_use block, with the
	// input JSON passed through verbatim.
	assistant, _ := msgs[1].(map[string]any)
	blocks, _ := assistant["content"].([]any)
	if len(blocks) != 2 {
		t.Fatalf("assistant content = %v, want [text, tool_use]", assistant["content"])
	}
	textBlock, _ := blocks[0].(map[string]any)
	if textBlock["type"] != "text" || textBlock["text"] != "on it" {
		t.Errorf("assistant text block = %v", textBlock)
	}
	toolUse, _ := blocks[1].(map[string]any)
	if toolUse["type"] != "tool_use" || toolUse["id"] != "tu_1" || toolUse["name"] != "search_memory" {
		t.Errorf("tool_use block = %v", toolUse)
	}
	input, _ := json.Marshal(toolUse["input"])
	var want, got map[string]any
	_ = json.Unmarshal([]byte(rawArgs), &want)
	_ = json.Unmarshal(input, &got)
	if got["a"] != want["a"] || got["b"] != want["b"] {
		t.Errorf("tool_use input = %s, want %s", input, rawArgs)
	}

	// Tool turn: a user message holding a tool_result block for the call.
	resultMsg, _ := msgs[2].(map[string]any)
	resultBlocks, _ := resultMsg["content"].([]any)
	if len(resultBlocks) != 1 {
		t.Fatalf("tool result message = %v, want 1 tool_result block", resultMsg)
	}
	result, _ := resultBlocks[0].(map[string]any)
	if result["type"] != "tool_result" || result["tool_use_id"] != "tu_1" {
		t.Errorf("tool_result block = %v", result)
	}

	// Tool definition: properties/required from the schema, nested intact.
	tools, _ := gotBody["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %v, want 1", gotBody["tools"])
	}
	toolDef, _ := tools[0].(map[string]any)
	if toolDef["name"] != "search_memory" || toolDef["description"] != "Search stored memories." {
		t.Errorf("tool definition = %v", toolDef)
	}
	schema, _ := toolDef["input_schema"].(map[string]any)
	if schema["type"] != "object" {
		t.Errorf("input_schema type = %v, want object", schema["type"])
	}
	props, _ := schema["properties"].(map[string]any)
	filters, _ := props["filters"].(map[string]any)
	nested, _ := filters["properties"].(map[string]any)
	tier, _ := nested["tier"].(map[string]any)
	if tier["type"] != "string" {
		t.Errorf("nested schema not passed through: %v", schema)
	}
	if req, _ := schema["required"].([]any); len(req) != 1 || req[0] != "query" {
		t.Errorf("required = %v, want [query]", schema["required"])
	}

	// ToolRequired maps to Anthropic's "any".
	choice, _ := gotBody["tool_choice"].(map[string]any)
	if choice["type"] != "any" {
		t.Errorf("tool_choice = %v, want type any", gotBody["tool_choice"])
	}
}

func TestAnthropicChatToolsCoalescesToolResults(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(anthropicToolReply))
	}))
	defer srv.Close()

	// Two consecutive tool results must land in ONE user message; a tool result
	// after an intervening turn starts a new one.
	turns := []llm.ChatTurn{
		{Role: llm.RoleAssistant, Calls: []llm.ToolCall{
			{ID: "tu_a", Name: "f", Args: json.RawMessage(`{}`)},
			{ID: "tu_b", Name: "g", Args: json.RawMessage(`{}`)},
		}},
		{Role: llm.RoleTool, CallID: "tu_a", Text: "ra"},
		{Role: llm.RoleTool, CallID: "tu_b", Text: "rb"},
		{Role: llm.RoleUser, Text: "and now?"},
		{Role: llm.RoleTool, CallID: "tu_c", Text: "rc"},
	}

	c, _ := llm.NewAnthropic(llm.Config{BaseURL: srv.URL, Model: "m"})
	if _, err := c.ChatTools(context.Background(), "sys", turns, []llm.Tool{searchTool}, llm.ToolAuto); err != nil {
		t.Fatalf("ChatTools: %v", err)
	}

	msgs, _ := gotBody["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("messages = %d, want 4 (assistant, coalesced results, user, results): %v", len(msgs), gotBody["messages"])
	}

	coalesced, _ := msgs[1].(map[string]any)
	blocks, _ := coalesced["content"].([]any)
	if coalesced["role"] != "user" || len(blocks) != 2 {
		t.Fatalf("coalesced message = %v, want one user message with 2 tool_result blocks", coalesced)
	}
	for i, wantID := range []string{"tu_a", "tu_b"} {
		block, _ := blocks[i].(map[string]any)
		if block["type"] != "tool_result" || block["tool_use_id"] != wantID {
			t.Errorf("coalesced block[%d] = %v, want tool_result for %s", i, block, wantID)
		}
	}

	last, _ := msgs[3].(map[string]any)
	lastBlocks, _ := last["content"].([]any)
	if len(lastBlocks) != 1 {
		t.Fatalf("last message = %v, want a single tool_result (not coalesced across the user turn)", last)
	}
	if block, _ := lastBlocks[0].(map[string]any); block["tool_use_id"] != "tu_c" {
		t.Errorf("last tool_result = %v, want tu_c", block)
	}
}

func TestAnthropicChatToolsToolChoiceMapping(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(anthropicToolReply))
	}))
	defer srv.Close()

	c, _ := llm.NewAnthropic(llm.Config{BaseURL: srv.URL, Model: "m"})
	turns := []llm.ChatTurn{{Role: llm.RoleUser, Text: "hi"}}

	tests := []struct {
		choice llm.ToolChoice
		want   string // "" = tool_choice (and tools) must be absent
	}{
		{llm.ToolAuto, "auto"},
		{llm.ToolRequired, "any"},
		{llm.ToolNone, ""},
	}
	for _, tt := range tests {
		t.Run(string(tt.choice), func(t *testing.T) {
			gotBody = nil
			if _, err := c.ChatTools(context.Background(), "s", turns, []llm.Tool{searchTool}, tt.choice); err != nil {
				t.Fatalf("ChatTools: %v", err)
			}
			choice, _ := gotBody["tool_choice"].(map[string]any)
			if got, _ := choice["type"].(string); got != tt.want {
				t.Errorf("tool_choice type = %q, want %q", got, tt.want)
			}
			if _, hasTools := gotBody["tools"]; hasTools != (tt.want != "") {
				t.Errorf("tools present = %v, want %v (ToolNone must omit tools entirely)", hasTools, tt.want != "")
			}
		})
	}
}

func TestAnthropicChatToolsParsesToolUse(t *testing.T) {
	// Mixed thinking + text + tool_use reply: thinking is skipped, text blocks
	// concatenate, and each tool_use round-trips ID/name/args.
	const reply = `{"id":"msg_1","type":"message","role":"assistant","model":"m",
		"content":[
			{"type":"thinking","thinking":"reasoning..."},
			{"type":"text","text":" checking "},
			{"type":"tool_use","id":"tu_1","name":"lookup","input":{"q":"x"}},
			{"type":"tool_use","id":"tu_2","name":"grep","input":{"pattern":"y","limit":3}}
		],
		"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(reply))
	}))
	defer srv.Close()

	c, _ := llm.NewAnthropic(llm.Config{BaseURL: srv.URL, Model: "m"})
	res, err := c.ChatTools(context.Background(), "s", []llm.ChatTurn{{Role: llm.RoleUser, Text: "go"}}, []llm.Tool{searchTool}, llm.ToolAuto)
	if err != nil {
		t.Fatalf("ChatTools: %v", err)
	}
	if res.Text != "checking" {
		t.Errorf("Text = %q, want trimmed %q", res.Text, "checking")
	}
	if len(res.Calls) != 2 {
		t.Fatalf("Calls = %+v, want 2", res.Calls)
	}
	if res.Calls[0].ID != "tu_1" || res.Calls[0].Name != "lookup" || string(res.Calls[0].Args) != `{"q":"x"}` {
		t.Errorf("Calls[0] = %+v", res.Calls[0])
	}
	if res.Calls[1].ID != "tu_2" || res.Calls[1].Name != "grep" || string(res.Calls[1].Args) != `{"pattern":"y","limit":3}` {
		t.Errorf("Calls[1] = %+v", res.Calls[1])
	}
}

func TestAnthropicChatToolsRejectsReasoningOnlyReply(t *testing.T) {
	// A reply with neither text nor tool_use (only thinking) must be a clear
	// error, not an empty success.
	const reasoningOnly = `{"id":"msg_1","type":"message","role":"assistant","model":"m",
		"content":[{"type":"thinking","thinking":"just reasoning"}],
		"stop_reason":"max_tokens","usage":{"input_tokens":1,"output_tokens":1}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(reasoningOnly))
	}))
	defer srv.Close()

	c, _ := llm.NewAnthropic(llm.Config{BaseURL: srv.URL, Model: "m"})
	_, err := c.ChatTools(context.Background(), "s", []llm.ChatTurn{{Role: llm.RoleUser, Text: "go"}}, nil, llm.ToolAuto)
	if err == nil {
		t.Fatal("expected an error for a reasoning-only reply, got nil")
	}
	if !strings.Contains(err.Error(), "no text") {
		t.Errorf("error should mention the empty response, got %v", err)
	}
}
