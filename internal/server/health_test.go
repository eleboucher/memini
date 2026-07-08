package server_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
			OK        bool   `json:"ok"`
			LastError string `json:"last_error"`
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
	if body.Deps.LLM.Configured {
		t.Errorf("llm.configured = true, want false")
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
