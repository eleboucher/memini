// Package mcp exposes memini over the Model Context Protocol. It is a thin
// adapter over the same service.Service the REST API uses, served over stdio
// and Streamable HTTP.
package mcp

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strings"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/eleboucher/memini/internal/httputil"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/service"
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
		Name:        "memory_recall",
		Description: "Recall relevant memories via hybrid (semantic + keyword) search.",
	}, h.recall)
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
// calls may still override it per-call. When apiKey is non-empty, requests must
// present it as a bearer token — required for any remote (non-localhost)
// deployment.
func HTTPHandler(svc *service.Service, nsHeader, defaultNS, apiKey string) http.Handler {
	h := mcpsdk.NewStreamableHTTPHandler(func(r *http.Request) *mcpsdk.Server {
		ns := defaultNS
		if v := strings.TrimSpace(r.Header.Get(nsHeader)); v != "" && httputil.ValidateNamespace(v) == nil {
			ns = v
		}
		return NewServer(svc, ns)
	}, nil)
	if apiKey == "" {
		return h
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(token), []byte(apiKey)) != 1 {
			http.Error(w, `{"error":"missing or invalid bearer token"}`, http.StatusUnauthorized)
			return
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

func (t *tools) ns(arg string) string {
	arg = strings.TrimSpace(arg)
	if arg != "" && httputil.ValidateNamespace(arg) == nil {
		return arg
	}
	return t.defaultNS
}

type rememberArgs struct {
	Content    string   `json:"content" jsonschema:"the text to remember"`
	Tier       string   `json:"tier,omitempty" jsonschema:"working, episodic, semantic, or procedural (default working)"`
	Tags       []string `json:"tags,omitempty" jsonschema:"optional labels"`
	Importance float64  `json:"importance,omitempty" jsonschema:"0..1 bias toward retention"`
	Namespace  string   `json:"namespace,omitempty" jsonschema:"tenant namespace; defaults to the server namespace"`
}

type rememberResult struct {
	ID   string `json:"id"`
	Tier string `json:"tier"`
}

func (t *tools) remember(ctx context.Context, _ *mcpsdk.CallToolRequest, in rememberArgs) (*mcpsdk.CallToolResult, rememberResult, error) {
	m, err := t.svc.Remember(ctx, service.RememberInput{
		Namespace:  t.ns(in.Namespace),
		Content:    in.Content,
		Tier:       memory.Tier(in.Tier),
		Tags:       in.Tags,
		Importance: in.Importance,
	})
	if err != nil {
		return nil, rememberResult{}, err
	}
	return nil, rememberResult{ID: m.ID, Tier: string(m.Tier)}, nil
}

type recallArgs struct {
	Query     string `json:"query" jsonschema:"what to search for"`
	Limit     int    `json:"limit,omitempty" jsonschema:"max results (default 10)"`
	Namespace string `json:"namespace,omitempty" jsonschema:"tenant namespace; defaults to the server namespace"`
}

type recallItem struct {
	ID      string  `json:"id"`
	Content string  `json:"content"`
	Tier    string  `json:"tier"`
	Score   float64 `json:"score"`
}

type recallResult struct {
	Results []recallItem `json:"results"`
}

func (t *tools) recall(ctx context.Context, _ *mcpsdk.CallToolRequest, in recallArgs) (*mcpsdk.CallToolResult, recallResult, error) {
	res, err := t.svc.Recall(ctx, service.RecallInput{
		Namespace: t.ns(in.Namespace),
		Query:     in.Query,
		Limit:     in.Limit,
	})
	if err != nil {
		return nil, recallResult{}, err
	}
	out := recallResult{Results: make([]recallItem, len(res))}
	for i, s := range res {
		out.Results[i] = recallItem{
			ID: s.Memory.ID, Content: s.Memory.Content, Tier: string(s.Memory.Tier), Score: s.Score,
		}
	}
	return nil, out, nil
}

type idArgs struct {
	ID        string `json:"id" jsonschema:"the memory ID"`
	Namespace string `json:"namespace,omitempty" jsonschema:"tenant namespace; defaults to the server namespace"`
}

func (t *tools) get(ctx context.Context, _ *mcpsdk.CallToolRequest, in idArgs) (*mcpsdk.CallToolResult, recallItem, error) {
	m, err := t.svc.Get(ctx, t.ns(in.Namespace), in.ID)
	if err != nil {
		return nil, recallItem{}, err
	}
	return nil, recallItem{ID: m.ID, Content: m.Content, Tier: string(m.Tier)}, nil
}

type forgetResult struct {
	Deleted bool `json:"deleted"`
}

func (t *tools) forget(ctx context.Context, _ *mcpsdk.CallToolRequest, in idArgs) (*mcpsdk.CallToolResult, forgetResult, error) {
	if err := t.svc.Forget(ctx, t.ns(in.Namespace), in.ID); err != nil {
		return nil, forgetResult{}, err
	}
	return nil, forgetResult{Deleted: true}, nil
}
