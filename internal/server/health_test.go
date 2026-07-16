package server_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/eleboucher/memini/internal/server"
)

func newTestServer(t *testing.T) *server.Server {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return server.New(server.Options{Addr: ":0", ShutdownTimeout: time.Second}, log, prometheus.NewRegistry())
}

func TestHealthz(t *testing.T) {
	srv := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
}

func TestReadyzReady(t *testing.T) {
	srv := newTestServer(t)
	srv.SetReady(func(context.Context) error { return nil })

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestReadyzNotReady(t *testing.T) {
	srv := newTestServer(t)
	srv.SetReady(func(context.Context) error { return errors.New("db down") })

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestReadyzNoCheckIsReady(t *testing.T) {
	// With no readiness func installed, /readyz reports ready.
	srv := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

type verboseHealthBody struct {
	Status string `json:"status"`
	Deps   struct {
		Store struct {
			OK          bool   `json:"ok"`
			LastError   string `json:"last_error"`
			LastSuccess string `json:"last_success"`
		} `json:"store"`
		Embedder struct {
			OK          bool   `json:"ok"`
			LastError   string `json:"last_error"`
			LastSuccess string `json:"last_success"`
		} `json:"embedder"`
		LLM struct {
			Configured  bool   `json:"configured"`
			OK          bool   `json:"ok"`
			LastError   string `json:"last_error"`
			LastSuccess string `json:"last_success"`
		} `json:"llm"`
		Reranker struct {
			Configured  bool   `json:"configured"`
			OK          bool   `json:"ok"`
			LastError   string `json:"last_error"`
			LastSuccess string `json:"last_success"`
		} `json:"reranker"`
	} `json:"deps"`
}

func getVerboseHealth(t *testing.T, srv *server.Server) (int, verboseHealthBody) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/healthz?verbose=1", nil)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)

	var body verboseHealthBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal body %q: %v", rec.Body.String(), err)
	}
	return rec.Code, body
}

func TestHealthzPlainUnaffectedByFailingEmbedder(t *testing.T) {
	// Plain /healthz must stay a pure liveness check: a failing embedder is
	// degraded, not dead, and must not show up here or change the status code.
	srv := newTestServer(t)
	deps := server.NewDepTracker()
	deps.Record("embedder", errors.New("dial tcp: connection refused"))
	srv.SetDeps(deps)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestHealthzVerboseReadyzUnaffectedByFailingEmbedder(t *testing.T) {
	// /readyz semantics must not change: it stays store-only, so an embedder
	// outage (degraded, keyword-only recall) never flips it to not-ready.
	srv := newTestServer(t)
	deps := server.NewDepTracker()
	deps.Record("embedder", errors.New("down"))
	srv.SetDeps(deps)
	srv.SetReady(func(context.Context) error { return nil })

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestHealthzVerboseEmbedderFailing(t *testing.T) {
	srv := newTestServer(t)
	deps := server.NewDepTracker()
	deps.Record("embedder", errors.New("dial tcp: connection refused"))
	srv.SetDeps(deps)

	code, body := getVerboseHealth(t, srv)

	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (verbose healthz stays a liveness check)", code)
	}
	if body.Status != "ok" {
		t.Errorf("status = %q, want ok", body.Status)
	}
	if !body.Deps.Store.OK {
		t.Errorf("store.ok = false, want true (no readiness func installed)")
	}
	if body.Deps.Embedder.OK {
		t.Errorf("embedder.ok = true, want false")
	}
	if body.Deps.Embedder.LastError == "" {
		t.Errorf("embedder.last_error empty, want the recorded error")
	}
	if body.Deps.Embedder.LastSuccess != "" {
		t.Errorf("embedder.last_success = %q, want empty (no success ever recorded)", body.Deps.Embedder.LastSuccess)
	}
	if body.Deps.LLM.Configured {
		t.Errorf("llm.configured = true, want false (none set)")
	}
}

func TestHealthzVerboseEmbedderHealthy(t *testing.T) {
	srv := newTestServer(t)
	deps := server.NewDepTracker()
	deps.Record("embedder", nil)
	srv.SetDeps(deps)

	_, body := getVerboseHealth(t, srv)

	if !body.Deps.Embedder.OK {
		t.Errorf("embedder.ok = false, want true")
	}
	if body.Deps.Embedder.LastError != "" {
		t.Errorf("embedder.last_error = %q, want empty", body.Deps.Embedder.LastError)
	}
	if body.Deps.Embedder.LastSuccess == "" {
		t.Fatalf("embedder.last_success empty, want an RFC3339 timestamp")
	}
	if _, err := time.Parse(time.RFC3339, body.Deps.Embedder.LastSuccess); err != nil {
		t.Errorf("last_success = %q, not RFC3339: %v", body.Deps.Embedder.LastSuccess, err)
	}
}

func TestHealthzVerboseLLMConfigured(t *testing.T) {
	srv := newTestServer(t)
	deps := server.NewDepTracker()
	deps.Record("llm", nil)
	srv.SetDeps(deps)
	srv.SetLLMConfigured(true)

	_, body := getVerboseHealth(t, srv)

	if !body.Deps.LLM.Configured {
		t.Errorf("llm.configured = false, want true")
	}
	if !body.Deps.LLM.OK {
		t.Errorf("llm.ok = false, want true")
	}
	if body.Deps.LLM.LastSuccess == "" {
		t.Errorf("llm.last_success empty, want an RFC3339 timestamp")
	}
}

func TestHealthzVerboseRerankerNotConfigured(t *testing.T) {
	srv := newTestServer(t)
	srv.SetDeps(server.NewDepTracker())

	_, body := getVerboseHealth(t, srv)
	if body.Deps.Reranker.Configured {
		t.Errorf("reranker.configured = true, want false")
	}
}

func TestHealthzVerboseRerankerConfigured(t *testing.T) {
	srv := newTestServer(t)
	deps := server.NewDepTracker()
	deps.Record("reranker", nil)
	srv.SetDeps(deps)
	srv.SetRerankConfigured(true)

	_, body := getVerboseHealth(t, srv)
	if !body.Deps.Reranker.Configured {
		t.Errorf("reranker.configured = false, want true")
	}
	if !body.Deps.Reranker.OK {
		t.Errorf("reranker.ok = false, want true")
	}
	if body.Deps.Reranker.LastSuccess == "" {
		t.Errorf("reranker.last_success empty, want an RFC3339 timestamp")
	}
}

func TestHealthzVerboseRerankerDown(t *testing.T) {
	srv := newTestServer(t)
	deps := server.NewDepTracker()
	deps.Record("reranker", errors.New("rerank backend unreachable"))
	srv.SetDeps(deps)
	srv.SetRerankConfigured(true)

	_, body := getVerboseHealth(t, srv)
	if body.Deps.Reranker.OK {
		t.Errorf("reranker.ok = true, want false")
	}
	if body.Deps.Reranker.LastError == "" {
		t.Errorf("reranker.last_error empty, want the recorded error")
	}
}

func TestHealthzVerboseLLMConfiguredButDown(t *testing.T) {
	// The failure mode the endpoint exists to expose: an LLM that is
	// configured but failing must render ok:false explicitly. Assert via a
	// raw map so an omitempty tag silently dropping the false "ok" key fails
	// the test instead of json.Unmarshal defaulting a struct field to false.
	srv := newTestServer(t)
	deps := server.NewDepTracker()
	deps.Record("llm", errors.New("upstream 502"))
	srv.SetDeps(deps)
	srv.SetLLMConfigured(true)

	req := httptest.NewRequest(http.MethodGet, "/healthz?verbose=1", nil)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)

	var raw struct {
		Deps struct {
			LLM map[string]any `json:"llm"`
		} `json:"deps"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("unmarshal body %q: %v", rec.Body.String(), err)
	}
	llm := raw.Deps.LLM

	if got, want := llm["configured"], true; got != want {
		t.Errorf("llm.configured = %v, want %v", got, want)
	}
	ok, present := llm["ok"]
	if !present {
		t.Fatalf("llm block %v: \"ok\" key missing — must be rendered explicitly when configured", llm)
	}
	if ok != false {
		t.Errorf("llm.ok = %v, want false", ok)
	}
	lastErr, _ := llm["last_error"].(string)
	if lastErr == "" {
		t.Errorf("llm.last_error = %v, want the recorded error", llm["last_error"])
	}
}

func TestHealthzVerboseNoDepsTrackerInstalled(t *testing.T) {
	// SetDeps is never called (e.g. a hand-built Server in a unit test):
	// verbose healthz must still respond instead of panicking on a nil tracker.
	srv := newTestServer(t)

	code, body := getVerboseHealth(t, srv)

	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if !body.Deps.Store.OK || !body.Deps.Embedder.OK {
		t.Errorf("deps = %+v, want ok defaults with no tracker installed", body.Deps)
	}
	if body.Deps.Store.LastSuccess == "" {
		t.Errorf("store.last_success empty, want the probe timestamp (RFC3339)")
	}
	if body.Deps.LLM.Configured {
		t.Errorf("llm.configured = true, want false")
	}
}

func TestHealthzVerboseStoreHealthy(t *testing.T) {
	// A healthy on-demand store ping must stamp last_success: the block has to
	// be self-describing, and consumers can't tell ok-freshly-probed from
	// ok-never-called without it.
	srv := newTestServer(t)
	srv.SetReady(func(context.Context) error { return nil })

	_, body := getVerboseHealth(t, srv)

	if !body.Deps.Store.OK {
		t.Fatalf("store.ok = false, want true")
	}
	if body.Deps.Store.LastSuccess == "" {
		t.Fatalf("store.last_success empty, want the probe timestamp (RFC3339)")
	}
	if _, err := time.Parse(time.RFC3339, body.Deps.Store.LastSuccess); err != nil {
		t.Errorf("store.last_success = %q, not RFC3339: %v", body.Deps.Store.LastSuccess, err)
	}
}

func TestHealthzVerboseStoreDown(t *testing.T) {
	srv := newTestServer(t)
	srv.SetReady(func(context.Context) error { return errors.New("db down") })

	code, body := getVerboseHealth(t, srv)

	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (verbose healthz is still a liveness check)", code)
	}
	if body.Deps.Store.OK {
		t.Errorf("store.ok = true, want false")
	}
	if body.Deps.Store.LastError == "" {
		t.Errorf("store.last_error empty, want the ping error")
	}
}

// newTestServerWithAPIKey builds a server with an API key configured, so
// verbose healthz auth-gating tests can exercise the with-token / without-token
// paths.
func newTestServerWithAPIKey(t *testing.T, key string) *server.Server {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return server.New(server.Options{Addr: ":0", ShutdownTimeout: time.Second, APIKey: key}, log, prometheus.NewRegistry())
}

// TestHealthzVerboseRequiresAuthWhenAPIKeySet confirms that with an API key
// configured, ?verbose=1 without a valid bearer token degrades to the plain
// body (200, no deps block, no leaked error internals) rather than 401ing —
// probes and naive monitors polling ?verbose=1 keep working.
func TestHealthzVerboseRequiresAuthWhenAPIKeySet(t *testing.T) {
	srv := newTestServerWithAPIKey(t, "s3cr3t")
	deps := server.NewDepTracker()
	deps.Record("embedder", errors.New("dial tcp 10.0.0.5:1234: connection refused"))
	srv.SetDeps(deps)

	req := httptest.NewRequest(http.MethodGet, "/healthz?verbose=1", nil)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (degrade, don't 401)", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "10.0.0.5") {
		t.Fatalf("body = %q, leaked internal dependency error to an unauthenticated caller", rec.Body.String())
	}

	var body verboseHealthBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal body %q: %v", rec.Body.String(), err)
	}
	if body.Status != "ok" {
		t.Errorf("status = %q, want ok", body.Status)
	}
	if body.Deps.Embedder.LastError != "" || body.Deps.Store.LastError != "" {
		t.Errorf("deps = %+v, want the plain body with no deps detail", body.Deps)
	}
}

// TestHealthzVerboseWithValidBearerReturnsFullPayload confirms the verbose
// deps payload is still available with the correct bearer token.
func TestHealthzVerboseWithValidBearerReturnsFullPayload(t *testing.T) {
	srv := newTestServerWithAPIKey(t, "s3cr3t")
	deps := server.NewDepTracker()
	deps.Record("embedder", errors.New("dial tcp 10.0.0.5:1234: connection refused"))
	srv.SetDeps(deps)

	req := httptest.NewRequest(http.MethodGet, "/healthz?verbose=1", nil)
	req.Header.Set("Authorization", "Bearer s3cr3t")
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body verboseHealthBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal body %q: %v", rec.Body.String(), err)
	}
	if body.Deps.Embedder.OK {
		t.Errorf("embedder.ok = true, want false")
	}
	if body.Deps.Embedder.LastError == "" {
		t.Errorf("embedder.last_error empty, want the recorded error with a valid bearer token")
	}
}

// TestHealthzVerboseUnauthenticatedWithoutAPIKeyConfigured confirms behavior
// is unchanged when no API key is configured at all: verbose works
// unauthenticated, as before this fix.
func TestHealthzVerboseUnauthenticatedWithoutAPIKeyConfigured(t *testing.T) {
	srv := newTestServer(t) // no APIKey set
	deps := server.NewDepTracker()
	deps.Record("embedder", errors.New("dial tcp: connection refused"))
	srv.SetDeps(deps)

	code, body := getVerboseHealth(t, srv)

	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if body.Deps.Embedder.LastError == "" {
		t.Errorf("embedder.last_error empty, want the recorded error (no API key configured, verbose stays open)")
	}
}
