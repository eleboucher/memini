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
// It also implements Complete, which serves the agentic early-exit gate: gate
// is what the first single-shot pass replies (default INSUFFICIENT so loop
// tests enter the tool rounds).
type fakeToolChat struct {
	script    []llm.ChatResult
	rounds    []roundSeen
	gate      string
	completes int // Complete calls (gate / single-shot path)
}

type roundSeen struct {
	choice    llm.ToolChoice
	toolNames []string
	turns     []llm.ChatTurn
}

func (f *fakeToolChat) Complete(context.Context, string, string) (string, error) {
	f.completes++
	if f.gate == "" {
		return "INSUFFICIENT", nil
	}
	return f.gate, nil
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
	if fake.completes != 1 {
		t.Fatalf("gate should run exactly once before the loop, got %d Complete calls", fake.completes)
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

// TestAnswerAgenticEarlyExit pins the gate: when the first single-shot pass
// over the prefetched memories answers directly, the loop never opens — no
// tool rounds, sources are the prefetch only.
func TestAnswerAgenticEarlyExit(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	fake := &fakeToolChat{gate: "postgres"}
	svc := service.New(st, embedtest.New(dims), service.WithSyncReinforce(), service.WithAnswerer(fake))
	if _, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "alice", Content: "the team standardized on postgres for services", Tier: memory.TierSemantic,
	}); err != nil {
		t.Fatalf("remember: %v", err)
	}

	res, err := svc.Answer(ctx, service.AnswerInput{
		Namespace: "alice", Query: "which database?", Limit: 5, Reasoning: service.ReasoningMedium,
	})
	if err != nil {
		t.Fatalf("answer: %v", err)
	}
	if res.Answer != "postgres" {
		t.Fatalf("answer = %q, want the gate's direct answer", res.Answer)
	}
	if len(fake.rounds) != 0 {
		t.Fatalf("gate answered directly, loop must not open; got %d tool rounds", len(fake.rounds))
	}
	if len(res.Sources) == 0 {
		t.Fatal("early exit should still ground on the prefetched memories")
	}
}

// scriptedCompleter pops one scripted reply per Complete call and records the
// user prompts, for testing multi-completion strategies (expand).
type scriptedCompleter struct {
	script []string
	users  []string
}

func (s *scriptedCompleter) Complete(_ context.Context, _, user string) (string, error) {
	s.users = append(s.users, user)
	if len(s.script) == 0 {
		return "", nil
	}
	next := s.script[0]
	s.script = s.script[1:]
	return next, nil
}

// TestAnswerExpand pins the query-expansion strategy: one rewrite completion,
// recalls unioned across the original and rewritten queries (deduped), one
// synthesis completion grounded on the union.
func TestAnswerExpand(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	sc := &scriptedCompleter{script: []string{
		"1. database decision\n- which db was standardized\n",
		"postgres",
	}}
	svc := service.New(st, embedtest.New(dims), service.WithSyncReinforce(), service.WithAnswerer(sc))
	if _, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "alice", Content: "the team standardized on postgres for services", Tier: memory.TierSemantic,
	}); err != nil {
		t.Fatalf("remember: %v", err)
	}

	res, err := svc.Answer(ctx, service.AnswerInput{
		Namespace: "alice", Query: "which database?", Limit: 5, Reasoning: service.ReasoningExpand,
	})
	if err != nil {
		t.Fatalf("answer: %v", err)
	}
	if res.Answer != "postgres" {
		t.Fatalf("answer = %q, want postgres", res.Answer)
	}
	if len(sc.users) != 2 {
		t.Fatalf("expand should cost exactly 2 completions (rewrite + synthesis), got %d", len(sc.users))
	}
	if !strings.Contains(sc.users[0], "which database?") {
		t.Fatalf("rewrite prompt should carry the question, got %q", sc.users[0])
	}
	// Union dedups: the same memory recalled by all three queries appears once.
	if !strings.Contains(sc.users[1], "postgres for services") || strings.Count(sc.users[1], "postgres for services") != 1 {
		t.Fatalf("synthesis context should carry the memory exactly once, got %q", sc.users[1])
	}
	if len(res.Sources) != 1 {
		t.Fatalf("sources should be the deduped union, got %d", len(res.Sources))
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
