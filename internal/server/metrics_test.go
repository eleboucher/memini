package server_test

import (
	"encoding/json"
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

// By default /metrics is served on the main router.
func TestMetricsOnMainPortByDefault(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := server.New(server.Options{Addr: ":0", ShutdownTimeout: time.Second}, log, prometheus.NewRegistry())

	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics on main router = %d, want 200", rec.Code)
	}
}

// With an API key configured, /metrics on the main port sits behind the same
// bearer token as /v1. The 401 must advertise the scheme (RFC 6750) and carry
// a JSON body under a matching content type — a bare 401 with a text/plain
// content type on a JSON body is what sends MCP clients into OAuth discovery
// and then fails them when they try to parse the answer.
func TestMetricsBearerAuth(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := server.New(server.Options{Addr: ":0", ShutdownTimeout: time.Second, APIKey: "s3cr3t"}, log, prometheus.NewRegistry())

	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("/metrics without a token = %d, want 401", rec.Code)
	}
	if got := rec.Header().Get("WWW-Authenticate"); got != `Bearer realm="memini"` {
		t.Errorf("WWW-Authenticate = %q, want `Bearer realm=\"memini\"`", got)
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
	if body.Error == "" {
		t.Errorf("body = %q, want an {\"error\": …} message", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("Authorization", "Bearer s3cr3t")
	srv.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics with a valid token = %d, want 200", rec.Code)
	}
}

// Every response is measured into memini_http_response_bytes, labelled by the
// matched chi route pattern (not the raw path) so cardinality stays bounded.
func TestResponseBytesHistogram(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg := prometheus.NewRegistry()
	srv := server.New(server.Options{Addr: ":0", ShutdownTimeout: time.Second}, log, reg)

	body := strings.Repeat("x", 1000)
	srv.Router().Get("/v1/things/{id}", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	})

	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/things/abc", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("request = %d, want 200", rec.Code)
	}

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != "memini_http_response_bytes" {
			continue
		}
		for _, m := range mf.GetMetric() {
			labels := map[string]string{}
			for _, lp := range m.GetLabel() {
				labels[lp.GetName()] = lp.GetValue()
			}
			if labels["route"] != "/v1/things/{id}" || labels["method"] != http.MethodGet {
				continue
			}
			h := m.GetHistogram()
			if h.GetSampleCount() != 1 {
				t.Fatalf("sample count = %d, want 1", h.GetSampleCount())
			}
			if h.GetSampleSum() != 1000 {
				t.Fatalf("sample sum = %v, want 1000", h.GetSampleSum())
			}
			return
		}
		t.Fatalf("memini_http_response_bytes present but no series for route=/v1/things/{id} method=GET")
	}
	t.Fatalf("memini_http_response_bytes not registered")
}

// A dedicated MetricsAddr moves /metrics off the main router so a route that
// forwards the main port can't reach it.
func TestMetricsMovedOffMainPort(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := server.New(server.Options{
		Addr: ":8080", ShutdownTimeout: time.Second, MetricsAddr: ":9090",
	}, log, prometheus.NewRegistry())

	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("/metrics on main router = %d, want 404 (served on dedicated port)", rec.Code)
	}
}
