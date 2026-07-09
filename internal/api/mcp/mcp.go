// Package mcp exposes memini over the Model Context Protocol. It is a thin
// adapter over the same service.Service the REST API uses, served over stdio
// and Streamable HTTP.
package mcp

import (
	"context"
	"crypto/subtle"
	"errors"
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
			"semantic=durable fact, procedural=how-to, episodic=event, working=scratch (default intake, omit to " +
			"auto-classify). If the result carries merge_hint, the content nearly duplicates an " +
			"existing memory — either call memory_update with id=merge_hint.similar_id to fold " +
			"them together, or ignore it to keep both. Returns {id, tier, stored}; stored=false " +
			"means a low-signal write was dropped by the value gate (not an error).",
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
			"an exact per-call read set (namespaces — replaces the default; writes are unaffected), " +
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
			"queries at session start. scope=subtree also briefs nested namespaces; namespaces " +
			"briefs an exact read set instead of the default (writes are unaffected).",
		Annotations: readOnly,
	}, h.briefing)
	// memory_answer requires an LLM; only advertise it when one is configured,
	// so headless deployments don't list a tool that would error on every call.
	if svc.HasAnswerer() {
		mcpsdk.AddTool(s, &mcpsdk.Tool{
			Name:  "memory_answer",
			Title: "Answer from memory",
			Description: "Recall relevant memories and answer a question grounded on them (requires " +
				"an LLM). Slower than memory_recall; use when you want a synthesized answer with " +
				"sources rather than raw memories.",
			Annotations: readOnly,
		}, h.answer)
	}
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
		Name:  "memory_update",
		Title: "Update a memory",
		Description: "Update fields of an existing memory by ID (partial: only provided fields " +
			"change; metadata merges key-by-key). Use to correct or enrich a fact — e.g. to fold " +
			"a near-duplicate flagged by memory_remember's merge_hint into the surviving memory. " +
			"To delete instead, use memory_forget.",
		Annotations: additive,
	}, h.update)
	mcpsdk.AddTool(s, &mcpsdk.Tool{
		Name:  "memory_forget",
		Title: "Forget a memory",
		Description: "Permanently delete a memory by ID — use for wrong, outdated, or unwanted " +
			"memories. To correct a fact instead, prefer memory_update so history is preserved.",
		Annotations: destructive,
	}, h.forget)
	// memory_namespace_link only appears when the backend supports persistent
	// links, so headless deployments never see a tool that would error on
	// every call — same gating memory_answer uses for HasAnswerer above.
	if svc.HasNamespaceLinks() {
		mcpsdk.AddTool(s, &mcpsdk.Tool{
			Name:  "memory_namespace_link",
			Title: "Manage namespace links",
			Description: "Manage persistent, read-only links between namespaces. action=add attaches " +
				"target so reads in namespace (recall/briefing without an explicit per-call namespaces " +
				"list) also see it — 1-hop only, target's own links are never followed. tiers='durable' " +
				"(default) surfaces only target's semantic/procedural memories; 'all' also includes " +
				"episodic/working. action=remove detaches target. action=list ignores target/tiers. " +
				"Writes always land in the request namespace and are never affected by links. Returns " +
				"the full current link list after the action.",
			Annotations: additive,
		}, h.namespaceLink)
	}

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
			return nil, fmt.Errorf("invalid tier %q: want one of working|episodic|semantic|procedural", t)
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
			return nil, fmt.Errorf("invalid level %q: want one of explicit|deduced", l)
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
	// Degraded is set to "pending_embed" when the content embed failed or timed
	// out (WriteEmbedTimeout) and the memory was stored keyword-searchable only,
	// without a vector. Empty on a healthy write.
	Degraded string `json:"degraded,omitempty"`
	// Note explains Degraded in plain language for an agent consuming the tool
	// result; omitted alongside Degraded on a healthy write.
	Note string `json:"note,omitempty"`
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
	if pending, ok := m.Metadata["pending_embed"].(string); ok && pending == "true" {
		res.Degraded = "pending_embed"
		res.Note = "embeddings unavailable; stored keyword-searchable only, vector will be backfilled automatically"
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
	//nolint:lll // the jsonschema description is agent-facing documentation and cannot be wrapped
	Namespaces []string `json:"namespaces,omitempty" jsonschema:"search exactly these namespaces instead of the default read set (namespace + global); entry 'ns/*' also includes namespaces nested under ns; max 16; writes are unaffected"`
	//nolint:lll // the jsonschema description is agent-facing documentation and cannot be wrapped
	ResponseFormat string `json:"response_format,omitempty" jsonschema:"'concise' returns summary-or-truncated content (~1 line each; fetch full text with memory_get); 'detailed' (default) returns full content"`
}

