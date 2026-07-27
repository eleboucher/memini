package rest_test

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/eleboucher/memini/internal/api/rest"
	"github.com/eleboucher/memini/internal/store"
)

// --- The fail-closed classification gate ---------------------------------
//
// rest.IsReadRequest is an ALLOWLIST: a read-only credential may issue
// GET/HEAD/OPTIONS plus a handful of read-shaped POSTs, and nothing else. The
// test below derives the full endpoint set from api/openapi.yaml (the source of
// truth every other artifact generates from) and asserts the classifier agrees
// with expectedSpecReads for every one of them.
//
// The point is the direction of failure. Adding an endpoint to the spec without
// touching expectedSpecReads fails this test, forcing a conscious read-or-write
// call. If someone ignores that and ships anyway, the default is DENIAL — the
// new endpoint is unreachable for read-only keys until it is allowlisted, which
// is a loud 403 rather than a silent write capability.

// expectedSpecReads is every /v1 endpoint a read-only credential may reach,
// keyed "METHOD /path" exactly as api/openapi.yaml spells it. Everything else
// in the spec must classify as a write.
//
// The three POSTs here are read-shaped despite their verb:
//   - /v1/search and /v1/answer are queries that happen to need a request body
//     (/v1/answer spends LLM tokens, but spending tokens is not mutating state).
//   - /v1/handshake is documented side-effect-free (see Server.Handshake) and is
//     what every client calls to bootstrap. Denying it would make a read-only
//     credential unusable rather than merely unprivileged.
var expectedSpecReads = map[string]bool{
	"GET /v1/memories":              true,
	"GET /v1/memories/{id}":         true,
	"GET /v1/memories/{id}/history": true,
	"POST /v1/search":               true,
	"POST /v1/answer":               true,
	"POST /v1/handshake":            true,
	"GET /v1/namespaces":            true,
	"GET /v1/namespaces/briefing":   true,
	"GET /v1/namespaces/readset":    true,
	"GET /v1/links":                 true,
	"GET /v1/stats":                 true,
	"GET /v1/activity":              true,
	"GET /v1/self":                  true,
	"GET /v1/pins":                  true,
	"GET /v1/keys":                  true,
	"GET /v1/settings/defaults":     true,
}

