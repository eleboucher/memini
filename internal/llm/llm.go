// Package llm holds the opt-in consolidation pipeline: on each write it decides
// whether a new memory is novel, a refinement, or a contradiction that
// supersedes an existing one. Without an LLM the service stores raw.
//
// Two backends implement the same surface: an OpenAI-compatible chat client
// (openai-go) and an Anthropic Messages client (anthropic-sdk-go). The
// Anthropic backend caches the static system prompt, which makes high-write
// consolidation cheaper against providers like MiniMax that support prompt
// caching on the Anthropic-compatible API.
package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Action is the consolidation decision for a new memory.
type Action string

const (
	// ActionNew stores the new memory as a distinct record.
	ActionNew Action = "new"
	// ActionUpdate merges the new memory into an existing one (Target), which is
	// rewritten with Content; no new record is created.
	ActionUpdate Action = "update"
	// ActionSupersede stores the new memory and tombstones Target as superseded.
	ActionSupersede Action = "supersede"
)

// Candidate is an existing memory offered to the consolidator for comparison.
type Candidate struct {
	ID      string `json:"id"`
	Content string `json:"content"`
}

// Input is the consolidation request.
type Input struct {
	New        string      `json:"new"`
	Tier       string      `json:"tier"`
	Candidates []Candidate `json:"candidates"`
}

// Decision is the consolidator's verdict.
type Decision struct {
	Action Action `json:"action"`
	// Target is the candidate ID affected by update/supersede (empty for new).
	Target string `json:"target"`
	// Content is the merged text to persist for an update (else the new content).
	Content string `json:"content"`
	// Summary is an optional one-line summary of the resulting memory.
	Summary string `json:"summary"`
	// Reason explains the decision (for logs/debugging).
	Reason string `json:"reason"`
}

// Consolidator decides how a new memory relates to existing candidates.
type Consolidator interface {
	Consolidate(ctx context.Context, in Input) (Decision, error)
}

// Completer is a single-turn chat completion (used by the benchmark harness).
type Completer interface {
	Complete(ctx context.Context, system, user string) (string, error)
}

// Fact is a durable memory distilled from episodic observations.
type Fact struct {
	Content string `json:"content"`
	Summary string `json:"summary,omitempty"`
}

// DistillInput is a batch of episodic memory contents to distill.
type DistillInput struct {
	Episodes []string `json:"episodes"`
}

// Distiller compresses episodic memories into durable semantic facts. Used by
// the episodic→semantic promotion job.
type Distiller interface {
	Distill(ctx context.Context, in DistillInput) ([]Fact, error)
}

// Client is a chat backend that can consolidate memories, distill facts, and
// answer single-turn prompts.
type Client interface {
	Consolidator
	Completer
	Distiller
}

// Config configures a chat client. The same fields apply to both the
// OpenAI-compatible and Anthropic backends.
type Config struct {
	BaseURL string // OpenAI: e.g. https://host/v1 ; Anthropic: e.g. https://api.minimax.io/anthropic
	APIKey  string
	Model   string
	// MaxTokens caps the completion length (defaults to defaultMaxTokens). A
	// budget is required for reasoning models, which otherwise spend the server
	// default on hidden reasoning and return empty content.
	MaxTokens  int
	HTTPClient *http.Client
}

// OpenAIConfig and AnthropicConfig are aliases kept for call-site clarity.
type (
	OpenAIConfig    = Config
	AnthropicConfig = Config
)

// API selects the chat backend.
type API string

const (
	// APIOpenAI talks to an OpenAI-compatible /chat/completions endpoint.
	APIOpenAI API = "openai"
	// APIAnthropic talks to an Anthropic Messages endpoint (with prompt caching).
	APIAnthropic API = "anthropic"
)

// maxRetries bounds SDK retries on rate-limit (429) and 5xx responses.
const maxRetries = 6

// defaultHTTPTimeout bounds a single LLM HTTP attempt so a hung provider socket
// cannot park a goroutine indefinitely. Each SDK retry gets a fresh attempt
// under this bound; callers add their own per-job deadline on top.
const defaultHTTPTimeout = 120 * time.Second

