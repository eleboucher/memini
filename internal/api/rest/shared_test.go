package rest_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"testing"

	"github.com/go-chi/chi/v5"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	meminimcp "github.com/eleboucher/memini/internal/api/mcp"
	"github.com/eleboucher/memini/internal/api/rest"
	"github.com/eleboucher/memini/internal/embed/embedtest"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/service"
	"github.com/eleboucher/memini/internal/store"
	"github.com/eleboucher/memini/internal/store/sqlitevec"
)

// TestSharedMemoryAcrossSurfaces is memini's headline guarantee: a memory
// written by one agent over REST is recalled by another agent over MCP, as long
// as they share a namespace and backing service.
func TestSharedMemoryAcrossSurfaces(t *testing.T) {
	ctx := context.Background()
	st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "shared.db"), dims)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	svc := service.New(st, embedtest.New(dims))
	const ns = "shared-project"

	// Agent A: remember over REST.
	r := chi.NewRouter()
	rest.New(svc, rest.AuthConfig{NamespaceHeader: nsHdr, DefaultNamespace: "default"}).Mount(r)
	rec := do(t, r, http.MethodPost, "/v1/memories", ns, "", map[string]any{
		"content": "the deploy key lives in vault at secret/ci/deploy", "tier": "semantic",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("REST remember: want 201, got %d (%s)", rec.Code, rec.Body)
	}

	// Agent B: recall over MCP, bound to the same namespace and service.
	srv := meminimcp.NewServer(svc, ns, "", "", "none")
	clientT, serverT := mcpsdk.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, serverT, nil); err != nil {
		t.Fatalf("mcp server connect: %v", err)
	}
	cs, err := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "agentB", Version: "0"}, nil).Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("mcp client connect: %v", err)
	}
	defer func() { _ = cs.Close() }()

	res, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      "memory_recall",
		Arguments: map[string]any{"query": "where is the deploy key", "limit": 5},
	})
	if err != nil {
		t.Fatalf("mcp recall: %v", err)
	}
	var out struct {
		Results []struct {
			Content string `json:"content"`
		} `json:"results"`
	}
	b, _ := json.Marshal(res.StructuredContent)
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Results) == 0 || out.Results[0].Content != "the deploy key lives in vault at secret/ci/deploy" {
		t.Fatalf("agent B did not recall agent A's memory: %+v", out.Results)
	}
}

// TestUpdateSemanticsMatchAcrossSurfaces pins the point of sharing
// service.Update: an identical partial edit sent as a REST PATCH and as an MCP
// memory_update must produce the identical row. Both surfaces used to be free to
// drift — the omit-to-keep rules and the metadata merge lived only in the MCP
// handler, and REST's nearest equivalent (POST with an id) replaces wholesale.
func TestUpdateSemanticsMatchAcrossSurfaces(t *testing.T) {
	ctx := context.Background()
	st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "parity.db"), dims)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	svc := service.New(st, embedtest.New(dims))
	const ns = "parity-project"

	seed := func(t *testing.T) *memory.Memory {
		t.Helper()
		m, err := svc.Remember(ctx, service.RememberInput{
			Namespace: ns, Content: "the deploy key lives in vault at secret/ci/deploy",
			Summary: "deploy key location", Tier: memory.TierSemantic, Tags: []string{"ops"},
			Metadata: map[string]any{"source": "handbook", "reviewed": "no"},
		})
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
		return m
	}

	// The same partial edit, expressed for each surface: change one metadata
	// key, retag, and leave content/summary/tier to carry over.
	viaREST := seed(t)
	r := chi.NewRouter()
	rest.New(svc, rest.AuthConfig{NamespaceHeader: nsHdr, DefaultNamespace: "default"}).Mount(r)
	rec := do(t, r, http.MethodPatch, "/v1/memories/"+viaREST.ID, ns, "", map[string]any{
		"tags": []string{"ops", "vault"}, "metadata": map[string]any{"reviewed": "yes"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("REST patch: want 200, got %d (%s)", rec.Code, rec.Body)
	}

	viaMCP := seed(t)
	srv := meminimcp.NewServer(svc, ns, "", "", "none")
	clientT, serverT := mcpsdk.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, serverT, nil); err != nil {
		t.Fatalf("mcp server connect: %v", err)
	}
	cs, err := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "agent", Version: "0"}, nil).Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("mcp client connect: %v", err)
	}
	defer func() { _ = cs.Close() }()
	if _, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{
		Name: "memory_update",
		Arguments: map[string]any{
			"id": viaMCP.ID, "tags": []string{"ops", "vault"},
			"metadata": map[string]any{"reviewed": "yes"},
		},
	}); err != nil {
		t.Fatalf("mcp update: %v", err)
	}

	// Compare what actually landed in the store, not what each surface echoed.
	gotREST, err := st.Get(ctx, ns, viaREST.ID)
	if err != nil {
		t.Fatalf("get rest row: %v", err)
	}
	gotMCP, err := st.Get(ctx, ns, viaMCP.ID)
	if err != nil {
		t.Fatalf("get mcp row: %v", err)
	}

	if gotREST.Content != gotMCP.Content {
		t.Fatalf("content diverged: REST %q vs MCP %q", gotREST.Content, gotMCP.Content)
	}
	if gotREST.Summary != gotMCP.Summary {
		t.Fatalf("summary diverged: REST %q vs MCP %q", gotREST.Summary, gotMCP.Summary)
	}
	if gotREST.Tier != gotMCP.Tier {
		t.Fatalf("tier diverged: REST %q vs MCP %q", gotREST.Tier, gotMCP.Tier)
	}
	if !slices.Equal(gotREST.Tags, gotMCP.Tags) {
		t.Fatalf("tags diverged: REST %v vs MCP %v", gotREST.Tags, gotMCP.Tags)
	}
	for _, k := range []string{"source", "reviewed"} {
		if gotREST.Metadata[k] != gotMCP.Metadata[k] {
			t.Fatalf("metadata[%q] diverged: REST %v vs MCP %v", k, gotREST.Metadata[k], gotMCP.Metadata[k])
		}
	}
	// And the merge actually happened on both: the untouched key survived.
	if gotREST.Metadata["source"] != "handbook" {
		t.Fatalf("metadata[source] = %v on both surfaces, want the merge to preserve it", gotREST.Metadata["source"])
	}
}