// specEndpoints parses api/openapi.yaml and returns every ("METHOD /path")
// operation under /v1. Non-/v1 paths (/healthz, /readyz) are excluded: they opt
// out of bearerAuth in the spec and are mounted outside rest.Mount's group, so
// the read-only middleware never sees them.
func specEndpoints(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "api", "openapi.yaml"))
	if err != nil {
		t.Fatalf("read openapi.yaml: %v", err)
	}
	var doc struct {
		Paths map[string]map[string]yaml.Node `yaml:"paths"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse openapi.yaml: %v", err)
	}
	if len(doc.Paths) == 0 {
		t.Fatalf("openapi.yaml parsed to zero paths — the spec shape changed and this gate is no longer checking anything")
	}
	verbs := map[string]bool{
		"get": true, "put": true, "post": true, "delete": true,
		"patch": true, "head": true, "options": true,
	}
	var out []string
	for path, ops := range doc.Paths {
		if !strings.HasPrefix(path, "/v1/") {
			continue
		}
		for verb := range ops {
			if !verbs[strings.ToLower(verb)] {
				continue // "parameters", "summary", etc. — not an operation
			}
			out = append(out, strings.ToUpper(verb)+" "+path)
		}
	}
	sort.Strings(out)
	return out
}

// concretePath substitutes a value for every {param} placeholder, so the
// classifier is exercised against paths shaped like real request URLs rather
// than spec templates.
func concretePath(specPath string) string {
	out := specPath
	for {
		open := strings.Index(out, "{")
		if open < 0 {
			return out
		}
		closeIdx := strings.Index(out[open:], "}")
		if closeIdx < 0 {
			return out
		}
		out = out[:open] + "sample" + out[open+closeIdx+1:]
	}
}

func TestIsReadRequestClassifiesEverySpecEndpoint(t *testing.T) {
	endpoints := specEndpoints(t)
	if len(endpoints) == 0 {
		t.Fatalf("no /v1 endpoints found in the spec")
	}
	seen := map[string]bool{}
	for _, ep := range endpoints {
		seen[ep] = true
		method, specPath, ok := strings.Cut(ep, " ")
		if !ok {
			t.Fatalf("malformed endpoint key %q", ep)
		}
		want := expectedSpecReads[ep]
		got := rest.IsReadRequest(method, concretePath(specPath))
		if got != want {
			verdict := map[bool]string{true: "read", false: "write"}
			t.Errorf("%s classified as %s, want %s — if this endpoint is new, "+
				"decide deliberately whether a read-only credential may call it and "+
				"update expectedSpecReads (and isReadRequest's allowlist for a read-shaped POST)",
				ep, verdict[got], verdict[want])
		}
	}
	// Catch stale entries: an allowlisted endpoint that no longer exists in the
	// spec is a typo or a leftover, and would silently stop protecting anything.
	for ep := range expectedSpecReads {
		if !seen[ep] {
			t.Errorf("expectedSpecReads names %q, which is not in api/openapi.yaml (renamed or removed?)", ep)
		}
	}
}

// TestIsReadRequestDeniesUnknownMutatingPaths pins the fail-closed default: a
// path the allowlist has never heard of is a write, so a future endpoint is
// denied until someone opts it in.
func TestIsReadRequestDeniesUnknownMutatingPaths(t *testing.T) {
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/v1/some-future-endpoint"},
		{http.MethodPut, "/v1/memories"},
		{http.MethodDelete, "/v1/anything"},
		{http.MethodPatch, "/v1/whatever"},
	} {
		if rest.IsReadRequest(tc.method, tc.path) {
			t.Errorf("%s %s classified as a read; unknown mutating paths must default to write", tc.method, tc.path)
		}
	}
}

// TestIsReadRequestIgnoresTrailingSlash: the classifier matches on the request
// path, so /v1/search/ must not slip past the allowlist comparison and be
// mistaken for an unknown (and therefore denied) endpoint.
func TestIsReadRequestIgnoresTrailingSlash(t *testing.T) {
	if !rest.IsReadRequest(http.MethodPost, "/v1/search/") {
		t.Errorf("POST /v1/search/ must classify as a read, same as without the trailing slash")
	}
}

// --- Enforcement over real requests --------------------------------------

// readOnlyWriteEndpoints is every mutating /v1 endpoint, as a request a
// read-only credential might actually issue. Bodies are deliberately minimal:
// the gate must reject before any handler validates the payload, so a 400 here
// would itself be a failure.
var readOnlyWriteEndpoints = []struct {
	method, path string
	body         any
}{
	{http.MethodPost, "/v1/memories", map[string]any{"content": "x", "tier": "semantic"}},
	{http.MethodPatch, "/v1/memories/some-id", map[string]any{"content": "x"}},
	{http.MethodDelete, "/v1/memories/some-id", nil},
	{http.MethodDelete, "/v1/memories?tag=x", nil},
	{http.MethodPost, "/v1/memories/some-id/supersede", map[string]any{"content": "x"}},
	{http.MethodPost, "/v1/memories/some-id/reassign", map[string]any{"namespace": "other"}},
	{http.MethodDelete, "/v1/namespaces", nil},
	{http.MethodPost, "/v1/namespaces/move", map[string]any{"from": "a", "to": "b"}},
	{http.MethodPost, "/v1/namespaces/split", map[string]any{"from": "a", "to": "b"}},
	{http.MethodPost, "/v1/links", map[string]any{"src_ns": "a", "dst_ns": "b"}},
	{http.MethodDelete, "/v1/links", nil},
	{http.MethodPost, "/v1/fsck", nil},
	{http.MethodPost, "/v1/dedup", nil},
	{http.MethodPost, "/v1/activity/injected", map[string]any{"namespace": "a"}},
	{http.MethodPut, "/v1/self/settings", map[string]any{}},
	{http.MethodPut, "/v1/pins", map[string]any{"key": "k", "namespace": "a"}},
	{http.MethodDelete, "/v1/pins", nil},
	{http.MethodPost, "/v1/keys", map[string]any{"name": "n"}},
	{http.MethodPatch, "/v1/keys/other", map[string]any{}},
	{http.MethodDelete, "/v1/keys/other", nil},
	{http.MethodPost, "/v1/keys/other/rotate", nil},
	{http.MethodPut, "/v1/settings/defaults", map[string]any{}},
}

// newReadOnlyServer builds a REST server with two named keys: "ci" is
// read-only, "bot" is an ordinary read-write key. adminKey may be "".
func newReadOnlyServer(t *testing.T, adminKey string) http.Handler {
	t.Helper()
	h, ks := newServerWithKeyStore(t, adminKey)
	ctx := context.Background()
	if err := ks.PutAPIKey(ctx, store.APIKey{
		Name: "ci", Hash: hashOf("tok-ci"), ReadOnly: true, Admin: true,
	}); err != nil {
		t.Fatalf("PutAPIKey(ci): %v", err)
	}
	if err := ks.PutAPIKey(ctx, store.APIKey{
		Name: "bot", Hash: hashOf("tok-bot"), Admin: true,
	}); err != nil {
		t.Fatalf("PutAPIKey(bot): %v", err)
	}
	return h
}

// TestReadOnlyKeyDeniedOnEveryWriteEndpoint is the core enforcement matrix: the
// read-only credential is also admin, so an admin gate can never be what
// rejects it — only the read-only gate can.
func TestReadOnlyKeyDeniedOnEveryWriteEndpoint(t *testing.T) {
	h := newReadOnlyServer(t, "")
	for _, ep := range readOnlyWriteEndpoints {
		rec := do(t, h, ep.method, ep.path, "alice", "tok-ci", ep.body)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s with a read-only key: want 403, got %d (%s)",
				ep.method, ep.path, rec.Code, strings.TrimSpace(rec.Body.String()))
		}
	}
}

// TestReadOnlyDenialNamesTheCredential: the 403 body must say why, or an
// operator sees a bare Forbidden and reaches for the admin gate instead.
func TestReadOnlyDenialNamesTheCredential(t *testing.T) {
	h := newReadOnlyServer(t, "")
	rec := do(t, h, http.MethodPost, "/v1/memories", "alice", "tok-ci",
		map[string]any{"content": "x", "tier": "semantic"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d", rec.Code)
	}
	if !strings.Contains(strings.ToLower(rec.Body.String()), "read-only") {
		t.Fatalf("403 body %q must name the read-only credential as the reason", rec.Body.String())
	}
}

// TestReadOnlyKeyAllowedOnReads: the same credential must still read. A gate
// that denied reads too would be indistinguishable from simply revoking the key.
func TestReadOnlyKeyAllowedOnReads(t *testing.T) {
	h := newReadOnlyServer(t, "")
	for _, tc := range []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, "/v1/memories", nil},
		{http.MethodPost, "/v1/search", map[string]any{"query": "x"}},
		{http.MethodGet, "/v1/namespaces", nil},
		{http.MethodGet, "/v1/stats", nil},
		{http.MethodGet, "/v1/self", nil},
		{http.MethodGet, "/v1/activity", nil},
		{http.MethodGet, "/v1/pins", nil},
		{http.MethodGet, "/v1/keys", nil},
		{http.MethodPost, "/v1/handshake", map[string]any{"project": map[string]any{"cwd_basename": "repo"}}},
	} {
		rec := do(t, h, tc.method, tc.path, "alice", "tok-ci", tc.body)
		if rec.Code == http.StatusForbidden {
			t.Errorf("%s %s with a read-only key: want it allowed, got 403 (%s)",
				tc.method, tc.path, strings.TrimSpace(rec.Body.String()))
		}
	}
}

// TestNonReadOnlyNamedKeyStillWrites guards against the gate leaking onto every
// named principal instead of only read-only ones.
func TestNonReadOnlyNamedKeyStillWrites(t *testing.T) {
	h := newReadOnlyServer(t, "")
	rec := do(t, h, http.MethodPost, "/v1/memories", "alice", "tok-bot",
		map[string]any{"content": "a normal write", "tier": "semantic"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("read-write named key: want 201, got %d (%s)", rec.Code, rec.Body)
	}
}

// TestEnvKeyUnaffectedByReadOnlyGate: the admin env key authenticates with NO
// principal, so it can carry no capability bits and must stay fully privileged.
func TestEnvKeyUnaffectedByReadOnlyGate(t *testing.T) {
	h := newReadOnlyServer(t, "admin-secret")
	rec := do(t, h, http.MethodPost, "/v1/memories", "alice", "admin-secret",
		map[string]any{"content": "an env-key write", "tier": "semantic"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("admin env key: want 201, got %d (%s)", rec.Code, rec.Body)
	}
}

// TestDevModeUnaffectedByReadOnlyGate: dev mode — no admin key configured and
// an empty key table — also resolves to a nil principal and must keep writing.
func TestDevModeUnaffectedByReadOnlyGate(t *testing.T) {
	// A key store that stays EMPTY is what makes this dev mode: the moment any
	// row exists, table auth becomes mandatory (see apiauth.Config.Authenticate).
	h, _ := newServerWithKeyStore(t, "")
	rec := do(t, h, http.MethodPost, "/v1/memories", "alice", "",
		map[string]any{"content": "a dev-mode write", "tier": "semantic"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("dev mode: want 201, got %d (%s)", rec.Code, rec.Body)
	}
}

// TestReadOnlyGateDoesNotMaskRouting pins how the gate composes with chi's
// routing. rest.Mount registers the /v1 routes inside a chi Group, and a Group's
// middleware runs only for paths that match a route registered IN that group —
// so an unknown path 404s from the parent router without the gate ever seeing
// it.
//
// That is the behavior we want: the gate never manufactures a 403 for an
// endpoint that does not exist, so it cannot mask a routing bug or make
// endpoint-probing responses differ from a normal key's. The security property
// lives elsewhere — every route actually registered in the group passes through
// the gate, which is what TestReadOnlyKeyDeniedOnEveryWriteEndpoint proves
// across the full mutating surface.
func TestReadOnlyGateDoesNotMaskRouting(t *testing.T) {
	h := newReadOnlyServer(t, "")
	rec := do(t, h, http.MethodPost, "/v1/no-such-endpoint", "alice", "tok-ci", map[string]any{})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unrouted POST with a read-only key: want 404 from routing, got %d (%s)",
			rec.Code, strings.TrimSpace(rec.Body.String()))
	}
	// And it 404s identically for a read-write key — the gate introduces no
	// observable difference for a nonexistent endpoint.
	rec = do(t, h, http.MethodPost, "/v1/no-such-endpoint", "alice", "tok-bot", map[string]any{})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unrouted POST with a read-write key: want 404, got %d", rec.Code)
	}
}

// --- Wire surface: the capability must be visible, settable and preserved ---

// TestSelfReportsReadOnlyCapability: GET /v1/self is how a client learns it must
// not attempt writes (see CallerIdentity.read_only). Getting this wrong makes an
// unattended agent retry writes forever and log a 403 per turn.
func TestSelfReportsReadOnlyCapability(t *testing.T) {
	h := newReadOnlyServer(t, "admin-secret")
	for _, tc := range []struct {
		label, token string
		wantReadOnly bool
	}{
		{"read-only named key", "tok-ci", true},
		{"read-write named key", "tok-bot", false},
		{"admin env key (no principal)", "admin-secret", false},
	} {
		rec := do(t, h, http.MethodGet, "/v1/self", "alice", tc.token, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: GET /v1/self want 200, got %d (%s)", tc.label, rec.Code, rec.Body)
		}
		var out struct {
			Identity struct {
				ReadOnly bool `json:"read_only"`
			} `json:"identity"`
		}
		mustJSON(t, rec, &out)
		if out.Identity.ReadOnly != tc.wantReadOnly {
			t.Errorf("%s: identity.read_only = %v, want %v", tc.label, out.Identity.ReadOnly, tc.wantReadOnly)
		}
	}
}

// TestHandshakeReportsReadOnlyCapability: the handshake is the one call a client
// makes before anything else, so the capability has to ride along with it — a
// client that had to make a second call to learn it would race its own writes.
func TestHandshakeReportsReadOnlyCapability(t *testing.T) {
	h := newReadOnlyServer(t, "")
	rec := do(t, h, http.MethodPost, "/v1/handshake", "alice", "tok-ci",
		map[string]any{"project": map[string]any{"cwd_basename": "repo"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("handshake: want 200, got %d (%s)", rec.Code, rec.Body)
	}
	var out struct {
		Identity struct {
			ReadOnly bool `json:"read_only"`
		} `json:"identity"`
	}
	mustJSON(t, rec, &out)
	if !out.Identity.ReadOnly {
		t.Errorf("handshake identity.read_only = false, want true for a read-only credential")
	}
}

// TestListApiKeysExposesReadOnly: an operator must be able to see which keys are
// read-only, or the capability is unauditable.
func TestListApiKeysExposesReadOnly(t *testing.T) {
	h, ks := newKeysTestServer(t, "admin-secret", "")
	ctx := context.Background()
	if err := ks.PutAPIKey(ctx, store.APIKey{Name: "ci", Hash: hashOf("tok-ci"), ReadOnly: true}); err != nil {
		t.Fatalf("seed ci: %v", err)
	}
	if err := ks.PutAPIKey(ctx, store.APIKey{Name: "bot", Hash: hashOf("tok-bot")}); err != nil {
		t.Fatalf("seed bot: %v", err)
	}
	rec := do(t, h, http.MethodGet, "/v1/keys", "", "admin-secret", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list keys: want 200, got %d (%s)", rec.Code, rec.Body)
	}
	var out struct {
		Keys []struct {
			Name     string `json:"name"`
			ReadOnly bool   `json:"read_only"`
		} `json:"keys"`
	}
	mustJSON(t, rec, &out)
	got := map[string]bool{}
	for _, k := range out.Keys {
		got[k.Name] = k.ReadOnly
	}
	if !got["ci"] {
		t.Errorf("list: ci read_only = false, want true")
	}
	if got["bot"] {
		t.Errorf("list: bot read_only = true, want false")
	}
}

// TestCreateApiKeyReadOnlyFlag covers POST /v1/keys honouring read_only, in both
// directions and when omitted.
func TestCreateApiKeyReadOnlyFlag(t *testing.T) {
	h, ks := newKeysTestServer(t, "admin-secret", "")
	for _, tc := range []struct {
		name string
		body map[string]any
		want bool
	}{
		{"ro", map[string]any{"name": "ro", "read_only": true}, true},
		{"rw", map[string]any{"name": "rw", "read_only": false}, false},
		{"omitted", map[string]any{"name": "omitted"}, false},
	} {
		rec := do(t, h, http.MethodPost, "/v1/keys", "", "admin-secret", tc.body)
		if rec.Code != http.StatusCreated {
			t.Fatalf("%s: create want 201, got %d (%s)", tc.name, rec.Code, rec.Body)
		}
		var out struct {
			ReadOnly bool `json:"read_only"`
		}
		mustJSON(t, rec, &out)
		if out.ReadOnly != tc.want {
			t.Errorf("%s: response read_only = %v, want %v", tc.name, out.ReadOnly, tc.want)
		}
		if k := findKey(t, ks, tc.name); k.ReadOnly != tc.want {
			t.Errorf("%s: stored ReadOnly = %v, want %v", tc.name, k.ReadOnly, tc.want)
		}
	}
}

// TestUpdateApiKeyReadOnlyPreserveAndSelfGuard covers PATCH /v1/keys/{name}:
// preserve-unspecified in both directions, cross-key edits, and the self-guard.
//
// The self-guard matters more here than for admin: a key that imposes read_only
// on ITSELF can no longer reach this endpoint to lift it (the read-only gate
// refuses the PATCH), so it would be a one-way trip recoverable only out-of-band.
func TestUpdateApiKeyReadOnlyPreserveAndSelfGuard(t *testing.T) {
	h, ks := newKeysTestServer(t, "", "")
	ctx := context.Background()
	if err := ks.PutAPIKey(ctx, store.APIKey{Name: "boss", Hash: hashOf("tok-boss"), Admin: true}); err != nil {
		t.Fatalf("seed boss: %v", err)
	}
	if err := ks.PutAPIKey(ctx, store.APIKey{Name: "worker", Hash: hashOf("tok-worker")}); err != nil {
		t.Fatalf("seed worker: %v", err)
	}

	// Self-impose read_only → 409, and the key is untouched.
	rec := do(t, h, http.MethodPatch, "/v1/keys/boss", "", "tok-boss", map[string]any{"read_only": true})
	if rec.Code != http.StatusConflict {
		t.Fatalf("self-impose read_only: want 409, got %d (%s)", rec.Code, rec.Body)
	}
	if k := findKey(t, ks, "boss"); k.ReadOnly {
		t.Fatalf("boss must be unchanged by the rejected self-edit, got ReadOnly=true")
	}

	// Cross-key: boss imposes read_only on worker, then lifts it.
	rec = do(t, h, http.MethodPatch, "/v1/keys/worker", "", "tok-boss", map[string]any{"read_only": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("impose read_only on worker: want 200, got %d (%s)", rec.Code, rec.Body)
	}
	if k := findKey(t, ks, "worker"); !k.ReadOnly {
		t.Errorf("worker ReadOnly = false, want true after the patch")
	}

	// A patch that OMITS read_only must preserve it.
	rec = do(t, h, http.MethodPatch, "/v1/keys/worker", "", "tok-boss", map[string]any{"home": "acme/w"})
	if rec.Code != http.StatusOK {
		t.Fatalf("home-only patch: want 200, got %d (%s)", rec.Code, rec.Body)
	}
	if k := findKey(t, ks, "worker"); !k.ReadOnly {
		t.Errorf("read_only must survive a PATCH that omits it")
	}

	rec = do(t, h, http.MethodPatch, "/v1/keys/worker", "", "tok-boss", map[string]any{"read_only": false})
	if rec.Code != http.StatusOK {
		t.Fatalf("lift read_only: want 200, got %d (%s)", rec.Code, rec.Body)
	}
	if k := findKey(t, ks, "worker"); k.ReadOnly {
		t.Errorf("worker ReadOnly = true, want false after lifting")
	}

	// A named key CAN lift read_only from itself — that direction is a
	// restoration, not a lockout, and is only reachable because the key is not
	// read-only yet at the time of the request.
	rec = do(t, h, http.MethodPatch, "/v1/keys/boss", "", "tok-boss", map[string]any{"read_only": false})
	if rec.Code != http.StatusOK {
		t.Fatalf("self-lift read_only (a no-op restoration): want 200, got %d (%s)", rec.Code, rec.Body)
	}
}

// TestRotateApiKeyPreservesReadOnly: rotation rewrites the secret, and must not
// quietly restore write access while doing so.
func TestRotateApiKeyPreservesReadOnly(t *testing.T) {
	h, ks := newKeysTestServer(t, "admin-secret", "")
	if err := ks.PutAPIKey(context.Background(), store.APIKey{
		Name: "ci", Hash: hashOf("tok-ci"), ReadOnly: true,
	}); err != nil {
		t.Fatalf("seed ci: %v", err)
	}
	rec := do(t, h, http.MethodPost, "/v1/keys/ci/rotate", "", "admin-secret", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("rotate: want 200, got %d (%s)", rec.Code, rec.Body)
	}
	var out struct {
		ReadOnly bool   `json:"read_only"`
		Secret   string `json:"secret"`
	}
	mustJSON(t, rec, &out)
	if out.Secret == "" {
		t.Fatalf("rotate must return the new secret")
	}
	if !out.ReadOnly {
		t.Errorf("rotate response read_only = false, want true")
	}
	if k := findKey(t, ks, "ci"); !k.ReadOnly {
		t.Errorf("stored ReadOnly = false after rotation, want true")
	}
}
