package rest_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/go-chi/chi/v5"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	meminimcp "github.com/eleboucher/memini/internal/api/mcp"
	"github.com/eleboucher/memini/internal/api/rest"
	"github.com/eleboucher/memini/internal/embed/embedtest"
	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/service"
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
	srv := meminimcp.NewServer(svc, ns, "", "")
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
	h := meminimcp.HTTPHandler(svc, nsHdr, "default", homeHdr, "", nil)
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