// homeHeaderRoundTripper injects fixed X-Memini-Namespace/X-Memini-Home
// headers on every outgoing request of an MCP Streamable HTTP client.
type homeHeaderRoundTripper struct {
	ns, home string
}

func (h homeHeaderRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	r.Header.Set(nsHdr, h.ns)
	r.Header.Set(homeHdr, h.home)
	return http.DefaultTransport.RoundTrip(r)
}

// TestHomeHeaderNormalizedIdenticallyAcrossSurfaces pins cross-transport
// parity for the home header: the identical non-canonical client input
// (X-Memini-Home: "Work/Proj/", trailing slash) must resolve to the same
// normalized home namespace ("Work/Proj") on both the REST path
// (homeMiddleware) and the MCP Streamable HTTP path (HTTPHandler's home
// capture). Before the fix, MCP only trimmed spaces, so the same client
// talking to both surfaces silently read two different home keys.
func TestHomeHeaderNormalizedIdenticallyAcrossSurfaces(t *testing.T) {
	ctx := context.Background()
	st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "home-parity.db"), dims)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	svc := service.New(st, embedtest.New(dims))

	// The memory lives under the CANONICAL home namespace; both transports
	// only see it if their header capture normalizes "Work/Proj/" to it.
	if _, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "Work/Proj", Content: "the vpn config lives in the team vault", Tier: memory.TierSemantic,
	}); err != nil {
		t.Fatalf("seed remember: %v", err)
	}
	const rawHome = "Work/Proj/" // non-canonical: trailing slash

	// REST: search from an unrelated namespace with the raw home header.
	r := chi.NewRouter()
	rest.New(svc, rest.AuthConfig{
		NamespaceHeader: nsHdr, DefaultNamespace: "default", HomeHeader: homeHdr,
	}).Mount(r)
	rec := doHome(t, r, http.MethodPost, "/v1/search", "acme/phoenix", rawHome, "", map[string]any{
		"query": "vpn config", "limit": 5,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("REST search: want 200, got %d (%s)", rec.Code, rec.Body)
	}
	var restOut struct {
		Results []struct {
			Memory struct {
				Namespace string `json:"namespace"`
			} `json:"memory"`
		} `json:"results"`
	}
	mustJSON(t, rec, &restOut)
	if len(restOut.Results) != 1 || restOut.Results[0].Memory.Namespace != "Work/Proj" {
		t.Fatalf("REST: raw home %q should normalize to Work/Proj and surface its memory, got %+v",
			rawHome, restOut.Results)
	}

	// MCP over real HTTP: same raw header, same expectation.
	h := meminimcp.HTTPHandler(svc, nsHdr, "default", homeHdr, "", nil, nil)
	hs := httptest.NewServer(h)
	t.Cleanup(hs.Close)
	transport := &mcpsdk.StreamableClientTransport{
		Endpoint:             hs.URL,
		HTTPClient:           &http.Client{Transport: homeHeaderRoundTripper{ns: "acme/phoenix", home: rawHome}},
		DisableStandaloneSSE: true,
	}
	cs, err := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "t", Version: "0"}, nil).Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("mcp connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	res, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      "memory_recall",
		Arguments: map[string]any{"query": "vpn config", "limit": 5},
	})
	if err != nil {
		t.Fatalf("mcp recall: %v", err)
	}
	var mcpOut struct {
		Results []struct {
			Namespace string `json:"namespace"`
		} `json:"results"`
	}
	b, _ := json.Marshal(res.StructuredContent)
	if err := json.Unmarshal(b, &mcpOut); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(mcpOut.Results) != 1 || mcpOut.Results[0].Namespace != "Work/Proj" {
		t.Fatalf("MCP HTTP: raw home %q should normalize to Work/Proj exactly like REST, got %+v",
			rawHome, mcpOut.Results)
	}
}

