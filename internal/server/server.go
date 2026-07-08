// Package server wires the HTTP surface: middleware, health probes, metrics,
// graceful shutdown, and a chi router that other packages mount routes onto.
package server

import (
	"context"
	"crypto/subtle"
	"errors"
	"log/slog"
	"net/http"
	"strings"
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
	// metricsHandler is non-nil only when metrics run on a dedicated port (see
	// Options.MetricsAddr); Run serves it on its own listener.
	metricsHandler http.Handler
	// uiHandler is non-nil only when the UI runs on a dedicated port (see
	// Options.UIAddr); Run serves it on its own listener.
	uiHandler http.Handler

	ready atomic.Pointer[ReadinessFunc]
	// deps and llmConfigured back the verbose healthz dependency blocks; both
	// are set once at startup (see SetDeps/SetLLMConfigured) but use atomics
	// for the same late-binding reason as ready.
	deps          atomic.Pointer[DepTracker]
	llmConfigured atomic.Bool
}

// Options configures the server without importing the config package.
type Options struct {
	Addr            string
	ShutdownTimeout time.Duration
	// APIKey, when non-empty, gates /metrics on the main port behind the same
	// bearer token used by the /v1 routes. A dedicated MetricsAddr port is
	// unauthenticated instead.
	APIKey string
	// MetricsAddr, when set and distinct from Addr, serves /metrics on its own
	// unauthenticated listener instead of the main router — a port meant to stay
	// in-cluster (keep it off any public route).
	MetricsAddr string
	// UIAddr, when set and distinct from Addr, serves the admin UI on its own
	// listener instead of the main router (see MountUI). The shell embeds the
	// API key, so isolating it keeps that key off the main port — expose UIAddr
	// only on a trusted (LAN) gateway.
	UIAddr string
}

// New builds a Server with base middleware, /healthz, /readyz and /metrics.
// Additional routes are mounted via Router before calling Run.
func New(opts Options, log *slog.Logger, reg *prometheus.Registry) *Server {
	r := chi.NewRouter()
	s := &Server{cfg: opts, log: log, router: r}

	m := newMetrics(reg)
	// Recoverer is innermost so a recovered panic propagates back out through
	// metrics and requestLogger (both record after next.ServeHTTP), keeping the
	// 500 counted and logged.
	r.Use(middleware.RequestID)
	r.Use(requestLogger(log))
	r.Use(m.middleware)
	r.Use(middleware.Recoverer)

	r.Get("/healthz", s.handleHealthz)
	r.Get("/readyz", s.handleReadyz)

	metricsHandler := promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
	if opts.MetricsAddr != "" && opts.MetricsAddr != opts.Addr {
		// Keep /metrics off the main router so a route forwarding the main port
		// can't reach it; Run serves it on the dedicated listener.
		s.metricsHandler = metricsHandler
	} else {
		if opts.APIKey != "" {
			metricsHandler = bearerAuth(opts.APIKey, metricsHandler)
		}
		r.Handle("/metrics", metricsHandler)
	}

	return s
}

// Router exposes the underlying router so other packages can mount routes.
func (s *Server) Router() chi.Router { return s.router }

// MountUI arranges for the SPA handler to be served. When UIAddr is set and
// distinct from Addr, spa is served ONLY on that dedicated listener, which
// delegates requests matching an API route to the main router so the
// same-origin SPA can still call /v1 — keeping the token-embedding shell off
// the main port. Otherwise spa is mounted as a catch-all on the main router,
// serving it on the main port (single-listener default).
func (s *Server) MountUI(spa http.Handler) {
	if s.cfg.UIAddr != "" && s.cfg.UIAddr != s.cfg.Addr {
		mux, _ := s.router.(*chi.Mux)
		s.uiHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Requests that match an API route go to the main router (with its
			// middleware); everything else falls through to the SPA shell.
			if mux != nil && mux.Match(chi.NewRouteContext(), r.Method, r.URL.Path) {
				s.router.ServeHTTP(w, r)
				return
			}
			spa.ServeHTTP(w, r)
		})
		return
	}
	s.router.Handle("/*", spa)
}

// bearerAuth wraps h, requiring a valid bearer token.
func bearerAuth(key string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(token), []byte(key)) != 1 {
			http.Error(w, `{"error":"missing or invalid bearer token"}`, http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// SetReady installs the readiness check used by /readyz.
func (s *Server) SetReady(fn ReadinessFunc) { s.ready.Store(&fn) }

// SetDeps installs the dependency tracker rendered by GET /healthz?verbose=1.
// Unset (nil) is fine: the verbose handler renders ok defaults for every dep.
func (s *Server) SetDeps(t *DepTracker) { s.deps.Store(t) }

// SetLLMConfigured records whether the LLM pipeline is configured, for the
// "llm.configured" field in verbose healthz.
func (s *Server) SetLLMConfigured(v bool) { s.llmConfigured.Store(v) }

// Run starts the HTTP server and blocks until ctx is cancelled, then performs
// a graceful shutdown bounded by the configured timeout.
func (s *Server) Run(ctx context.Context) error {
	s.http = &http.Server{
		Addr:              s.cfg.Addr,
		Handler:           s.router,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		// No WriteTimeout: it is an absolute deadline on the whole response, which
		// would sever the long-lived MCP SSE stream at /mcp. IdleTimeout reaps
		// idle keep-alive connections instead.
		IdleTimeout: 120 * time.Second,
	}

	errCh := make(chan error, 3)
	go func() {
		s.log.Info("http server listening", "addr", s.cfg.Addr, "version", version.String())
		if err := s.http.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	// Dedicated UI listener (Options.UIAddr): serves the token-embedding SPA
	// shell (and delegates API routes to the main router), on a port meant to
	// stay behind a trusted gateway — kept off the main port.
	var uiSrv *http.Server
	if s.uiHandler != nil {
		uiSrv = &http.Server{
			Addr:              s.cfg.UIAddr,
			Handler:           s.uiHandler,
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       30 * time.Second,
			IdleTimeout:       120 * time.Second,
		}
		go func() {
			s.log.Info("ui server listening", "addr", s.cfg.UIAddr)
			if err := uiSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- err
			}
		}()
	}

	// Dedicated metrics listener (Options.MetricsAddr): serves only /metrics,
	// unauthenticated, on an in-cluster port.
	var metricsSrv *http.Server
	if s.metricsHandler != nil {
		mux := http.NewServeMux()
		mux.Handle("/metrics", s.metricsHandler)
		metricsSrv = &http.Server{
			Addr:              s.cfg.MetricsAddr,
			Handler:           mux,
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       30 * time.Second,
			IdleTimeout:       120 * time.Second,
		}
		go func() {
			s.log.Info("metrics server listening", "addr", s.cfg.MetricsAddr)
			if err := metricsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- err
			}
		}()
	}

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		s.log.Info("shutdown signal received, draining")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), s.cfg.ShutdownTimeout)
		defer cancel()
		if metricsSrv != nil {
			_ = metricsSrv.Shutdown(shutdownCtx)
		}
		if uiSrv != nil {
			_ = uiSrv.Shutdown(shutdownCtx)
		}
		return s.http.Shutdown(shutdownCtx)
	}
}
