package server_test

import (
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
