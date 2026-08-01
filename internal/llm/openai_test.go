package llm_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/eleboucher/memini/internal/llm"
)

func TestConsolidateParsesDecision(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":
			"{\"action\":\"update\",\"target\":\"m1\",\"content\":\"merged\",\"summary\":\"s\",\"reason\":\"dup\"}"
		}}]}`))
	}))
	defer srv.Close()

	c, err := llm.NewOpenAI(llm.OpenAIConfig{BaseURL: srv.URL, Model: "m"})
	if err != nil {
		t.Fatalf("NewOpenAI: %v", err)
	}
	dec, err := c.Consolidate(context.Background(), llm.Input{New: "x"})
	if err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	if dec.Action != llm.ActionUpdate {
		t.Errorf("Action = %q, want update", dec.Action)
	}
	if dec.Target != "m1" || dec.Content != "merged" {
		t.Errorf("decision = %+v", dec)
	}
}

func TestConsolidateRejectsInvalidAction(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"action\":\"delete\"}"}}]}`))
	}))
	defer srv.Close()

	c, _ := llm.NewOpenAI(llm.OpenAIConfig{BaseURL: srv.URL, Model: "m"})
	if _, err := c.Consolidate(context.Background(), llm.Input{New: "x"}); err == nil {
		t.Fatal("expected invalid-action error, got nil")
	}
}

func TestCompleteReturnsText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"the answer"}}]}`))
	}))
	defer srv.Close()

	c, _ := llm.NewOpenAI(llm.OpenAIConfig{BaseURL: srv.URL, Model: "m"})
	got, err := c.Complete(context.Background(), "sys", "user")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got != "the answer" {
		t.Errorf("Complete = %q, want %q", got, "the answer")
	}
}

func TestCompleteRejectsEmptyContent(t *testing.T) {
	// A reasoning model can return a choice with empty content (budget spent on
	// hidden reasoning). That must be a clear error, not an empty success.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":""},"finish_reason":"length"}]}`))
	}))
	defer srv.Close()

	c, _ := llm.NewOpenAI(llm.OpenAIConfig{BaseURL: srv.URL, Model: "m"})
	_, err := c.Complete(context.Background(), "sys", "user")
	if err == nil {
		t.Fatal("expected an error for empty content, got nil")
	}
	if !strings.Contains(err.Error(), "no text") {
		t.Errorf("error should mention the empty response, got %v", err)
	}
}

func TestCompleteRetriesAfter429(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&hits, 1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"slow down"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()

	c, _ := llm.NewOpenAI(llm.OpenAIConfig{BaseURL: srv.URL, Model: "m"})
	got, err := c.Complete(context.Background(), "s", "u")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got != "ok" {
		t.Errorf("Complete = %q, want ok", got)
	}
	if atomic.LoadInt32(&hits) != 2 {
		t.Errorf("expected 2 requests (429 then 200), got %d", hits)
	}
}

// searchTool is a canonical tool definition with a nested schema, shared by the
// ChatTools encoding tests of both backends.
var searchTool = llm.Tool{
	Name:        "search_memory",
	Description: "Search stored memories.",
	Schema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{"type": "string"},
			"filters": map[string]any{
				"type":       "object",
				"properties": map[string]any{"tier": map[string]any{"type": "string"}},
			},
		},
		"required": []any{"query"},
	},
}

