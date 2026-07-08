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
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// errEmptyResponse is returned when a chat backend yields no usable text (e.g.
// a reasoning-only reply, or a choice with empty content). Callers can log it
// distinctly instead of surfacing a downstream JSON-decode failure.
var errEmptyResponse = errors.New("response contained no text")

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
	// LinkedIDs are IDs of existing memories related to this one (same
	// entity/topic) but neither duplicate nor contradiction. Empty for "new"
	// with no related candidates or update/supersede actions.
	LinkedIDs []string `json:"linked_ids,omitempty"`
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
	// Category routes the fact to a tier: "procedure" (incl. error→recovery) →
	// procedural; "preference" and "fact" → semantic. Empty defaults to semantic.
	Category string `json:"category,omitempty"`
	// Confidence is the LLM's self-assessed reliability of this fact, in [0.1, 0.7].
	// nil means unset; the service layer falls back to ConfidenceSeedFresh.
	Confidence *float64 `json:"confidence,omitempty"`
}

// Episode is one episodic memory to distill, paired with the date it was
// recorded so the model can resolve relative dates in the text ("yesterday",
// "last week") to absolute ones.
type Episode struct {
	Content string `json:"content"`
	// Date is the YYYY-MM-DD the episode was recorded. Empty when unknown.
	Date string `json:"date,omitempty"`
}

// DistillInput is a batch of episodic memories to distill. Now is the current
// date (YYYY-MM-DD); the model grounds relative dates against each episode's
// Date, falling back to Now. Both empty disables grounding.
type DistillInput struct {
	Episodes []Episode `json:"episodes"`
	Now      string    `json:"now,omitempty"`
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
 "content":"<text to store>","summary":"<one line>","reason":"<short>",
 "linked_ids":["<id>",...]}

A DUPLICATE restates the same fact in different words (same value, same subject, same meaning).
A CONTRADICTION makes the old fact wrong or outdated: the SAME property now has a DIFFERENT value,
the state changed, or an error was corrected. When two memories assign different values to the same
property (different number, different name, different provider, different setting), the newer one
CONTRADICTS the older one — they cannot both be true.
Two facts about DIFFERENT properties or DIFFERENT events of the same entity are DISTINCT (new).

Rules:
- "new": the new memory is about a different property, event, or topic than all candidates.
  Set content to the new memory text and target to empty.
- "update": the new memory DUPLICATES a candidate (same fact, same value, reworded or refined).
  Set target to that candidate's id and content to the merged, deduplicated text.
- "supersede": the new memory CONTRADICTS a candidate — same property, different value, or corrected.
  Set target to that candidate's id and content to the new memory text.
- "linked_ids": for "new" actions, include the IDs of any candidate about the same entity/topic
  but that is a DISTINCT fact (not duplicate, not contradiction). Skip candidates that are
  a different entity/topic entirely. For "update" and "supersede", leave empty.

Test: would the NEW memory make the EXISTING candidate FALSE if both were stored? If yes → supersede.
Can both be true simultaneously? If yes → update (same fact) or new (different fact).

Examples:
- EXISTING "Cache entries expire after a 10 minute TTL" / NEW "Cached items live for ten minutes"
  → update (same value 10 min, reworded)
- EXISTING "Cache entries expire after a 10 minute TTL" / NEW "Cache entries expire after a 30 minute TTL"
  → supersede (same property TTL, different value 10→30)
- EXISTING "The reranker is served on port 8002" / NEW "The reranker is served on port 9002"
  → supersede (same property port, different value)
- EXISTING "Email is sent through Postmark" / NEW "Email is sent through SES"
  → supersede (same property email provider, different value)
- EXISTING "The frontend is built with React and Vite" / NEW "The frontend is built with Svelte and Vite"
  → supersede (same property frontend framework, different value)
- EXISTING "Bob ran 5 miles on Tuesday" / NEW "Bob ran 3 miles on Wednesday"
  → new (different events on different days, both can be true)