type recallItem struct {
	ID      string `json:"id"`
	Content string `json:"content"`
	Tier    string `json:"tier"`
	Level   string `json:"level,omitempty"`
	// Namespace is read provenance: the namespace the memory lives in. With a
	// multi-namespace read set (namespaces/scope=subtree) it tells the caller
	// which partition each hit came from.
	Namespace  string   `json:"namespace,omitempty"`
	Score      float64  `json:"score"`
	Confidence *float64 `json:"confidence,omitempty"`
	CreatedAt  string   `json:"created_at"`
	Tags       []string `json:"tags,omitempty"`
}

// conciseContentMax is the rune limit for concise content (before truncation).
const conciseContentMax = 240

// conciseContent returns a concise representation of memory content: the summary
// if present, or the first 240 runes with a "…" suffix if no summary. Both the
// truncation decision and the cut are in runes — deciding on bytes would append
// a spurious "…" to multi-byte content under the rune limit, and cutting on
// bytes could split a UTF-8 sequence. Used when response_format="concise".
func conciseContent(m *memory.Memory) string {
	if m.Summary != "" {
		return m.Summary
	}
	runes := []rune(m.Content)
	if len(runes) <= conciseContentMax {
		return m.Content
	}
	return string(runes[:conciseContentMax]) + "…"
}

func scoredItem(s store.Scored, responseFormat string) recallItem {
	content := s.Memory.Content
	if responseFormat == "concise" {
		content = conciseContent(s.Memory)
	}
	return recallItem{
		ID: s.Memory.ID, Content: content, Tier: string(s.Memory.Tier),
		Level: string(s.Memory.Level), Namespace: s.Memory.Namespace,
		Score: s.Score, Confidence: s.Memory.Confidence,
		CreatedAt: s.Memory.CreatedAt.Format(time.RFC3339), Tags: s.Memory.Tags,
	}
}

type recallResult struct {
	Results []recallItem `json:"results"`
	// Degraded is set to "keyword_only" when the query embed failed or timed
	// out and recall fell back to keyword-only search; omitted on a healthy
	// (vector+keyword) recall.
	Degraded string `json:"degraded,omitempty"`
	// Note explains Degraded in plain language for an agent consuming the
	// tool result; omitted alongside Degraded on a healthy recall.
	Note string `json:"note,omitempty"`
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
		Namespaces:        in.Namespaces,
	}
	if in.AsOf != "" {
		asOf, perr := time.Parse(time.RFC3339, in.AsOf)
		if perr != nil {
			return nil, recallResult{}, fmt.Errorf("invalid as_of %q: want RFC3339", in.AsOf)
		}
		input.AsOf = asOf.UTC()
	}
	var degraded string
	input.Degraded = &degraded
	res, err := t.svc.Recall(ctx, input)
	if err != nil {
		return nil, recallResult{}, err
	}
	out := recallResult{Results: make([]recallItem, len(res))}
	for i, s := range res {
		out.Results[i] = scoredItem(s, in.ResponseFormat)
	}
	if degraded != "" {
		out.Degraded = "keyword_only"
		out.Note = "semantic search unavailable (" + degraded + "); results are keyword-only and may be incomplete"
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
	Scope            string `json:"scope,omitempty" jsonschema:"'subtree' also includes namespaces nested under the namespace; default 'exact'"`
	//nolint:lll // the jsonschema description is agent-facing documentation and cannot be wrapped
	Namespaces []string `json:"namespaces,omitempty" jsonschema:"brief exactly these namespaces instead of the default read set (namespace + global); entry 'ns/*' also includes namespaces nested under ns; max 16; writes are unaffected"`
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
			ID: m.ID, Content: m.Content, Tier: string(m.Tier), Namespace: m.Namespace,
			Confidence: m.Confidence,
			CreatedAt:  m.CreatedAt.Format(time.RFC3339), Tags: m.Tags,
		}
	}
	return out
}

