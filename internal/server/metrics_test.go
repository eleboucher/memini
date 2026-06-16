package server_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
