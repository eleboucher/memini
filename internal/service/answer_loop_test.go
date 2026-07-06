package service_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/eleboucher/memini/internal/embed/embedtest"
	"github.com/eleboucher/memini/internal/llm"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/service"
)

// fakeToolChat scripts a tool loop: each round pops the next scripted result.
// It also implements Complete so WithAnswerer accepts it.
type fakeToolChat struct {
	script  []llm.ChatResult
	rounds  []roundSeen
	oneShot bool // Complete was called (single-shot path)
}

type roundSeen struct {
	choice    llm.ToolChoice
	toolNames []string
	turns     []llm.ChatTurn
}

func (f *fakeToolChat) Complete(context.Context, string, string) (string, error) {
	f.oneShot = true
	return "single-shot", nil
}

func (f *fakeToolChat) ChatTools(
	_ context.Context, _ string, turns []llm.ChatTurn, tools []llm.Tool, choice llm.ToolChoice,
) (llm.ChatResult, error) {
	names := make([]string, len(tools))
	for i, t := range tools {
		names[i] = t.Name
	}
	f.rounds = append(f.rounds, roundSeen{choice: choice, toolNames: names, turns: append([]llm.ChatTurn(nil), turns...)})
	if len(f.script) == 0 {
		return llm.ChatResult{Text: "out of script"}, nil
	}
	next := f.script[0]
	f.script = f.script[1:]
	return next, nil
}

func callOf(name, argsJSON string) llm.ToolCall {
	return llm.ToolCall{ID: "call-" + name, Name: name, Args: json.RawMessage(argsJSON)}
}

// TestAnswerAgenticLoop pins the happy path: the model searches once, the tool
// result carries real recalled content, and the second round's text is the
// answer with the searched memory in Sources.
func TestAnswerAgenticLoop(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	fake := &fakeToolChat{script: []llm.ChatResult{
		{Calls: []llm.ToolCall{callOf("search_memory", `{"query":"database choice"}`)}},
		{Text: "postgres"},
	}}
	svc := service.New(st, embedtest.New(dims), service.WithSyncReinforce(), service.WithAnswerer(fake))
	if _, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "alice", Content: "the team standardized on postgres for services", Tier: memory.TierSemantic,
	}); err != nil {
		t.Fatalf("remember: %v", err)
	}

	res, err := svc.Answer(ctx, service.AnswerInput{
		Namespace: "alice", Query: "which database?", Limit: 5, Reasoning: service.ReasoningLow,
	})
	if err != nil {
		t.Fatalf("answer: %v", err)
	}
	if res.Answer != "postgres" {
		t.Fatalf("answer = %q, want postgres", res.Answer)
	}
	if fake.oneShot {
		t.Fatal("agentic level must not take the single-shot path")
	}
	if len(res.Sources) == 0 {
		t.Fatal("sources should accumulate from prefetch/tool retrievals")
	}
	// Round 2's transcript must contain the tool result with the stored memory.
	last := fake.rounds[len(fake.rounds)-1]
	var toolTurn string
	for _, turn := range last.turns {
		if turn.Role == "tool" {
			toolTurn = turn.Text
		}
	}
	if !strings.Contains(toolTurn, "postgres for services") {
		t.Fatalf("tool result should carry the recalled memory, got %q", toolTurn)
	}
	if len(last.toolNames) != 3 {
		t.Fatalf("loop should expose 3 tools, got %v", last.toolNames)
	}
}

// TestAnswerAgenticForcedSynthesis pins the budget: a model that never stops
// calling tools gets exactly the level's iterations, then one forced
// text-only synthesis round (ToolNone, no tools).
func TestAnswerAgenticForcedSynthesis(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	fake := &fakeToolChat{script: []llm.ChatResult{
		{Calls: []llm.ToolCall{callOf("search_memory", `{"query":"a"}`)}},
		{Calls: []llm.ToolCall{callOf("keyword_search", `{"query":"b"}`)}},
		{Calls: []llm.ToolCall{callOf("recall_as_of", `{"query":"c","date":"2023-01-01"}`)}},
		{Text: "forced answer"},
	}}
	svc := service.New(st, embedtest.New(dims), service.WithSyncReinforce(), service.WithAnswerer(fake))

	res, err := svc.Answer(ctx, service.AnswerInput{
		Namespace: "alice", Query: "q", Reasoning: service.ReasoningLow, // budget 3
	})
	if err != nil {
		t.Fatalf("answer: %v", err)
	}
	if res.Answer != "forced answer" {
		t.Fatalf("answer = %q, want the forced synthesis text", res.Answer)
	}
	if len(fake.rounds) != 4 {
		t.Fatalf("rounds = %d, want 3 tool iterations + 1 synthesis", len(fake.rounds))
	}
	final := fake.rounds[3]
	if final.choice != llm.ToolNone || len(final.toolNames) != 0 {
		t.Fatalf("final round must force synthesis (ToolNone, no tools), got choice=%q tools=%v",
			final.choice, final.toolNames)
	}
}

// TestAnswerAgenticBadToolArgs pins that malformed or unknown calls come back
// as tool-visible errors instead of failing the answer.
func TestAnswerAgenticBadToolArgs(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	fake := &fakeToolChat{script: []llm.ChatResult{
		{Calls: []llm.ToolCall{
			callOf("recall_as_of", `{"query":"x","date":"not-a-date"}`),
			callOf("no_such_tool", `{}`),
		}},
		{Text: "done"},
	}}
	svc := service.New(st, embedtest.New(dims), service.WithSyncReinforce(), service.WithAnswerer(fake))

	if _, err := svc.Answer(ctx, service.AnswerInput{
		Namespace: "alice", Query: "q", Reasoning: service.ReasoningLow,
	}); err != nil {
		t.Fatalf("answer: %v", err)
	}
	last := fake.rounds[len(fake.rounds)-1]
	var errs []string
	for _, turn := range last.turns {
		if turn.Role == "tool" && strings.HasPrefix(turn.Text, "error:") {
			errs = append(errs, turn.Text)
		}
	}
	if len(errs) != 2 {
		t.Fatalf("both bad calls should yield tool-visible errors, got %v", errs)
	}
}

// TestAnswerReasoningFallsBackWithoutToolChat pins that a plain Completer
// still answers single-shot when a reasoning level is requested.
func TestAnswerReasoningFallsBackWithoutToolChat(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	ans := &fakeAnswerer{resp: "plain"}
	svc := service.New(st, embedtest.New(dims), service.WithSyncReinforce(), service.WithAnswerer(ans))
	res, err := svc.Answer(ctx, service.AnswerInput{Namespace: "alice", Query: "q", Reasoning: service.ReasoningHigh})
	if err != nil {
		t.Fatalf("answer: %v", err)
	}
	if res.Answer != "plain" {
		t.Fatalf("answer = %q, want the single-shot fallback", res.Answer)
	}
}