- EXISTING "The cache is sharded across four nodes" / NEW "Cache entries expire after a 30 minute TTL"
  → new (different properties of the same system, both can be true)

Output only the JSON object.`

// distillPrompt instructs the model to compress episodic memories into durable
// semantic facts, used by the promotion job.
const distillPrompt = `You compress an AI agent's episodic memories into durable, reusable knowledge.
Input is a JSON object {"now":"YYYY-MM-DD","episodes":[{"content":"...","date":"YYYY-MM-DD"}]} of past
observations, each with the date it was recorded; "now" is today. Extract only the DURABLE knowledge
worth keeping long-term and classify each item with a "category":
- "preference": a user preference or correction ("don't use X", "always prefer Y", "stop doing Z").
- "procedure": reusable how-to knowledge, including error→recovery ("when X fails, do Y instead").
- "fact": a stable fact, decision, or convention that is neither of the above.
Episodes prefixed with "[failed]" were captured from a failed turn or command; pair one with a later
success to form an error→recovery "procedure".
	Discard transient noise (one-off actions, routine file edits with no lasting lesson). Split compound
facts into separate items: "User's name is John and works at Google" becomes two items ("User's name is
John" and "User works at Google"). Each item must be a single atomic fact.
Each item must be self-contained and readable without the episodes: name the subject explicitly (no bare
"he/she/it/this"), and keep the context that makes it actionable ("prefers pnpm for the frontend repo",
not "prefers pnpm").
When an episode states a relative time (e.g. "yesterday", "last week", "two days ago"), resolve it to an
absolute YYYY-MM-DD date in the item, grounding against that episode's "date" (or "now" if it has none).
	Leave already-absolute dates unchanged.
Assign each fact a confidence score (0.0 to 1.0) reflecting how certain you are it is a durable, accurate
observation: 0.9+ for explicit user statements ("I prefer X"), 0.6-0.8 for inferred preferences, 0.3-0.5 for
speculative or second-hand facts. Clamp to [0.1, 0.7] — no fact starts above 0.7; corroboration raises it later.
Respond with a single JSON object:
{"facts":[{"content":"<durable item>","summary":"<one line>","category":"preference|procedure|fact","confidence":0.0_to_1.0}]}
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

// extractJSON returns the first balanced top-level JSON object embedded in s,
// for models that wrap the JSON in prose despite instructions. String-aware so
// braces inside string values don't unbalance the scan; "" when no complete
// object exists.
func extractJSON(s string) string {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return ""
	}
	depth, inStr, esc := 0, false, false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return ""
}

// unmarshalLoose parses JSON from an LLM reply: bare, fenced, or embedded
// mid-prose. A direct parse of the fence-trimmed text wins; otherwise the
// first balanced JSON object is tried before reporting the original error.
func unmarshalLoose(content string, v any) error {
	trimmed := trimFence(content)
	err := json.Unmarshal([]byte(trimmed), v)
	if err == nil {
		return nil
	}
	if obj := extractJSON(trimmed); obj != "" {
		if json.Unmarshal([]byte(obj), v) == nil {
			return nil
		}
	}
	return err
}

// decodeDecision parses and validates a consolidation decision, tolerating a
// markdown code fence or surrounding prose around the JSON.
func decodeDecision(content string) (Decision, error) {
	var d Decision
	if err := unmarshalLoose(content, &d); err != nil {
		return Decision{}, fmt.Errorf("llm: decode decision JSON: %w", err)
	}
	if d.Action != ActionNew && d.Action != ActionUpdate && d.Action != ActionSupersede {
		return Decision{}, fmt.Errorf("llm: invalid action %q", d.Action)
	}
	return d, nil
}

// decodeFacts parses a distillation response into durable facts, tolerating a
// markdown code fence or surrounding prose and dropping any with empty content.
func decodeFacts(content string) ([]Fact, error) {
	var out struct {
		Facts []Fact `json:"facts"`
	}
	if err := unmarshalLoose(content, &out); err != nil {
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
