// Package mcp exposes memini over the Model Context Protocol. It is a thin
// adapter over the same service.Service the REST API uses, served over stdio
// and Streamable HTTP.
package mcp

import (
	"context"
	"crypto/subtle"
	"fmt"
	"net/http"
	"strings"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/eleboucher/memini/internal/httputil"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/service"
	"github.com/eleboucher/memini/internal/store"
	"github.com/eleboucher/memini/internal/version"
)

// boolPtr returns a pointer to a bool value.
func boolPtr(b bool) *bool { return &b }

// Annotation sets for tool discovery and auto-approval by MCP clients.
var (
	readOnly = &mcpsdk.ToolAnnotations{
		ReadOnlyHint:  true,
		OpenWorldHint: boolPtr(false),
	}
	additive = &mcpsdk.ToolAnnotations{
		DestructiveHint: boolPtr(false),
		IdempotentHint:  true,
		OpenWorldHint:   boolPtr(false),
	}
	destructive = &mcpsdk.ToolAnnotations{
		DestructiveHint: boolPtr(true),
		IdempotentHint:  true,
		OpenWorldHint:   boolPtr(false),
	}
)

// NewServer builds an MCP server exposing memini's memory tools. defaultNS is
// used whenever a tool call omits the namespace argument.
func NewServer(svc *service.Service, defaultNS string) *mcpsdk.Server {
	s := mcpsdk.NewServer(&mcpsdk.Implementation{
		Name:    "memini",
		Version: version.Version,
	}, nil)

	h := &tools{svc: svc, defaultNS: defaultNS}

	mcpsdk.AddTool(s, &mcpsdk.Tool{
		Name:  "memory_remember",
		Title: "Remember a fact",
		Description: "Store a fact, decision, preference, or event for later recall. Call " +
			"proactively when the user says 'remember this', after an architectural decision " +
			"(capture the why), after discovering a non-obvious bug or convention, or when a " +
			"stated preference should outlive this session. Keep memories atomic — one " +
			"self-contained fact per call; search works better on small records. Do NOT store " +
			"facts already in project docs/CLAUDE.md or trivially recoverable from code. tier: " +
			"semantic=durable fact, procedural=how-to, episodic=event, working=scratch (omit to " +
			"auto-classify). If the result carries merge_hint, the content nearly duplicates an " +
			"existing memory — either call memory_remember with id (or memory_update, if " +
			"available) on merge_hint.similar_id to fold them together, or ignore it to keep " +
			"both. Returns {id, tier, stored}; stored=false means a low-signal write was dropped " +
			"by the value gate (not an error).",
		Annotations: additive,
	}, h.remember)
	mcpsdk.AddTool(s, &mcpsdk.Tool{
		Name:  "memory_recall",
		Title: "Recall memories",
		Description: "Search prior context via hybrid (semantic + keyword) retrieval, ranked by " +
			"relevance, recency, and corroboration. Call BEFORE starting work that may have " +
			"history: editing an unfamiliar file, debugging a recurring issue, making a " +
			"non-obvious decision, or when asked 'what do we know about X'. Prefer a short " +
			"descriptive query ('JWT auth setup'). Results include created_at — on conflicting " +
			"memories prefer the most recent and surface the conflict. Empty results mean " +
			"nothing is known: proceed from first principles, never invent a remembered fact. A " +
			"degraded field means semantic search was unavailable and results are keyword-only — " +
			"treat as incomplete. Supports time-travel (as_of), nested namespaces (scope=subtree), " +
			"and query_rewrite (LLM expands the query into variants, fused via RRF — better " +
			"recall, slower). When the current session's turns are being captured as memories, " +
			"pass exclude_metadata {\"session_id\": \"<current session id>\"} so the session's " +
			"own captured turns are not echoed back as memory.",
		Annotations: readOnly,
	}, h.recall)
	mcpsdk.AddTool(s, &mcpsdk.Tool{
		Name:  "memory_briefing",
		Title: "Session briefing",
		Description: "Layered session-start briefing for this namespace — pinned context, " +
			"durable facts, how-to procedures, and recent activity — in one query-less call. " +
			"Call it when a session opens to orient yourself. Prefer this over broad recall " +
			"queries at session start.",
		Annotations: readOnly,
	}, h.briefing)
	mcpsdk.AddTool(s, &mcpsdk.Tool{
		Name:  "memory_answer",
		Title: "Answer from memory",
		Description: "Recall relevant memories and answer a question grounded on them (requires " +
			"an LLM). Slower than memory_recall; use when you want a synthesized answer with " +
			"sources rather than raw memories.",
		Annotations: readOnly,
	}, h.answer)
	mcpsdk.AddTool(s, &mcpsdk.Tool{
		Name:  "memory_list",
		Title: "Browse memories",
		Description: "Browse memories without a query — filter by tier, tags, or metadata " +
			"(e.g. all procedural memories, or everything tagged/categorized X). Returns at " +
			"most limit (default 20) newest-first; page with offset. For relevance-ranked " +
			"search use memory_recall.",
		Annotations: readOnly,
	}, h.list)
	mcpsdk.AddTool(s, &mcpsdk.Tool{
		Name:  "memory_get",
		Title: "Get a memory",
		Description: "Fetch one memory with full metadata, tags, and timestamps by ID (ids come " +
			"from memory_recall / memory_list results).",
		Annotations: readOnly,
	}, h.get)
	mcpsdk.AddTool(s, &mcpsdk.Tool{
		Name:  "memory_forget",
		Title: "Forget a memory",
		Description: "Permanently delete a memory by ID — use for wrong, outdated, or unwanted " +
			"memories. To correct a fact instead, prefer memory_remember with id (or " +
			"memory_update, if available) so history is preserved.",
		Annotations: destructive,
	}, h.forget)

	return s
}

