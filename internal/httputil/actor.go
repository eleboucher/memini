package httputil

import (
	"context"
	"sync"
)

// The actor holder carries "who authenticated this request" OUTWARD to the
// request logger. Context values only flow inward — the access logger wraps the
// whole stack, auth runs several middlewares deeper, so anything auth puts on
// its (child) context is invisible to the logger's (parent) context. The
// holder inverts that: the logger installs a mutable box up front, auth writes
// into the box through whatever derived context it holds, and the logger reads
// the box back after the handler returns.
//
// It lives here rather than in internal/server because both REST and MCP must
// write into it and server mounts them both — server → rest/mcp → httputil is
// the one import direction with no cycle.
type actorHolder struct {
	mu   sync.Mutex
	name string
	kind string
	set  bool
}

type actorHolderKey struct{}

// WithActorHolder returns ctx carrying a fresh, empty actor holder for
// RecordActor/RecordedActor to meet through. Installed once per request by the
// outermost request logger.
func WithActorHolder(ctx context.Context) context.Context {
	return context.WithValue(ctx, actorHolderKey{}, &actorHolder{})
}

// RecordActor records the authenticated actor — name is the named key that
// authenticated ("" for the admin env key or auth-disabled dev mode), kind the
// same "key"/"env"/"none" classification the activity log's attribution uses —
// into the holder, if the context carries one. A context with no holder (a
// surface not wrapped by the request logger) is a silent no-op: recording is
// best-effort telemetry, never a reason to fail a request.
func RecordActor(ctx context.Context, name, kind string) {
	h, _ := ctx.Value(actorHolderKey{}).(*actorHolder)
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.name, h.kind, h.set = name, kind, true
}

// RecordedActor returns what RecordActor stored for this request, ok=false
// when no holder is present or nothing was recorded (the request never reached
// an authenticating middleware — e.g. rejected before auth, or a route outside
// the authenticated groups).
func RecordedActor(ctx context.Context) (name, kind string, ok bool) {
	h, _ := ctx.Value(actorHolderKey{}).(*actorHolder)
	if h == nil {
		return "", "", false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.name, h.kind, h.set
}