func (t *tools) briefing(ctx context.Context, _ *mcpsdk.CallToolRequest, in briefingArgs) (*mcpsdk.CallToolResult, briefingResult, error) {
	ns, err := t.ns(in.Namespace)
	if err != nil {
		return nil, briefingResult{}, err
	}
	var subtree bool
	switch scope := strings.TrimSpace(in.Scope); {
	case scope == "" || strings.EqualFold(scope, "exact"):
	case strings.EqualFold(scope, "subtree"):
		subtree = true
	default:
		return nil, briefingResult{}, fmt.Errorf("invalid scope %q: want exact or subtree", in.Scope)
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
		Namespaces: in.Namespaces,
		Subtree:    subtree,
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
		out.Sources[i] = scoredItem(s, "")
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

// notFoundErr wraps err with the standard "no memory" guidance when err is
// store.ErrNotFound, and returns err unchanged otherwise — a transient store
// error must never be misreported as "this id doesn't exist", which would
// send an agent down the wrong path (fabricating an id or giving up) instead
// of retrying. Used by get, forget, and update so the wording is identical
// across all three surfaces.
func notFoundErr(id, ns string, err error) error {
	if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	return fmt.Errorf("no memory %q in namespace %q — ids come from memory_recall "+
		"or memory_list results: %w", id, ns, err)
}

func (t *tools) get(ctx context.Context, _ *mcpsdk.CallToolRequest, in idArgs) (*mcpsdk.CallToolResult, memoryItem, error) {
	ns, err := t.ns(in.Namespace)
	if err != nil {
		return nil, memoryItem{}, err
	}
	m, err := t.svc.Get(ctx, ns, in.ID)
	if err != nil {
		return nil, memoryItem{}, notFoundErr(in.ID, ns, err)
	}
	return nil, toMemoryItem(m), nil
}

type updateArgs struct {
	ID         string         `json:"id" jsonschema:"the memory ID to update (from memory_recall/memory_list)"`
	Content    string         `json:"content,omitempty" jsonschema:"replacement content; omit to keep"`
	Summary    string         `json:"summary,omitempty" jsonschema:"replacement summary; omit to keep"`
	Tier       string         `json:"tier,omitempty" jsonschema:"move to this tier; omit to keep"`
	Tags       []string       `json:"tags,omitempty" jsonschema:"replacement tag set; omit to keep"`
	Metadata   map[string]any `json:"metadata,omitempty" jsonschema:"merged into existing metadata key-by-key"`
	Importance *float64       `json:"importance,omitempty" jsonschema:"0..1; omit to keep"`
	Confidence *float64       `json:"confidence,omitempty" jsonschema:"0..1; omit to keep"`
	Namespace  string         `json:"namespace,omitempty" jsonschema:"tenant namespace; defaults to the server namespace"`
}

// update composes svc.Get + svc.Remember with the current ID (the documented
// upsert path): it re-embeds content and re-runs the write-time lifecycle
// (corroborate/contradict), so an update is not a bare field patch. Only
// fields explicitly provided in in are changed; everything else carries over
// from the current record. Metadata merges key-by-key rather than replacing
// wholesale, so a caller enriching one key never has to resend the rest.
func (t *tools) update(ctx context.Context, _ *mcpsdk.CallToolRequest, in updateArgs) (*mcpsdk.CallToolResult, memoryItem, error) {
	ns, err := t.ns(in.Namespace)
	if err != nil {
		return nil, memoryItem{}, err
	}
	cur, err := t.svc.Get(ctx, ns, in.ID)
	if err != nil {
		return nil, memoryItem{}, notFoundErr(in.ID, ns, err)
	}
	upd := service.RememberInput{
		Namespace: ns, ID: cur.ID,
		Content: cur.Content, Summary: cur.Summary, Tier: cur.Tier,
		Tags: cur.Tags, Metadata: cur.Metadata, Importance: cur.Importance, Confidence: cur.Confidence,
		Level: cur.Level, ValidFrom: cur.ValidFrom, ValidTo: cur.ValidTo,
	}
	if in.Content != "" {
		upd.Content = in.Content
	}
	if in.Summary != "" {
		upd.Summary = in.Summary
	}
	if in.Tier != "" {
		tr := memory.Tier(in.Tier)
		if !tr.Valid() {
			return nil, memoryItem{}, fmt.Errorf("invalid tier %q: want working|episodic|semantic|procedural", in.Tier)
		}
		upd.Tier = tr
	}
	if in.Tags != nil {
		upd.Tags = in.Tags
	}
	for k, v := range in.Metadata {
		if upd.Metadata == nil {
			upd.Metadata = map[string]any{}
		}
		upd.Metadata[k] = v
	}
	if in.Importance != nil {
		upd.Importance = *in.Importance
	}
	if in.Confidence != nil {
		upd.Confidence = in.Confidence
	}
	m, err := t.svc.Remember(ctx, upd)
	if err != nil {
		return nil, memoryItem{}, err
	}
	// The episodic value gate returns (nil, nil) when it drops a low-signal
	// write. For memory_remember that is a stored=false result, but here the
	// caller asked to change an existing memory and nothing changed — surface
	// it as an error rather than dereferencing nil or claiming success.
	if m == nil {
		return nil, memoryItem{}, fmt.Errorf("update dropped: the new content is below the episodic value gate " +
			"(too short/low-signal); provide more substantive content or set a durable tier")
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
		return nil, forgetResult{}, notFoundErr(in.ID, ns, err)
	}
	return nil, forgetResult{Deleted: true}, nil
}

type linkArgs struct {
	Action    string `json:"action" jsonschema:"'add', 'remove', or 'list'"`
	Target    string `json:"target,omitempty" jsonschema:"namespace to attach read-only (required for add/remove); 'ns/*' also includes namespaces nested under ns"`
	Tiers     string `json:"tiers,omitempty" jsonschema:"'durable' (default: semantic+procedural only) or 'all'"`
	Namespace string `json:"namespace,omitempty" jsonschema:"the namespace whose read set changes; defaults to the server namespace"`
}

// linkItem is the wire DTO for one namespace link.
type linkItem struct {
	Target    string `json:"target"`
	Tiers     string `json:"tiers"`
	CreatedAt string `json:"created_at"`
}

func toLinkItem(l store.NamespaceLink) linkItem {
	return linkItem{Target: l.Target, Tiers: l.Tiers, CreatedAt: l.CreatedAt.Format(time.RFC3339)}
}

type linkResult struct {
	Links []linkItem `json:"links"`
}

// namespaceLink implements memory_namespace_link: add/remove a persistent
// read-only link, or list the namespace's current links. Every action
// (including list) returns the full current link list, so a caller sees the
// result of add/remove without a follow-up call.
func (t *tools) namespaceLink(ctx context.Context, _ *mcpsdk.CallToolRequest, in linkArgs) (*mcpsdk.CallToolResult, linkResult, error) {
	ns, err := t.ns(in.Namespace)
	if err != nil {
		return nil, linkResult{}, err
	}
	switch in.Action {
	case "add":
		if in.Target == "" {
			return nil, linkResult{}, fmt.Errorf("target is required for action=%q", in.Action)
		}
		if err := t.svc.LinkNamespaces(ctx, ns, in.Target, in.Tiers); err != nil {
			return nil, linkResult{}, err
		}
	case "remove":
		if in.Target == "" {
			return nil, linkResult{}, fmt.Errorf("target is required for action=%q", in.Action)
		}
		if err := t.svc.UnlinkNamespaces(ctx, ns, in.Target); err != nil {
			return nil, linkResult{}, err
		}
	case "list":
		// Nothing to do; fall through to the current-links read below.
	default:
		return nil, linkResult{}, fmt.Errorf("invalid action %q: want add, remove, or list", in.Action)
	}
	links, err := t.svc.NamespaceLinks(ctx, ns)
	if err != nil {
		return nil, linkResult{}, err
	}
	out := linkResult{Links: make([]linkItem, len(links))}
	for i, l := range links {
		out.Links[i] = toLinkItem(l)
	}
	return nil, out, nil
}