// HTTPHandler returns an http.Handler serving MCP over Streamable HTTP. The
// tenant namespace is taken from nsHeader when present, else defaultNS; tool
// calls may still override it per-call. An invalid header value is rejected
// with 400 (matching the REST API) rather than silently falling back to the
// default tenant. When apiKey is non-empty, requests must present it as a
// bearer token — required for any remote (non-localhost) deployment.
func HTTPHandler(svc *service.Service, nsHeader, defaultNS, apiKey string) http.Handler {
	h := mcpsdk.NewStreamableHTTPHandler(func(r *http.Request) *mcpsdk.Server {
		ns := defaultNS
		if v := strings.TrimSpace(r.Header.Get(nsHeader)); v != "" {
			ns = v
		}
		return NewServer(svc, ns)
	}, nil)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if apiKey != "" {
			token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if subtle.ConstantTimeCompare([]byte(token), []byte(apiKey)) != 1 {
				http.Error(w, `{"error":"missing or invalid bearer token"}`, http.StatusUnauthorized)
				return
			}
		}
		if v := strings.TrimSpace(r.Header.Get(nsHeader)); v != "" {
			if err := httputil.ValidateNamespace(v); err != nil {
				http.Error(w, `{"error":"invalid namespace header"}`, http.StatusBadRequest)
				return
			}
		}
		h.ServeHTTP(w, r)
	})
}

// RunStdio serves the MCP server over stdio, blocking until ctx is cancelled or
// the client disconnects. Used by `memini mcp` for local agent integrations.
func RunStdio(ctx context.Context, svc *service.Service, defaultNS string) error {
	return NewServer(svc, defaultNS).Run(ctx, &mcpsdk.StdioTransport{})
}

// tools holds the MCP tool handlers.
type tools struct {
	svc       *service.Service
	defaultNS string
}

// parseTiers validates a tier filter. An unknown tier is an error rather than
// silently unfiltered results, matching the REST surface.
func parseTiers(in []string) ([]memory.Tier, error) {
	tiers := make([]memory.Tier, 0, len(in))
	for _, v := range in {
		t := memory.Tier(strings.TrimSpace(v))
		if !t.Valid() {
			return nil, fmt.Errorf("invalid tier %q", t)
		}
		tiers = append(tiers, t)
	}
	return tiers, nil
}

