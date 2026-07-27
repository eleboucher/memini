package rest

import (
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/eleboucher/memini/internal/httputil"
)

// readShapedPaths are the /v1 endpoints whose HTTP method does not identify
// them as reads: each is a query that needs a request body, so it is a POST
// despite mutating nothing.
//
//   - /v1/search and /v1/answer are retrieval. /v1/answer spends LLM tokens,
//     but spending tokens is not mutating stored state.
//   - /v1/handshake is deliberately side-effect-free (see Server.Handshake) and
//     is what every client calls to resolve its namespace before doing anything
//     else. Denying it would make a read-only credential unusable rather than
//     merely unprivileged.
//
// Entries must be parameter-free: isReadRequest matches a concrete request path
// literally, and chi has not resolved the route pattern yet (see below).
var readShapedPaths = map[string]bool{
	"/v1/search":    true,
	"/v1/answer":    true,
	"/v1/handshake": true,
}

// isReadRequest reports whether (method, path) is a read, and is deliberately
// an ALLOWLIST: safe methods, plus the handful of read-shaped POSTs above.
// Everything else — including a path this function has never heard of — is a
// write.
//
// That default is the point. A mutating endpoint added later is refused for
// read-only credentials until someone consciously classifies it, so the failure
// mode of forgetting is an over-restriction (a loud 403 on something that should
// have been readable) rather than a silent grant of write access. The
// spec-derived test in readonly_test.go turns "someone forgot" into a CI
// failure rather than a latent hole.
//
// It matches on the request path rather than chi's route pattern because chi
// resolves routes AFTER the middleware chain runs, so RouteContext().RoutePattern()
// is still empty here. Hence the parameter-free constraint on readShapedPaths.
func isReadRequest(method, path string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	return readShapedPaths[strings.TrimSuffix(path, "/")]
}

// readOnlyMiddleware enforces store.APIKey.ReadOnly: a request authenticated by
// a read-only credential may only issue reads (isReadRequest), and is refused
// 403 otherwise.
//
// It runs immediately after authMiddleware so a refused write does no further
// work — no attribution, no namespace resolution, no handler.
//
// A caller with NO principal on the context is never read-only: that is the
// admin env key or dev/bootstrap mode, both of which authenticate without a
// principal and so cannot carry per-key capability bits. This mirrors
// requireAdmin's treatment of the nil principal exactly.
//
// Enforcing here rather than per-handler is what makes the gate hold as the API
// grows: there is one choke point every /v1 route passes through, instead of ~20
// call sites a new endpoint can forget to join.
//
// Mount registers those routes inside a chi Group, and a Group's middleware runs
// only for paths matching a route registered in that group — so an unknown path
// 404s from the parent router without reaching this gate. That is deliberate:
// the gate never manufactures a 403 for an endpoint that does not exist, so it
// cannot mask a routing bug (see TestReadOnlyGateDoesNotMaskRouting).
//
// Reads still reinforce (access counts, last_accessed_at) and still append to
// the activity log. That is internal relevance bookkeeping, not caller-facing
// mutation: "read-only" bounds what the CALLER can change, not whether serving
// a read leaves a trace. Do not "fix" this into suppressing reinforcement — it
// would quietly degrade ranking for every read-only consumer.
func (a AuthConfig) readOnlyMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFromContext(r.Context())
		if !ok || !p.ReadOnly || isReadRequest(r.Method, r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		// Debug, not warn: for a read-only credential this is expected steady
		// state, not an anomaly, and an unattended agent whose hooks still
		// attempt writes would emit one line per turn at a louder level. The
		// 403 body already tells the caller why.
		slog.DebugContext(r.Context(), "read-only credential refused a mutating request",
			"key", p.Name, "method", r.Method, "path", r.URL.Path)
		httputil.Error(w, http.StatusForbidden, fmt.Sprintf(
			"read-only credential: API key %q has read_only=true and cannot perform mutating requests", p.Name))
	})
}
