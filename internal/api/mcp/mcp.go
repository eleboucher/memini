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

// NewServer builds an MCP server exposing memini's memory tools. defaultNS is
// used whenever a tool call omits the namespace argument.
func NewServer(svc *service.Service, defaultNS string) *mcpsdk.Server {
	s := mcpsdk.NewServer(&mcpsdk.Implementation{
		Name:    "memini",
		Version: version.Version,
	}, nil)

	h := &tools{svc: svc, defaultNS: defaultNS}

	mcpsdk.AddTool(s, &mcpsdk.Tool{
		Name:        "memory_remember",
		Description: "Store a memory for later recall. Returns the new memory's ID.",
	}, h.remember)
	mcpsdk.AddTool(s, &mcpsdk.Tool{
		Name: "memory_recall",
		Description: "Recall relevant memories via hybrid (semantic + keyword) search. " +
			"Supports time-travel (as_of) and reading nested namespaces (scope=subtree).",
	}, h.recall)
	mcpsdk.AddTool(s, &mcpsdk.Tool{
		Name: "memory_briefing",
		Description: "Layered session-start briefing for this namespace — pinned context, " +
			"durable facts, how-to procedures, and recent activity — in one query-less call. " +
			"Call it when a session opens to orient yourself.",
	}, h.briefing)
	mcpsdk.AddTool(s, &mcpsdk.Tool{
		Name:        "memory_answer",
		Description: "Recall relevant memories and answer a question grounded on them (requires an LLM).",
	}, h.answer)
	mcpsdk.AddTool(s, &mcpsdk.Tool{
		Name: "memory_list",
		Description: "Browse memories without a query — filter by tier, tags, or metadata " +
			"(e.g. all procedural memories, or everything tagged/categorized X). Newest first.",
	}, h.list)
	mcpsdk.AddTool(s, &mcpsdk.Tool{
		Name:        "memory_get",
		Description: "Fetch a single memory by its ID.",
	}, h.get)
	mcpsdk.AddTool(s, &mcpsdk.Tool{
		Name:        "memory_forget",
		Description: "Delete a memory by its ID.",
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
	Tier       string         `json:"tier,omitempty" jsonschema:"working, episodic, semantic, or procedural (default working)"`
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
	m, err := t.svc.Remember(ctx, input)
	if err != nil {
		return nil, rememberResult{}, err
	}
	return nil, rememberResult{ID: m.ID, Tier: string(m.Tier)}, nil
}

type recallArgs struct {
	Query     string            `json:"query" jsonschema:"what to search for"`
	Tiers     []string          `json:"tiers,omitempty" jsonschema:"restrict to tiers (working/episodic/semantic/procedural); empty means all"`
	Tags      []string          `json:"tags,omitempty" jsonschema:"only memories carrying every listed tag (AND)"`
	Metadata  map[string]string `json:"metadata,omitempty" jsonschema:"only memories whose metadata has each key=value pair (AND)"`
	Limit     int               `json:"limit,omitempty" jsonschema:"max results (default 10)"`
	Scope     string            `json:"scope,omitempty" jsonschema:"'subtree' also searches nested namespaces; default 'exact'"`
	AsOf      string            `json:"as_of,omitempty" jsonschema:"RFC3339 time for time-travel recall (facts true then)"`
	Namespace string            `json:"namespace,omitempty" jsonschema:"tenant namespace; defaults to the server namespace"`
}

type recallItem struct {
	ID         string   `json:"id"`
	Content    string   `json:"content"`
	Tier       string   `json:"tier"`
	Score      float64  `json:"score"`
	Confidence *float64 `json:"confidence,omitempty"`
}

func scoredItem(s store.Scored) recallItem {
	return recallItem{
		ID: s.Memory.ID, Content: s.Memory.Content, Tier: string(s.Memory.Tier),
		Score: s.Score, Confidence: s.Memory.Confidence,
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
	input := service.RecallInput{
		Namespace: ns,
		Query:     in.Query,
		Tiers:     tiers,
		Tags:      in.Tags,
		Metadata:  in.Metadata,
		Limit:     in.Limit,
		Subtree:   strings.EqualFold(strings.TrimSpace(in.Scope), "subtree"),
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
	PerSection int    `json:"per_section,omitempty" jsonschema:"max memories per section (default 5)"`
	Namespace  string `json:"namespace,omitempty" jsonschema:"tenant namespace; defaults to the server namespace"`
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
		out[i] = recallItem{ID: m.ID, Content: m.Content, Tier: string(m.Tier), Confidence: m.Confidence}
	}
	return out
}

func (t *tools) briefing(ctx context.Context, _ *mcpsdk.CallToolRequest, in briefingArgs) (*mcpsdk.CallToolResult, briefingResult, error) {
	ns, err := t.ns(in.Namespace)
	if err != nil {
		return nil, briefingResult{}, err
	}
	b, err := t.svc.Briefing(ctx, ns, in.PerSection)
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
	Tags      []string          `json:"tags,omitempty" jsonschema:"ground only on memories with every listed tag (AND)"`
	Metadata  map[string]string `json:"metadata,omitempty" jsonschema:"ground only on memories whose metadata has each key=value pair (AND)"`
	Limit     int               `json:"limit,omitempty" jsonschema:"max memories to ground on (default 10)"`
	Namespace string            `json:"namespace,omitempty" jsonschema:"tenant namespace; defaults to the server namespace"`
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
	res, err := t.svc.Answer(ctx, service.AnswerInput{
		Namespace: ns,
		Query:     in.Query,
		Tags:      in.Tags,
		Metadata:  in.Metadata,
		Limit:     in.Limit,
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
		ID: m.ID, Content: m.Content, Tier: string(m.Tier), Summary: m.Summary,
		Tags: m.Tags, Metadata: m.Metadata, Importance: m.Importance,
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

type listArgs struct {
	Tiers     []string          `json:"tiers,omitempty" jsonschema:"restrict to tiers (working/episodic/semantic/procedural); empty means all"`
	Tags      []string          `json:"tags,omitempty" jsonschema:"only memories carrying every listed tag (AND)"`
	Metadata  map[string]string `json:"metadata,omitempty" jsonschema:"only memories whose metadata has each key=value pair (AND)"`
	Limit     int               `json:"limit,omitempty" jsonschema:"max results (0 = all, newest first)"`
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
	mems, err := t.svc.List(ctx, service.ListInput{
		Namespace: ns,
		Tiers:     tiers,
		Tags:      in.Tags,
		Metadata:  in.Metadata,
		Limit:     in.Limit,
	})
	if err != nil {
		return nil, listResult{}, err
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
