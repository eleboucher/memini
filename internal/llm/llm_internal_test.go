package llm

import (
	"net/http"
	"testing"
)

func TestHTTPClientOr(t *testing.T) {
	// nil → a client with the default per-attempt timeout (so a hung provider
	// cannot park a goroutine forever).
	got := httpClientOr(nil)
	if got == nil {
		t.Fatal("httpClientOr(nil) = nil, want a default client")
	}
	if got.Timeout != defaultHTTPTimeout {
		t.Errorf("default client Timeout = %v, want %v", got.Timeout, defaultHTTPTimeout)
	}

	// A caller-supplied client is returned as-is (tests inject one).
	custom := &http.Client{}
	if httpClientOr(custom) != custom {
		t.Error("httpClientOr(custom) should return the provided client unchanged")
	}
}

func TestUnmarshalLoose(t *testing.T) {
	tests := []struct {
		name    string
		content string
		wantErr bool
		action  Action
	}{
		{name: "bare", content: `{"action":"new"}`, action: ActionNew},
		{name: "fenced", content: "```json\n{\"action\":\"update\",\"target\":\"m1\"}\n```", action: ActionUpdate},
		{name: "prose-wrapped", content: `Sure! Here is the decision: {"action":"supersede","target":"m2"} Hope that helps.`, action: ActionSupersede},
		{name: "brace in string", content: `The answer {"action":"new","content":"use {x} braces"} done`, action: ActionNew},
		{name: "escaped quote in string", content: `note: {"action":"new","content":"she said \"{\" loudly"} end`, action: ActionNew},
		{name: "truncated", content: `{"action":"new","content":"cut off`, wantErr: true},
		{name: "no json", content: `I could not produce a decision.`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, err := decodeDecision(tt.content)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("decodeDecision(%q): expected error, got %+v", tt.content, d)
				}
				return
			}
			if err != nil {
				t.Fatalf("decodeDecision(%q): %v", tt.content, err)
			}
			if d.Action != tt.action {
				t.Errorf("action = %q, want %q", d.Action, tt.action)
			}
		})
	}
}

func TestDecodeFactsProseWrapped(t *testing.T) {
	facts, err := decodeFacts(`Here you go:
{"facts":[{"content":"User prefers tabs","summary":"tabs","category":"preference"},{"content":"  "}]}
Let me know if you need more.`)
	if err != nil {
		t.Fatalf("decodeFacts: %v", err)
	}
	if len(facts) != 1 || facts[0].Content != "User prefers tabs" {
		t.Fatalf("facts = %+v, want the single non-empty fact", facts)
	}
}