func TestOpenAIChatToolsEncodesRequest(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"done"}}]}`))
	}))
	defer srv.Close()

	// Args must survive as a RawMessage: key order and formatting untouched.
	rawArgs := `{"b":2,"a":"x"}`
	turns := []llm.ChatTurn{
		{Role: llm.RoleUser, Text: "find it"},
		{Role: llm.RoleAssistant, Text: "on it", Calls: []llm.ToolCall{
			{ID: "call_1", Name: "search_memory", Args: json.RawMessage(rawArgs)},
		}},
		{Role: llm.RoleTool, CallID: "call_1", Name: "search_memory", Text: "found 2"},
	}

	c, _ := llm.NewOpenAI(llm.OpenAIConfig{BaseURL: srv.URL, Model: "m"})
	if _, err := c.ChatTools(context.Background(), "sys", turns, []llm.Tool{searchTool}, llm.ToolRequired); err != nil {
		t.Fatalf("ChatTools: %v", err)
	}

	msgs, _ := gotBody["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("messages = %d, want 4 (system,user,assistant,tool): %v", len(msgs), gotBody["messages"])
	}
	for i, role := range []string{"system", "user", "assistant", "tool"} {
		if m, _ := msgs[i].(map[string]any); m["role"] != role {
			t.Errorf("messages[%d].role = %v, want %q", i, m["role"], role)
		}
	}

	// Assistant turn: text plus the tool call, arguments byte-identical.
	assistant, _ := msgs[2].(map[string]any)
	if assistant["content"] != "on it" {
		t.Errorf("assistant content = %v, want %q", assistant["content"], "on it")
	}
	calls, _ := assistant["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("assistant tool_calls = %v, want 1 call", assistant["tool_calls"])
	}
	call, _ := calls[0].(map[string]any)
	fn, _ := call["function"].(map[string]any)
	if call["id"] != "call_1" || call["type"] != "function" || fn["name"] != "search_memory" {
		t.Errorf("tool call = %v", call)
	}
	if fn["arguments"] != rawArgs {
		t.Errorf("arguments = %q, want verbatim %q", fn["arguments"], rawArgs)
	}

	// Tool turn: result text bound to the call it answers.
	toolMsg, _ := msgs[3].(map[string]any)
	if toolMsg["tool_call_id"] != "call_1" || toolMsg["content"] != "found 2" {
		t.Errorf("tool message = %v", toolMsg)
	}

	// Tool definition: full schema (with nested properties) passed through.
	tools, _ := gotBody["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %v, want 1", gotBody["tools"])
	}
	toolDef, _ := tools[0].(map[string]any)
	toolFn, _ := toolDef["function"].(map[string]any)
	if toolDef["type"] != "function" || toolFn["name"] != "search_memory" || toolFn["description"] != "Search stored memories." {
		t.Errorf("tool definition = %v", toolDef)
	}
	params, _ := toolFn["parameters"].(map[string]any)
	props, _ := params["properties"].(map[string]any)
	filters, _ := props["filters"].(map[string]any)
	nested, _ := filters["properties"].(map[string]any)
	tier, _ := nested["tier"].(map[string]any)
	if tier["type"] != "string" {
		t.Errorf("nested schema not passed through: %v", params)
	}
	if req, _ := params["required"].([]any); len(req) != 1 || req[0] != "query" {
		t.Errorf("required = %v, want [query]", params["required"])
	}

	if gotBody["tool_choice"] != "required" {
		t.Errorf("tool_choice = %v, want %q", gotBody["tool_choice"], "required")
	}
}

func TestOpenAIChatToolsToolChoiceMapping(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()

	c, _ := llm.NewOpenAI(llm.OpenAIConfig{BaseURL: srv.URL, Model: "m"})
	turns := []llm.ChatTurn{{Role: llm.RoleUser, Text: "hi"}}

	tests := []struct {
		choice llm.ToolChoice
		want   any // nil = tool_choice (and tools) must be absent
	}{
		{llm.ToolAuto, "auto"},
		{llm.ToolRequired, "required"},
		{llm.ToolNone, nil},
	}
	for _, tt := range tests {
		t.Run(string(tt.choice), func(t *testing.T) {
			gotBody = nil
			if _, err := c.ChatTools(context.Background(), "s", turns, []llm.Tool{searchTool}, tt.choice); err != nil {
				t.Fatalf("ChatTools: %v", err)
			}
			if got := gotBody["tool_choice"]; got != tt.want {
				t.Errorf("tool_choice = %v, want %v", got, tt.want)
			}
			if _, hasTools := gotBody["tools"]; hasTools != (tt.want != nil) {
				t.Errorf("tools present = %v, want %v (ToolNone must omit tools entirely)", hasTools, tt.want != nil)
			}
		})
	}
}

func TestOpenAIChatToolsParsesToolCalls(t *testing.T) {
	// Mixed text + tool calls; one call carries malformed argument JSON, which
	// must pass through verbatim (ToolCall.Args is validate-before-use), and a
	// non-function call type must be skipped.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":" let me look ","tool_calls":[
			{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"x\"}"}},
			{"id":"call_2","type":"custom","function":{"name":"skipped"}},
			{"id":"call_3","type":"function","function":{"name":"bad","arguments":"{not json"}}
		]},"finish_reason":"tool_calls"}]}`))
	}))
	defer srv.Close()

	c, _ := llm.NewOpenAI(llm.OpenAIConfig{BaseURL: srv.URL, Model: "m"})
	res, err := c.ChatTools(context.Background(), "s", []llm.ChatTurn{{Role: llm.RoleUser, Text: "go"}}, []llm.Tool{searchTool}, llm.ToolAuto)
	if err != nil {
		t.Fatalf("ChatTools: %v", err)
	}
	if res.Text != "let me look" {
		t.Errorf("Text = %q, want trimmed %q", res.Text, "let me look")
	}
	if len(res.Calls) != 2 {
		t.Fatalf("Calls = %+v, want 2 (the custom-type call is skipped)", res.Calls)
	}
	if res.Calls[0].ID != "call_1" || res.Calls[0].Name != "lookup" || string(res.Calls[0].Args) != `{"q":"x"}` {
		t.Errorf("Calls[0] = %+v", res.Calls[0])
	}
	if res.Calls[1].Name != "bad" || string(res.Calls[1].Args) != "{not json" {
		t.Errorf("Calls[1] = %+v, want malformed args passed through verbatim", res.Calls[1])
	}
}

func TestOpenAIChatToolsRejectsEmptyReply(t *testing.T) {
	// No text and no tool calls must be a clear error, not an empty success.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":""},"finish_reason":"length"}]}`))
	}))
	defer srv.Close()

	c, _ := llm.NewOpenAI(llm.OpenAIConfig{BaseURL: srv.URL, Model: "m"})
	_, err := c.ChatTools(context.Background(), "s", []llm.ChatTurn{{Role: llm.RoleUser, Text: "go"}}, nil, llm.ToolAuto)
	if err == nil {
		t.Fatal("expected an error for an empty reply, got nil")
	}
	if !strings.Contains(err.Error(), "no text") {
		t.Errorf("error should mention the empty response, got %v", err)
	}
}

func TestOpenAIExtraBodyMergedIntoRequest(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{}"}}]}`))
	}))
	defer srv.Close()

	c, err := llm.NewOpenAI(llm.Config{
		BaseURL: srv.URL, Model: "m",
		ExtraBody: map[string]json.RawMessage{
			"thinking": json.RawMessage(`{"type":"disabled"}`),
		},
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if _, err := c.Complete(context.Background(), "sys", "user"); err != nil {
		t.Fatalf("complete: %v", err)
	}
	th, ok := got["thinking"].(map[string]any)
	if !ok || th["type"] != "disabled" {
		t.Fatalf("thinking not merged into request body: %v", got["thinking"])
	}
	if got["model"] != "m" {
		t.Fatalf("client-set field lost: model=%v", got["model"])
	}
}
