package llm_test

import (
	"encoding/json"
	"testing"

	"github.com/eleboucher/memini/internal/llm"
)

// TestFactImportanceParse pins that the self-assessed importance key is optional
// on the wire: a model that emits it is honoured, and one that omits it (every
// model predating the prompt change) parses to nil rather than a zero value that
// would read as "rated this worthless".
func TestFactImportanceParse(t *testing.T) {
	tests := []struct {
		name string
		body string
		want *float64
	}{
		{"present", `{"content":"c","importance":0.85}`, new(0.85)},
		{"absent", `{"content":"c"}`, nil},
		{"explicit null", `{"content":"c","importance":null}`, nil},
		{"zero", `{"content":"c","importance":0}`, new(0.0)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var f llm.Fact
			if err := json.Unmarshal([]byte(tt.body), &f); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			assertPtr(t, f.Importance, tt.want)
		})
	}
}

// TestDecisionImportanceParse is the same contract for the consolidator's
// verdict, whose importance rates the resulting memory.
func TestDecisionImportanceParse(t *testing.T) {
	tests := []struct {
		name string
		body string
		want *float64
	}{
		{"present", `{"action":"new","content":"c","importance":0.4}`, new(0.4)},
		{"absent", `{"action":"new","content":"c"}`, nil},
		{"explicit null", `{"action":"new","content":"c","importance":null}`, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var d llm.Decision
			if err := json.Unmarshal([]byte(tt.body), &d); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			assertPtr(t, d.Importance, tt.want)
		})
	}
}

func assertPtr(t *testing.T, got, want *float64) {
	t.Helper()
	switch {
	case want == nil && got != nil:
		t.Fatalf("importance = %v, want nil (an omitted key must not become a value)", *got)
	case want != nil && got == nil:
		t.Fatalf("importance = nil, want %v", *want)
	case want != nil && *got != *want:
		t.Fatalf("importance = %v, want %v", *got, *want)
	}
}
