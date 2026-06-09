// Package server wires the HTTP surface: middleware, health probes, metrics,
// graceful shutdown, and a chi router that other packages mount routes onto.
package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/eleboucher/memini/internal/version"
)

// ReadinessFunc reports whether the service is ready to serve traffic.
// A nil error means ready; a non-nil error is surfaced on /readyz.
type ReadinessFunc func(context.Context) error

// Server owns the HTTP server and its router.
type Server struct {
	cfg    Options
	log    *slog.Logger
	router chi.Router
	http   *http.Server

	ready atomic.Pointer[ReadinessFunc]
}

// Options configures the server without importing the config package.
type Options struct {
	Addr            string
	ShutdownTimeout time.Duration
}

// New builds a Server with base middleware, /healthz, /readyz and /metrics.
// Additional routes are mounted via Router before calling Run.
func New(opts Options, log *slog.Logger, reg *prometheus.Registry) *Server {
	r := chi.NewRouter()
	s := &Server{cfg: opts, log: log, router: r}

	m := newMetrics(reg)
	r.Use(middleware.RequestID)
	r.Use(middleware.Recoverer)
	r.Use(requestLogger(log))
	r.Use(m.middleware)

	r.Get("/healthz", s.handleHealthz)
	r.Get("/readyz", s.handleReadyz)
	r.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))

	return s
}

// Router exposes the underlying router so other packages can mount routes.
func (s *Server) Router() chi.Router { return s.router }

// SetReady installs the readiness check used by /readyz.
func (s *Server) SetReady(fn ReadinessFunc) { s.ready.Store(&fn) }

// Run starts the HTTP server and blocks until ctx is cancelled, then performs
// a graceful shutdown bounded by the configured timeout.
func (s *Server) Run(ctx context.Context) error {
	s.http = &http.Server{
		Addr:              s.cfg.Addr,
		Handler:           s.router,
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		s.log.Info("http server listening", "addr", s.cfg.Addr, "version", version.String())
		if err := s.http.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		s.log.Info("shutdown signal received, draining")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), s.cfg.ShutdownTimeout)
		defer cancel()
		return s.http.Shutdown(shutdownCtx)
	}
}
