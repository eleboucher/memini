package rest_test

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/go-chi/chi/v5"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	meminimcp "github.com/eleboucher/memini/internal/api/mcp"
	"github.com/eleboucher/memini/internal/api/rest"
	"github.com/eleboucher/memini/internal/embed/embedtest"
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
	srv := meminimcp.NewServer(svc, ns)
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
