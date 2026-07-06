package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/eleboucher/memini/internal/llm"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/store"
)

// ReasoningLevel selects the answer strategy: empty/minimal is the single-shot
// recall+complete path; expand is one query-rewrite completion plus a unioned
// multi-query recall and one synthesis (no tool loop); low/medium/high run a
// bounded tool loop where the model may search memory again before answering —
// the latency/cost dial for multi-hop and temporal questions.
type ReasoningLevel string

const (
	ReasoningMinimal ReasoningLevel = "minimal"
	ReasoningExpand  ReasoningLevel = "expand"
	ReasoningLow     ReasoningLevel = "low"
	ReasoningMedium  ReasoningLevel = "medium"
	ReasoningHigh    ReasoningLevel = "high"
)

// iterations maps a level to its tool-iteration budget (0 = single-shot).
func (r ReasoningLevel) iterations() int {
	switch r {
	case ReasoningLow:
		return 3
	case ReasoningMedium:
		return 6
	case ReasoningHigh:
		return 10
	default:
		return 0
	}
}

// answerGateSystem is the early-exit gate of the agentic loop: one single-shot
// read over the prefetched memories that either answers directly or declares
// them insufficient. The pilot bench showed the ungated loop regresses
// direct-answer questions by over-searching (rationale 88->75%), so the loop
// only opens when this first pass cannot settle the question.
const answerGateSystem = answerSystem +
	" Exception: you are the first pass of a deeper memory search, so instead of guessing or replying " +
	"\"I don't know\", reply with exactly INSUFFICIENT when the provided memories do not settle the " +
	"question: the specific fact asked for is missing, the question aggregates across time or sessions " +
	"(how many, list all, first/last), the memories conflict without a clear latest value, or the answer " +
	"depends on an update these memories may not include. If the memories do settle the question, answer " +
	"it directly."

// answerInsufficient is the gate's escape hatch sentinel.
const answerInsufficient = "INSUFFICIENT"

// answerLoopSystem extends the single-shot reader prompt with tool guidance.
const answerLoopSystem = answerSystem +
	" You have memory-search tools. Use them when the provided memories are insufficient: for questions " +
	"that aggregate across time or sessions (how many, list all, first/last), search several differently-" +
	"phrased queries and use keyword_search for the exact term being counted; for date questions, use " +
	"recall_as_of to see what was true at a specific date, and search for later updates ('changed', " +
	"'rescheduled', 'instead', 'moved') before trusting a dated fact. Stop searching as soon as the " +
	"memories answer the question, then reply with the final answer only."

// argQuery is the shared query argument name across the answer tools.
const argQuery = "query"

// objectSchema builds a JSON-Schema object from named properties and the
// required set, keeping the tool definitions declarative.
func objectSchema(props map[string]any, required ...string) map[string]any {
	return map[string]any{"type": "object", "properties": props, "required": required}
}

// strProp is a string property with a description (and optional enum).
func strProp(desc string, enum ...string) map[string]any {
	p := map[string]any{"type": "string", "description": desc}
	if len(enum) > 0 {
		p["enum"] = enum
	}
	return p
}

// answerTools is the read-only tool surface of the agentic answer loop.
var answerTools = []llm.Tool{
	{
		Name: "search_memory",
		Description: "Hybrid semantic+keyword search over stored memories, ranked and dated. " +
			"Phrase each call differently; repeating a failed query verbatim wastes an iteration.",
		Schema: objectSchema(map[string]any{
			argQuery: strProp("natural-language search query"),
			"tier":   strProp("episodic = raw conversation captures; durable = distilled facts; default all", "all", "episodic", "durable"),
		}, "query"),
	},
	{
		Name: "keyword_search",
		Description: "Exact lexical (BM25) search. Use for enumeration/counting questions and for rare " +
			"exact terms (names, codes, quoted phrases) semantic search misses.",
		Schema: objectSchema(map[string]any{
			argQuery: strProp("the exact terms to match"),
		}, "query"),
	},
	{
		Name: "recall_as_of",
		Description: "Time-travel search: returns the memories that were valid on a given date, including " +
			"facts later superseded. Use for 'what was true on/before X' and to reconstruct update history.",
		Schema: objectSchema(map[string]any{
			argQuery: strProp("natural-language search query"),
			"date":   strProp("YYYY-MM-DD"),
		}, "query", "date"),
	},
}

