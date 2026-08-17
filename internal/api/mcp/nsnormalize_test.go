package mcp_test

import (
	"context"
	"path/filepath"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	meminimcp "github.com/eleboucher/memini/internal/api/mcp"
	"github.com/eleboucher/memini/internal/embed/embedtest"
	"github.com/eleboucher/memini/internal/service"
	"github.com/eleboucher/memini/internal/store/sqlitevec"
)

// TestHTTPHandlerNormalizesNamespaceHeader pins that the MCP transport
// canonicalizes X-Memini-Namespace exactly like REST's namespaceMiddleware
// (trim spaces, strip surrounding slashes, collapse "//"): the same client
// input must address the same rows on both transports, or a caller switching
// between them silently reads and writes two different namespaces.
//
// Referenced by docs/how-it-works/namespaces.md.
func TestHTTPHandlerNormalizesNamespaceHeader(t *testing.T) {
	ctx := context.Background()
	st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "mcp-nsnorm.db"), dims)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	svc := service.New(st, embedtest.New(dims), service.WithSyncReinforce())
	h := meminimcp.HTTPHandler(svc, "X-Memini-Namespace", "default", "X-Memini-Home", "", nil, nil)

	// Write through a session whose namespace header is messy.
	messy, err := connectHTTP(t, h, " team//proj/ ", "", "")
	if err != nil {
		t.Fatalf("connect messy: %v", err)
	}
	res, err := messy.CallTool(ctx, &mcpsdk.CallToolParams{
		Name: "memory_remember",
		Arguments: map[string]any{
			"content": "the deploy pipeline runs on forgejo and publishes multi-arch images",
			"tier":    "semantic",
			"id":      "nsnorm-1",
		},
	})
	if err != nil {
		t.Fatalf("remember: %v", err)
	}
	var stored struct {
		Stored bool `json:"stored"`
	}
	structured(t, res, &stored)
	if !stored.Stored {
		t.Fatal("setup write was not stored")
	}

	// Read through a session using the canonical form of the same namespace.
	clean, err := connectHTTP(t, h, "team/proj", "", "")
	if err != nil {
		t.Fatalf("connect clean: %v", err)
	}
	res, err = clean.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      "memory_get",
		Arguments: map[string]any{"id": "nsnorm-1"},
	})
	if err != nil {
		t.Fatalf("get transport: %v", err)
	}
	if res.IsError {
		t.Fatalf("get from the canonical namespace failed: the messy header was not normalized (%v)", res.Content)
	}
	var got struct {
		Content string `json:"content"`
	}
	structured(t, res, &got)
	if got.Content == "" {
		t.Fatal("get returned an empty memory")
	}

	// And the messy session reads the same row back too — one namespace, two
	// spellings of the header.
	res, err = messy.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      "memory_get",
		Arguments: map[string]any{"id": "nsnorm-1"},
	})
	if err != nil {
		t.Fatalf("get via messy session: %v", err)
	}
	if res.IsError {
		t.Fatalf("messy-header session lost sight of its own write: %v", res.Content)
	}
}
