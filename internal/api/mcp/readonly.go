package mcp

import (
	"context"
	"fmt"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// readTools is the ALLOWLIST of tools a read-only credential
// (store.APIKey.ReadOnly) may call. It mirrors the readOnly ToolAnnotations
// applied at registration, and TestMCPReadToolAllowlistMatchesAnnotations
// asserts the two agree for every registered tool — so a tool added on one side
// only fails CI instead of silently becoming reachable.
//
// Like the REST classifier, the default is denial: a tool absent from this map
// is refused, so forgetting to classify a new write tool over-restricts (a loud
// refusal) rather than granting write access.
var readTools = map[string]bool{
	"memory_recall":   true,
	"memory_briefing": true,
	"memory_answer":   true,
	"memory_list":     true,
	"memory_get":      true,
	"memory_history":  true,
}

// IsReadTool reports whether name is a tool a read-only session may call. It is
// exported for the parity test that cross-checks this allowlist against the
// tools' own ReadOnlyHint annotations.
func IsReadTool(name string) bool { return readTools[name] }

// ServerOption customizes NewServer. Options exist so the common call sites —
// stdio, the docs generator, and a pile of tests — stay unchanged as
// per-session capabilities accumulate, rather than growing another positional
// bool argument each time.
type ServerOption func(*serverOpts)

type serverOpts struct {
	readOnly bool
}

// WithReadOnly marks the session as authenticated by a read-only credential, so
// every tool outside readTools is refused at call time.
func WithReadOnly(v bool) ServerOption {
	return func(o *serverOpts) { o.readOnly = v }
}

// readOnlyMiddleware refuses a tools/call for any tool outside readTools.
//
// It is a receiving middleware rather than a guard inside each write handler
// for the same reason the REST side uses one middleware instead of ~20
// per-handler calls: one choke point that a newly added tool cannot forget to
// join. Non-tools/call methods (initialize, tools/list, ...) pass through
// untouched, which is what keeps the write tools VISIBLE to a read-only session
// — this build refuses them at call time rather than hiding them (see
// TestMCPWriteToolsStayVisibleToReadOnlySession).
//
// The refusal is a CallToolResult with IsError set, NOT a returned error.
// Returning an error makes the SDK emit a protocol-level failure, which an agent
// reads as "the call broke" — the kind of thing worth retrying. An error tool
// RESULT is ordinary tool output the model sees and can act on, so it learns
// once that this credential cannot write and stops attempting it. That
// distinction is the whole point for an unattended agent whose hooks would
// otherwise retry a write every turn.
func readOnlyMiddleware(next mcpsdk.MethodHandler) mcpsdk.MethodHandler {
	return func(ctx context.Context, method string, req mcpsdk.Request) (mcpsdk.Result, error) {
		call, ok := req.(*mcpsdk.CallToolRequest)
		if !ok || call.Params == nil || IsReadTool(call.Params.Name) {
			return next(ctx, method, req)
		}
		return &mcpsdk.CallToolResult{
			IsError: true,
			Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: fmt.Sprintf(
				"read-only credential: this API key has read_only=true and cannot call %q, "+
					"which modifies stored memories. Do not retry — reads (memory_recall, "+
					"memory_briefing, memory_get, memory_list, memory_history) still work.",
				call.Params.Name)}},
		}, nil
	}
}
