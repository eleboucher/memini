package mcp_test

import (
	"context"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	meminimcp "github.com/eleboucher/memini/internal/api/mcp"
	"github.com/eleboucher/memini/internal/embed/embedtest"
	"github.com/eleboucher/memini/internal/service"
	"github.com/eleboucher/memini/internal/store"
	"github.com/eleboucher/memini/internal/store/sqlitevec"
)

// mcpWriteToolCalls are the tool calls a read-only session must be refused.
// Arguments are valid so a refusal can never be confused with a schema error.
var mcpWriteToolCalls = []mcpsdk.CallToolParams{
	{Name: "memory_remember", Arguments: map[string]any{"content": "a write attempt", "tier": "semantic"}},
	{Name: "memory_update", Arguments: map[string]any{"id": "some-id", "content": "an update attempt"}},
	{Name: "memory_forget", Arguments: map[string]any{"id": "some-id"}},
}

// newReadOnlyMCP builds an MCP HTTP handler backed by a key store holding a
// read-only key ("tok-ci") and an ordinary read-write key ("tok-bot").
func newReadOnlyMCP(t *testing.T) http.Handler {
	t.Helper()
	ctx := context.Background()
	st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "mcp-readonly.db"), dims)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	var ks store.APIKeyStore = st
	if err := ks.PutAPIKey(ctx, store.APIKey{Name: "ci", Hash: hashOf("tok-ci"), ReadOnly: true}); err != nil {
		t.Fatalf("PutAPIKey(ci): %v", err)
	}
	if err := ks.PutAPIKey(ctx, store.APIKey{Name: "bot", Hash: hashOf("tok-bot")}); err != nil {
		t.Fatalf("PutAPIKey(bot): %v", err)
	}
	svc := service.New(st, embedtest.New(dims))
	return meminimcp.HTTPHandler(svc, "X-Memini-Namespace", "default", "X-Memini-Home", "", ks, nil)
}

// TestMCPReadOnlyKeyRefusedOnWriteTools is the MCP counterpart of the REST
// enforcement matrix. MCP is mounted OUTSIDE rest.Mount's group, so the REST
// middleware does not cover it — without this gate a read-only credential could
// simply write over MCP instead.
func TestMCPReadOnlyKeyRefusedOnWriteTools(t *testing.T) {
	h := newReadOnlyMCP(t)
	ctx := context.Background()
	cs, err := connectHTTP(t, h, "acme", "", "tok-ci")
	if err != nil {
		t.Fatalf("connect with read-only key: %v", err)
	}
	for _, call := range mcpWriteToolCalls {
		res, err := cs.CallTool(ctx, &call)
		// The refusal must arrive as an error tool RESULT, not a protocol error.
		// A protocol error reads to an agent as "the call broke" and invites a
		// retry; an error result is output the model sees and adapts to. Asserting
		// err == nil is what pins that distinction — do not relax it to "either
		// shape is fine", or the retry-forever behavior this prevents comes back.
		if err != nil {
			t.Errorf("%s: want an error tool result, got a protocol error: %v", call.Name, err)
			continue
		}
		if !res.IsError {
			t.Errorf("%s with a read-only key: want a refusal, got success %+v", call.Name, res)
			continue
		}
		text := strings.ToLower(resultText(res))
		if !strings.Contains(text, "read-only") {
			t.Errorf("%s refusal must say why; got %q", call.Name, resultText(res))
		}
		if !strings.Contains(text, "do not retry") {
			t.Errorf("%s refusal must tell the agent not to retry; got %q", call.Name, resultText(res))
		}
	}
}

// TestMCPReadOnlyKeyAllowedOnReadTools: the same session must still recall, or
// the credential is useless rather than merely unprivileged.
func TestMCPReadOnlyKeyAllowedOnReadTools(t *testing.T) {
	h := newReadOnlyMCP(t)
	ctx := context.Background()
	cs, err := connectHTTP(t, h, "acme", "", "tok-ci")
	if err != nil {
		t.Fatalf("connect with read-only key: %v", err)
	}
	for _, call := range []mcpsdk.CallToolParams{
		{Name: "memory_recall", Arguments: map[string]any{"query": "anything"}},
		{Name: "memory_briefing", Arguments: map[string]any{}},
		{Name: "memory_list", Arguments: map[string]any{}},
	} {
		res, err := cs.CallTool(ctx, &call)
		if err != nil {
			t.Errorf("%s with a read-only key: unexpected transport error %v", call.Name, err)
			continue
		}
		if res.IsError && strings.Contains(strings.ToLower(resultText(res)), "read-only") {
			t.Errorf("%s must not be refused for a read-only key; got %q", call.Name, resultText(res))
		}
	}
}

