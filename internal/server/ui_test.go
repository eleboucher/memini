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

// spa is a stand-in for the UI handler: it answers everything, so a request
// reaching it means the shell (which would carry the embedded token) was served.
func spa() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("SPA"))
	})
}

// By default the UI is mounted as a catch-all on the main router.
func TestUIOnMainPortByDefault(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := server.New(server.Options{Addr: ":0", ShutdownTimeout: time.Second}, log, prometheus.NewRegistry())
	srv.MountUI(spa())

	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "SPA" {
		t.Fatalf("/ on main router = %d %q, want 200 SPA", rec.Code, rec.Body.String())
	}
}

// A dedicated UIAddr keeps the token-embedding shell off the main router, so an
// in-cluster client hitting the main port never receives the API key.
func TestUIMovedOffMainPort(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := server.New(server.Options{
		Addr: ":8080", ShutdownTimeout: time.Second, UIAddr: ":8081",
	}, log, prometheus.NewRegistry())
	srv.MountUI(spa())

	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("/ on main router = %d, want 404 (UI served on dedicated port)", rec.Code)
	}
	// The main router still serves the API so the split doesn't break /v1 callers.
	rec = httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/healthz on main router = %d, want 200", rec.Code)
	}
}