// parseLevels validates a level filter. An unknown level is an error rather than
// silently unfiltered results, matching the REST surface.
func parseLevels(in []string) ([]memory.Level, error) {
	levels := make([]memory.Level, 0, len(in))
	for _, v := range in {
		l := memory.Level(strings.TrimSpace(v))
		if !l.Valid() {
			return nil, fmt.Errorf("invalid level %q", l)
		}
		levels = append(levels, l)
	}
	return levels, nil
}

// parseOptionalTime parses an optional RFC3339 timestamp, returning nil for an
// empty string. field names the argument for error messages.
func parseOptionalTime(s, field string) (*time.Time, error) {
	if s == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil, fmt.Errorf("invalid %s %q: want RFC3339", field, s)
	}
	u := t.UTC()
	return &u, nil
}

// ns resolves a tool call's namespace argument: empty falls back to the server
// default, an invalid value is an error (never silently rerouted to the
// default tenant, which would mix data across namespaces).
func (t *tools) ns(arg string) (string, error) {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return t.defaultNS, nil
	}
	if err := httputil.ValidateNamespace(arg); err != nil {
		return "", fmt.Errorf("invalid namespace: %w", err)
	}
	return arg, nil
}

type rememberArgs struct {
	Content    string         `json:"content" jsonschema:"the text to remember"`
	Tier       string         `json:"tier,omitempty" jsonschema:"working, episodic, semantic, or procedural (omit to auto-classify)"`
	Level      string         `json:"level,omitempty" jsonschema:"explicit (user-stated) or deduced (LLM-distilled); omit to leave unset"`
	Summary    string         `json:"summary,omitempty" jsonschema:"optional one-line summary"`
	Tags       []string       `json:"tags,omitempty" jsonschema:"optional labels"`
	Metadata   map[string]any `json:"metadata,omitempty" jsonschema:"optional structured metadata"`
	Importance float64        `json:"importance,omitempty" jsonschema:"0..1 bias toward retention"`
	TTLSeconds *int           `json:"ttl_seconds,omitempty" jsonschema:"overrides the tier default TTL; negative means never expire"`
	ID         string         `json:"id,omitempty" jsonschema:"upserts an existing memory when provided"`
	Confidence *float64       `json:"confidence,omitempty" jsonschema:"0..1 seed corroboration for a durable fact; omit for default"`
	ValidFrom  string         `json:"valid_from,omitempty" jsonschema:"RFC3339 start of the fact's validity; backdate for as_of recall"`
	ValidTo    string         `json:"valid_to,omitempty" jsonschema:"RFC3339 end of the fact's validity; omit if still true"`
	Namespace  string         `json:"namespace,omitempty" jsonschema:"tenant namespace; defaults to the server namespace"`
}

type rememberResult struct {
	ID   string `json:"id"`
	Tier string `json:"tier"`
	// Stored is false when the episodic value gate dropped the write (low signal).
	Stored bool `json:"stored"`
	// AutoSuperseded is true when the write's near-duplicate crossed the
	// auto-supersede gate and the older memory was tombstoned in the background.
	AutoSuperseded bool `json:"auto_superseded,omitempty"`
	// MergeHint points at a near-duplicate the caller may want to merge into
	// (via memory_update) when the write landed in the merge-hint band. nil
	// otherwise.
	MergeHint *mergeHintResult `json:"merge_hint,omitempty"`
}

// mergeHintResult mirrors service.MergeHint for the MCP wire shape.
type mergeHintResult struct {
	SimilarID      string  `json:"similar_id"`
	SimilarContent string  `json:"similar_content"`
	Score          float64 `json:"score"`
	Tier           string  `json:"tier"`
}