// TestBoundKeyHomeOverrideIdenticalAcrossSurfaces pins cross-transport parity
// for the K2 bound-key precedence: a table key bound to a home namespace
// (APIKey.HomeNS) must win over a CONFLICTING X-Memini-Home header on both
// REST and MCP HTTP for the exact same key + headers. Before this test, the
// two surfaces implemented auth independently (REST's authMiddleware vs
// MCP's HTTPHandler home capture) and could drift; both now resolve through
// the shared internal/apiauth.Config, and this test is the guarantee that
// stays true.
func TestBoundKeyHomeOverrideIdenticalAcrossSurfaces(t *testing.T) {
	ctx := context.Background()
	st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "bound-home-parity.db"), dims)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	var ks store.APIKeyStore = st
	if err := ks.PutAPIKey(ctx, store.APIKey{
		Name: "bound-parity-bot", Hash: hashOf("tok-bound-parity"), HomeNS: "acme/parityhome",
	}); err != nil {
		t.Fatalf("PutAPIKey: %v", err)
	}
	svc := service.New(st, embedtest.New(dims))
	if _, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "acme/parityhome", Content: "the release runbook lives in confluence", Tier: memory.TierSemantic,
	}); err != nil {
		t.Fatalf("seed remember: %v", err)
	}
	const conflictingHome = "someone/elses/home"

	// REST: search from an unrelated namespace with a conflicting home header
	// and the bound key as the bearer.
	r := chi.NewRouter()
	rest.New(svc, rest.AuthConfig{
		APIKeyStore: ks, NamespaceHeader: nsHdr, DefaultNamespace: "default", HomeHeader: homeHdr,
	}).Mount(r)
	rec := doHome(t, r, http.MethodPost, "/v1/search", "acme/unrelated", conflictingHome, "tok-bound-parity", map[string]any{
		"query": "release runbook", "limit": 5,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("REST search: want 200 (never 400 for the conflict), got %d (%s)", rec.Code, rec.Body)
	}
	var restOut struct {
		Results []struct {
			Memory struct {
				Namespace string `json:"namespace"`
			} `json:"memory"`
		} `json:"results"`
	}
	mustJSON(t, rec, &restOut)
	if len(restOut.Results) != 1 || restOut.Results[0].Memory.Namespace != "acme/parityhome" {
		t.Fatalf("REST: bound key home must win over the conflicting header, got %+v", restOut.Results)
	}

	// MCP over real HTTP: identical key, identical conflicting header.
	h := meminimcp.HTTPHandler(svc, nsHdr, "default", homeHdr, "", ks, nil)
	hs := httptest.NewServer(h)
	t.Cleanup(hs.Close)
	transport := &mcpsdk.StreamableClientTransport{
		Endpoint: hs.URL,
		HTTPClient: &http.Client{Transport: tokenRoundTripperShared{
			ns: "acme/unrelated", home: conflictingHome, token: "tok-bound-parity",
		}},
		DisableStandaloneSSE: true,
	}
	cs, err := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "t", Version: "0"}, nil).Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("mcp connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	res, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      "memory_recall",
		Arguments: map[string]any{"query": "release runbook", "limit": 5},
	})
	if err != nil {
		t.Fatalf("mcp recall: %v", err)
	}
	var mcpOut struct {
		Results []struct {
			Namespace string `json:"namespace"`
		} `json:"results"`
	}
	b, _ := json.Marshal(res.StructuredContent)
	if err := json.Unmarshal(b, &mcpOut); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(mcpOut.Results) != 1 || mcpOut.Results[0].Namespace != "acme/parityhome" {
		t.Fatalf("MCP HTTP: bound key home must win over the conflicting header exactly like REST, got %+v",
			mcpOut.Results)
	}
}

// tokenRoundTripperShared injects fixed X-Memini-Namespace/X-Memini-Home/
// Authorization headers, local to this file to avoid depending on the
// mcp_test package's own tokenRoundTripper (different Go package).
type tokenRoundTripperShared struct {
	ns, home, token string
}

func (t tokenRoundTripperShared) RoundTrip(r *http.Request) (*http.Response, error) {
	if t.ns != "" {
		r.Header.Set(nsHdr, t.ns)
	}
	if t.home != "" {
		r.Header.Set(homeHdr, t.home)
	}
	if t.token != "" {
		r.Header.Set("Authorization", "Bearer "+t.token)
	}
	return http.DefaultTransport.RoundTrip(r)
}
