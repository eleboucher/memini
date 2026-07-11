package mcp_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	meminimcp "github.com/eleboucher/memini/internal/api/mcp"
	"github.com/eleboucher/memini/internal/embed/embedtest"
	"github.com/eleboucher/memini/internal/service"
	"github.com/eleboucher/memini/internal/store"
	"github.com/eleboucher/memini/internal/store/sqlitevec"
)

func hashOf(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// tokenRoundTripper injects a fixed Authorization bearer and optional
// namespace/home headers on every outgoing request of an MCP Streamable HTTP
// client, mirroring headerRoundTripper (mcp_test.go) plus a token.
type tokenRoundTripper struct {
	ns, home, token string
}

func (t tokenRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	if t.ns != "" {
		r.Header.Set("X-Memini-Namespace", t.ns)
	}
	if t.home != "" {
		r.Header.Set("X-Memini-Home", t.home)
	}
	if t.token != "" {
		r.Header.Set("Authorization", "Bearer "+t.token)
	}
	return http.DefaultTransport.RoundTrip(r)
}

// connectHTTP dials h (an meminimcp.HTTPHandler) with the given ns/home/token
// headers on every request, returning a connected client session.
func connectHTTP(t *testing.T, h http.Handler, ns, home, token string) (*mcpsdk.ClientSession, error) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	transport := &mcpsdk.StreamableClientTransport{
		Endpoint:             srv.URL,
		HTTPClient:           &http.Client{Transport: tokenRoundTripper{ns: ns, home: home, token: token}},
		DisableStandaloneSSE: true,
	}
	cs, err := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "t", Version: "0"}, nil).Connect(context.Background(), transport, nil)
	if err == nil {
		t.Cleanup(func() { _ = cs.Close() })
	}
	return cs, err
}

func TestHTTPHandlerTableKeyAuthenticates(t *testing.T) {
	ctx := context.Background()
	st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "mcp-apikey.db"), dims)
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

	cs, err := connectHTTP(t, h, "acme", "", "tok-bot")
	if err != nil {
		t.Fatalf("connect with valid table key: %v", err)
	}
	res, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      "memory_remember",
		Arguments: map[string]any{"content": "a table-key mcp write", "tier": "semantic"},
	})
	if err != nil || res.IsError {
		t.Fatalf("memory_remember: err=%v res=%+v", err, res)
	}
}

func TestHTTPHandlerUnknownKeyRejected(t *testing.T) {
	ctx := context.Background()
	st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "mcp-apikey2.db"), dims)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	var ks store.APIKeyStore = st
	if err := ks.PutAPIKey(ctx, store.APIKey{Name: "real-bot", Hash: hashOf("tok-real")}); err != nil {
		t.Fatalf("PutAPIKey: %v", err)
	}
	svc := service.New(st, embedtest.New(dims))
	h := meminimcp.HTTPHandler(svc, "X-Memini-Namespace", "default", "X-Memini-Home", "", ks, nil)

	if _, err := connectHTTP(t, h, "acme", "", "no-such-token"); err == nil {
		t.Fatalf("connect with unknown token: want an error (401), got none")
	}
}

func TestHTTPHandlerDisabledKeyRejected(t *testing.T) {
	ctx := context.Background()
	st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "mcp-apikey3.db"), dims)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	var ks store.APIKeyStore = st
	if err := ks.PutAPIKey(ctx, store.APIKey{Name: "retired", Hash: hashOf("tok-retired"), Disabled: true}); err != nil {
		t.Fatalf("PutAPIKey: %v", err)
	}
	svc := service.New(st, embedtest.New(dims))
	h := meminimcp.HTTPHandler(svc, "X-Memini-Namespace", "default", "X-Memini-Home", "", ks, nil)

	if _, err := connectHTTP(t, h, "acme", "", "tok-retired"); err == nil {
		t.Fatalf("connect with disabled key: want an error (401), got none")
	}
}

// TestHTTPHandlerBoundKeyHomeOverridesConflictingHeader: a key bound to a
// home namespace must win over a conflicting X-Memini-Home header, and the
// request must never be rejected with 400 for the conflict (log-and-ignore).
func TestHTTPHandlerBoundKeyHomeOverridesConflictingHeader(t *testing.T) {
	ctx := context.Background()
	st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "mcp-apikey4.db"), dims)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	var ks store.APIKeyStore = st
	if err := ks.PutAPIKey(ctx, store.APIKey{Name: "bound-bot", Hash: hashOf("tok-bound"), HomeNS: "acme/home"}); err != nil {
		t.Fatalf("PutAPIKey: %v", err)
	}
	svc := service.New(st, embedtest.New(dims))
	if _, err := svc.Remember(ctx, service.RememberInput{
		Namespace: "acme/home", Content: "the vpn config lives in the team vault", Tier: "semantic",
	}); err != nil {
		t.Fatalf("seed remember: %v", err)
	}
	h := meminimcp.HTTPHandler(svc, "X-Memini-Namespace", "default", "X-Memini-Home", "", ks, nil)

	cs, err := connectHTTP(t, h, "acme/unrelated", "someone/elses/home", "tok-bound")
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	res, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      "memory_recall",
		Arguments: map[string]any{"query": "vpn config", "limit": 5},
	})
	if err != nil || res.IsError {
		t.Fatalf("memory_recall: err=%v res=%+v", err, res)
	}
	var out struct {
		Results []struct {
			Namespace string `json:"namespace"`
		} `json:"results"`
	}
	structured(t, res, &out)
	if len(out.Results) != 1 || out.Results[0].Namespace != "acme/home" {
		t.Fatalf("bound key home must win over a conflicting header, got %+v", out.Results)
	}
}

