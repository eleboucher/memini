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

// TestDecodeScores pins the importance-assessment contract: scores are
// positional, so anything but exactly one entry per input is an error rather
// than a partial result, and a null entry survives as "the model declined"
// instead of collapsing to a zero that would read as "rated this worthless".
func TestDecodeScores(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    int
		scores  []*float64
		wantErr bool
	}{
		{name: "bare", content: `{"scores":[0.8,0.2]}`, want: 2, scores: []*float64{new(0.8), new(0.2)}},
		{name: "fenced", content: "```json\n{\"scores\":[0.5]}\n```", want: 1, scores: []*float64{new(0.5)}},
		{
			name:    "prose-wrapped",
			content: `Sure! Here are the ratings: {"scores":[0.6,0.7]} Hope that helps.`,
			want:    2, scores: []*float64{new(0.6), new(0.7)},
		},
		{name: "declined entry", content: `{"scores":[null,0.5]}`, want: 2, scores: []*float64{nil, new(0.5)}},
		{name: "short", content: `{"scores":[0.5]}`, want: 2, wantErr: true},
		{name: "long", content: `{"scores":[0.5,0.6,0.7]}`, want: 2, wantErr: true},
		{name: "missing key", content: `{"ratings":[0.5]}`, want: 1, wantErr: true},
		{name: "no json", content: `I could not rate these memories.`, want: 1, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := decodeScores(tt.content, tt.want)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("decodeScores(%q, %d): expected error, got %v", tt.content, tt.want, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("decodeScores(%q, %d): %v", tt.content, tt.want, err)
			}
			if len(got) != len(tt.scores) {
				t.Fatalf("got %d scores, want %d", len(got), len(tt.scores))
			}
			for i := range got {
				switch {
				case tt.scores[i] == nil && got[i] != nil:
					t.Errorf("score[%d] = %v, want nil", i, *got[i])
				case tt.scores[i] != nil && got[i] == nil:
					t.Errorf("score[%d] = nil, want %v", i, *tt.scores[i])
				case tt.scores[i] != nil && *got[i] != *tt.scores[i]:
					t.Errorf("score[%d] = %v, want %v", i, *got[i], *tt.scores[i])
				}
			}
		})
	}
}