func (t *tools) remember(ctx context.Context, _ *mcpsdk.CallToolRequest, in rememberArgs) (*mcpsdk.CallToolResult, rememberResult, error) {
	ns, err := t.ns(in.Namespace)
	if err != nil {
		return nil, rememberResult{}, err
	}
	input := service.RememberInput{
		Namespace:  ns,
		Content:    in.Content,
		Tier:       memory.Tier(in.Tier),
		Level:      memory.Level(in.Level),
		Summary:    in.Summary,
		Tags:       in.Tags,
		Metadata:   in.Metadata,
		Importance: in.Importance,
		ID:         in.ID,
		Confidence: in.Confidence,
	}
	if in.TTLSeconds != nil {
		d := time.Duration(*in.TTLSeconds) * time.Second
		input.TTL = &d
	}
	if input.ValidFrom, err = parseOptionalTime(in.ValidFrom, "valid_from"); err != nil {
		return nil, rememberResult{}, err
	}
	if input.ValidTo, err = parseOptionalTime(in.ValidTo, "valid_to"); err != nil {
		return nil, rememberResult{}, err
	}
	var hint service.MergeHint
	var superseded bool
	input.MergeHint = &hint
	input.AutoSuperseded = &superseded
	m, err := t.svc.Remember(ctx, input)
	if err != nil {
		return nil, rememberResult{}, err
	}
	if m == nil { // episodic value gate dropped the write
		return nil, rememberResult{Tier: string(input.Tier), Stored: false}, nil
	}
	res := rememberResult{ID: m.ID, Tier: string(m.Tier), Stored: true, AutoSuperseded: superseded}
	if hint.SimilarID != "" {
		res.MergeHint = &mergeHintResult{
			SimilarID:      hint.SimilarID,
			SimilarContent: hint.SimilarContent,
			Score:          hint.Score,
			Tier:           string(hint.Tier),
		}
	}
	return nil, res, nil
}

type recallArgs struct {
	Query             string            `json:"query" jsonschema:"what to search for"`
	Tiers             []string          `json:"tiers,omitempty" jsonschema:"restrict to tiers; empty means all"`
	Levels            []string          `json:"levels,omitempty" jsonschema:"restrict to levels (explicit/deduced); empty means all"`
	Tags              []string          `json:"tags,omitempty" jsonschema:"only memories carrying every listed tag (AND)"`
	Metadata          map[string]string `json:"metadata,omitempty" jsonschema:"only memories whose metadata has each key=value pair (AND)"`
	ExcludeMetadata   map[string]string `json:"exclude_metadata,omitempty" jsonschema:"inverse of metadata; drops matching memories"`
	IncludeFreshTurns bool              `json:"include_fresh_turns,omitempty" jsonschema:"keep just-captured turns the echo guard would drop"`
	QueryRewrite      bool              `json:"query_rewrite,omitempty" jsonschema:"rewrite query into 2-3 variants and fuse via RRF"`
	Limit             int               `json:"limit,omitempty" jsonschema:"max results (default 10)"`
	Scope             string            `json:"scope,omitempty" jsonschema:"'subtree' also searches nested namespaces; default 'exact'"`
	AsOf              string            `json:"as_of,omitempty" jsonschema:"RFC3339 time for time-travel recall (facts true then)"`
	Namespace         string            `json:"namespace,omitempty" jsonschema:"tenant namespace; defaults to the server namespace"`
}

type recallItem struct {
	ID         string   `json:"id"`
	Content    string   `json:"content"`
	Tier       string   `json:"tier"`
	Level      string   `json:"level,omitempty"`
	Score      float64  `json:"score"`
	Confidence *float64 `json:"confidence,omitempty"`
	CreatedAt  string   `json:"created_at"`
	Tags       []string `json:"tags,omitempty"`
}

func scoredItem(s store.Scored) recallItem {
	return recallItem{
		ID: s.Memory.ID, Content: s.Memory.Content, Tier: string(s.Memory.Tier),
		Level: string(s.Memory.Level), Score: s.Score, Confidence: s.Memory.Confidence,
		CreatedAt: s.Memory.CreatedAt.Format(time.RFC3339), Tags: s.Memory.Tags,
	}
}

type recallResult struct {
	Results []recallItem `json:"results"`
}

