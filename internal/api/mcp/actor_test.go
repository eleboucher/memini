package mcp_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	meminimcp "github.com/eleboucher/memini/internal/api/mcp"
	"github.com/eleboucher/memini/internal/embed/embedtest"
	"github.com/eleboucher/memini/internal/httputil"
	"github.com/eleboucher/memini/internal/service"
	"github.com/eleboucher/memini/internal/store"
	"github.com/eleboucher/memini/internal/store/sqlitevec"
)

// TestHTTPHandlerRecordsActor asserts the MCP surface records the
// authenticated actor into an actor holder installed by an outer wrapper (in
// production, internal/server's request logger) — the same attribution REST's
// /v1 chain records, so /mcp access-log lines name the key too. Recording
// happens at auth time, so even a request the MCP layer itself rejects is
// attributed.
func TestHTTPHandlerRecordsActor(t *testing.T) {
	ctx := context.Background()
	st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "mcp-actor.db"), dims)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	var ks store.APIKeyStore = st
	if err := ks.PutAPIKey(ctx, store.APIKey{Name: "bot", Hash: hashOf("tok-bot")}); err != nil {
		t.Fatalf("PutAPIKey: %v", err)
	}
	svc := service.New(st, embedtest.New(dims))
	h := meminimcp.HTTPHandler(svc, "X-Memini-Namespace", "default", "X-Memini-Home", "", ks, nil)

	var gotName, gotKind string
	var recorded bool
	wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(httputil.WithActorHolder(r.Context()))
		h.ServeHTTP(w, r)
		gotName, gotKind, recorded = httputil.RecordedActor(r.Context())
	})

	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer tok-bot")
	req.Header.Set("Content-Type", "application/json")
	wrapped.ServeHTTP(httptest.NewRecorder(), req)

	if !recorded {
		t.Fatal("no actor recorded, want one")
	}
	if gotName != "bot" || gotKind != "key" {
		t.Errorf("recorded actor = (%q, %q), want (bot, key)", gotName, gotKind)
	}
}
