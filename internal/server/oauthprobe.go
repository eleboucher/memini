package server

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/eleboucher/memini/internal/httputil"
)

// oauthProbeMessage is the whole point of this file. memini has no OAuth, so an
// MCP client only probes these routes after a request failed to authenticate —
// a bare 401 sends Claude Code into OAuth discovery, which ends at POST
// /register. Whatever this endpoint answers *is* the message the user sees
// (Claude Code parses the JSON body and surfaces the text verbatim), so it says
// what actually went wrong and where to look, instead of the blank "{}" that
// produced "Dynamic Client Registration rejected (HTTP 404): {}".
//
// Keep it one short, searchable line: it is rendered in a client error toast.
// The README section title is single-quoted on purpose — this string is
// delivered inside a JSON body, where double quotes come back out as \" and
// clutter the one line the user actually reads.
const oauthProbeMessage = "memini does not use OAuth (no valid bearer reached the server). " +
	"plugin: run /memini:status, see plugin README 'Claude Code 2.1.238 and credential env vars'; " +
	"behind a proxy: check it forwards Authorization"

// handleOAuthProbe answers every OAuth discovery/registration probe with a
// 404 carrying oauthProbeMessage. 404 (not 501/400) is deliberate: it is what
// "this server has no such endpoint" means to a client, and the status is the
// one clients already handle by falling back to static bearer auth.
func handleOAuthProbe(w http.ResponseWriter, _ *http.Request) {
	httputil.Error(w, http.StatusNotFound, oauthProbeMessage)
}

// registerOAuthProbes mounts the probe routes on r. They are registered
// unconditionally, next to /healthz, and before any catch-all: chi prefers
// these static routes over the SPA's "/*", so all three UI modes (no UI,
// main-port UI, dedicated UIAddr — whose listener delegates matched routes back
// to this router) answer identically. The SPA keeps its own blanket
// ".well-known/*" 404 as defense in depth for standalone ui.Mount.
//
// The "/*" variants cover RFC 9728's path-suffixed form, where the client
// appends the protected resource's path (e.g.
// /.well-known/oauth-protected-resource/mcp).
func registerOAuthProbes(r chi.Router) {
	r.Post("/register", handleOAuthProbe)
	for _, p := range []string{
		"/.well-known/oauth-protected-resource",
		"/.well-known/oauth-authorization-server",
		"/.well-known/openid-configuration",
	} {
		r.Get(p, handleOAuthProbe)
		r.Get(p+"/*", handleOAuthProbe)
	}
}
