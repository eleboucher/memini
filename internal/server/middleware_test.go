package server_test

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/eleboucher/memini/internal/httputil"
	"github.com/eleboucher/memini/internal/server"
)

// TestPanicHandlerStillLoggedAndCounted installs a route that panics, then
// asserts that the recovered 500 still (a) gets recorded in the
// memini_http_requests_total counter and (b) appears as a structured log line.
//
// Regression guard: if Recoverer is registered OUTSIDE the request logger /
// metrics middleware, a handler panic unwinds past both before the recover
// catches it, so the post-ServeHTTP log + counter code never runs.
func TestPanicHandlerStillLoggedAndCounted(t *testing.T) {
	var logBuf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	reg := prometheus.NewRegistry()
	srv := server.New(server.Options{Addr: ":0", ShutdownTimeout: time.Second}, log, reg)

	srv.Router().Handle("GET /boom", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("kaboom")
	}))

	req := httptest.NewRequest(http.MethodGet, "/boom", nil)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}

	// The HTTP request must be counted (route="/boom", method="GET", status="500").
	if got := readCounter(t, reg, "memini_http_requests_total",
		map[string]string{"route": "/boom", "method": "GET", "status": "500"}); got != 1 {
		t.Errorf("memini_http_requests_total{status=500} = %d, want 1", got)
	}

	// The request-completion log line must be present.
	logged := logBuf.String()
	if !strings.Contains(logged, `"path":"/boom"`) {
		t.Errorf("request-completion log missing /boom entry; got: %s", logged)
	}
	if !strings.Contains(logged, `"status":500`) {
		t.Errorf("request-completion log missing status=500; got: %s", logged)
	}
}

// TestRequestLoggerStatsAtDebug pins /v1/stats to debug level in the request
// log: the admin UI polls it once per namespace per refresh, which at info
// level buried every meaningful line under ~40-line bursts of identical
// entries. Same treatment the health/readiness/metrics probes already get.
func TestRequestLoggerStatsAtDebug(t *testing.T) {
	var logBuf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	srv := server.New(server.Options{Addr: ":0", ShutdownTimeout: time.Second}, log, prometheus.NewRegistry())

	srv.Router().Handle("GET /v1/stats", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/stats", nil))

	logged := logBuf.String()
	if !strings.Contains(logged, `"path":"/v1/stats"`) {
		t.Fatalf("request log missing /v1/stats entry; got: %s", logged)
	}
	if !strings.Contains(logged, `"level":"DEBUG"`) {
		t.Errorf("/v1/stats logged above debug; got: %s", logged)
	}
}

// TestRequestLoggerIncludesRecordedActor asserts the request-completion line
// carries the actor an inner (auth) middleware recorded via
// httputil.RecordActor: the key name as "key" when a named key authenticated,
// and the auth kind always. Without this every line is anonymous and log
// triage cannot tell whose session did what.
func TestRequestLoggerIncludesRecordedActor(t *testing.T) {
	cases := []struct {
		name, actor, kind string
		wantInLog         []string
		wantAbsent        string
	}{
		{"named key", "nicole_tyrfing", "key",
			[]string{`"key":"nicole_tyrfing"`, `"auth":"key"`}, ""},
		{"admin env key", "", "env",
			[]string{`"auth":"env"`}, `"key":`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var logBuf bytes.Buffer
			log := slog.New(slog.NewJSONHandler(&logBuf, nil))
			srv := server.New(server.Options{Addr: ":0", ShutdownTimeout: time.Second}, log, prometheus.NewRegistry())

			srv.Router().Handle("GET /v1/memories", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				httputil.RecordActor(r.Context(), c.actor, c.kind)
				w.WriteHeader(http.StatusOK)
			}))

			rec := httptest.NewRecorder()
			srv.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/memories", nil))

			logged := logBuf.String()
			for _, want := range c.wantInLog {
				if !strings.Contains(logged, want) {
					t.Errorf("request log missing %s; got: %s", want, logged)
				}
			}
			if c.wantAbsent != "" && strings.Contains(logged, c.wantAbsent) {
				t.Errorf("request log has %s, want it absent; got: %s", c.wantAbsent, logged)
			}
		})
	}
}

// readCounter returns the value of a labelled counter series. Returns 0 if the
// series has no samples yet.
func readCounter(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) int {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			if !labelsMatch(m.GetLabel(), labels) {
				continue
			}
			if c := m.GetCounter(); c != nil {
				return int(c.GetValue())
			}
		}
	}
	return 0
}

func labelsMatch(got []*dto.LabelPair, want map[string]string) bool {
	seen := map[string]string{}
	for _, lp := range got {
		seen[lp.GetName()] = lp.GetValue()
	}
	for k, v := range want {
		if seen[k] != v {
			return false
		}
	}
	return true
}