// TestHTTPHandlerKeyDefaultNamespace: a key's DefaultNS is used when the
// request carries no namespace header; the header, when present, still wins.
func TestHTTPHandlerKeyDefaultNamespace(t *testing.T) {
	ctx := context.Background()
	st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "mcp-apikey5.db"), dims)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	var ks store.APIKeyStore = st
	if err := ks.PutAPIKey(ctx, store.APIKey{
		Name: "ns-bot", Hash: hashOf("tok-ns"), DefaultNS: "acme/keydefault",
	}); err != nil {
		t.Fatalf("PutAPIKey: %v", err)
	}
	svc := service.New(st, embedtest.New(dims))
	h := meminimcp.HTTPHandler(svc, "X-Memini-Namespace", "default", "X-Memini-Home", "", ks, nil)

	// No namespace header: lands in the key's DefaultNS.
	cs, err := connectHTTP(t, h, "", "", "tok-ns")
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	res, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      "memory_remember",
		Arguments: map[string]any{"content": "lands in the key default namespace", "tier": "semantic"},
	})
	if err != nil || res.IsError {
		t.Fatalf("memory_remember: err=%v res=%+v", err, res)
	}
	var rem struct {
		ID string `json:"id"`
	}
	structured(t, res, &rem)
	if _, gerr := svc.Get(ctx, "acme/keydefault", rem.ID); gerr != nil {
		t.Fatalf("memory not found in key DefaultNS acme/keydefault: %v", gerr)
	}

	// Explicit namespace header wins over the key default.
	cs2, err := connectHTTP(t, h, "acme/explicit", "", "tok-ns")
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	res2, err := cs2.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      "memory_remember",
		Arguments: map[string]any{"content": "the header wins over the key default", "tier": "semantic"},
	})
	if err != nil || res2.IsError {
		t.Fatalf("memory_remember: err=%v res=%+v", err, res2)
	}
	var rem2 struct {
		ID string `json:"id"`
	}
	structured(t, res2, &rem2)
	if _, gerr := svc.Get(ctx, "acme/explicit", rem2.ID); gerr != nil {
		t.Fatalf("memory not found in header namespace acme/explicit: %v", gerr)
	}
}

// TestHTTPHandlerNamedKeyAttribution: a named table key stamps
// metadata.author on an MCP write; the admin key does not.
func TestHTTPHandlerNamedKeyAttribution(t *testing.T) {
	ctx := context.Background()
	st, err := sqlitevec.Open(ctx, filepath.Join(t.TempDir(), "mcp-apikey6.db"), dims)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	var ks store.APIKeyStore = st
	if err := ks.PutAPIKey(ctx, store.APIKey{Name: "author-bot", Hash: hashOf("tok-author")}); err != nil {
		t.Fatalf("PutAPIKey: %v", err)
	}
	svc := service.New(st, embedtest.New(dims))
	h := meminimcp.HTTPHandler(svc, "X-Memini-Namespace", "default", "X-Memini-Home", "admin-secret", ks, nil)

	// Named key: attributed.
	cs, err := connectHTTP(t, h, "acme", "", "tok-author")
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	res, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      "memory_remember",
		Arguments: map[string]any{"content": "a fact written by a named mcp key", "tier": "semantic"},
	})
	if err != nil || res.IsError {
		t.Fatalf("memory_remember: err=%v res=%+v", err, res)
	}
	var rem struct {
		ID string `json:"id"`
	}
	structured(t, res, &rem)
	m, gerr := svc.Get(ctx, "acme", rem.ID)
	if gerr != nil {
		t.Fatalf("get: %v", gerr)
	}
	if m.Metadata["author"] != "author-bot" {
		t.Fatalf("metadata.author = %v, want author-bot", m.Metadata["author"])
	}

	// Admin key: no attribution.
	csAdmin, err := connectHTTP(t, h, "acme", "", "admin-secret")
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	resAdmin, err := csAdmin.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      "memory_remember",
		Arguments: map[string]any{"content": "a fact written by the admin mcp key", "tier": "semantic"},
	})
	if err != nil || resAdmin.IsError {
		t.Fatalf("memory_remember (admin): err=%v res=%+v", err, resAdmin)
	}
	var remAdmin struct {
		ID string `json:"id"`
	}
	structured(t, resAdmin, &remAdmin)
	mAdmin, gerr := svc.Get(ctx, "acme", remAdmin.ID)
	if gerr != nil {
		t.Fatalf("get admin: %v", gerr)
	}
	if _, ok := mAdmin.Metadata["author"]; ok {
		t.Fatalf("admin key write must carry no author stamp, got metadata=%v", mAdmin.Metadata)
	}
}