// httpClientOr returns cfg.HTTPClient when set (tests inject one), else a client
// with the default per-attempt timeout.
func httpClientOr(c *http.Client) *http.Client {
	if c != nil {
		return c
	}
	return &http.Client{Timeout: defaultHTTPTimeout}
}

// defaultMaxTokens leaves headroom for JSON consolidation decisions, short QA
// answers, and reasoning-model traces.
const defaultMaxTokens = 4096

// New builds a chat client for the given API ("openai" default, or "anthropic").
func New(api API, cfg Config) (Client, error) {
	switch api {
	case APIAnthropic:
		return NewAnthropic(cfg)
	case APIOpenAI, "":
		return NewOpenAI(cfg)
	default:
		return nil, fmt.Errorf("llm: unknown api %q (want openai or anthropic)", api)
	}
}

const systemPrompt = `You maintain an AI agent's long-term memory. Given a NEW memory and a list of
EXISTING candidate memories, decide how the new one relates to them. Respond with a single JSON object:
{"action":"new|update|supersede","target":"<candidate id or empty>",
 "content":"<text to store>","summary":"<one line>","reason":"<short>"}
Rules:
- "new": the new memory is distinct from all candidates.
  Set content to the new memory text and target to empty.
- "update": the new memory duplicates or refines a candidate.
  Set target to that candidate's id and content to the merged, deduplicated text.
- "supersede": the new memory contradicts a candidate (it is now wrong/outdated).
  Set target to that candidate's id and content to the new memory text.
Prefer "new" unless there is a clear match. Output only the JSON object.`

// distillPrompt instructs the model to compress episodic memories into durable
// semantic facts, used by the promotion job.
const distillPrompt = `You compress an AI agent's episodic memories into durable, reusable knowledge.
Given a JSON object {"episodes":[...]} of past observations, extract only the DURABLE knowledge worth
keeping long-term: stable facts, decisions, conventions, preferences, and how-to knowledge. Discard
transient noise (one-off actions, timestamps, routine file edits with no lasting lesson). Merge
overlapping observations into single facts. Respond with a single JSON object:
{"facts":[{"content":"<durable fact>","summary":"<one line>"}]}
Return {"facts":[]} if nothing is durable. Output only the JSON object.`

// trimFence strips a leading/trailing markdown code fence some models wrap JSON
// in despite instructions, and surrounding whitespace.
func trimFence(content string) string {
	s := strings.TrimSpace(content)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	return strings.TrimSpace(s)
}

// decodeDecision parses and validates a consolidation decision, tolerating a
// markdown code fence around the JSON.
func decodeDecision(content string) (Decision, error) {
	var d Decision
	if err := json.Unmarshal([]byte(trimFence(content)), &d); err != nil {
		return Decision{}, fmt.Errorf("llm: decode decision JSON: %w", err)
	}
	if d.Action != ActionNew && d.Action != ActionUpdate && d.Action != ActionSupersede {
		return Decision{}, fmt.Errorf("llm: invalid action %q", d.Action)
	}
	return d, nil
}

// decodeFacts parses a distillation response into durable facts, tolerating a
// markdown code fence and dropping any with empty content.
func decodeFacts(content string) ([]Fact, error) {
	var out struct {
		Facts []Fact `json:"facts"`
	}
	if err := json.Unmarshal([]byte(trimFence(content)), &out); err != nil {
		return nil, fmt.Errorf("llm: decode facts JSON: %w", err)
	}
	kept := out.Facts[:0]
	for _, f := range out.Facts {
		if strings.TrimSpace(f.Content) != "" {
			kept = append(kept, f)
		}
	}
	return kept, nil
}

// maxTokensOr applies the default budget when unset.
func maxTokensOr(n int) int64 {
	if n <= 0 {
		return defaultMaxTokens
	}
	return int64(n)
}

// apiKeyOr substitutes a placeholder so the SDKs (which require a key) work
// against local/compatible endpoints that ignore auth.
func apiKeyOr(k string) string {
	if k == "" {
		return "noauth"
	}
	return k
}
