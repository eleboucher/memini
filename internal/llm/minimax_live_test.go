package llm_test

import (
	"context"
	"os"
	"testing"

	"github.com/eleboucher/memini/internal/llm"
)

// TestMiniMaxLive exercises the Anthropic backend against MiniMax. It is gated
// on MINIMAX_API_KEY so it never runs in CI; run locally with:
//
//	MINIMAX_API_KEY=... go test ./internal/llm -run MiniMaxLive -v
func TestMiniMaxLive(t *testing.T) {
	key := os.Getenv("MINIMAX_API_KEY")
	if key == "" {
		t.Skip("set MINIMAX_API_KEY to run the live MiniMax test")
	}
	c, err := llm.New(llm.APIAnthropic, llm.Config{
		BaseURL: "https://api.minimax.io/anthropic",
		APIKey:  key,
		Model:   "MiniMax-M2.7",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()

	got, err := c.Complete(ctx, "You answer with just the answer, one word.",
		"What city is the Eiffel Tower in?")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	t.Logf("Complete -> %q", got)
	if got == "" {
		t.Error("Complete returned empty (thinking-only response not handled?)")
	}

	dec, err := c.Consolidate(ctx, llm.Input{
		New:  "The user's favorite color is blue.",
		Tier: "semantic",
		Candidates: []llm.Candidate{
			{ID: "m1", Content: "The user's favorite color is red."},
		},
	})
	if err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	t.Logf("Consolidate -> action=%s target=%s content=%q reason=%q",
		dec.Action, dec.Target, dec.Content, dec.Reason)
}
