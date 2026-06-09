package server

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// metrics holds the Prometheus collectors for the HTTP surface.
type metrics struct {
	reqTotal    *prometheus.CounterVec
	reqDuration *prometheus.HistogramVec
}

func newMetrics(reg prometheus.Registerer) *metrics {
	factory := promauto.With(reg)
	return &metrics{
		reqTotal: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "memini_http_requests_total",
			Help: "Total HTTP requests by route, method and status.",
		}, []string{"route", "method", "status"}),
		reqDuration: factory.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "memini_http_request_duration_seconds",
			Help:    "HTTP request latency by route and method.",
			Buckets: prometheus.DefBuckets,
		}, []string{"route", "method"}),
	}
}

// middleware records request counts and latency, labelling by the matched
// chi route pattern (low cardinality) rather than the raw path.
func (m *metrics) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)

		route := chi.RouteContext(r.Context()).RoutePattern()
		if route == "" {
			route = "unmatched"
		}
		m.reqTotal.WithLabelValues(route, r.Method, strconv.Itoa(ww.Status())).Inc()
		m.reqDuration.WithLabelValues(route, r.Method).Observe(time.Since(start).Seconds())
	})
}