func (t *tools) recall(ctx context.Context, _ *mcpsdk.CallToolRequest, in recallArgs) (*mcpsdk.CallToolResult, recallResult, error) {
	ns, err := t.ns(in.Namespace)
	if err != nil {
		return nil, recallResult{}, err
	}
	tiers, err := parseTiers(in.Tiers)
	if err != nil {
		return nil, recallResult{}, err
	}
	levels, err := parseLevels(in.Levels)
	if err != nil {
		return nil, recallResult{}, err
	}
	input := service.RecallInput{
		Namespace:         ns,
		Query:             in.Query,
		Tiers:             tiers,
		Levels:            levels,
		Tags:              in.Tags,
		Metadata:          in.Metadata,
		ExcludeMetadata:   in.ExcludeMetadata,
		IncludeFreshTurns: in.IncludeFreshTurns,
		QueryRewrite:      in.QueryRewrite,
		Limit:             in.Limit,
		Subtree:           strings.EqualFold(strings.TrimSpace(in.Scope), "subtree"),
	}
	if in.AsOf != "" {
		asOf, perr := time.Parse(time.RFC3339, in.AsOf)
		if perr != nil {
			return nil, recallResult{}, fmt.Errorf("invalid as_of %q: want RFC3339", in.AsOf)
		}
		input.AsOf = asOf.UTC()
	}
	res, err := t.svc.Recall(ctx, input)
	if err != nil {
		return nil, recallResult{}, err
	}
	out := recallResult{Results: make([]recallItem, len(res))}
	for i, s := range res {
		out.Results[i] = scoredItem(s)
	}
	return nil, out, nil
}

type briefingArgs struct {
	PerSection       *int   `json:"per_section,omitempty" jsonschema:"default cap for any section when its dedicated cap is unset (default 5)"`
	PerSectionPinned *int   `json:"per_section_pinned,omitempty" jsonschema:"max pinned memories; 0 disables this section"`
	PerSectionFacts  *int   `json:"per_section_facts,omitempty" jsonschema:"max durable semantic facts; 0 disables"`
	PerSectionProc   *int   `json:"per_section_procedures,omitempty" jsonschema:"max procedural how-to memories; 0 disables"`
	PerSectionRecent *int   `json:"per_section_recent,omitempty" jsonschema:"max recent episodic entries; 0 disables"`
	Namespace        string `json:"namespace,omitempty" jsonschema:"tenant namespace; defaults to the server namespace"`
}

type briefingResult struct {
	Namespace  string       `json:"namespace"`
	Pinned     []recallItem `json:"pinned,omitempty"`
	Facts      []recallItem `json:"facts,omitempty"`
	Procedures []recallItem `json:"procedures,omitempty"`
	Recent     []recallItem `json:"recent,omitempty"`
}

func briefingItems(mems []*memory.Memory) []recallItem {
	out := make([]recallItem, len(mems))
	for i, m := range mems {
		out[i] = recallItem{
			ID: m.ID, Content: m.Content, Tier: string(m.Tier), Confidence: m.Confidence,
			CreatedAt: m.CreatedAt.Format(time.RFC3339), Tags: m.Tags,
		}
	}
	return out
}

func (t *tools) briefing(ctx context.Context, _ *mcpsdk.CallToolRequest, in briefingArgs) (*mcpsdk.CallToolResult, briefingResult, error) {
	ns, err := t.ns(in.Namespace)
	if err != nil {
		return nil, briefingResult{}, err
	}
	// PerSection is the default cap applied to any section whose dedicated
	// per_section_X is unset (nil). Matches the REST /briefing semantics so
	// MCP and HTTP callers see the same shape: nil = default, 0 = disable.
	// The section fields are *int so "unset" (omitted) and "explicitly 0"
	// (disable) are distinguishable — a plain int can't express that.
	pick := func(dedicated *int) *int {
		if dedicated != nil {
			return dedicated
		}
		return in.PerSection
	}
	b, err := t.svc.Briefing(ctx, ns, service.BriefingOpts{
		Pinned:     pick(in.PerSectionPinned),
		Facts:      pick(in.PerSectionFacts),
		Procedures: pick(in.PerSectionProc),
		Recent:     pick(in.PerSectionRecent),
	})
	if err != nil {
		return nil, briefingResult{}, err
	}
	return nil, briefingResult{
		Namespace:  b.Namespace,
		Pinned:     briefingItems(b.Pinned),
		Facts:      briefingItems(b.Facts),
		Procedures: briefingItems(b.Procedures),
		Recent:     briefingItems(b.Recent),
	}, nil
}

