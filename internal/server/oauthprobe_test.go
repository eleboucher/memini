package server_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// oauthProbePaths is every discovery/registration path an MCP client probes
// when it decides (wrongly) that memini speaks OAuth. Each must answer with the
// self-diagnosing 404 rather than falling through to the SPA catch-all's blank
// "{}", which is what produces the cryptic "Dynamic Client Registration
// rejected (HTTP 404): {}" in Claude Code.
var oauthProbePaths = []struct {
	method string
	path   string
}{
	{http.MethodPost, "/register"},
	{http.MethodGet, "/.well-known/oauth-protected-resource"},
	// RFC 9728 path-suffixed variants: clients append the resource path.
	{http.MethodGet, "/.well-known/oauth-protected-resource/mcp"},
	{http.MethodGet, "/.well-known/oauth-authorization-server"},
	{http.MethodGet, "/.well-known/oauth-authorization-server/mcp"},
	{http.MethodGet, "/.well-known/openid-configuration"},
	{http.MethodGet, "/.well-known/openid-configuration/mcp"},
}

// assertOAuthProbeResponse checks the contract the diagnosis depends on: a 404
// with a JSON content type whose body unmarshals to {"error": …} carrying the
// searchable marker. Claude Code parses this body and surfaces the text
// verbatim, so the shape is load-bearing, not cosmetic.
func assertOAuthProbeResponse(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal body %q: %v", rec.Body.String(), err)
	}
	if !strings.Contains(body.Error, "does not use OAuth") {
		t.Errorf("error = %q, want it to explain that memini does not use OAuth", body.Error)
	}
}

func TestOAuthProbeRoutes(t *testing.T) {
	for _, tc := range oauthProbePaths {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			srv := newTestServer(t)
			rec := httptest.NewRecorder()
			srv.Router().ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
			assertOAuthProbeResponse(t, rec)
		})
	}
}

// TestOAuthProbeBeatsMountedSPA is the regression that matters: with the SPA
// mounted as the "/*" catch-all (the single-listener default), the probe routes
// must still win, so clients get the diagnosis instead of the SPA handler's
// blank "{}" 404. The dedicated-UIAddr mode reaches the same handlers via the
// UI listener's mux.Match delegation back to this router.
func TestOAuthProbeBeatsMountedSPA(t *testing.T) {
	for _, tc := range oauthProbePaths {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			srv := newTestServer(t)
			srv.MountUI(spa())
			rec := httptest.NewRecorder()
			srv.Router().ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
			if rec.Body.String() == "SPA" {
				t.Fatalf("%s %s reached the SPA catch-all, want the OAuth-probe handler", tc.method, tc.path)
			}
			assertOAuthProbeResponse(t, rec)
		})
	}
}