// answerAgentic runs the gated tool loop: prefetch context like the
// single-shot path, answer directly when that context already suffices
// (the early-exit gate), and otherwise let the model search up to iters
// rounds before forcing a final synthesis. Sources accumulate across every
// retrieval.
func (s *Service) answerAgentic(ctx context.Context, in AnswerInput, tc llm.ToolChat, iters int) (AnswerResult, error) {
	prefetch, err := s.Recall(ctx, RecallInput{
		Namespace: in.Namespace, Query: in.Query, Limit: in.Limit, Tiers: in.Tiers,
		Levels: in.Levels, Tags: in.Tags, Metadata: in.Metadata,
	})
	if err != nil {
		s.metrics.AnswerResult("error")
		return AnswerResult{}, err
	}
	seen := map[string]bool{}
	var sources []store.Scored
	collect := func(res []store.Scored) {
		for _, r := range res {
			if !seen[r.Memory.ID] {
				seen[r.Memory.ID] = true
				sources = append(sources, r)
			}
		}
	}
	collect(prefetch)

	// Early-exit gate: if the prefetched context already answers the question,
	// return that answer at single-shot cost instead of entering the loop.
	direct, err := s.answerer.Complete(ctx, answerGateSystem,
		"Memories:\n"+formatAnswerContext(prefetch)+"\nQuestion: "+in.Query+"\nAnswer:")
	if err != nil {
		s.metrics.AnswerResult("error")
		return AnswerResult{}, fmt.Errorf("answer: gate: %w", err)
	}
	if !strings.Contains(strings.ToUpper(direct), answerInsufficient) {
		s.metrics.AnswerResult("ok")
		return AnswerResult{Answer: strings.TrimSpace(direct), Sources: sources}, nil
	}

	turns := []llm.ChatTurn{{Role: "user", Text: "Memories:\n" + formatAnswerContext(prefetch) +
		"\nQuestion: " + in.Query + "\nA first read found these memories insufficient. Search for what is missing, then answer."}}
	for range iters {
		res, err := tc.ChatTools(ctx, answerLoopSystem, turns, answerTools, llm.ToolAuto)
		if err != nil {
			s.metrics.AnswerResult("error")
			return AnswerResult{}, fmt.Errorf("answer: agentic loop: %w", err)
		}
		if len(res.Calls) == 0 {
			s.metrics.AnswerResult("ok")
			return AnswerResult{Answer: strings.TrimSpace(res.Text), Sources: sources}, nil
		}
		turns = append(turns, llm.ChatTurn{Role: "assistant", Text: res.Text, Calls: res.Calls})
		for _, call := range res.Calls {
			turns = append(turns, llm.ChatTurn{
				Role: "tool", CallID: call.ID, Name: call.Name,
				Text: s.execAnswerTool(ctx, in, call, collect),
			})
		}
	}
	// Budget exhausted: force synthesis from what was found.
	turns = append(turns, llm.ChatTurn{Role: "user", Text: "Answer now from the memories you have found."})
	res, err := tc.ChatTools(ctx, answerLoopSystem, turns, nil, llm.ToolNone)
	if err != nil {
		s.metrics.AnswerResult("error")
		return AnswerResult{}, fmt.Errorf("answer: forced synthesis: %w", err)
	}
	s.metrics.AnswerResult("ok")
	return AnswerResult{Answer: strings.TrimSpace(res.Text), Sources: sources}, nil
}

// execAnswerTool runs one read-only tool call and formats its result for the
// transcript. Failures come back as tool-visible error text so the model can
// rephrase instead of the loop dying on a malformed argument.
func (s *Service) execAnswerTool(
	ctx context.Context, in AnswerInput, call llm.ToolCall, collect func([]store.Scored),
) string {
	var args struct {
		Query string `json:"query"`
		Tier  string `json:"tier"`
		Date  string `json:"date"`
	}
	if err := json.Unmarshal(call.Args, &args); err != nil {
		return "error: bad arguments: " + err.Error()
	}
	if strings.TrimSpace(args.Query) == "" {
		return "error: query is required"
	}
	k := in.Limit
	if k <= 0 {
		k = 5
	}

	var res []store.Scored
	var err error
	switch call.Name {
	case "search_memory":
		ri := RecallInput{Namespace: in.Namespace, Query: args.Query, Limit: k,
			Levels: in.Levels, Tags: in.Tags, Metadata: in.Metadata}
		switch args.Tier {
		case "episodic":
			ri.Tiers = []memory.Tier{memory.TierEpisodic}
		case "durable":
			ri.Tiers = []memory.Tier{memory.TierSemantic, memory.TierProcedural}
		default:
			ri.Tiers = in.Tiers
		}
		res, err = s.Recall(ctx, ri)
	case "keyword_search":
		res, err = s.store.KeywordSearch(ctx, in.Namespace, args.Query, store.Filter{Now: s.now()}, k)
	case "recall_as_of":
		asOf, perr := time.Parse("2006-01-02", strings.TrimSpace(args.Date))
		if perr != nil {
			return "error: date must be YYYY-MM-DD"
		}
		res, err = s.Recall(ctx, RecallInput{Namespace: in.Namespace, Query: args.Query, Limit: k,
			Levels: in.Levels, Tags: in.Tags, Metadata: in.Metadata, AsOf: asOf})
	default:
		return "error: unknown tool " + call.Name
	}
	if err != nil {
		return "error: " + err.Error()
	}
	if len(res) == 0 {
		return "no memories found"
	}
	collect(res)
	return formatAnswerContext(res)
}