type answerArgs struct {
	Query     string            `json:"query" jsonschema:"the question to answer from memory"`
	Tiers     []string          `json:"tiers,omitempty" jsonschema:"restrict grounding to tiers (working/episodic/semantic/procedural)"`
	Levels    []string          `json:"levels,omitempty" jsonschema:"restrict grounding to levels (explicit/deduced); empty means all"`
	Tags      []string          `json:"tags,omitempty" jsonschema:"ground only on memories with every listed tag (AND)"`
	Metadata  map[string]string `json:"metadata,omitempty" jsonschema:"ground only on memories whose metadata has each key=value pair (AND)"`
	Limit     int               `json:"limit,omitempty" jsonschema:"max memories to ground on (default 10)"`
	Namespace string            `json:"namespace,omitempty" jsonschema:"tenant namespace; defaults to the server namespace"`
	// ReasoningLevel is the latency/cost dial: higher levels let the model
	// search memory iteratively before answering.
	ReasoningLevel string `json:"reasoning_level,omitempty" jsonschema:"effort: minimal|low|medium|high; higher = iterative search (slower)"`
}

type answerResult struct {
	Answer  string       `json:"answer"`
	Sources []recallItem `json:"sources"`
}

func (t *tools) answer(ctx context.Context, _ *mcpsdk.CallToolRequest, in answerArgs) (*mcpsdk.CallToolResult, answerResult, error) {
	ns, err := t.ns(in.Namespace)
	if err != nil {
		return nil, answerResult{}, err
	}
	tiers, err := parseTiers(in.Tiers)
	if err != nil {
		return nil, answerResult{}, err
	}
	levels, err := parseLevels(in.Levels)
	if err != nil {
		return nil, answerResult{}, err
	}
	res, err := t.svc.Answer(ctx, service.AnswerInput{
		Namespace: ns,
		Query:     in.Query,
		Tiers:     tiers,
		Levels:    levels,
		Tags:      in.Tags,
		Metadata:  in.Metadata,
		Limit:     in.Limit,
		Reasoning: service.ReasoningLevel(in.ReasoningLevel),
	})
	if err != nil {
		return nil, answerResult{}, err
	}
	out := answerResult{Answer: res.Answer, Sources: make([]recallItem, len(res.Sources))}
	for i, s := range res.Sources {
		out.Sources[i] = scoredItem(s)
	}
	return nil, out, nil
}

type idArgs struct {
	ID        string `json:"id" jsonschema:"the memory ID"`
	Namespace string `json:"namespace,omitempty" jsonschema:"tenant namespace; defaults to the server namespace"`
}

// memoryItem is the full single-memory DTO returned by memory_get (recall
// results stay slim via recallItem; a get has no score and should not drop
// the record's metadata).
type memoryItem struct {
	ID          string         `json:"id"`
	Content     string         `json:"content"`
	Tier        string         `json:"tier"`
	Level       string         `json:"level,omitempty"`
	Summary     string         `json:"summary,omitempty"`
	Tags        []string       `json:"tags,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`
	Importance  float64        `json:"importance"`
	CreatedAt   string         `json:"created_at"`
	UpdatedAt   string         `json:"updated_at"`
	AccessCount int            `json:"access_count"`
	ExpiresAt   string         `json:"expires_at,omitempty"`
	ValidFrom   string         `json:"valid_from,omitempty"`
	ValidTo     string         `json:"valid_to,omitempty"`
}

func toMemoryItem(m *memory.Memory) memoryItem {
	out := memoryItem{
		ID: m.ID, Content: m.Content, Tier: string(m.Tier), Level: string(m.Level),
		Summary: m.Summary, Tags: m.Tags, Metadata: m.Metadata, Importance: m.Importance,
		CreatedAt: m.CreatedAt.Format(time.RFC3339), UpdatedAt: m.UpdatedAt.Format(time.RFC3339),
		AccessCount: m.AccessCount,
	}
	if m.ExpiresAt != nil {
		out.ExpiresAt = m.ExpiresAt.Format(time.RFC3339)
	}
	if m.ValidFrom != nil {
		out.ValidFrom = m.ValidFrom.Format(time.RFC3339)
	}
	if m.ValidTo != nil {
		out.ValidTo = m.ValidTo.Format(time.RFC3339)
	}
	return out
}