// TestMCPReadWriteKeyStillWrites guards against the gate leaking onto every
// named key rather than only read-only ones.
func TestMCPReadWriteKeyStillWrites(t *testing.T) {
	h := newReadOnlyMCP(t)
	ctx := context.Background()
	cs, err := connectHTTP(t, h, "acme", "", "tok-bot")
	if err != nil {
		t.Fatalf("connect with read-write key: %v", err)
	}
	res, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      "memory_remember",
		Arguments: map[string]any{"content": "an ordinary mcp write", "tier": "semantic"},
	})
	if err != nil || res.IsError {
		t.Fatalf("memory_remember with a read-write key: err=%v res=%q", err, resultText(res))
	}
}

// TestMCPWriteToolsStayVisibleToReadOnlySession pins the deliberate choice to
// reject at call time rather than hide the tools: the tool list is identical for
// a read-only and a read-write session. Hiding them would be a reasonable design
// too, but it is NOT the one taken here, and silently switching would change the
// agent-visible contract.
func TestMCPWriteToolsStayVisibleToReadOnlySession(t *testing.T) {
	h := newReadOnlyMCP(t)
	ctx := context.Background()

	listFor := func(token string) []string {
		cs, err := connectHTTP(t, h, "acme", "", token)
		if err != nil {
			t.Fatalf("connect %s: %v", token, err)
		}
		res, err := cs.ListTools(ctx, nil)
		if err != nil {
			t.Fatalf("ListTools %s: %v", token, err)
		}
		names := make([]string, 0, len(res.Tools))
		for _, tool := range res.Tools {
			names = append(names, tool.Name)
		}
		sort.Strings(names)
		return names
	}

	ro, rw := listFor("tok-ci"), listFor("tok-bot")
	if strings.Join(ro, ",") != strings.Join(rw, ",") {
		t.Fatalf("read-only session tool list %v differs from read-write %v; "+
			"this build rejects write tools at call time and does not hide them", ro, rw)
	}
	for _, call := range mcpWriteToolCalls {
		found := false
		for _, n := range ro {
			if n == call.Name {
				found = true
			}
		}
		if !found {
			t.Errorf("%s missing from a read-only session's tool list", call.Name)
		}
	}
}

// TestMCPReadToolAllowlistMatchesAnnotations is the fail-closed gate for the MCP
// surface, the counterpart of the spec-derived test on the REST side.
//
// Every tool is already annotated readOnly / additive / destructive at
// registration, and cmd/gendocs treats those annotations as authoritative when
// it labels the tool reference. This asserts the enforcement allowlist agrees
// with them exactly: a tool annotated read-only must be callable by a read-only
// session, and a tool that is not must be refused.
//
// So adding a write tool without allowlisting it is caught here rather than
// becoming a silent hole — and the default for an unlisted tool is refusal.
func TestMCPReadToolAllowlistMatchesAnnotations(t *testing.T) {
	h := newReadOnlyMCP(t)
	ctx := context.Background()
	cs, err := connectHTTP(t, h, "acme", "", "tok-ci")
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(res.Tools) == 0 {
		t.Fatalf("no tools registered — this gate would be checking nothing")
	}
	for _, tool := range res.Tools {
		annotatedRead := tool.Annotations != nil && tool.Annotations.ReadOnlyHint
		allowed := meminimcp.IsReadTool(tool.Name)
		if allowed != annotatedRead {
			t.Errorf("tool %q: allowlist says read=%v but its ReadOnlyHint annotation says read=%v — "+
				"a new tool must be classified in BOTH places, and they must agree",
				tool.Name, allowed, annotatedRead)
		}
	}
}

// resultText flattens a tool result's text content for assertions.
func resultText(res *mcpsdk.CallToolResult) string {
	if res == nil {
		return ""
	}
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcpsdk.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}
