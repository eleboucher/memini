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