func (t *tools) get(ctx context.Context, _ *mcpsdk.CallToolRequest, in idArgs) (*mcpsdk.CallToolResult, memoryItem, error) {
	ns, err := t.ns(in.Namespace)
	if err != nil {
		return nil, memoryItem{}, err
	}
	m, err := t.svc.Get(ctx, ns, in.ID)
	if err != nil {
		return nil, memoryItem{}, err
	}
	return nil, toMemoryItem(m), nil
}

// defaultListLimit caps memory_list results when the caller omits (or sets
// non-positive) limit. This is an MCP-only cap: an unbounded list is a
// context blowout for LLM callers. The REST API keeps its 0 = all semantics.
const defaultListLimit = 20

type listArgs struct {
	Tiers     []string          `json:"tiers,omitempty" jsonschema:"restrict to tiers (working/episodic/semantic/procedural); empty means all"`
	Levels    []string          `json:"levels,omitempty" jsonschema:"restrict to levels (explicit/deduced); empty means all"`
	Tags      []string          `json:"tags,omitempty" jsonschema:"only memories carrying every listed tag (AND)"`
	Metadata  map[string]string `json:"metadata,omitempty" jsonschema:"only memories whose metadata has each key=value pair (AND)"`
	Limit     int               `json:"limit,omitempty" jsonschema:"max results (default 20, newest first)"`
	Offset    int               `json:"offset,omitempty" jsonschema:"skip this many results for paging"`
	Namespace string            `json:"namespace,omitempty" jsonschema:"tenant namespace; defaults to the server namespace"`
}

type listResult struct {
	Memories []memoryItem `json:"memories"`
}

func (t *tools) list(ctx context.Context, _ *mcpsdk.CallToolRequest, in listArgs) (*mcpsdk.CallToolResult, listResult, error) {
	ns, err := t.ns(in.Namespace)
	if err != nil {
		return nil, listResult{}, err
	}
	tiers, err := parseTiers(in.Tiers)
	if err != nil {
		return nil, listResult{}, err
	}
	levels, err := parseLevels(in.Levels)
	if err != nil {
		return nil, listResult{}, err
	}
	// Clamp a negative offset to 0: limit+offset below would otherwise go
	// <= 0, which the service layer treats as "no limit" — reintroducing
	// the unbounded listing the default cap exists to prevent.
	if in.Offset < 0 {
		in.Offset = 0
	}
	limit := in.Limit
	if limit <= 0 {
		limit = defaultListLimit
	}
	mems, err := t.svc.List(ctx, service.ListInput{
		Namespace: ns,
		Tiers:     tiers,
		Levels:    levels,
		Tags:      in.Tags,
		Metadata:  in.Metadata,
		Limit:     limit + in.Offset,
	})
	if err != nil {
		return nil, listResult{}, err
	}
	if in.Offset > 0 {
		if in.Offset >= len(mems) {
			mems = nil
		} else {
			mems = mems[in.Offset:]
		}
	}
	out := listResult{Memories: make([]memoryItem, len(mems))}
	for i, m := range mems {
		out.Memories[i] = toMemoryItem(m)
	}
	return nil, out, nil
}

type forgetResult struct {
	Deleted bool `json:"deleted"`
}

func (t *tools) forget(ctx context.Context, _ *mcpsdk.CallToolRequest, in idArgs) (*mcpsdk.CallToolResult, forgetResult, error) {
	ns, err := t.ns(in.Namespace)
	if err != nil {
		return nil, forgetResult{}, err
	}
	if err := t.svc.Forget(ctx, ns, in.ID); err != nil {
		return nil, forgetResult{}, err
	}
	return nil, forgetResult{Deleted: true}, nil
}
